// Package agent runs the tool-calling loop: it sends the conversation to the
// LLM, executes the tools the model asks for, feeds results back, and keeps
// the conversation inside the model's context window.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/mycroft/agent-nexus/internal/llm"
	"github.com/mycroft/agent-nexus/internal/metrics"
	"github.com/mycroft/agent-nexus/internal/tools"
)

const (
	elidedNotice = "[output elided to save context; call the tool again if you still need it]"
	// keepRecentTools is how many of the latest tool outputs are never
	// elided, so the model keeps what it is working on.
	keepRecentTools = 2
	// Budget thresholds as fractions of the context window.
	elideAbove = 0.75
	elideTo    = 0.55
)

// Completer is the subset of *llm.Client the loop needs.
type Completer interface {
	Complete(ctx context.Context, r llm.Request) (llm.Completion, error)
}

// Step is one transcript entry.
type Step struct {
	Role             string
	ToolName         string
	Content          string
	PromptTokens     int
	CompletionTokens int
}

// Runner holds the loop settings.
type Runner struct {
	LLM Completer
	// ContextTokens is the model's context window.
	ContextTokens int
	// MaxTurns bounds the number of LLM calls in the tool loop.
	MaxTurns int
	// MaxToolOutput caps one tool result, in bytes.
	MaxToolOutput int
	// Record, if set, receives every transcript step.
	Record func(Step)
	Logger *slog.Logger
}

// Session is a conversation in progress.
type Session struct {
	r        *Runner
	tools    map[string]tools.Tool
	defs     []llm.ToolDef
	Messages []llm.Message
	// Usage accumulates token counts over all calls. Prompt tokens are
	// counted again on every call, so their sum overstates the work when
	// the server caches the shared prompt prefix.
	Usage llm.Usage
	// PeakPromptTokens is the largest prompt sent, i.e. the most context
	// the conversation used.
	PeakPromptTokens int
	// Turns is the number of LLM calls made by Run.
	Turns int
	// TurnLimitReached is set when Run stopped at MaxTurns.
	TurnLimitReached bool

	charsPerToken float64
}

// Run starts a conversation and loops until the model answers without
// calling a tool, or MaxTurns is reached. Hitting the turn limit is not an
// error: the caller can still ask for a final answer with Finish.
func (r *Runner) Run(ctx context.Context, system, user string, toolset []tools.Tool) (*Session, error) {
	s := &Session{
		r:             r,
		tools:         map[string]tools.Tool{},
		charsPerToken: 3,
		Messages: []llm.Message{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
	}
	for _, t := range toolset {
		s.tools[t.Name] = t
		s.defs = append(s.defs, llm.ToolDef{Name: t.Name, Description: t.Description, Parameters: t.Parameters})
	}
	r.record(Step{Role: "system", Content: system})
	r.record(Step{Role: "user", Content: user})

	for s.Turns < r.MaxTurns {
		comp, err := s.complete(ctx, llm.Request{Tools: s.defs})
		if err != nil {
			return s, err
		}
		s.Turns++
		msg := comp.Message
		for i := range msg.ToolCalls {
			if msg.ToolCalls[i].ID == "" {
				msg.ToolCalls[i].ID = fmt.Sprintf("call_%d_%d", s.Turns, i)
			}
			if msg.ToolCalls[i].Type == "" {
				msg.ToolCalls[i].Type = "function"
			}
		}
		s.Messages = append(s.Messages, msg)
		r.record(Step{
			Role:             "assistant",
			Content:          describeAssistant(msg),
			PromptTokens:     comp.Usage.PromptTokens,
			CompletionTokens: comp.Usage.CompletionTokens,
		})
		if len(msg.ToolCalls) == 0 {
			return s, nil
		}
		for _, call := range msg.ToolCalls {
			out := s.runTool(ctx, call)
			s.Messages = append(s.Messages, llm.Message{Role: "tool", ToolCallID: call.ID, Content: out})
			r.record(Step{Role: "tool", ToolName: call.Function.Name, Content: out})
		}
		if err := ctx.Err(); err != nil {
			return s, err
		}
	}
	s.TurnLimitReached = true
	return s, nil
}

// Finish appends instruction as a user message and asks for a
// schema-constrained answer without tools. validate checks the decoded
// answer; on failure the model gets one retry with the error.
func (s *Session) Finish(ctx context.Context, instruction string, schema llm.JSONSchema, validate func([]byte) error) error {
	s.Messages = append(s.Messages, llm.Message{Role: "user", Content: instruction})
	s.r.record(Step{Role: "user", Content: instruction})

	var lastErr error
	for attempt := range 2 {
		comp, err := s.complete(ctx, llm.Request{Schema: &schema, DisableThinking: true})
		if err != nil {
			return err
		}
		content := comp.Message.Content
		s.r.record(Step{
			Role:             "assistant",
			Content:          content,
			PromptTokens:     comp.Usage.PromptTokens,
			CompletionTokens: comp.Usage.CompletionTokens,
		})
		if lastErr = validate([]byte(content)); lastErr == nil {
			return nil
		}
		if attempt == 0 {
			retry := fmt.Sprintf("That answer was rejected: %v. Reply again with the complete, corrected JSON document.", lastErr)
			s.Messages = append(s.Messages,
				llm.Message{Role: "assistant", Content: content},
				llm.Message{Role: "user", Content: retry})
			s.r.record(Step{Role: "user", Content: retry})
		}
	}
	return fmt.Errorf("final answer rejected twice: %w", lastErr)
}

// complete sends the conversation, eliding old tool output when it nears
// the context window, and once more after a context-size rejection.
func (s *Session) complete(ctx context.Context, req llm.Request) (llm.Completion, error) {
	limit := float64(s.r.ContextTokens)
	if s.estimateTokens() > elideAbove*limit {
		s.elide(int(elideTo * limit))
	}
	for attempt := 0; ; attempt++ {
		req.Messages = s.Messages
		comp, err := s.r.LLM.Complete(ctx, req)
		if errors.Is(err, llm.ErrContextExceeded) && attempt == 0 {
			// The server measured more tokens than the window: recalibrate
			// so the estimate agrees, then elide down to the target.
			s.charsPerToken = min(s.charsPerToken, float64(chars(s.Messages))/limit)
			if s.elide(int(elideTo*limit)) == 0 {
				return comp, err
			}
			s.r.logger().WarnContext(ctx, "context exceeded, retrying with elided tool output")
			continue
		}
		if err != nil {
			return comp, err
		}
		s.Usage.PromptTokens += comp.Usage.PromptTokens
		s.Usage.CompletionTokens += comp.Usage.CompletionTokens
		s.PeakPromptTokens = max(s.PeakPromptTokens, comp.Usage.PromptTokens)
		if comp.Usage.PromptTokens > 0 {
			s.charsPerToken = float64(chars(s.Messages)) / float64(comp.Usage.PromptTokens)
		}
		return comp, nil
	}
}

func (s *Session) estimateTokens() float64 {
	return float64(chars(s.Messages)) / s.charsPerToken
}

// elide drops reasoning from old assistant messages, then replaces the
// oldest tool outputs with a notice, until the estimate is under target.
// It returns the number of messages changed.
func (s *Session) elide(target int) int {
	changed := 0
	lastAssistant := -1
	for i, m := range s.Messages {
		if m.Role == "assistant" {
			lastAssistant = i
		}
	}
	for i := range s.Messages {
		if s.estimateTokens() <= float64(target) {
			return changed
		}
		if s.Messages[i].Role == "assistant" && i != lastAssistant && s.Messages[i].ReasoningContent != "" {
			s.Messages[i].ReasoningContent = ""
			changed++
		}
	}
	var toolIdx []int
	for i, m := range s.Messages {
		if m.Role == "tool" && m.Content != elidedNotice {
			toolIdx = append(toolIdx, i)
		}
	}
	for n, i := range toolIdx {
		if s.estimateTokens() <= float64(target) || n >= len(toolIdx)-keepRecentTools {
			break
		}
		s.Messages[i].Content = elidedNotice
		changed++
	}
	return changed
}

func (s *Session) runTool(ctx context.Context, call llm.ToolCall) string {
	t, ok := s.tools[call.Function.Name]
	if !ok {
		metrics.ToolCallsTotal.WithLabelValues("unknown", "error").Inc()
		return fmt.Sprintf("error: unknown tool %q", call.Function.Name)
	}
	out, err := t.Run(ctx, json.RawMessage(call.Function.Arguments))
	if err != nil {
		metrics.ToolCallsTotal.WithLabelValues(t.Name, "error").Inc()
		s.r.logger().DebugContext(ctx, "tool error", "tool", t.Name, "error", err)
		return "error: " + err.Error()
	}
	metrics.ToolCallsTotal.WithLabelValues(t.Name, "ok").Inc()
	maxOut := s.r.MaxToolOutput
	if maxOut > 0 && len(out) > maxOut {
		out = strings.ToValidUTF8(out[:maxOut], "") + "\n[output truncated; narrow the request]"
	}
	return out
}

func (r *Runner) record(step Step) {
	if r.Record != nil {
		r.Record(step)
	}
}

func (r *Runner) logger() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}

func describeAssistant(m llm.Message) string {
	var b strings.Builder
	if m.ReasoningContent != "" {
		b.WriteString("<reasoning>\n")
		b.WriteString(m.ReasoningContent)
		b.WriteString("\n</reasoning>\n")
	}
	b.WriteString(m.Content)
	for _, c := range m.ToolCalls {
		fmt.Fprintf(&b, "\n→ %s(%s)", c.Function.Name, c.Function.Arguments)
	}
	return b.String()
}

func chars(msgs []llm.Message) int {
	n := 0
	for _, m := range msgs {
		n += len(m.Content) + len(m.ReasoningContent) + 16
		for _, c := range m.ToolCalls {
			n += len(c.Function.Name) + len(c.Function.Arguments) + 16
		}
	}
	return n
}

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
	"time"

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
	// finishAttempts is how many times the final answer is asked in the
	// cache-friendly form before the schema-constrained fallback.
	finishAttempts = 3
	// maxRepeatTurns ends the investigation after this many consecutive
	// turns whose tool calls all repeat earlier ones: at temperature 0 the
	// same conversation keeps producing the same turn.
	maxRepeatTurns = 2
	// minTurnEstimate is the least time a turn is assumed to take when
	// planning around the context's deadline.
	minTurnEstimate = time.Minute
	// deadlineMargin is left after the final answer, to post it.
	deadlineMargin = 30 * time.Second
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
	// now is the clock; tests replace it.
	now func() time.Time
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
	// TurnLimitReached is set when Run cut the investigation short: at
	// MaxTurns, because the model kept repeating its tool calls, or to
	// leave time for the final answer before the context's deadline.
	TurnLimitReached bool

	charsPerToken float64
	// calls maps a tool call (name and canonical arguments) to the index
	// of its result in Messages.
	calls map[string]int
	// repeatTurns counts consecutive turns made only of repeated calls.
	repeatTurns int
	// longestTurn is the slowest LLM call of the loop so far.
	longestTurn time.Duration
}

// Run starts a conversation and loops until the model answers without
// calling a tool, MaxTurns is reached, or the context's deadline leaves
// time for no more than the final answer. Cutting the investigation short
// is not an error: the caller can still ask for a final answer with
// Finish, instead of the job timing out with nothing to show.
func (r *Runner) Run(ctx context.Context, system, user string, toolset []tools.Tool) (*Session, error) {
	s := &Session{
		r:             r,
		tools:         map[string]tools.Tool{},
		charsPerToken: 3,
		calls:         map[string]int{},
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
		if s.nearDeadline(ctx) {
			r.logger().WarnContext(ctx, "ending the investigation to answer before the deadline", "turns", s.Turns, "longest_turn", s.longestTurn.Round(time.Second))
			break
		}
		start := r.clock()
		comp, err := s.complete(ctx, llm.Request{Tools: s.defs})
		if err != nil {
			return s, err
		}
		s.longestTurn = max(s.longestTurn, r.clock().Sub(start))
		s.Turns++
		msg := s.assistant(comp)
		if len(msg.ToolCalls) == 0 {
			return s, nil
		}
		if s.runTools(ctx, msg) {
			s.repeatTurns++
		} else {
			s.repeatTurns = 0
		}
		if err := ctx.Err(); err != nil {
			return s, err
		}
		if s.repeatTurns >= maxRepeatTurns {
			r.logger().WarnContext(ctx, "the model keeps repeating its tool calls; ending the investigation", "turns", s.Turns)
			break
		}
	}
	s.TurnLimitReached = true
	return s, nil
}

// nearDeadline reports whether the context's deadline leaves too little
// time for another turn and the final answer, each estimated as long as
// the slowest turn so far, plus a margin to post the result.
func (s *Session) nearDeadline(ctx context.Context) bool {
	deadline, ok := ctx.Deadline()
	if !ok {
		return false
	}
	turn := max(s.longestTurn, minTurnEstimate)
	return deadline.Sub(s.r.clock()) < 2*turn+deadlineMargin
}

// Finish asks for the final answer, a JSON document matching schema, and
// checks it with validate.
//
// It first asks exactly like the tool loop does: same tools, no grammar,
// no template options. Changing any of them changes the start of the
// prompt, and llama-server then re-processes the whole conversation
// instead of reusing its prompt cache, which takes minutes on a long
// review. Such an answer is free text, so it is parsed and validated, with
// one corrected retry. Only then does Finish fall back to a
// schema-constrained request without tools, which always parses but pays
// for the whole prompt.
//
// Tools stay declared, so the model may still call one: the call is
// refused rather than run, since a new result only invites more
// investigation.
func (s *Session) Finish(ctx context.Context, instruction string, schema llm.JSONSchema, validate func([]byte) error) error {
	s.say(instruction + "\n\nReply with only a JSON object matching this JSON Schema, without a code fence and without calling a tool:\n" + string(schema.Schema))

	var lastErr error
	for range finishAttempts {
		comp, err := s.complete(ctx, llm.Request{Tools: s.defs})
		if err != nil {
			return err
		}
		msg := s.assistant(comp)
		if len(msg.ToolCalls) > 0 {
			// Answer the calls, so the conversation stays well formed.
			s.refuseTools(msg)
			lastErr = errors.New("you called a tool instead of answering")
		} else if lastErr = validate([]byte(extractJSON(msg.Content))); lastErr == nil {
			return nil
		}
		s.say(fmt.Sprintf("That answer was rejected: %v. Reply again with only the complete, corrected JSON object.", lastErr))
	}

	s.r.logger().WarnContext(ctx, "falling back to a schema-constrained answer", "error", lastErr)
	comp, err := s.complete(ctx, llm.Request{Schema: &schema, DisableThinking: true})
	if err != nil {
		return err
	}
	msg := s.assistant(comp)
	if err := validate([]byte(msg.Content)); err != nil {
		return fmt.Errorf("final answer rejected: %w", err)
	}
	return nil
}

// say appends a user message.
func (s *Session) say(content string) {
	s.Messages = append(s.Messages, llm.Message{Role: "user", Content: content})
	s.r.record(Step{Role: "user", Content: content})
}

// assistant appends and records the model's message, giving its tool calls
// the IDs and type llama-server may leave out.
func (s *Session) assistant(comp llm.Completion) llm.Message {
	msg := comp.Message
	for i := range msg.ToolCalls {
		if msg.ToolCalls[i].ID == "" {
			msg.ToolCalls[i].ID = fmt.Sprintf("call_%d_%d", len(s.Messages), i)
		}
		if msg.ToolCalls[i].Type == "" {
			msg.ToolCalls[i].Type = "function"
		}
	}
	s.Messages = append(s.Messages, msg)
	s.r.record(Step{
		Role:             "assistant",
		Content:          describeAssistant(msg),
		PromptTokens:     comp.Usage.PromptTokens,
		CompletionTokens: comp.Usage.CompletionTokens,
	})
	return msg
}

// runTools executes the tool calls of msg and appends their results. A
// call repeating an earlier one whose output is still in the conversation
// is not run again: the model is pointed at that output. It reports
// whether every call was such a repeat.
func (s *Session) runTools(ctx context.Context, msg llm.Message) bool {
	allRepeats := len(msg.ToolCalls) > 0
	for _, call := range msg.ToolCalls {
		sig := callSignature(call)
		var out string
		if i, seen := s.calls[sig]; seen && s.Messages[i].Content != elidedNotice {
			metrics.ToolCallsTotal.WithLabelValues(call.Function.Name, "repeat").Inc()
			out = fmt.Sprintf("You already called %s with these arguments and its output, above, has not changed. Use it, or stop investigating and give your answer.", call.Function.Name)
		} else {
			allRepeats = false
			out = s.runTool(ctx, call)
			s.calls[sig] = len(s.Messages)
		}
		s.Messages = append(s.Messages, llm.Message{Role: "tool", ToolCallID: call.ID, Content: out})
		s.r.record(Step{Role: "tool", ToolName: call.Function.Name, Content: out})
	}
	return allRepeats
}

// refuseTools answers the tool calls of msg without running them.
func (s *Session) refuseTools(msg llm.Message) {
	for _, call := range msg.ToolCalls {
		metrics.ToolCallsTotal.WithLabelValues(call.Function.Name, "refused").Inc()
		out := "The investigation is over: tools are closed. Answer now with only the JSON object."
		s.Messages = append(s.Messages, llm.Message{Role: "tool", ToolCallID: call.ID, Content: out})
		s.r.record(Step{Role: "tool", ToolName: call.Function.Name, Content: out})
	}
}

// callSignature identifies a call by its name and arguments, with the
// arguments re-encoded so key order and spacing do not matter.
func callSignature(call llm.ToolCall) string {
	args := call.Function.Arguments
	var v any
	if json.Unmarshal([]byte(args), &v) == nil {
		if canonical, err := json.Marshal(v); err == nil {
			args = string(canonical)
		}
	}
	return call.Function.Name + "\x00" + args
}

// extractJSON returns the JSON object in a free-text answer: models often
// wrap it in a code fence or a sentence.
func extractJSON(content string) string {
	start, end := strings.Index(content, "{"), strings.LastIndex(content, "}")
	if start < 0 || end < start {
		return strings.TrimSpace(content)
	}
	return content[start : end+1]
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

func (r *Runner) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
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

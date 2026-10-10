package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/mycroft/agent-nexus/internal/llm"
	"github.com/mycroft/agent-nexus/internal/tools"
)

// script is a fake LLM returning canned completions in order and keeping
// the requests it saw.
type script struct {
	replies  []func(llm.Request) (llm.Completion, error)
	requests []llm.Request
}

func (s *script) Complete(_ context.Context, r llm.Request) (llm.Completion, error) {
	r.Messages = append([]llm.Message(nil), r.Messages...)
	s.requests = append(s.requests, r)
	if len(s.replies) == 0 {
		return llm.Completion{}, errors.New("script exhausted")
	}
	next := s.replies[0]
	s.replies = s.replies[1:]
	return next(r)
}

// reply returns a canned completion whose prompt token count is derived
// from the request, like a real server's (3 characters per token).
func reply(content string, calls ...llm.ToolCall) func(llm.Request) (llm.Completion, error) {
	return func(r llm.Request) (llm.Completion, error) {
		return llm.Completion{
			Message: llm.Message{Role: "assistant", Content: content, ToolCalls: calls},
			Usage:   llm.Usage{PromptTokens: chars(r.Messages) / 3, CompletionTokens: 10},
		}, nil
	}
}

func call(name, args string) llm.ToolCall {
	return llm.ToolCall{Function: llm.FunctionCall{Name: name, Arguments: args}}
}

var echo = tools.Tool{
	Name:       "echo",
	Parameters: json.RawMessage(`{"type":"object"}`),
	Run: func(_ context.Context, args json.RawMessage) (string, error) {
		var a struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return "", err
		}
		if a.Text == "fail" {
			return "", errors.New("boom")
		}
		return "echo: " + a.Text, nil
	},
}

func TestRunExecutesToolsUntilAnswer(t *testing.T) {
	fake := &script{replies: []func(llm.Request) (llm.Completion, error){
		reply("", call("echo", `{"text":"one"}`), call("missing", `{}`), call("echo", `{"text":"fail"}`)),
		reply("done"),
	}}
	var steps []Step
	r := &Runner{LLM: fake, ContextTokens: 100000, MaxTurns: 5, Record: func(s Step) { steps = append(steps, s) }}
	s, err := r.Run(context.Background(), "sys", "user", []tools.Tool{echo})
	if err != nil {
		t.Fatal(err)
	}
	if s.Turns != 2 || s.TurnLimitReached {
		t.Fatalf("turns = %d, limit = %v", s.Turns, s.TurnLimitReached)
	}
	second := fake.requests[1].Messages
	var results []string
	for _, m := range second {
		if m.Role == "tool" {
			if m.ToolCallID == "" {
				t.Error("tool result without a call ID")
			}
			results = append(results, m.Content)
		}
	}
	want := []string{"echo: one", `error: unknown tool "missing"`, "error: boom"}
	if strings.Join(results, "|") != strings.Join(want, "|") {
		t.Errorf("tool results = %q, want %q", results, want)
	}
	if len(fake.requests[0].Tools) != 1 || fake.requests[0].Tools[0].Name != "echo" {
		t.Errorf("tool definitions not sent: %+v", fake.requests[0].Tools)
	}
	if s.Usage.PromptTokens == 0 || s.Usage.CompletionTokens != 20 || len(steps) != 2+1+3+1 {
		t.Errorf("usage = %+v, steps = %d", s.Usage, len(steps))
	}
	// The second prompt holds the first plus the tool results: it is the
	// peak, and smaller than the sum over both calls.
	if second := chars(fake.requests[1].Messages) / 3; s.PeakPromptTokens != second || s.PeakPromptTokens >= s.Usage.PromptTokens {
		t.Errorf("peak = %d, want %d (sum %d)", s.PeakPromptTokens, second, s.Usage.PromptTokens)
	}
}

func TestRunStopsAtTurnLimit(t *testing.T) {
	loop := func(llm.Request) (llm.Completion, error) {
		return llm.Completion{Message: llm.Message{ToolCalls: []llm.ToolCall{call("echo", `{"text":"x"}`)}}}, nil
	}
	fake := &script{replies: []func(llm.Request) (llm.Completion, error){loop, loop, loop}}
	r := &Runner{LLM: fake, ContextTokens: 100000, MaxTurns: 2}
	s, err := r.Run(context.Background(), "sys", "user", []tools.Tool{echo})
	if err != nil {
		t.Fatal(err)
	}
	if !s.TurnLimitReached || s.Turns != 2 {
		t.Fatalf("turns = %d, limit = %v", s.Turns, s.TurnLimitReached)
	}
}

func TestFinishRetriesInvalidAnswer(t *testing.T) {
	fake := &script{replies: []func(llm.Request) (llm.Completion, error){
		reply("ready"),
		reply(`{"ok": false}`),
		reply(`{"ok": true}`),
	}}
	r := &Runner{LLM: fake, ContextTokens: 100000, MaxTurns: 5}
	s, err := r.Run(context.Background(), "sys", "user", nil)
	if err != nil {
		t.Fatal(err)
	}
	validate := func(raw []byte) error {
		var v struct {
			OK bool `json:"ok"`
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			return err
		}
		if !v.OK {
			return errors.New("not ok")
		}
		return nil
	}
	if err := s.Finish(context.Background(), "answer", llm.JSONSchema{Name: "x", Schema: json.RawMessage(`{}`)}, validate); err != nil {
		t.Fatal(err)
	}
	final := fake.requests[2]
	if final.Schema == nil || !final.DisableThinking || len(final.Tools) != 0 {
		t.Errorf("final request = %+v", final)
	}
	last := final.Messages[len(final.Messages)-1]
	if !strings.Contains(last.Content, "not ok") {
		t.Errorf("retry message does not carry the validation error: %q", last.Content)
	}
}

func TestFinishFailsAfterTwoInvalidAnswers(t *testing.T) {
	fake := &script{replies: []func(llm.Request) (llm.Completion, error){reply("ready"), reply("x"), reply("y")}}
	r := &Runner{LLM: fake, ContextTokens: 100000, MaxTurns: 5}
	s, err := r.Run(context.Background(), "sys", "user", nil)
	if err != nil {
		t.Fatal(err)
	}
	err = s.Finish(context.Background(), "answer", llm.JSONSchema{Name: "x"}, func([]byte) error { return errors.New("bad") })
	if err == nil {
		t.Fatal("Finish accepted invalid answers")
	}
}

// Large tool outputs are elided (oldest first, keeping the latest two)
// once the conversation nears the context window.
func TestElidesOldToolOutput(t *testing.T) {
	big := tools.Tool{
		Name:       "big",
		Parameters: json.RawMessage(`{"type":"object"}`),
		Run: func(context.Context, json.RawMessage) (string, error) {
			return strings.Repeat("x", 3000), nil
		},
	}
	var replies []func(llm.Request) (llm.Completion, error)
	for i := range 5 {
		replies = append(replies, reply("", llm.ToolCall{ID: strconv.Itoa(i), Function: llm.FunctionCall{Name: "big", Arguments: "{}"}}))
	}
	replies = append(replies, reply("done"))
	fake := &script{replies: replies}
	r := &Runner{LLM: fake, ContextTokens: 4000, MaxTurns: 10}
	if _, err := r.Run(context.Background(), "sys", "user", []tools.Tool{big}); err != nil {
		t.Fatal(err)
	}
	last := fake.requests[len(fake.requests)-1].Messages
	var elided, kept int
	for _, m := range last {
		if m.Role != "tool" {
			continue
		}
		if m.Content == elidedNotice {
			elided++
		} else {
			kept++
		}
	}
	if elided == 0 || kept < keepRecentTools {
		t.Errorf("elided = %d, kept = %d", elided, kept)
	}
}

func TestRetriesAfterContextExceeded(t *testing.T) {
	big := tools.Tool{
		Name:       "big",
		Parameters: json.RawMessage(`{"type":"object"}`),
		Run: func(context.Context, json.RawMessage) (string, error) {
			return strings.Repeat("y", 500), nil
		},
	}
	exceeded := func(llm.Request) (llm.Completion, error) {
		return llm.Completion{}, fmt.Errorf("wrapped: %w", llm.ErrContextExceeded)
	}
	fake := &script{replies: []func(llm.Request) (llm.Completion, error){
		reply("", llm.ToolCall{ID: "a", Function: llm.FunctionCall{Name: "big", Arguments: "{}"}}),
		reply("", llm.ToolCall{ID: "b", Function: llm.FunctionCall{Name: "big", Arguments: "{}"}}),
		reply("", llm.ToolCall{ID: "c", Function: llm.FunctionCall{Name: "big", Arguments: "{}"}}),
		exceeded,
		reply("done"),
	}}
	r := &Runner{LLM: fake, ContextTokens: 1_000_000, MaxTurns: 10}
	s, err := r.Run(context.Background(), "sys", "user", []tools.Tool{big})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if s.Turns != 4 {
		t.Errorf("turns = %d, want 4", s.Turns)
	}
}

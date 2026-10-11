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

func okValidator(raw []byte) error {
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

var okSchema = llm.JSONSchema{Name: "x", Schema: json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}}}`)}

// The final answer is asked like the tool loop's turns (same tools, no
// grammar, no template options), so llama-server reuses its prompt cache.
func TestFinishAsksLikeTheToolLoop(t *testing.T) {
	fake := &script{replies: []func(llm.Request) (llm.Completion, error){
		reply("", call("echo", `{"text":"x"}`)),
		reply("ready"),
		reply("Here it is:\n```json\n{\"ok\": false}\n```"),
		reply(`{"ok": true}`),
	}}
	r := &Runner{LLM: fake, ContextTokens: 100000, MaxTurns: 5}
	s, err := r.Run(context.Background(), "sys", "user", []tools.Tool{echo})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(context.Background(), "answer", okSchema, okValidator); err != nil {
		t.Fatal(err)
	}
	loop, final := fake.requests[1], fake.requests[2]
	if final.Schema != nil || final.DisableThinking || len(final.Tools) != len(loop.Tools) || len(final.Tools) != 1 {
		t.Errorf("final request = %+v, want the tool loop's shape", final)
	}
	ask := final.Messages[len(final.Messages)-1].Content
	if !strings.HasPrefix(ask, "answer\n\nReply with only a JSON object") || !strings.Contains(ask, `"ok":{"type":"boolean"}`) {
		t.Errorf("final instruction = %q", ask)
	}
	retry := fake.requests[3].Messages[len(fake.requests[3].Messages)-1].Content
	if !strings.Contains(retry, "not ok") || fake.requests[3].Schema != nil {
		t.Errorf("retry = %q (schema %v)", retry, fake.requests[3].Schema)
	}
}

// Asked for its answer, a model may still call a tool: the call is
// refused, not run, since a new result invites more investigation.
func TestFinishRefusesToolsCalledAtTheEnd(t *testing.T) {
	runs := 0
	counted := tools.Tool{
		Name:       "read",
		Parameters: json.RawMessage(`{"type":"object"}`),
		Run: func(context.Context, json.RawMessage) (string, error) {
			runs++
			return "content", nil
		},
	}
	fake := &script{replies: []func(llm.Request) (llm.Completion, error){
		reply("ready"),
		reply("", call("read", `{"path":"late"}`)),
		reply(`{"ok": true}`),
	}}
	r := &Runner{LLM: fake, ContextTokens: 100000, MaxTurns: 5}
	s, err := r.Run(context.Background(), "sys", "user", []tools.Tool{counted})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(context.Background(), "answer", okSchema, okValidator); err != nil {
		t.Fatal(err)
	}
	msgs := fake.requests[2].Messages
	refusal, retry := msgs[len(msgs)-2], msgs[len(msgs)-1]
	if runs != 0 || refusal.Role != "tool" || !strings.HasPrefix(refusal.Content, "The investigation is over") || !strings.Contains(retry.Content, "called a tool") {
		t.Errorf("runs = %d, late tool call answered with %+v", runs, msgs[len(msgs)-2:])
	}
}

func TestFinishFallsBackToSchema(t *testing.T) {
	fake := &script{replies: []func(llm.Request) (llm.Completion, error){reply("ready"), reply("x"), reply("y"), reply("z"), reply(`{"ok": true}`)}}
	r := &Runner{LLM: fake, ContextTokens: 100000, MaxTurns: 5}
	s, err := r.Run(context.Background(), "sys", "user", []tools.Tool{echo})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(context.Background(), "answer", okSchema, okValidator); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= finishAttempts; i++ {
		if fake.requests[i].Schema != nil {
			t.Errorf("attempt %d used the schema before the fallback", i)
		}
	}
	last := fake.requests[finishAttempts+1]
	if last.Schema == nil || !last.DisableThinking || len(last.Tools) != 0 {
		t.Errorf("fallback request = %+v", last)
	}
}

func TestFinishFailsAfterFallback(t *testing.T) {
	fake := &script{replies: []func(llm.Request) (llm.Completion, error){reply("ready"), reply("x"), reply("y"), reply("z"), reply("w")}}
	r := &Runner{LLM: fake, ContextTokens: 100000, MaxTurns: 5}
	s, err := r.Run(context.Background(), "sys", "user", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(context.Background(), "answer", okSchema, func([]byte) error { return errors.New("bad") }); err == nil {
		t.Fatal("Finish accepted invalid answers")
	}
	if want := 1 + finishAttempts + 1; len(fake.requests) != want {
		t.Errorf("requests = %d, want %d (loop, answers, fallback)", len(fake.requests), want)
	}
}

func TestExtractJSON(t *testing.T) {
	for in, want := range map[string]string{
		`{"a":1}`:                         `{"a":1}`,
		"```json\n{\"a\":{\"b\":2}}\n```": `{"a":{"b":2}}`,
		"Sure: {\"a\":1} done":            `{"a":1}`,
		" no json ":                       "no json",
	} {
		if got := extractJSON(in); got != want {
			t.Errorf("extractJSON(%q) = %q, want %q", in, got, want)
		}
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
		replies = append(replies, reply("", llm.ToolCall{ID: strconv.Itoa(i), Function: llm.FunctionCall{Name: "big", Arguments: fmt.Sprintf(`{"n":%d}`, i)}}))
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
		reply("", llm.ToolCall{ID: "a", Function: llm.FunctionCall{Name: "big", Arguments: `{"n":1}`}}),
		reply("", llm.ToolCall{ID: "b", Function: llm.FunctionCall{Name: "big", Arguments: `{"n":2}`}}),
		reply("", llm.ToolCall{ID: "c", Function: llm.FunctionCall{Name: "big", Arguments: `{"n":3}`}}),
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

// A model repeating the same call (at temperature 0 the same conversation
// repeats itself) is pointed at the output it already has, and two turns
// of repeats end the investigation.
func TestRunStopsRepeatedCalls(t *testing.T) {
	runs := 0
	counted := tools.Tool{
		Name:       "read",
		Parameters: json.RawMessage(`{"type":"object"}`),
		Run: func(context.Context, json.RawMessage) (string, error) {
			runs++
			return "content", nil
		},
	}
	same := func(args string) func(llm.Request) (llm.Completion, error) {
		return reply("", call("read", args))
	}
	fake := &script{replies: []func(llm.Request) (llm.Completion, error){
		same(`{"path":"a","line":1}`),
		same(`{"line": 1, "path": "a"}`), // same call, other key order
		same(`{"path":"b"}`),             // a new call resets the count
		same(`{"path":"a","line":1}`),
		same(`{"path":"b"}`),
		same(`{"path":"c"}`),
	}}
	r := &Runner{LLM: fake, ContextTokens: 100000, MaxTurns: 10}
	s, err := r.Run(context.Background(), "sys", "user", []tools.Tool{counted})
	if err != nil {
		t.Fatal(err)
	}
	if s.Turns != 5 || !s.TurnLimitReached || runs != 2 {
		t.Errorf("turns = %d, cut short = %v, tool runs = %d; want 5, true, 2", s.Turns, s.TurnLimitReached, runs)
	}
	last := s.Messages[len(s.Messages)-1]
	if last.Role != "tool" || !strings.HasPrefix(last.Content, "You already called read") {
		t.Errorf("repeat answered with %q", last.Content)
	}
}

func TestRepeatAfterElisionRuns(t *testing.T) {
	runs := 0
	counted := tools.Tool{
		Name:       "read",
		Parameters: json.RawMessage(`{"type":"object"}`),
		Run: func(context.Context, json.RawMessage) (string, error) {
			runs++
			return "content", nil
		},
	}
	s := &Session{r: &Runner{}, tools: map[string]tools.Tool{"read": counted}, calls: map[string]int{}}
	msg := llm.Message{ToolCalls: []llm.ToolCall{call("read", `{"path":"a"}`)}}
	s.runTools(context.Background(), msg)
	s.Messages[len(s.Messages)-1].Content = elidedNotice
	if s.runTools(context.Background(), msg) || runs != 2 {
		t.Errorf("a call whose output was elided must run again (runs = %d)", runs)
	}
}

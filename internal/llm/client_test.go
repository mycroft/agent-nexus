package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mycroft/agent-nexus/internal/httpclient"
)

func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(srv.URL+"/v1", "secret", "test-model", 5*time.Second,
		WithHTTPClient(httpclient.New(httpclient.WithRetry(2, time.Millisecond, time.Millisecond), httpclient.WithRetryPOST())))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCompleteToolCallRoundTrip(t *testing.T) {
	var got map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("unexpected request %s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(w, `{"model":"m","choices":[{"finish_reason":"tool_calls","message":{
			"role":"assistant","content":"","reasoning_content":"think",
			"tool_calls":[{"id":"c1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a\"}"}}]}}],
			"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	})
	comp, err := c.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
		Tools:    []ToolDef{{Name: "read_file", Description: "d", Parameters: json.RawMessage(`{"type":"object"}`)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got["model"] != "test-model" || got["response_format"] != nil || got["chat_template_kwargs"] != nil || got["thinking_budget_tokens"] != nil {
		t.Errorf("unexpected request body: %v", got)
	}
	tools, _ := got["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["type"] != "function" {
		t.Errorf("tools not sent as functions: %v", got["tools"])
	}
	if len(comp.Message.ToolCalls) != 1 || comp.Message.ToolCalls[0].Function.Name != "read_file" ||
		comp.Message.ReasoningContent != "think" || comp.Usage.PromptTokens != 10 || comp.Message.Role != "assistant" {
		t.Errorf("unexpected completion: %+v", comp)
	}
}

func TestCompleteSchema(t *testing.T) {
	var got map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{}"}}]}`)
	})
	_, err := c.Complete(context.Background(), Request{
		Messages:        []Message{{Role: "user", Content: "hi"}},
		Schema:          &JSONSchema{Name: "out", Schema: json.RawMessage(`{"type":"object"}`)},
		DisableThinking: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	rf, _ := got["response_format"].(map[string]any)
	js, _ := rf["json_schema"].(map[string]any)
	if rf["type"] != "json_schema" || js["schema"] == nil {
		t.Errorf("response_format = %v", got["response_format"])
	}
	kw, _ := got["chat_template_kwargs"].(map[string]any)
	if kw["enable_thinking"] != false {
		t.Errorf("chat_template_kwargs = %v", got["chat_template_kwargs"])
	}

	if _, err := c.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
		Schema:   &JSONSchema{Name: "out", Schema: json.RawMessage(`{}`)},
		Tools:    []ToolDef{{Name: "x"}},
	}); err == nil {
		t.Error("schema combined with tools was accepted")
	}
}

func TestThinkingBudget(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = nil
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{}"}}]}`)
	}))
	defer srv.Close()
	c, err := New(srv.URL+"/v1", "", "test-model", 5*time.Second, WithThinkingBudget(6144))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Complete(context.Background(), Request{Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatal(err)
	}
	if got["thinking_budget_tokens"] != float64(6144) {
		t.Errorf("thinking_budget_tokens = %v, want 6144", got["thinking_budget_tokens"])
	}
	// Without thinking, a budget means nothing: it is not sent.
	if _, err := c.Complete(context.Background(), Request{Messages: []Message{{Role: "user", Content: "hi"}}, DisableThinking: true}); err != nil {
		t.Fatal(err)
	}
	if got["thinking_budget_tokens"] != nil {
		t.Errorf("thinking_budget_tokens = %v with thinking disabled", got["thinking_budget_tokens"])
	}
}

func TestCompleteContextExceeded(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"code":400,"type":"exceed_context_size_error","n_prompt_tokens":9000,"n_ctx":8192}}`)
	})
	_, err := c.Complete(context.Background(), Request{Messages: []Message{{Role: "user", Content: "x"}}})
	if !errors.Is(err, ErrContextExceeded) {
		t.Fatalf("error = %v, want ErrContextExceeded", err)
	}
}

func TestCompleteRetriesServerErrors(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	})
	comp, err := c.Complete(context.Background(), Request{Messages: []Message{{Role: "user", Content: "x"}}})
	if err != nil || comp.Message.Content != "ok" || calls.Load() != 2 {
		t.Fatalf("Complete = %+v, %v after %d calls", comp, err, calls.Load())
	}
}

func TestContextSize(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/props" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"default_generation_settings":{"n_ctx":32768}}`)
	})
	n, err := c.ContextSize(context.Background())
	if err != nil || n != 32768 {
		t.Fatalf("ContextSize = %d, %v", n, err)
	}
}

func TestNewValidatesBaseURL(t *testing.T) {
	for _, u := range []string{"", "llm-api", "ftp://x/v1", "https://x/v1?a=b"} {
		if _, err := New(u, "", "m", time.Second); err == nil {
			t.Errorf("New accepted base URL %q", u)
		}
	}
	if _, err := New("https://x/v1", "", "", time.Second); err == nil {
		t.Error("New accepted an empty model")
	}
}

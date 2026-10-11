// Package llm is a client for OpenAI-compatible chat-completions APIs, with
// the llama.cpp llama-server extensions agent-nexus relies on
// (reasoning_content, chat_template_kwargs, /props). It owns only the wire
// contract; the tool loop lives in internal/agent.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mycroft/agent-nexus/internal/httpclient"
	"github.com/mycroft/agent-nexus/internal/metrics"
)

const (
	chatCompletionsPath = "/chat/completions"
	maxResponseBytes    = 16 << 20
)

// ErrContextExceeded is returned when the prompt does not fit the server's
// context window (llama-server: HTTP 400 exceed_context_size_error).
var ErrContextExceeded = errors.New("prompt exceeds the LLM context size")

// Message is one chat message. Assistant messages may carry tool calls and
// reasoning; tool messages answer a tool call by ID.
type Message struct {
	Role             string     `json:"role"`
	Content          string     `json:"content"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
}

// ToolCall is a function call requested by the model.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// FunctionCall names the function and carries its JSON-encoded arguments.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolDef declares a function the model may call. Parameters is a JSON
// Schema object.
type ToolDef struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// JSONSchema constrains the response to a JSON document. llama-server turns
// it into a grammar, so the output always parses.
type JSONSchema struct {
	Name   string
	Schema json.RawMessage
}

// Request is one chat-completion call.
type Request struct {
	Messages []Message
	Tools    []ToolDef
	// Schema, if set, requests a schema-constrained response. It cannot be
	// combined with Tools on llama-server.
	Schema *JSONSchema
	// DisableThinking asks reasoning chat templates not to emit thinking.
	DisableThinking bool
}

// Usage reports token counts returned by the server.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// Completion is the first choice of a non-streaming chat completion.
type Completion struct {
	Message      Message
	FinishReason string
	Model        string
	Usage        Usage
}

// Client sends requests to an OpenAI-compatible chat-completions endpoint.
type Client struct {
	httpClient     *httpclient.Client
	baseURL        *url.URL
	apiKey         string
	model          string
	thinkingBudget int
}

// Option customizes a Client.
type Option func(*Client)

// WithHTTPClient replaces the HTTP wrapper, primarily for tests.
func WithHTTPClient(client *httpclient.Client) Option {
	return func(c *Client) { c.httpClient = client }
}

// WithThinkingBudget caps the model's reasoning per request, in tokens
// (llama-server's thinking_budget_tokens), instead of the server's
// default. Zero keeps the default. It is a sampling parameter, not part
// of the prompt, so the prompt cache is unaffected.
func WithThinkingBudget(tokens int) Option {
	return func(c *Client) { c.thinkingBudget = tokens }
}

// New constructs a client. baseURL is the API root (for example
// https://llm-api.iop.cx/v1), not the full endpoint. timeout bounds one
// request, including the time it waits in llama-server's queue.
func New(baseURL, apiKey, model string, timeout time.Duration, options ...Option) (*Client, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return nil, fmt.Errorf("parse LLM base URL %q: %w", baseURL, err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("LLM base URL must be an absolute http(s) URL, got %q", baseURL)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("LLM base URL must not contain a query or fragment, got %q", baseURL)
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	if strings.TrimSpace(model) == "" {
		return nil, errors.New("LLM model is required")
	}
	c := &Client{
		httpClient: httpclient.New(
			httpclient.WithHTTPClient(&http.Client{Timeout: timeout}),
			httpclient.WithRetry(2, 5*time.Second, 30*time.Second),
			httpclient.WithRetryPOST(),
		),
		baseURL: parsed,
		apiKey:  apiKey,
		model:   model,
	}
	for _, option := range options {
		option(c)
	}
	return c, nil
}

// Model returns the configured model identifier.
func (c *Client) Model() string { return c.model }

// Complete sends one non-streaming chat completion with deterministic
// sampling.
func (c *Client) Complete(ctx context.Context, r Request) (Completion, error) {
	if len(r.Messages) == 0 {
		return Completion{}, errors.New("at least one LLM message is required")
	}
	if r.Schema != nil && len(r.Tools) > 0 {
		return Completion{}, errors.New("a response schema cannot be combined with tools")
	}

	body := chatRequest{
		Model:       c.model,
		Messages:    r.Messages,
		Temperature: 0,
	}
	for _, t := range r.Tools {
		body.Tools = append(body.Tools, wireTool{
			Type:     "function",
			Function: wireFunction(t),
		})
	}
	if r.Schema != nil {
		body.ResponseFormat = &responseFormat{
			Type:       "json_schema",
			JSONSchema: wireJSONSchema{Name: r.Schema.Name, Strict: true, Schema: r.Schema.Schema},
		}
	}
	if r.DisableThinking {
		body.ChatTemplateKwargs = map[string]any{"enable_thinking": false}
	} else if c.thinkingBudget > 0 {
		body.ThinkingBudgetTokens = c.thinkingBudget
	}

	start := time.Now()
	completion, err := c.send(ctx, body)
	outcome := "ok"
	if err != nil {
		outcome = "error"
	}
	metrics.LLMRequestDuration.WithLabelValues(outcome).Observe(time.Since(start).Seconds())
	if err == nil {
		metrics.LLMTokensTotal.WithLabelValues("prompt").Add(float64(completion.Usage.PromptTokens))
		metrics.LLMTokensTotal.WithLabelValues("completion").Add(float64(completion.Usage.CompletionTokens))
	}
	return completion, err
}

func (c *Client) send(ctx context.Context, body chatRequest) (Completion, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return Completion{}, fmt.Errorf("marshal chat-completion request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(chatCompletionsPath), bytes.NewReader(raw))
	if err != nil {
		return Completion{}, fmt.Errorf("build chat-completion request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	c.authorize(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return Completion{}, fmt.Errorf("send chat-completion request: %w", err)
	}
	defer resp.Body.Close()
	rawResp, err := httpclient.Read(resp, maxResponseBytes)
	if err != nil {
		return Completion{}, err
	}
	if err := httpclient.EnsureOK(resp, rawResp); err != nil {
		if resp.StatusCode == http.StatusBadRequest && bytes.Contains(rawResp, []byte("exceed_context_size")) {
			return Completion{}, fmt.Errorf("chat completion: %w: %w", ErrContextExceeded, err)
		}
		return Completion{}, fmt.Errorf("chat completion: %w", err)
	}

	var decoded chatResponse
	if err := json.Unmarshal(rawResp, &decoded); err != nil {
		return Completion{}, fmt.Errorf("decode chat-completion response: %w", err)
	}
	if len(decoded.Choices) == 0 {
		return Completion{}, errors.New("chat-completion response contained no choices")
	}
	choice := decoded.Choices[0]
	msg := choice.Message
	msg.Role = "assistant"
	return Completion{
		Message:      msg,
		FinishReason: choice.FinishReason,
		Model:        decoded.Model,
		Usage:        decoded.Usage,
	}, nil
}

// ContextSize asks llama-server for the per-slot context size via /props,
// which lives at the server root rather than under /v1.
func (c *Client) ContextSize(ctx context.Context) (int, error) {
	root := *c.baseURL
	root.Path = strings.TrimSuffix(root.Path, "/v1")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, root.JoinPath("props").String(), nil)
	if err != nil {
		return 0, fmt.Errorf("build props request: %w", err)
	}
	c.authorize(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("get props: %w", err)
	}
	defer resp.Body.Close()
	raw, err := httpclient.Read(resp, 1<<20)
	if err != nil {
		return 0, err
	}
	if err := httpclient.EnsureOK(resp, raw); err != nil {
		return 0, fmt.Errorf("get props: %w", err)
	}
	var props struct {
		DefaultGenerationSettings struct {
			NCtx int `json:"n_ctx"`
		} `json:"default_generation_settings"`
	}
	if err := json.Unmarshal(raw, &props); err != nil {
		return 0, fmt.Errorf("decode props: %w", err)
	}
	if props.DefaultGenerationSettings.NCtx <= 0 {
		return 0, errors.New("props did not report n_ctx")
	}
	return props.DefaultGenerationSettings.NCtx, nil
}

func (c *Client) endpoint(path string) string {
	u := *c.baseURL
	u.Path += path
	return u.String()
}

func (c *Client) authorize(req *http.Request) {
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
}

type chatRequest struct {
	Model              string          `json:"model"`
	Messages           []Message       `json:"messages"`
	Temperature        float64         `json:"temperature"`
	Tools              []wireTool      `json:"tools,omitempty"`
	ResponseFormat     *responseFormat `json:"response_format,omitempty"`
	ChatTemplateKwargs map[string]any  `json:"chat_template_kwargs,omitempty"`
	// ThinkingBudgetTokens is a llama-server extension.
	ThinkingBudgetTokens int `json:"thinking_budget_tokens,omitempty"`
}

type wireTool struct {
	Type     string       `json:"type"`
	Function wireFunction `json:"function"`
}

type wireFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type responseFormat struct {
	Type       string         `json:"type"`
	JSONSchema wireJSONSchema `json:"json_schema"`
}

type wireJSONSchema struct {
	Name   string          `json:"name"`
	Strict bool            `json:"strict"`
	Schema json.RawMessage `json:"schema"`
}

type chatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Usage Usage `json:"usage"`
}

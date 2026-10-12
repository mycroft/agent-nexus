// Package mcptest serves an MCP server for tests: the official SDK's
// server over Streamable HTTP, behind an httptest server that records the
// requests it receives.
package mcptest

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Options configures a Fake.
type Options struct {
	// Token, if set, is the bearer token each request must carry.
	Token string
	// LegacyOnly serves sessions, as the SDK's stateful handler does. Such
	// a server only speaks the protocol before 2026-07-28, so clients fall
	// back to initialize. Otherwise the server is stateless, and speaks
	// the current protocol too.
	LegacyOnly bool
	// Instructions are the server's instructions for its clients.
	Instructions string
	// PageSize splits tool lists into pages of that many tools.
	PageSize int
}

// Request is a request the fake received.
type Request struct {
	// HTTPMethod is POST, GET or DELETE.
	HTTPMethod string
	// Method is the JSON-RPC method of a POST.
	Method        string
	Authorization string
}

// Fake is an MCP server. Add tools to it with AddTool, or to Server.
type Fake struct {
	Server *sdk.Server
	srv    *httptest.Server

	mu       sync.Mutex
	requests []Request
}

// New starts a server, stopped when the test ends.
func New(t *testing.T, opts Options) *Fake {
	t.Helper()
	f := &Fake{Server: sdk.NewServer(&sdk.Implementation{Name: "mcptest", Version: "v1"}, &sdk.ServerOptions{
		Instructions: opts.Instructions,
		PageSize:     opts.PageSize,
	})}
	h := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return f.Server },
		&sdk.StreamableHTTPOptions{Stateless: !opts.LegacyOnly})
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := Request{HTTPMethod: r.Method, Authorization: r.Header.Get("Authorization")}
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(body))
			var msg struct {
				Method string `json:"method"`
			}
			_ = json.Unmarshal(body, &msg)
			req.Method = msg.Method
		}
		f.mu.Lock()
		f.requests = append(f.requests, req)
		f.mu.Unlock()
		if opts.Token != "" && req.Authorization != "Bearer "+opts.Token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// URL is the server's MCP endpoint.
func (f *Fake) URL() string { return f.srv.URL + "/mcp" }

// Close stops the server, as if it went down.
func (f *Fake) Close() { f.srv.Close() }

// AddTool adds a tool that answers run's text, or a tool error when run
// fails. A nil schema accepts any object.
func (f *Fake) AddTool(name, description string, schema any, run func(args json.RawMessage) (string, error)) {
	if schema == nil {
		schema = map[string]any{"type": "object"}
	}
	f.Server.AddTool(&sdk.Tool{Name: name, Description: description, InputSchema: schema},
		func(_ context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			out, err := run(req.Params.Arguments)
			if err != nil {
				// A tool's failure is its result, not a protocol error.
				return &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: err.Error()}}}, nil //nolint:nilerr // see above
			}
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: out}}}, nil
		})
}

// Requests returns the requests received so far.
func (f *Fake) Requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Request(nil), f.requests...)
}

// Methods returns the JSON-RPC methods of the POSTs received so far.
func (f *Fake) Methods() []string {
	var out []string
	for _, r := range f.Requests() {
		if r.Method != "" {
			out = append(out, r.Method)
		}
	}
	return out
}

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/mycroft/agent-nexus/internal/mcp/mcptest"
	"github.com/mycroft/agent-nexus/internal/metrics"
)

func TestParseConfig(t *testing.T) {
	t.Setenv("MCP_TEST_TOKEN", "s3cret")
	servers, err := ParseConfig([]byte(`
servers:
  - name: k8s
    url: http://k8s-mcp.mcp.svc:8080/mcp
    description: the cluster
    tools: [pods_*, events_list]
  - name: grafana
    url: https://grafana-mcp.example.com/mcp
    tools: ["*"]
    jobs: [chat, review]
    load: eager
    timeout: 30s
    tokenEnv: MCP_TEST_TOKEN
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 2 {
		t.Fatalf("servers = %+v", servers)
	}
	k8s, grafana := servers[0], servers[1]
	if k8s.Name != "k8s" || k8s.Description != "the cluster" || !slices.Equal(k8s.Jobs, []string{JobChat}) || k8s.Load != LoadLazy || !k8s.Lazy() || k8s.Timeout != DefaultTimeout || k8s.token != "" {
		t.Errorf("k8s = %+v", k8s)
	}
	if grafana.Timeout != 30*time.Second || !grafana.Serves(JobReview) || grafana.Serves(JobDiscussion) || grafana.Lazy() || grafana.token != "s3cret" {
		t.Errorf("grafana = %+v", grafana)
	}
	if !k8s.allowed("pods_list") || !k8s.allowed("events_list") || k8s.allowed("secrets_get") {
		t.Error("the k8s allowlist does not match as configured")
	}

	if servers, err := ParseConfig(nil); err != nil || servers != nil {
		t.Errorf("empty config = %v, %v", servers, err)
	}
	if servers, err := LoadConfig(""); err != nil || servers != nil {
		t.Errorf("no config = %v, %v", servers, err)
	}

	for _, tc := range []struct{ yaml, want string }{
		{"servers:\n  - name: K8s\n    url: http://x/mcp\n    tools: [a]\n", "name"},
		{"servers:\n  - name: mcp\n    url: http://x/mcp\n    tools: [a]\n", "reserved"},
		{"servers:\n  - name: a\n    url: http://x/mcp\n    tools: [a]\n  - name: a\n    url: http://x/mcp\n    tools: [a]\n", "duplicate"},
		{"servers:\n  - name: a\n    url: x/mcp\n    tools: [a]\n", "absolute http"},
		{"servers:\n  - name: a\n    url: ftp://x/mcp\n    tools: [a]\n", "absolute http"},
		{"servers:\n  - name: a\n    url: http://x/mcp\n", "tools is required"},
		{"servers:\n  - name: a\n    url: http://x/mcp\n    tools: [\"[\"]\n", "pattern"},
		{"servers:\n  - name: a\n    url: http://x/mcp\n    tools: [a]\n    jobs: [deploy]\n", "job"},
		{"servers:\n  - name: a\n    url: http://x/mcp\n    tools: [a]\n    load: later\n", "load"},
		{"servers:\n  - name: a\n    url: http://x/mcp\n    tools: [a]\n    timeout: soon\n", "timeout"},
		{"servers:\n  - name: a\n    url: http://x/mcp\n    tools: [a]\n    timeout: -1s\n", "positive"},
		{"servers:\n  - name: a\n    url: http://x/mcp\n    tools: [a]\n    tokenEnv: MCP_TEST_UNSET\n", "not set"},
		{"servers:\n  - name: a\n    url: http://x/mcp\n    tool: [a]\n", "field tool not found"},
	} {
		if _, err := ParseConfig([]byte(tc.yaml)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("ParseConfig(%q) = %v, want an error about %q", tc.yaml, err, tc.want)
		}
	}
}

// newFake serves the tools pods_list, pods_get and secrets_get.
func newFake(t *testing.T, opts mcptest.Options) *mcptest.Fake {
	t.Helper()
	f := mcptest.New(t, opts)
	f.AddTool("pods_list", "List pods.", map[string]any{
		"type":       "object",
		"properties": map[string]any{"namespace": map[string]any{"type": "string"}},
	}, func(args json.RawMessage) (string, error) {
		var a struct {
			Namespace string `json:"namespace"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return "", err
		}
		return "pods in " + a.Namespace + ": web-0", nil
	})
	f.AddTool("pods_get", "Get a pod.", nil, func(json.RawMessage) (string, error) {
		return "", errors.New("pod not found")
	})
	f.AddTool("secrets_get", "Get a secret.", nil, func(json.RawMessage) (string, error) {
		return "hunter2", nil
	})
	return f
}

func server(url string, tools ...string) Server {
	return Server{Name: "k8s", URL: url, Tools: tools, Jobs: []string{JobChat}, Timeout: DefaultTimeout}
}

func toolNames(s *Session) []string {
	var names []string
	for _, t := range s.Tools {
		names = append(names, t.Name)
	}
	slices.Sort(names)
	return names
}

func TestOpenAndCall(t *testing.T) {
	for _, tc := range []struct {
		name    string
		legacy  bool
		version string
		method  string
	}{
		{"current", false, "2026-07-28", "server/discover"},
		{"legacy", true, "2025-11-25", "initialize"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t, mcptest.Options{LegacyOnly: tc.legacy, Instructions: "Use pods_list first."})
			ctx := context.Background()
			s, err := New(server(f.URL(), "pods_*", "nodes_*"), "test", nil).Open(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if s.ProtocolVersion != tc.version || s.Instructions != "Use pods_list first." {
				t.Errorf("session = version %q, instructions %q", s.ProtocolVersion, s.Instructions)
			}
			if got := toolNames(s); !slices.Equal(got, []string{"pods_get", "pods_list"}) {
				t.Errorf("tools = %v", got)
			}
			if !slices.Equal(s.Dropped, []string{"secrets_get"}) || !slices.Equal(s.Unmatched, []string{"nodes_*"}) {
				t.Errorf("dropped = %v, unmatched = %v", s.Dropped, s.Unmatched)
			}
			if !slices.Contains(f.Methods(), tc.method) {
				t.Errorf("methods = %v, want %s", f.Methods(), tc.method)
			}

			out, err := s.Call(ctx, "pods_list", json.RawMessage(`{"namespace":"web"}`))
			if err != nil || out != "pods in web: web-0" {
				t.Errorf("pods_list = %q, %v", out, err)
			}
			if _, err := s.Call(ctx, "pods_get", nil); err == nil || err.Error() != "pod not found" {
				t.Errorf("pods_get = %v, want the tool's error", err)
			}
			if _, err := s.Call(ctx, "pods_list", json.RawMessage(`{"namespace":`)); err == nil || !strings.Contains(err.Error(), "not valid JSON") {
				t.Errorf("invalid arguments = %v", err)
			}
			// A tool the allowlist leaves out is never called.
			if out, err := s.Call(ctx, "secrets_get", nil); err == nil || strings.Contains(out, "hunter2") {
				t.Errorf("secrets_get = %q, %v; want a refusal", out, err)
			}

			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			// A session-based (legacy) connection ends with a DELETE.
			deleted := slices.ContainsFunc(f.Requests(), func(r mcptest.Request) bool { return r.HTTPMethod == http.MethodDelete })
			if deleted != tc.legacy {
				t.Errorf("DELETE sent = %v, want %v", deleted, tc.legacy)
			}
		})
	}
}

func TestOpenPaginates(t *testing.T) {
	f := mcptest.New(t, mcptest.Options{PageSize: 2})
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		f.AddTool(name, "", nil, func(json.RawMessage) (string, error) { return name, nil })
	}
	s, err := New(server(f.URL(), "*"), "test", nil).Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got := toolNames(s); !slices.Equal(got, []string{"a", "b", "c", "d", "e"}) {
		t.Errorf("tools = %v", got)
	}
}

func TestCallRendersContent(t *testing.T) {
	f := mcptest.New(t, mcptest.Options{})
	result := func(res *sdk.CallToolResult) sdk.ToolHandler {
		return func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) { return res, nil }
	}
	object := map[string]any{"type": "object"}
	f.Server.AddTool(&sdk.Tool{Name: "mixed", InputSchema: object}, result(&sdk.CallToolResult{Content: []sdk.Content{
		&sdk.TextContent{Text: "first"},
		&sdk.ImageContent{Data: []byte("png"), MIMEType: "image/png"},
		&sdk.ResourceLink{URI: "file:///var/log/x", Name: "x"},
		&sdk.EmbeddedResource{Resource: &sdk.ResourceContents{URI: "file:///a", MIMEType: "text/plain", Text: "embedded"}},
		&sdk.EmbeddedResource{Resource: &sdk.ResourceContents{URI: "file:///b", MIMEType: "application/octet-stream", Blob: []byte{1}}},
	}}))
	f.Server.AddTool(&sdk.Tool{Name: "structured", InputSchema: object}, result(&sdk.CallToolResult{
		Content: []sdk.Content{}, StructuredContent: map[string]any{"ready": 2},
	}))
	f.Server.AddTool(&sdk.Tool{Name: "silent_failure", InputSchema: object}, result(&sdk.CallToolResult{
		Content: []sdk.Content{}, IsError: true,
	}))
	ctx := context.Background()
	s, err := New(server(f.URL(), "*"), "test", nil).Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	want := "first\n[image omitted]\n[resource file:///var/log/x]\nembedded\n[binary resource omitted]"
	if out, err := s.Call(ctx, "mixed", nil); err != nil || out != want {
		t.Errorf("mixed = %q, %v; want %q", out, err, want)
	}
	if out, err := s.Call(ctx, "structured", nil); err != nil || out != `{"ready":2}` {
		t.Errorf("structured = %q, %v", out, err)
	}
	if _, err := s.Call(ctx, "silent_failure", nil); err == nil || !strings.Contains(err.Error(), "without saying why") {
		t.Errorf("silent_failure = %v", err)
	}
	if _, err := s.Call(ctx, "unknown", nil); err == nil {
		t.Error("calling an unknown tool succeeded")
	}
}

func TestCallTimeout(t *testing.T) {
	f := mcptest.New(t, mcptest.Options{})
	f.Server.AddTool(&sdk.Tool{Name: "slow", InputSchema: map[string]any{"type": "object"}},
		func(ctx context.Context, _ *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "late"}}}, nil
		})
	srv := server(f.URL(), "*")
	srv.Timeout = 100 * time.Millisecond
	ctx := context.Background()
	s, err := New(srv, "test", nil).Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	start := time.Now()
	if _, err := s.Call(ctx, "slow", nil); err == nil {
		t.Error("a call past its timeout succeeded")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("the call took %s", elapsed)
	}
}

func TestToken(t *testing.T) {
	f := newFake(t, mcptest.Options{Token: "s3cret"})
	ctx := context.Background()

	before := testutil.ToFloat64(metrics.MCPErrorsTotal.WithLabelValues("k8s"))
	if _, err := New(server(f.URL(), "*"), "test", nil).Open(ctx); err == nil {
		t.Error("opened without the token")
	}
	if got := testutil.ToFloat64(metrics.MCPErrorsTotal.WithLabelValues("k8s")); got != before+1 {
		t.Errorf("mcp errors = %v, want %v", got, before+1)
	}

	srv := server(f.URL(), "*")
	srv.token = "s3cret"
	seen := len(f.Requests())
	s, err := New(srv, "test", nil).Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Call(ctx, "pods_list", nil); err != nil {
		t.Error(err)
	}
	for _, r := range f.Requests()[seen:] {
		if r.HTTPMethod == http.MethodPost && r.Authorization != "Bearer s3cret" {
			t.Errorf("request %+v lacks the token", r)
		}
	}
}

func TestRedirectsAreRefused(t *testing.T) {
	f := newFake(t, mcptest.Options{})
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, f.URL(), http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	_, err := New(server(redirect.URL+"/mcp", "*"), "test", nil).Open(context.Background())
	if err == nil || !strings.Contains(err.Error(), "redirects are not followed") {
		t.Errorf("Open through a redirect = %v", err)
	}
	if len(f.Requests()) != 0 {
		t.Errorf("the redirect was followed: %+v", f.Requests())
	}
}

func TestOpenDownServer(t *testing.T) {
	f := newFake(t, mcptest.Options{})
	f.Close()
	start := time.Now()
	if _, err := New(server(f.URL(), "*"), "test", nil).Open(context.Background()); err == nil {
		t.Error("opened a server that is down")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("Open took %s", elapsed)
	}
}

func TestLazy(t *testing.T) {
	up := newFake(t, mcptest.Options{LegacyOnly: true})
	down := newFake(t, mcptest.Options{})
	down.Close()
	downServer := server(down.URL(), "*")
	downServer.Name = "prom"
	l := NewLazy([]*Client{New(server(up.URL(), "pods_*"), "test", nil), New(downServer, "test", nil)}, nil)
	ctx := context.Background()

	if names := []string{l.Servers()[0].Name, l.Servers()[1].Name}; !slices.Equal(names, []string{"k8s", "prom"}) {
		t.Errorf("servers = %v", names)
	}
	if len(up.Requests()) != 0 {
		t.Errorf("a lazy server was reached before its first use: %+v", up.Requests())
	}
	s, err := l.Session(ctx, "k8s")
	if err != nil {
		t.Fatal(err)
	}
	seen := len(up.Requests())
	if again, err := l.Session(ctx, "k8s"); err != nil || again != s || len(up.Requests()) != seen {
		t.Errorf("the second use opened another session: %v, %d requests", err, len(up.Requests())-seen)
	}

	before := testutil.ToFloat64(metrics.MCPErrorsTotal.WithLabelValues("prom"))
	for range 2 {
		if _, err := l.Session(ctx, "prom"); err == nil || err.Error() != "the MCP server prom is unavailable" {
			t.Errorf("down server = %v", err)
		}
	}
	if got := testutil.ToFloat64(metrics.MCPErrorsTotal.WithLabelValues("prom")); got != before+1 {
		t.Errorf("mcp errors = %v, want one attempt (%v)", got, before+1)
	}
	if _, err := l.Session(ctx, "secrets"); err == nil {
		t.Error("opened an unknown server")
	}

	l.Close()
	if !slices.ContainsFunc(up.Requests(), func(r mcptest.Request) bool { return r.HTTPMethod == http.MethodDelete }) {
		t.Error("Close did not end the session")
	}
}

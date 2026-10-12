package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"slices"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mycroft/agent-nexus/internal/metrics"
)

// openTimeout bounds connecting to a server and listing its tools.
const openTimeout = 30 * time.Second

var errRedirect = errors.New("mcp: redirects are not followed")

// Client opens sessions with one MCP server. Its URL comes from the
// operator, never from the model: it is reached with a plain HTTP client,
// which may dial private addresses, and follows no redirect.
type Client struct {
	Server Server
	sdk    *sdk.Client
	http   *http.Client
}

// New returns a client of s; version is agent-nexus's, sent to the server.
func New(s Server, version string, logger *slog.Logger) *Client {
	hc := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return errRedirect },
	}
	if s.token != "" {
		hc.Transport = bearer{token: s.token, next: http.DefaultTransport}
	}
	return &Client{
		Server: s,
		http:   hc,
		sdk: sdk.NewClient(&sdk.Implementation{Name: "agent-nexus", Version: version}, &sdk.ClientOptions{
			Logger: logger,
			// No roots, sampling or elicitation: agent-nexus only calls
			// tools.
			Capabilities: &sdk.ClientCapabilities{},
			// A tool asking for input gets an error, not a prompt.
			MultiRoundTrip: &sdk.MultiRoundTripOptions{Disabled: true},
		}),
	}
}

// bearer adds the server's token to each request.
type bearer struct {
	token string
	next  http.RoundTripper
}

func (b bearer) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+b.token)
	return b.next.RoundTrip(req)
}

// Session is an open session with a server, and the tools the model may
// call. Its tools are listed once, so they do not change during a job.
type Session struct {
	Server Server
	// ProtocolVersion is the version the server agreed to.
	ProtocolVersion string
	// Instructions are the server's instructions for its clients.
	Instructions string
	// Tools are the server's tools the configuration allows.
	Tools []*sdk.Tool
	// Dropped are the names of the tools it does not allow.
	Dropped []string
	// Unmatched are the configured patterns no tool matches.
	Unmatched []string

	cs *sdk.ClientSession
}

// Open connects to the server and lists its tools. A failure is counted
// in agent_nexus_mcp_errors_total.
func (c *Client) Open(ctx context.Context) (*Session, error) {
	s, err := c.open(ctx)
	if err != nil {
		metrics.MCPErrorsTotal.WithLabelValues(c.Server.Name).Inc()
		return nil, fmt.Errorf("mcp %s: %w", c.Server.Name, err)
	}
	return s, nil
}

func (c *Client) open(ctx context.Context) (*Session, error) {
	ctx, cancel := context.WithTimeout(ctx, openTimeout)
	defer cancel()
	// The session outlives ctx: the SDK detaches it.
	cs, err := c.sdk.Connect(ctx, &sdk.StreamableClientTransport{
		Endpoint:             c.Server.URL,
		HTTPClient:           c.http,
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	s := &Session{Server: c.Server, cs: cs}
	if init := cs.InitializeResult(); init != nil {
		s.ProtocolVersion, s.Instructions = init.ProtocolVersion, init.Instructions
	}
	matched := map[string]bool{}
	for t, err := range cs.Tools(ctx, nil) {
		if err != nil {
			// The list error is the one to report.
			cs.Close()
			return nil, fmt.Errorf("list tools: %w", err)
		}
		if !c.Server.allowed(t.Name) {
			s.Dropped = append(s.Dropped, t.Name)
			continue
		}
		s.Tools = append(s.Tools, t)
		for _, p := range c.Server.Tools {
			if ok, _ := path.Match(p, t.Name); ok {
				matched[p] = true
			}
		}
	}
	for _, p := range c.Server.Tools {
		if !matched[p] && !slices.Contains(s.Unmatched, p) {
			s.Unmatched = append(s.Unmatched, p)
		}
	}
	return s, nil
}

// Call calls the tool name with the model's JSON arguments, and returns
// its result as text. A tool error is an error, for the model to read.
func (s *Session) Call(ctx context.Context, name string, args json.RawMessage) (string, error) {
	if s.Tool(name) == nil {
		return "", fmt.Errorf("the MCP server %s has no tool %q that you may call", s.Server.Name, name)
	}
	if trimmed := strings.TrimSpace(string(args)); trimmed == "" || trimmed == "null" {
		args = json.RawMessage("{}")
	}
	if !json.Valid(args) {
		return "", errors.New("the arguments are not valid JSON")
	}
	timeout := s.Server.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, err := s.cs.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return "", fmt.Errorf("call %s on %s: %w", name, s.Server.Name, err)
	}
	if res.NeedsInput() {
		return "", errors.New("the tool asks for input, which agent-nexus cannot give")
	}
	text := resultText(res)
	if res.IsError {
		if text == "" {
			text = "the tool failed without saying why"
		}
		return "", errors.New(text)
	}
	return text, nil
}

// Tool returns the allowed tool name, or nil.
func (s *Session) Tool(name string) *sdk.Tool {
	i := slices.IndexFunc(s.Tools, func(t *sdk.Tool) bool { return t.Name == name })
	if i < 0 {
		return nil
	}
	return s.Tools[i]
}

// Close ends the session.
func (s *Session) Close() error {
	if err := s.cs.Close(); err != nil {
		return fmt.Errorf("mcp %s: close: %w", s.Server.Name, err)
	}
	return nil
}

// resultText renders a tool result for the model. Media is left out; the
// structured result is used only when the tool gave no text, since tools
// are to repeat it as text.
func resultText(res *sdk.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		switch c := c.(type) {
		case *sdk.TextContent:
			parts = append(parts, c.Text)
		case *sdk.ImageContent:
			parts = append(parts, "[image omitted]")
		case *sdk.AudioContent:
			parts = append(parts, "[audio omitted]")
		case *sdk.ResourceLink:
			parts = append(parts, fmt.Sprintf("[resource %s]", c.URI))
		case *sdk.EmbeddedResource:
			if c.Resource != nil && c.Resource.Text != "" {
				parts = append(parts, c.Resource.Text)
			} else {
				parts = append(parts, "[binary resource omitted]")
			}
		}
	}
	if len(parts) == 0 && res.StructuredContent != nil {
		if raw, err := json.Marshal(res.StructuredContent); err == nil {
			parts = append(parts, string(raw))
		}
	}
	return strings.Join(parts, "\n")
}

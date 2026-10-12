package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/mycroft/agent-nexus/internal/mcp"
)

const (
	// maxToolName is the longest tool name OpenAI-compatible servers
	// accept.
	maxToolName = 64
	// maxMCPInstructions caps the instructions of one MCP server.
	maxMCPInstructions = 1500
	// maxMCPListing caps a full mcp_tools listing. Past it, the tools are
	// listed without their parameters, which the model asks for by name.
	maxMCPListing = 12 << 10
)

// unsafeToolChars are those a tool name may not hold.
var unsafeToolChars = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// MCP returns the tools of an MCP session, named "<prefix>_<tool>" so
// servers cannot collide with each other or with the built-in tools. It
// also returns the names of the tools it leaves out: those too long once
// prefixed, or colliding once their unsafe characters are replaced.
func MCP(prefix string, s *mcp.Session) ([]Tool, []string) {
	var out []Tool
	var skipped []string
	seen := map[string]bool{}
	for _, t := range s.Tools {
		name := prefix + "_" + unsafeToolChars.ReplaceAllString(t.Name, "_")
		params, err := json.Marshal(t.InputSchema)
		if len(name) > maxToolName || seen[name] || err != nil {
			skipped = append(skipped, t.Name)
			continue
		}
		seen[name] = true
		description := t.Description
		if description == "" {
			description = t.Title
		}
		remoteName := t.Name
		out = append(out, Tool{
			Name:        name,
			Description: description,
			Parameters:  params,
			Run: func(ctx context.Context, args json.RawMessage) (string, error) {
				return s.Call(ctx, remoteName, args)
			},
		})
	}
	return out, skipped
}

// MCPInstructions returns the instructions of an MCP server for the model,
// capped and fenced, or "" when it has none.
func MCPInstructions(s *mcp.Session) string {
	text := strings.TrimSpace(s.Instructions)
	if text == "" {
		return ""
	}
	if len(text) > maxMCPInstructions {
		text = strings.ToValidUTF8(text[:maxMCPInstructions], "") + "\n[truncated]"
	}
	return fmt.Sprintf("<mcp-instructions server=%q>\n%s\n</mcp-instructions>\n", s.Server.Name, text)
}

// MCPLazy returns mcp_tools and mcp_call, through which the model loads
// and calls the tools of the servers of l. Their own tool list never
// changes, so loading a server keeps llama-server's prompt cache.
func MCPLazy(l *mcp.Lazy) []Tool {
	var names []string
	for _, s := range l.Servers() {
		names = append(names, s.Name)
	}
	enum, _ := json.Marshal(names)
	server := `"server":{"type":"string","enum":` + string(enum) + `,"description":"The MCP server"}`
	return []Tool{
		{
			Name:        "mcp_tools",
			Description: "List the tools of an MCP server, with their parameters, to call them with mcp_call.",
			Parameters: json.RawMessage(`{"type":"object","properties":{` + server + `,` +
				`"tools":{"type":"array","items":{"type":"string"},"description":"Only these tools: to see their parameters when the full list leaves them out"}},` +
				`"required":["server"]}`),
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Server string   `json:"server"`
					Tools  []string `json:"tools"`
				}
				if err := decode(raw, &args); err != nil {
					return "", err
				}
				s, err := l.Session(ctx, args.Server)
				if err != nil {
					return "", err
				}
				listing, _ := MCPListing(s, args.Tools)
				return listing, nil
			},
		},
		{
			Name:        "mcp_call",
			Description: "Call a tool of an MCP server, with arguments following the parameters mcp_tools lists.",
			Parameters: json.RawMessage(`{"type":"object","properties":{` + server + `,` +
				`"tool":{"type":"string","description":"The tool, named as mcp_tools lists it"},` +
				`"arguments":{"type":"object","description":"The tool's arguments"}},` +
				`"required":["server","tool"]}`),
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Server    string          `json:"server"`
					Tool      string          `json:"tool"`
					Arguments json.RawMessage `json:"arguments"`
				}
				if err := decode(raw, &args); err != nil {
					return "", err
				}
				if args.Tool == "" {
					return "", errors.New("tool is required")
				}
				s, err := l.Session(ctx, args.Server)
				if err != nil {
					return "", err
				}
				// Models sometimes give the arguments as a JSON string.
				var inner string
				if json.Unmarshal(args.Arguments, &inner) == nil && json.Valid([]byte(inner)) {
					args.Arguments = json.RawMessage(inner)
				}
				return s.Call(ctx, args.Tool, args.Arguments)
			},
		},
	}
}

// MCPListing describes the tools of s for mcp_tools: all of them, or
// those named. Listing them all also gives the server's instructions;
// past maxMCPListing, it leaves out their parameters, and reports it.
func MCPListing(s *mcp.Session, names []string) (string, bool) {
	var b strings.Builder
	if len(names) == 0 {
		if instructions := MCPInstructions(s); instructions != "" {
			fmt.Fprintf(&b, "The instructions of the MCP server %s:\n%s\n", s.Server.Name, instructions)
		}
		fmt.Fprintf(&b, "The tools of the MCP server %s, to call with mcp_call:\n", s.Server.Name)
		head := b.Len()
		for _, t := range s.Tools {
			writeMCPTool(&b, t.Name, t.Description, t.InputSchema)
		}
		if b.Len() <= maxMCPListing {
			return b.String(), false
		}
		short := b.String()[:head]
		b.Reset()
		b.WriteString(short)
		for _, t := range s.Tools {
			fmt.Fprintf(&b, "\n- %s: %s", t.Name, firstLine(t.Description))
		}
		b.WriteString("\n\nThe list is too long to give the parameters: call mcp_tools again with the tools you need.\n")
		return b.String(), true
	}
	fmt.Fprintf(&b, "Tools of the MCP server %s, to call with mcp_call:\n", s.Server.Name)
	for _, name := range names {
		t := s.Tool(name)
		if t == nil {
			fmt.Fprintf(&b, "\n%s: there is no such tool that you may call.\n", name)
			continue
		}
		writeMCPTool(&b, t.Name, t.Description, t.InputSchema)
	}
	return b.String(), false
}

func writeMCPTool(b *strings.Builder, name, description string, schema any) {
	params, err := json.Marshal(schema)
	if err != nil {
		params = []byte("{}")
	}
	fmt.Fprintf(b, "\n%s: %s\nParameters: %s\n", name, strings.TrimSpace(description), params)
}

// firstLine returns the first line of s.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mycroft/agent-nexus/internal/mcp"
	"github.com/mycroft/agent-nexus/internal/mcp/mcptest"
)

func TestMCP(t *testing.T) {
	f := mcptest.New(t, mcptest.Options{})
	echo := func(name string) func(json.RawMessage) (string, error) {
		return func(args json.RawMessage) (string, error) { return name + " " + string(args), nil }
	}
	schema := map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}, "required": []any{"q"}}
	f.AddTool("query.range", "Run a range query.", schema, echo("query.range"))
	f.AddTool("query_range", "Collides once sanitized.", nil, echo("query_range"))
	f.AddTool(strings.Repeat("x", 60), "Too long once prefixed.", nil, echo("long"))
	f.Server.AddTool(&sdk.Tool{Name: "titled", Title: "A titled tool", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "ok"}}}, nil
		})

	ctx := context.Background()
	s, err := mcp.New(mcp.Server{Name: "prom", URL: f.URL(), Tools: []string{"*"}, Timeout: mcp.DefaultTimeout}, "test", nil).Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	toolset, skipped := MCP("prom", s)
	byName := map[string]Tool{}
	for _, tool := range toolset {
		byName[tool.Name] = tool
	}
	if len(toolset) != 2 || byName["prom_query_range"].Name == "" || byName["prom_titled"].Name == "" {
		t.Fatalf("tools = %v", slices.Sorted(maps.Keys(byName)))
	}
	if len(skipped) != 2 || !slices.Contains(skipped, strings.Repeat("x", 60)) {
		t.Errorf("skipped = %v, want the long tool and one of the colliding ones", skipped)
	}
	if byName["prom_titled"].Description != "A titled tool" {
		t.Errorf("description = %q, want the title", byName["prom_titled"].Description)
	}

	query := byName["prom_query_range"]
	var params map[string]any
	if err := json.Unmarshal(query.Parameters, &params); err != nil || params["required"] == nil {
		t.Errorf("parameters = %s, %v", query.Parameters, err)
	}
	// The call reaches the server under its own name.
	out, err := query.Run(ctx, json.RawMessage(`{"q":"up"}`))
	if err != nil || !strings.HasSuffix(out, ` {"q":"up"}`) || !strings.HasPrefix(out, "query") {
		t.Errorf("run = %q, %v", out, err)
	}
}

func TestMCPLazy(t *testing.T) {
	f := mcptest.New(t, mcptest.Options{Instructions: "Prefer pods_list."})
	schema := map[string]any{"type": "object", "properties": map[string]any{"namespace": map[string]any{"type": "string"}}}
	f.AddTool("pods_list", "List pods.\nAll of them.", schema, func(args json.RawMessage) (string, error) {
		return "pods " + string(args), nil
	})
	f.AddTool("secrets_get", "Read a secret.", nil, func(json.RawMessage) (string, error) { return "hunter2", nil })
	server := mcp.Server{Name: "k8s", URL: f.URL(), Tools: []string{"pods_*"}, Timeout: mcp.DefaultTimeout}
	l := mcp.NewLazy([]*mcp.Client{mcp.New(server, "test", nil)}, nil)
	defer l.Close()
	ctx := context.Background()

	toolset := MCPLazy(l)
	if len(toolset) != 2 || toolset[0].Name != "mcp_tools" || toolset[1].Name != "mcp_call" {
		t.Fatalf("tools = %+v", toolset)
	}
	for _, tool := range toolset {
		if !strings.Contains(string(tool.Parameters), `"enum":["k8s"]`) {
			t.Errorf("%s parameters = %s, want the servers as an enum", tool.Name, tool.Parameters)
		}
	}
	list, call := toolset[0], toolset[1]

	out, err := list.Run(ctx, json.RawMessage(`{"server":"k8s"}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"<mcp-instructions server=\"k8s\">\nPrefer pods_list.\n</mcp-instructions>",
		"pods_list: List pods.\nAll of them.\nParameters: {",
		`"namespace":{"type":"string"}`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("listing lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "secrets_get") {
		t.Errorf("listing shows a tool the allowlist leaves out:\n%s", out)
	}
	if out, err := list.Run(ctx, json.RawMessage(`{"server":"k8s","tools":["pods_list","secrets_get"]}`)); err != nil ||
		!strings.Contains(out, "pods_list: List pods.") || !strings.Contains(out, "secrets_get: there is no such tool") || strings.Contains(out, "mcp-instructions") {
		t.Errorf("listing by name = %q, %v", out, err)
	}

	for _, args := range []string{
		`{"server":"k8s","tool":"pods_list","arguments":{"namespace":"web"}}`,
		`{"server":"k8s","tool":"pods_list","arguments":"{\"namespace\":\"web\"}"}`,
	} {
		if out, err := call.Run(ctx, json.RawMessage(args)); err != nil || out != `pods {"namespace":"web"}` {
			t.Errorf("mcp_call %s = %q, %v", args, out, err)
		}
	}
	if out, err := call.Run(ctx, json.RawMessage(`{"server":"k8s","tool":"secrets_get"}`)); err == nil || strings.Contains(out, "hunter2") {
		t.Errorf("mcp_call secrets_get = %q, %v; want a refusal", out, err)
	}
	if _, err := call.Run(ctx, json.RawMessage(`{"server":"grafana","tool":"pods_list"}`)); err == nil {
		t.Error("mcp_call reached an unknown server")
	}
}

func TestMCPListingBrief(t *testing.T) {
	f := mcptest.New(t, mcptest.Options{})
	long := strings.Repeat("Explains everything. ", 100)
	for i := range 20 {
		f.AddTool(fmt.Sprintf("tool_%02d", i), "Does thing "+strconv.Itoa(i)+".\n"+long, nil, func(json.RawMessage) (string, error) { return "", nil })
	}
	ctx := context.Background()
	s, err := mcp.New(mcp.Server{Name: "big", URL: f.URL(), Tools: []string{"*"}, Timeout: mcp.DefaultTimeout}, "test", nil).Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	out, brief := MCPListing(s, nil)
	if !brief || len(out) > maxMCPListing || !strings.Contains(out, "- tool_07: Does thing 7.") || strings.Contains(out, "Parameters:") || !strings.Contains(out, "call mcp_tools again") {
		t.Errorf("brief = %v, listing:\n%s", brief, out)
	}
	if out, brief := MCPListing(s, []string{"tool_07"}); brief || !strings.Contains(out, "Parameters:") {
		t.Errorf("listing by name: brief = %v\n%s", brief, out)
	}
}

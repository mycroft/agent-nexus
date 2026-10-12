package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mycroft/agent-nexus/internal/mcp"
	"github.com/mycroft/agent-nexus/internal/mcp/mcptest"
)

func TestPrintMCPServer(t *testing.T) {
	f := mcptest.New(t, mcptest.Options{Instructions: "Prefer pods_list."})
	f.AddTool("pods_list", "List pods.", nil, func(json.RawMessage) (string, error) { return "", nil })
	f.AddTool("secrets_get", "Read a secret.", nil, func(json.RawMessage) (string, error) { return "", nil })
	path := filepath.Join(t.TempDir(), "mcp.yaml")
	config := "servers:\n" +
		"  - name: k8s\n    url: " + f.URL() + "\n    tools: [pods_*, nodes_*]\n    load: eager\n" +
		"  - name: kube\n    url: " + f.URL() + "\n    description: the cluster\n    tools: [pods_*]\n"
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	clients, err := newMCPClients(ctx, path, slog.New(slog.DiscardHandler))
	if err != nil || len(clients) != 2 {
		t.Fatalf("clients = %v, %v", clients, err)
	}
	for _, tc := range []struct {
		client *mcp.Client
		want   []string
	}{
		{clients[0], []string{
			"k8s: " + f.URL() + ", protocol 2026-07-28, jobs chat, load eager\n",
			"  k8s_pods_list ",
			"  1 tools, ~",
			"  instructions: ~",
			"  not allowed: secrets_get\n",
			"  pattern matching no tool: nodes_*\n",
		}},
		{clients[1], []string{
			"kube: " + f.URL() + ", protocol 2026-07-28, jobs chat, load lazy\n",
			"\n  pods_list ",
			"  in each prompt: ~",
			"  1 tools, ~",
			" tokens once the model loads them\n",
			"  not allowed: secrets_get\n",
		}},
	} {
		var out strings.Builder
		if err := printMCPServer(ctx, &out, tc.client); err != nil {
			t.Fatal(err)
		}
		for _, want := range tc.want {
			if !strings.Contains(out.String(), want) {
				t.Errorf("output lacks %q:\n%s", want, out.String())
			}
		}
	}
}

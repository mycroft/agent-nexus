package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/mycroft/agent-nexus/internal/mcp"
	"github.com/mycroft/agent-nexus/internal/tools"
)

// newMCPClients returns a client of each server configured in the file at
// path. The SDK logs only at debug level, where it helps to diagnose a
// server.
func newMCPClients(ctx context.Context, path string, log *slog.Logger) ([]*mcp.Client, error) {
	servers, err := mcp.LoadConfig(path)
	if err != nil {
		return nil, err
	}
	var clients []*mcp.Client
	var names []string
	for _, s := range servers {
		var sdkLog *slog.Logger
		if log.Enabled(ctx, slog.LevelDebug) {
			sdkLog = log.With("mcp_server", s.Name)
		}
		clients = append(clients, mcp.New(s, version, sdkLog))
		names = append(names, s.Name)
	}
	if len(clients) > 0 {
		log.Info("mcp servers configured", "servers", names)
	}
	return clients, nil
}

// checkMCP connects to each server of MCP_CONFIG and prints the tools it
// gives the agent, with their size in the prompt, and the tools the
// allowlist leaves out.
func checkMCP(args []string) error {
	fl := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fl.Usage = func() {
		fmt.Fprintln(fl.Output(), "usage: agent-nexus mcp\n\nConnects to each MCP server of MCP_CONFIG and lists the tools the agent gets.")
	}
	if err := fl.Parse(args); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	clients, err := newMCPClients(ctx, os.Getenv("MCP_CONFIG"), slog.New(slog.DiscardHandler))
	if err != nil {
		return err
	}
	if len(clients) == 0 {
		return errors.New("MCP_CONFIG names no MCP server")
	}
	failed := 0
	for _, c := range clients {
		if err := printMCPServer(ctx, os.Stdout, c); err != nil {
			fmt.Printf("%v\n\n", err)
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d MCP servers failed", failed, len(clients))
	}
	return nil
}

func printMCPServer(ctx context.Context, w io.Writer, c *mcp.Client) error {
	s, err := c.Open(ctx)
	if err != nil {
		return err
	}
	defer s.Close()
	fmt.Fprintf(w, "%s: %s, protocol %s, jobs %s, load %s\n", c.Server.Name, c.Server.URL, s.ProtocolVersion, strings.Join(c.Server.Jobs, ", "), c.Server.Load)
	// Rough counts, at 3 bytes a token, like the agent's estimate.
	var skipped []string
	if c.Server.Lazy() {
		for _, t := range s.Tools {
			params, _ := json.Marshal(t.InputSchema)
			fmt.Fprintf(w, "  %-48s ~%d tokens\n", t.Name, (len(t.Name)+len(t.Description)+len(params))/3)
		}
		// Its line in the prompt, and the tools loading it.
		size := len("- " + c.Server.Name + ": " + c.Server.Description + "\n")
		for _, t := range tools.MCPLazy(mcp.NewLazy([]*mcp.Client{c}, nil)) {
			size += len(t.Name) + len(t.Description) + len(t.Parameters)
		}
		fmt.Fprintf(w, "  in each prompt: ~%d tokens, with mcp_tools and mcp_call\n", size/3)
		listing, brief := tools.MCPListing(s, nil)
		fmt.Fprintf(w, "  %d tools, ~%d tokens once the model loads them\n", len(s.Tools), len(listing)/3)
		if brief {
			fmt.Fprintln(w, "  too many to list with their parameters: the model asks for those it needs")
		}
	} else {
		var toolset []tools.Tool
		toolset, skipped = tools.MCP(c.Server.Name, s)
		total := 0
		for _, t := range toolset {
			size := len(t.Name) + len(t.Description) + len(t.Parameters)
			total += size
			fmt.Fprintf(w, "  %-48s ~%d tokens\n", t.Name, size/3)
		}
		fmt.Fprintf(w, "  %d tools, ~%d tokens in each prompt\n", len(toolset), total/3)
		if len(s.Instructions) > 0 {
			fmt.Fprintf(w, "  instructions: ~%d tokens\n", len(s.Instructions)/3)
		}
	}
	for _, name := range s.Dropped {
		fmt.Fprintf(w, "  not allowed: %s\n", name)
	}
	for _, name := range skipped {
		fmt.Fprintf(w, "  left out, its name does not fit: %s\n", name)
	}
	for _, p := range s.Unmatched {
		fmt.Fprintf(w, "  pattern matching no tool: %s\n", p)
	}
	fmt.Fprintln(w)
	return nil
}

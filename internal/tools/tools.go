// Package tools defines the functions an agent may call, and the read-only
// repository tools used by reviews.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/mycroft/agent-nexus/internal/gitrepo"
)

// Tool is a function exposed to the model. Run receives the raw JSON
// arguments and returns text for the model; an error is also reported to
// the model, so it can correct itself.
type Tool struct {
	Name        string
	Description string
	// Parameters is a JSON Schema object describing the arguments.
	Parameters json.RawMessage
	Run        func(ctx context.Context, args json.RawMessage) (string, error)
}

const (
	defaultReadLines = 400
	maxLogEntries    = 20
	maxGrepLines     = 200
)

// Repo returns read-only tools over a repository at a fixed commit.
// fileDiff returns the annotated diff of one changed file, if it changed.
func Repo(repo *gitrepo.Repo, rev string, fileDiff func(path string) (string, bool)) []Tool {
	return []Tool{
		{
			Name:        "read_file",
			Description: "Read a file at the pull request head, with line numbers. Defaults to the first 400 lines; use start_line/end_line for other ranges.",
			Parameters: json.RawMessage(`{"type":"object","properties":{` +
				`"path":{"type":"string","description":"Repository-relative file path"},` +
				`"start_line":{"type":"integer","description":"First line (1-based), default 1"},` +
				`"end_line":{"type":"integer","description":"Last line, inclusive"}},` +
				`"required":["path"]}`),
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Path      string `json:"path"`
					StartLine int    `json:"start_line"`
					EndLine   int    `json:"end_line"`
				}
				if err := decode(raw, &args); err != nil {
					return "", err
				}
				content, err := repo.ReadFile(ctx, rev, args.Path)
				if err != nil {
					return "", err
				}
				return numberLines(string(content), args.StartLine, args.EndLine), nil
			},
		},
		{
			Name:        "list_dir",
			Description: "List a directory at the pull request head. Directories end with '/'.",
			Parameters: json.RawMessage(`{"type":"object","properties":{` +
				`"path":{"type":"string","description":"Repository-relative directory; empty for the root"}}}`),
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Path string `json:"path"`
				}
				if err := decode(raw, &args); err != nil {
					return "", err
				}
				entries, err := repo.ListDir(ctx, rev, args.Path)
				if err != nil {
					return "", err
				}
				var b strings.Builder
				for _, e := range entries {
					b.WriteString(e.Name)
					if e.IsDir {
						b.WriteByte('/')
					}
					b.WriteByte('\n')
				}
				return b.String(), nil
			},
		},
		{
			Name:        "grep",
			Description: "Search tracked files at the pull request head with an extended regular expression (git grep -E). Returns path:line:text matches.",
			Parameters: json.RawMessage(`{"type":"object","properties":{` +
				`"pattern":{"type":"string","description":"Extended regular expression"},` +
				`"path":{"type":"string","description":"Optional file or directory to limit the search to"},` +
				`"ignore_case":{"type":"boolean"}},` +
				`"required":["pattern"]}`),
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Pattern    string `json:"pattern"`
					Path       string `json:"path"`
					IgnoreCase bool   `json:"ignore_case"`
				}
				if err := decode(raw, &args); err != nil {
					return "", err
				}
				out, err := repo.Grep(ctx, rev, args.Pattern, args.Path, args.IgnoreCase)
				if err != nil && !errors.Is(err, gitrepo.ErrTruncated) {
					return "", err
				}
				if out == "" {
					return "no matches", nil
				}
				return limitLines(out, maxGrepLines), nil
			},
		},
		{
			Name:        "git_log",
			Description: "Show recent commits touching a path (or the whole repository), newest first.",
			Parameters: json.RawMessage(`{"type":"object","properties":{` +
				`"path":{"type":"string","description":"Optional file or directory"},` +
				`"limit":{"type":"integer","description":"Number of commits, 1-20, default 10"}}}`),
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Path  string `json:"path"`
					Limit int    `json:"limit"`
				}
				if err := decode(raw, &args); err != nil {
					return "", err
				}
				if args.Limit <= 0 {
					args.Limit = 10
				}
				args.Limit = min(args.Limit, maxLogEntries)
				out, err := repo.Log(ctx, rev, args.Path, args.Limit)
				if err != nil {
					return "", err
				}
				if out == "" {
					return "no commits", nil
				}
				return out, nil
			},
		},
		{
			Name:        "get_file_diff",
			Description: "Show the pull request diff of one changed file, with new-file line numbers in the left column.",
			Parameters: json.RawMessage(`{"type":"object","properties":{` +
				`"path":{"type":"string","description":"Path of a changed file"}},` +
				`"required":["path"]}`),
			Run: func(_ context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Path string `json:"path"`
				}
				if err := decode(raw, &args); err != nil {
					return "", err
				}
				clean, err := gitrepo.CleanPath(args.Path)
				if err != nil {
					return "", err
				}
				d, ok := fileDiff(clean)
				if !ok {
					return "", fmt.Errorf("%q is not changed by this pull request", clean)
				}
				return d, nil
			},
		},
	}
}

func decode(raw json.RawMessage, v any) error {
	if len(raw) == 0 || string(raw) == "null" {
		raw = json.RawMessage("{}")
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

// numberLines returns lines [start, end] of content prefixed with their
// line numbers, plus a note when the range does not cover the whole file.
func numberLines(content string, start, end int) string {
	lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	total := len(lines)
	if start <= 0 {
		start = 1
	}
	if end <= 0 || end < start {
		end = start + defaultReadLines - 1
	}
	end = min(end, total)
	if start > total {
		return fmt.Sprintf("(file has %d lines)", total)
	}
	var b strings.Builder
	for i := start; i <= end; i++ {
		fmt.Fprintf(&b, "%5d  %s\n", i, lines[i-1])
	}
	if start > 1 || end < total {
		fmt.Fprintf(&b, "(showing lines %d-%d of %d)\n", start, end, total)
	}
	return b.String()
}

func limitLines(s string, n int) string {
	lines := strings.SplitAfter(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[:n], "") + fmt.Sprintf("(%d more matches omitted; narrow the pattern or path)\n", len(lines)-n)
}

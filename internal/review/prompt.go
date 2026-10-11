package review

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/mycroft/agent-nexus/internal/diff"
	"github.com/mycroft/agent-nexus/internal/forge"
)

const (
	maxDescription = 4 << 10
	maxGuidance    = 8 << 10
	// diffBudgetShare is the share of the context window the inline diff
	// may use; the rest is for the skill, tool results and the answer.
	diffBudgetShare = 0.4
	charsPerToken   = 3
	// maxDeletedLines is the largest whole-file deletion shown inline.
	maxDeletedLines = 100
)

// Severities in display order.
var severities = []string{"blocking", "important", "question", "nit"}

// Finding is one reported problem, as the model returns it.
type Finding struct {
	Severity string `json:"severity"`
	Path     string `json:"path"`
	Line     int    `json:"line"`
	Title    string `json:"title"`
	Evidence string `json:"evidence"`
	Impact   string `json:"impact"`
}

// Output is the model's structured review.
type Output struct {
	Summary       string    `json:"summary"`
	Findings      []Finding `json:"findings"`
	Verdict       string    `json:"verdict"`
	VerdictReason string    `json:"verdict_reason"`
}

// outputSchema constrains the final answer. Property order matters: the
// model writes findings before committing to a verdict.
var outputSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "summary": {"type": "string"},
    "findings": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "severity": {"type": "string", "enum": ["blocking", "important", "nit", "question"]},
          "path": {"type": "string"},
          "line": {"type": "integer"},
          "title": {"type": "string"},
          "evidence": {"type": "string"},
          "impact": {"type": "string"}
        },
        "required": ["severity", "path", "line", "title", "evidence", "impact"]
      }
    },
    "verdict": {"type": "string", "enum": ["approve", "comment", "request_changes"]},
    "verdict_reason": {"type": "string"}
  },
  "required": ["summary", "findings", "verdict", "verdict_reason"]
}`)

func parseOutput(raw []byte) (Output, error) {
	var out Output
	if err := json.Unmarshal(raw, &out); err != nil {
		return Output{}, fmt.Errorf("invalid JSON: %w", err)
	}
	if strings.TrimSpace(out.Summary) == "" {
		return Output{}, errors.New("summary is empty")
	}
	if !slices.Contains([]string{"approve", "comment", "request_changes"}, out.Verdict) {
		return Output{}, fmt.Errorf("verdict %q is not approve, comment or request_changes", out.Verdict)
	}
	for i, f := range out.Findings {
		if !slices.Contains(severities, f.Severity) {
			return Output{}, fmt.Errorf("finding %d: severity %q is not one of %v", i, f.Severity, severities)
		}
	}
	return out, nil
}

// changedFile is a diff entry plus how it is presented to the model.
type changedFile struct {
	*diff.File
	// omitted explains why the diff is not inline ("" when it is).
	omitted string
}

var lockfiles = []string{
	"go.sum", "package-lock.json", "yarn.lock", "pnpm-lock.yaml", "Cargo.lock",
	"flake.lock", "poetry.lock", "uv.lock", "Gemfile.lock", "composer.lock",
}

// exclusionReason returns why a file's diff should not be inlined by
// default, or "". head reads the first bytes of the file at the head commit.
func exclusionReason(f *diff.File, head func(string) []byte) string {
	p := f.Path()
	base := path.Base(p)
	switch {
	case f.Binary:
		return "binary file"
	case slices.Contains(lockfiles, base) || strings.HasSuffix(base, ".lock"):
		return "lockfile"
	case strings.HasSuffix(base, ".min.js"), strings.HasSuffix(base, ".min.css"), strings.HasSuffix(base, ".map"):
		return "minified or generated asset"
	case slices.Contains(strings.Split(path.Dir(p), "/"), "vendor"),
		slices.Contains(strings.Split(path.Dir(p), "/"), "node_modules"),
		strings.HasPrefix(p, "dist/"):
		return "vendored or build output"
	case f.Status == diff.Deleted && f.Deletions > maxDeletedLines:
		return "deleted file"
	}
	if f.Status != diff.Deleted {
		header := string(head(p))
		if (strings.Contains(header, "Code generated") && strings.Contains(header, "DO NOT EDIT")) || strings.Contains(header, "@generated") {
			return "generated file"
		}
	}
	return ""
}

// selectDiffs decides which files get their diff inlined: excluded files
// never do, and the rest are inlined in order while they fit budget chars.
func selectDiffs(files []*diff.File, budget int, head func(string) []byte) []changedFile {
	out := make([]changedFile, 0, len(files))
	used := 0
	for _, f := range files {
		cf := changedFile{File: f, omitted: exclusionReason(f, head)}
		if cf.omitted == "" {
			size := len(f.Annotated())
			if used+size > budget {
				cf.omitted = "diff too large to include"
			} else {
				used += size
			}
		}
		out = append(out, cf)
	}
	return out
}

func systemPrompt(skillBody string, repo forge.Repo, forgeName string, pr forge.PR, tools, guidanceName, guidance string) string {
	var b strings.Builder
	b.WriteString(skillBody)
	fmt.Fprintf(&b, `

---

## Your situation

You are agent-nexus, an automated reviewer, reviewing pull request #%d of %s on %s.
%s You may call several tools in one turn.

How to work:
1. Read the pull request and its diff in the next message.
2. Use the tools to confirm or drop each concern.
3. When you are done investigating, reply in plain text with the findings you intend to report, without calling any tool. You will then be asked for the final review as JSON.
`, pr.Number, repo, forgeName, tools)
	if guidance != "" {
		fmt.Fprintf(&b, `
## Repository guidance

The repository's %s, read from the base branch, says:

<repository-guidance>
%s
</repository-guidance>
`, guidanceName, truncate(guidance, maxGuidance))
	}
	return b.String()
}

func userPrompt(repo forge.Repo, forgeName string, pr forge.PR, baseSHA string, files []changedFile) string {
	var b strings.Builder
	adds, dels := 0, 0
	for _, f := range files {
		adds += f.Additions
		dels += f.Deletions
	}
	fmt.Fprintf(&b, "<pull-request>\nRepository: %s (%s)\nPull request: #%d: %s\nAuthor: %s\nBase: %s @ %s; head: %s\nChanged: %d files, +%d -%d\n",
		repo, forgeName, pr.Number, pr.Title, pr.Author, pr.BaseRef, short(baseSHA), short(pr.HeadSHA), len(files), adds, dels)
	if desc := strings.TrimSpace(pr.Body); desc != "" {
		fmt.Fprintf(&b, "\nDescription:\n%s\n", truncate(desc, maxDescription))
	}
	b.WriteString("</pull-request>\n\n## Changed files\n\n")
	for _, f := range files {
		fmt.Fprintf(&b, "- %s %s (+%d -%d)", f.Status, displayPath(f.File), f.Additions, f.Deletions)
		if f.omitted != "" {
			fmt.Fprintf(&b, ": diff not shown (%s); use get_file_diff if needed", f.omitted)
		}
		b.WriteByte('\n')
	}
	b.WriteString("\n## Diff\n\nNew-file line numbers are in the left column; removed lines have none.\n")
	for _, f := range files {
		if f.omitted != "" {
			continue
		}
		fmt.Fprintf(&b, "\n<file path=%q status=%q>\n%s</file>\n", f.Path(), f.Status, f.Annotated())
	}
	return b.String()
}

func finalInstruction(turnLimitReached bool) string {
	var b strings.Builder
	if turnLimitReached {
		b.WriteString("You have reached the investigation limit. ")
	}
	b.WriteString(`Now write the final review as JSON.
- Include only findings you confirmed. Each finding's evidence is copied verbatim from the change.
- line is the new-file line number from the left column of the annotated diff (or from read_file); use 0 when the finding is not tied to one line.
- Order findings hardest-hitting first. An empty findings list is fine.
- verdict follows the skill's rules; verdict_reason is one sentence.`)
	return b.String()
}

func displayPath(f *diff.File) string {
	if f.Status == diff.Renamed {
		return f.OldPath + " → " + f.NewPath
	}
	return f.Path()
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "\n[truncated]"
}

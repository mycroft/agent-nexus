package web

import (
	"strings"
	"time"

	"github.com/mycroft/agent-nexus/internal/jobs"
)

const (
	reasoningOpen  = "<reasoning>\n"
	reasoningClose = "\n</reasoning>\n"
	callPrefix     = "\n→ "
	// Outputs longer than this start collapsed.
	collapseLines = 12
	collapseChars = 1500
)

// stepView is a transcript step as the job page shows it.
type stepView struct {
	Seq              int
	Role             string
	Title            string
	Tool             string
	At               time.Time
	PromptTokens     int
	CompletionTokens int
	// Assistant steps: reasoning, text and tool calls, split apart.
	Reasoning string
	Text      string
	Calls     []string
	// Collapsed starts the content folded: prompts, and long outputs.
	Collapsed bool
}

// newStepView parses a step. prompt marks the job's first two steps, the
// system and user prompts, which start folded.
func newStepView(st jobs.Step, prompt bool) stepView {
	v := stepView{
		Seq: st.Seq, Role: st.Role, Tool: st.ToolName, At: st.CreatedAt,
		PromptTokens: st.PromptTokens, CompletionTokens: st.CompletionTokens,
	}
	switch st.Role {
	case "assistant":
		v.Title = "Model"
		v.Reasoning, v.Text, v.Calls = splitAssistant(st.Content)
	case "tool":
		v.Title = st.ToolName
		v.Text = st.Content
		v.Collapsed = long(st.Content)
	case "system":
		v.Title = "System prompt"
		v.Text = st.Content
		v.Collapsed = true
	default:
		v.Title = "Instruction"
		if prompt {
			v.Title = "Request"
		}
		v.Text = st.Content
		v.Collapsed = prompt || long(st.Content)
	}
	return v
}

// splitAssistant undoes the agent's transcript format for model turns:
// optional "<reasoning>…</reasoning>", the text, then "→ tool(args)" lines.
func splitAssistant(content string) (string, string, []string) {
	reasoning := ""
	if rest, ok := strings.CutPrefix(content, reasoningOpen); ok {
		if r, after, found := strings.Cut(rest, reasoningClose); found {
			reasoning, content = r, after
		}
	}
	parts := strings.Split(content, callPrefix)
	text := strings.TrimSpace(parts[0])
	// A turn with only calls starts with the first call.
	if c, ok := strings.CutPrefix(text, "→ "); ok {
		parts[0], text = "", ""
		parts = append([]string{"", c}, parts[1:]...)
	}
	var calls []string
	for _, c := range parts[1:] {
		if c = strings.TrimSpace(c); c != "" {
			calls = append(calls, c)
		}
	}
	return strings.TrimSpace(reasoning), text, calls
}

func long(s string) bool {
	return len(s) > collapseChars || strings.Count(s, "\n") >= collapseLines
}

package review

import (
	"fmt"
	"slices"
	"strings"

	"github.com/mycroft/agent-nexus/internal/diff"
	"github.com/mycroft/agent-nexus/internal/forge"
)

// footer is the provenance line appended to every review body.
type footer struct {
	HeadSHA          string
	Model            string
	Turns            int
	PeakPromptTokens int
	CompletionTokens int
	JobID            int64
	// Warning, if set, is shown above the footer (e.g. an invalid
	// repository config file).
	Warning string
}

// verdict is the verdict a review is submitted with, in verdict mode.
type verdict struct {
	Event  forge.Event
	Reason string
	// Note explains a verdict downgraded by verdictEvent.
	Note string
}

// marker tags a review with the commit it covers, so a retried job can
// tell the review was already posted.
func marker(headSHA string) string {
	return "<!-- agent-nexus:sha=" + headSHA + " -->"
}

// render turns the model output into a review with inline comments for
// findings anchored on diff lines, and a body-only fallback carrying every
// finding (used when the forge rejects the inline comments). v is nil in
// comment mode: the review is a plain comment and the body has no verdict.
func render(out Output, files []*diff.File, f footer, v *verdict) (forge.NewReview, string) {
	byPath := map[string]*diff.File{}
	for _, file := range files {
		byPath[file.Path()] = file
	}

	findings := slices.Clone(out.Findings)
	slices.SortStableFunc(findings, func(a, b Finding) int {
		return slices.Index(severities, a.Severity) - slices.Index(severities, b.Severity)
	})

	review := forge.NewReview{CommitSHA: f.HeadSHA, Event: forge.EventComment}
	if v != nil {
		review.Event = v.Event
	}
	var inBody []Finding
	for _, fd := range findings {
		if file, ok := byPath[fd.Path]; ok && file.Commentable(fd.Line) {
			review.Comments = append(review.Comments, forge.Comment{Path: fd.Path, Line: fd.Line, Body: inlineBody(fd)})
			continue
		}
		inBody = append(inBody, fd)
	}
	review.Body = renderBody(out, inBody, len(review.Comments), f, v)
	return review, renderBody(out, findings, 0, f, v)
}

func renderBody(out Output, findings []Finding, inlined int, f footer, v *verdict) string {
	var b strings.Builder
	b.WriteString("## Summary\n\n")
	b.WriteString(strings.TrimSpace(out.Summary))
	b.WriteString("\n\n## Findings\n\n")
	switch {
	case len(findings) == 0 && inlined == 0:
		b.WriteString("No findings.\n\n")
	case inlined > 0:
		fmt.Fprintf(&b, "%d finding(s) posted as inline comments.\n\n", inlined)
	}
	for _, fd := range findings {
		fmt.Fprintf(&b, "**[%s]** %s: %s\n", fd.Severity, location(fd), oneLine(fd.Title))
		writeDetails(&b, fd)
		b.WriteString("\n")
	}
	if v != nil {
		writeVerdict(&b, v)
	}
	if f.Warning != "" {
		fmt.Fprintf(&b, "\n> [!WARNING]\n> %s\n", oneLine(f.Warning))
	}
	fmt.Fprintf(&b, "\n---\n<sub>agent-nexus · reviewed %s · %s · %d turns · peak context %d tokens · %d tokens generated · job %d</sub>\n%s\n",
		short(f.HeadSHA), f.Model, f.Turns, f.PeakPromptTokens, f.CompletionTokens, f.JobID, marker(f.HeadSHA))
	return b.String()
}

func writeVerdict(b *strings.Builder, v *verdict) {
	b.WriteString("## Verdict\n\n")
	for _, e := range []struct {
		event forge.Event
		label string
	}{
		{forge.EventApprove, "Approve"}, {forge.EventComment, "Comment"}, {forge.EventRequestChanges, "Request changes"},
	} {
		box := " "
		if v.Event == e.event {
			box = "x"
		}
		fmt.Fprintf(b, "- [%s] %s\n", box, e.label)
	}
	if reason := strings.TrimSpace(v.Reason); reason != "" {
		fmt.Fprintf(b, "\n%s\n", reason)
	}
	if v.Note != "" {
		fmt.Fprintf(b, "\n_%s_\n", v.Note)
	}
}

func inlineBody(fd Finding) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**[%s]** %s\n", fd.Severity, oneLine(fd.Title))
	writeDetails(&b, fd)
	return b.String()
}

func writeDetails(b *strings.Builder, fd Finding) {
	evidence := strings.TrimRight(fd.Evidence, "\n")
	if !strings.Contains(evidence, "\n") && !strings.Contains(evidence, "`") {
		fmt.Fprintf(b, "> evidence: `%s`\n", strings.TrimSpace(evidence))
	} else {
		fence := codeFence(evidence)
		b.WriteString("> evidence:\n> " + fence + "\n")
		for line := range strings.SplitSeq(evidence, "\n") {
			b.WriteString("> " + line + "\n")
		}
		b.WriteString("> " + fence + "\n")
	}
	if impact := strings.TrimSpace(fd.Impact); impact != "" {
		fmt.Fprintf(b, "> impact: %s\n", oneLine(impact))
	}
}

// codeFence returns a backtick fence longer than any backtick run in s.
func codeFence(s string) string {
	longest, run := 0, 0
	for _, r := range s {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	return strings.Repeat("`", max(3, longest+1))
}

func location(fd Finding) string {
	if fd.Line > 0 {
		return fmt.Sprintf("`%s:%d`", fd.Path, fd.Line)
	}
	if fd.Path != "" {
		return "`" + fd.Path + "`"
	}
	return "(general)"
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

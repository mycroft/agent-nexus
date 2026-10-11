package web

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mycroft/agent-nexus/internal/jobs"
)

func TestSplitAssistant(t *testing.T) {
	for _, tc := range []struct {
		content, reasoning, text string
		calls                    []string
	}{
		{"Done.", "", "Done.", nil},
		{"<reasoning>\nthink\nmore\n</reasoning>\nAnswer", "think\nmore", "Answer", nil},
		{"Looking.\n→ grep({})\n→ read_file({})", "", "Looking.", []string{"grep({})", "read_file({})"}},
		{"→ grep({})\n→ list_dir({})", "", "", []string{"grep({})", "list_dir({})"}},
		{"<reasoning>\nr\n</reasoning>\n→ grep({})", "r", "", []string{"grep({})"}},
		// An unterminated block is left as text.
		{"<reasoning>\nr", "", "<reasoning>\nr", nil},
	} {
		reasoning, text, calls := splitAssistant(tc.content)
		if reasoning != tc.reasoning || text != tc.text || fmt.Sprint(calls) != fmt.Sprint(tc.calls) {
			t.Errorf("splitAssistant(%q) = %q, %q, %q; want %q, %q, %q", tc.content, reasoning, text, calls, tc.reasoning, tc.text, tc.calls)
		}
	}
}

func TestNewStepView(t *testing.T) {
	longOutput := strings.Repeat("line\n", collapseLines)
	for _, tc := range []struct {
		name      string
		step      jobs.Step
		prompt    bool
		title     string
		collapsed bool
	}{
		{"system", jobs.Step{Role: "system", Content: "s"}, true, "System prompt", true},
		{"request", jobs.Step{Role: "user", Content: "u"}, true, "Request", true},
		{"instruction", jobs.Step{Role: "user", Content: "u"}, false, "Instruction", false},
		{"short tool", jobs.Step{Role: "tool", ToolName: "grep", Content: "x"}, false, "grep", false},
		{"long tool", jobs.Step{Role: "tool", ToolName: "grep", Content: longOutput}, false, "grep", true},
		{"model", jobs.Step{Role: "assistant", Content: longOutput}, false, "Model", false},
	} {
		v := newStepView(tc.step, tc.prompt)
		if v.Title != tc.title || v.Collapsed != tc.collapsed {
			t.Errorf("%s: title %q collapsed %v, want %q %v", tc.name, v.Title, v.Collapsed, tc.title, tc.collapsed)
		}
	}
}

func TestOutcome(t *testing.T) {
	for _, tc := range []struct {
		status jobs.Status
		result string
		want   string
	}{
		{jobs.Succeeded, `{"verdict":"request_changes","event":"comment","findings":2}`, "comment · 2 findings"},
		{jobs.Succeeded, `{"verdict":"approve","findings":1}`, "approve · 1 finding"},
		{jobs.Succeeded, `{"outcome":"answered","answered":3,"approved":false}`, "3 answered"},
		{jobs.Succeeded, `{"outcome":"answered","answered":1,"approved":true}`, "1 answered · approved"},
		{jobs.Succeeded, `{"outcome":"approved","approved":true}`, "approved"},
		{jobs.Succeeded, `{"outcome":"already_posted"}`, "already posted"},
		{jobs.Skipped, `{"outcome":"skipped_closed","verdict":"approve"}`, "pull request closed"},
		{jobs.Skipped, `{"outcome":"something_new"}`, "something_new"},
		{jobs.Superseded, ``, "newer commit"},
		{jobs.Failed, `{"verdict":"approve"}`, "failed"},
		{jobs.Running, ``, ""},
	} {
		v := newJobView(jobs.JobSummary{Job: jobs.Job{Status: tc.status, Result: []byte(tc.result)}}, nil)
		if got := v.Outcome(); got != tc.want {
			t.Errorf("Outcome(%s, %s) = %q, want %q", tc.status, tc.result, got, tc.want)
		}
	}
}

func TestNewJobViewWithoutPullRequest(t *testing.T) {
	v := newJobView(jobs.JobSummary{Job: jobs.Job{Payload: []byte(`{"question":"?"}`)}}, map[string]string{"gitea": "https://git.example"})
	if v.Repo != "" || v.PRURL != "" {
		t.Errorf("view = %+v, want no pull request", v)
	}
}

func TestDuration(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { t := start.Add(d); return &t }
	for _, tc := range []struct {
		end  *time.Time
		want string
	}{
		{at(45 * time.Second), "45s"},
		{at(3*time.Minute + 5*time.Second), "3m 05s"},
		{at(2*time.Hour + 7*time.Minute), "2h 07m"},
	} {
		if got := duration(&start, tc.end); got != tc.want {
			t.Errorf("duration = %q, want %q", got, tc.want)
		}
	}
	if got := duration(nil, nil); got != "" {
		t.Errorf("duration of an unstarted job = %q", got)
	}
}

func TestFormatters(t *testing.T) {
	for n, want := range map[int]string{950: "950", 12_345: "12.3k", 1_580_000: "1.58M"} {
		if got := tokens(n); got != want {
			t.Errorf("tokens(%d) = %q, want %q", n, got, want)
		}
	}
	for d, want := range map[time.Duration]string{-time.Second: "0s", 90 * time.Second: "1m", 5 * time.Hour: "5h", 72 * time.Hour: "3d"} {
		if got := ago(time.Now().Add(-d)); got != want {
			t.Errorf("ago(%s) = %q, want %q", d, got, want)
		}
	}
	for s, want := range map[string]string{"one": "1 line", "a\nb": "2 lines"} {
		if got := countLines(s); got != want {
			t.Errorf("countLines(%q) = %q, want %q", s, got, want)
		}
	}
	version := funcs["displayVersion"].(func(string) string)
	for in, want := range map[string]string{"": "dev", "dev": "dev", "0.1.4": "v0.1.4", "v0.1.4": "v0.1.4"} {
		if got := version(in); got != want {
			t.Errorf("displayVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

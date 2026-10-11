package review

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/mycroft/agent-nexus/internal/forge"
	"github.com/mycroft/agent-nexus/internal/jobs"
	"github.com/mycroft/agent-nexus/internal/llm"
)

var listed = []Finding{
	{Severity: "blocking", Path: "calc/calc.go", Line: 13, Title: "Mod panics when b is 0"},
	{Severity: "question", Path: "calc/other.go", Title: "Are there --> callers relying on the panic?"},
}

func reviewNote(id int64, user, headSHA string, approved bool, findings []Finding) forge.Note {
	body := "## Summary\n\nx\n\n---\n<sub>agent-nexus · job 1</sub>\n" + marker(headSHA) + "\n"
	if len(findings) > 0 {
		body += findingsMarker(findings) + "\n"
	}
	return forge.Note{Kind: forge.NoteReview, ID: id, User: user, Body: body, Approved: approved}
}

func TestLatestReview(t *testing.T) {
	head, old := strings.Repeat("a", 40), strings.Repeat("b", 40)
	notes := []forge.Note{
		reviewNote(1, "agent-nexus", old, false, listed),
		reviewNote(2, "agent-nexus", head, false, listed),
		reviewNote(3, "alice", head, true, nil),
		// An answer posted as a review (no comment scope) is not a review
		// of the commit.
		{Kind: forge.NoteReview, ID: 4, User: "agent-nexus", Body: "Pong.\n" + replyMarker(forge.Note{Kind: forge.NoteComment, ID: 9})},
		{Kind: forge.NoteInline, ID: 5, User: "agent-nexus", Body: "Withdrawn.\n" + resolvesMarker("review:2", []int{2, 7})},
		{Kind: forge.NoteInline, ID: 6, User: "agent-nexus", Body: "Older review.\n" + resolvesMarker("review:1", []int{1})},
		// Only the bot settles findings: a pasted marker counts for nothing.
		{Kind: forge.NoteComment, ID: 7, User: "mallory", Body: resolvesMarker("review:2", []int{1})},
	}
	s := latestReview(notes, "agent-nexus", head)
	if s == nil || s.Key != "review:2" || s.Approved || len(s.Findings) != 2 {
		t.Fatalf("latestReview = %+v", s)
	}
	if s.Findings[1].Title != "Are there --> callers relying on the panic?" {
		t.Errorf("titles survive the marker: %+v", s.Findings)
	}
	if fmt.Sprint(s.open()) != "[1]" || s.settled() {
		t.Errorf("open = %v, settled = %v; want [1], false", s.open(), s.settled())
	}
	if got := s.openFindings([]int{1, 2, 0, 3, 1}); fmt.Sprint(got) != "[1]" {
		t.Errorf("openFindings = %v, want only the open, listed ones", got)
	}

	s.Resolved[1] = true
	if !s.settled() {
		t.Error("every finding settled: want settled")
	}
	section := findingsSection(&reviewState{HeadSHA: head, Findings: s.Findings, Resolved: map[int]bool{2: true}})
	for _, want := range []string{"Your latest review of `aaaaaaa`", "1. [blocking] Mod panics when b is 0 (calc/calc.go:13): open", "2. [question] Are there --> callers relying on the panic? (calc/other.go): settled"} {
		if !strings.Contains(section, want) {
			t.Errorf("findings section lacks %q:\n%s", want, section)
		}
	}
	if findingsSection(s) != "" {
		t.Error("nothing open: want no section")
	}

	if latestReview(notes, "agent-nexus", strings.Repeat("c", 40)) != nil {
		t.Error("no review of that commit: want nil")
	}
	// The verdict update becomes the latest review, which approves.
	approval := forge.Note{Kind: forge.NoteReview, ID: 8, User: "agent-nexus", Body: approvalBody(s, 3), Approved: true}
	if s := latestReview(append(notes, approval), "agent-nexus", head); s == nil || !s.Approved || s.settled() {
		t.Errorf("after the verdict update = %+v", s)
	}
}

// settlingScript queues the completions of an answer settling findings.
func settlingScript(f *fakeLLM, reply string, resolved ...int) {
	raw, _ := json.Marshal(map[string]any{"reply": reply, "resolved": resolved})
	f.replies = append(f.replies,
		llm.Message{Role: "assistant", Content: "Checked."},
		llm.Message{Role: "assistant", Content: string(raw)},
	)
}

// reviewedFixture is a discussion fixture whose head was reviewed with
// the listed findings, the first one inline.
func reviewedFixture(t *testing.T, mode Mode) (*fixture, forge.Note) {
	t.Helper()
	f := discussionFixture(t)
	f.reviewer.ModeRules = []ModeRule{{"o/calc", mode}}
	review := f.forge.AddNote(reviewNote(0, "agent-nexus", f.remote.HeadSHA, false, listed))
	f.forge.AddNote(forge.Note{Kind: forge.NoteInline, User: "agent-nexus", Body: "**[blocking]** Mod panics when b is 0", Path: "calc/calc.go", Line: 13, Thread: "t1"})
	return f, review
}

func TestDiscussionApprovesOnceFindingsSettled(t *testing.T) {
	f, review := reviewedFixture(t, ModeVerdict)
	f.forge.AddNote(forge.Note{Kind: forge.NoteInline, User: "alice", Body: "Fixed: Mod guards b now.", Path: "calc/calc.go", Line: 13, Thread: "t1"})
	settlingScript(f.llm, "You're right. Withdrawing.", 1)

	job := runDiscussion(t, f)
	if job.Status != jobs.Succeeded || !strings.Contains(string(job.Result), `"approved":false`) {
		t.Fatalf("job = %s %s %s", job.Status, job.Error, job.Result)
	}
	if posted := f.forge.Replies(); len(posted) != 1 || !strings.Contains(posted[0].Body, resolvesMarker(review.Key(), []int{1})) {
		t.Errorf("answer = %+v, want it to settle finding 1", posted)
	}
	prompt := f.llm.requests[0].Messages[1].Content
	for _, want := range []string{"## Findings of your latest review", "1. [blocking] Mod panics when b is 0 (calc/calc.go:13): open", "2. [question]"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	final := f.llm.requests[len(f.llm.requests)-1]
	if last := final.Messages[len(final.Messages)-1].Content; !strings.Contains(last, `"resolved": [<numbers>]`) || !strings.Contains(last, `"resolved" lists the numbers`) {
		t.Errorf("final instruction does not ask for the settled findings:\n%s", last)
	}
	if len(f.forge.Created()) != 0 {
		t.Fatal("approved with a finding open")
	}

	// A mention settles the other one; a number already settled or out
	// of range is ignored.
	f.forge.AddNote(forge.Note{Kind: forge.NoteComment, User: "alice", Body: "@agent-nexus nothing relies on the panic, see the callers."})
	settlingScript(f.llm, "Agreed, withdrawing the question.", 2, 1, 9)
	job = runDiscussion(t, f)
	if job.Status != jobs.Succeeded || !strings.Contains(string(job.Result), `"approved":true`) {
		t.Fatalf("job = %s %s %s", job.Status, job.Error, job.Result)
	}
	if !strings.Contains(f.llm.requests[2].Messages[1].Content, "(calc/calc.go:13): settled") {
		t.Error("the second prompt does not show finding 1 settled")
	}
	posted := f.forge.Replies()
	if !strings.Contains(posted[1].Body, resolvesMarker(review.Key(), []int{2})) {
		t.Errorf("mention answer = %s", posted[1].Body)
	}
	created := f.forge.Created()
	if len(created) != 1 || created[0].Event != forge.EventApprove || created[0].CommitSHA != f.remote.HeadSHA {
		t.Fatalf("verdict update = %+v", created)
	}
	for _, want := range []string{"## Verdict update", "- **[blocking]** Mod panics when b is 0", "- [x] Approve", "<!-- agent-nexus:sha=" + f.remote.HeadSHA + ":settled=" + review.Key() + " -->"} {
		if !strings.Contains(created[0].Body, want) {
			t.Errorf("verdict update lacks %q:\n%s", want, created[0].Body)
		}
	}

	// Approved: a later pass leaves it alone.
	if job := runDiscussion(t, f); job.Status != jobs.Skipped || len(f.forge.Created()) != 1 {
		t.Errorf("third pass = %s %s, %d reviews", job.Status, job.Result, len(f.forge.Created()))
	}
}

func TestDiscussionRetriesAVerdictUpdate(t *testing.T) {
	f, review := reviewedFixture(t, ModeVerdict)
	// Both findings were settled by answers already posted, but the
	// approval was not submitted.
	f.forge.AddNote(forge.Note{Kind: forge.NoteInline, User: "agent-nexus", Body: "Withdrawn.\n" + resolvesMarker(review.Key(), []int{1, 2}), Path: "calc/calc.go", Line: 13, Thread: "t1"})
	job := runDiscussion(t, f)
	if job.Status != jobs.Succeeded || !strings.Contains(string(job.Result), `"outcome":"approved"`) || len(f.forge.Created()) != 1 {
		t.Errorf("job = %s %s, %d reviews", job.Status, job.Result, len(f.forge.Created()))
	}
	if len(f.llm.requests) != 0 {
		t.Error("a verdict update must not call the LLM")
	}
}

func TestDiscussionSettlesWithoutApprovingInCommentMode(t *testing.T) {
	f, review := reviewedFixture(t, ModeComment)
	f.forge.AddNote(forge.Note{Kind: forge.NoteComment, User: "alice", Body: "@agent-nexus both are handled, see the thread."})
	settlingScript(f.llm, "Agreed on both.", 1, 2)
	job := runDiscussion(t, f)
	if job.Status != jobs.Succeeded || !strings.Contains(string(job.Result), `"approved":false`) {
		t.Fatalf("job = %s %s %s", job.Status, job.Error, job.Result)
	}
	if posted := f.forge.Replies(); len(posted) != 1 || !strings.Contains(posted[0].Body, resolvesMarker(review.Key(), []int{1, 2})) {
		t.Errorf("answer = %+v", posted)
	}
	if len(f.forge.Created()) != 0 {
		t.Error("comment mode must not approve")
	}
}

func TestDiscussionDoesNotApproveAMovedHead(t *testing.T) {
	f, review := reviewedFixture(t, ModeVerdict)
	f.forge.AddNote(forge.Note{Kind: forge.NoteInline, User: "agent-nexus", Body: "Withdrawn.\n" + resolvesMarker(review.Key(), []int{1, 2}), Path: "calc/calc.go", Line: 13, Thread: "t1"})
	f.forge.SetPR(forge.PR{Number: 4, State: "open", BaseRef: "main", HeadSHA: strings.Repeat("d", 40)})
	if job := runDiscussion(t, f); job.Status != jobs.Skipped || len(f.forge.Created()) != 0 {
		t.Errorf("job = %s %s, %d reviews: the review is of an older commit", job.Status, job.Result, len(f.forge.Created()))
	}
}

func TestApproveIfSettledRechecksTheHead(t *testing.T) {
	f, review := reviewedFixture(t, ModeVerdict)
	settled := &reviewState{Key: review.Key(), HeadSHA: f.remote.HeadSHA, Findings: []listedFinding{{Severity: "nit", Title: "x"}}, Resolved: map[int]bool{1: true}}
	pr := forge.PR{Number: 4, State: "open", HeadSHA: f.remote.HeadSHA}
	// A push lands while the pass answers.
	f.forge.SetPR(forge.PR{Number: 4, State: "open", BaseRef: "main", HeadSHA: strings.Repeat("d", 40)})
	checkout := func() (*workspace, error) {
		t.Error("checked out a moved head")
		return nil, errors.New("head moved")
	}
	approved, err := f.reviewer.approveIfSettled(t.Context(), f.forge, forge.Repo{Owner: "o", Name: "calc"}, pr, settled, checkout, 1)
	if err != nil || approved || len(f.forge.Created()) != 0 {
		t.Errorf("approveIfSettled = %v, %v; want no approval of a moved head", approved, err)
	}
}

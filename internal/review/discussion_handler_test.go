package review

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mycroft/agent-nexus/internal/forge"
	"github.com/mycroft/agent-nexus/internal/jobs"
	"github.com/mycroft/agent-nexus/internal/llm"
	"github.com/mycroft/agent-nexus/internal/skill"
	"github.com/mycroft/agent-nexus/skills"
)

// replyScript queues the completions of one answer: an investigation turn
// with a tool call, a plain-text turn, and the final JSON.
func replyScript(f *fakeLLM, reply string) {
	raw, _ := json.Marshal(map[string]string{"reply": reply})
	f.replies = append(f.replies,
		llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "1", Type: "function", Function: llm.FunctionCall{Name: "read_file", Arguments: `{"path":"calc/calc.go"}`}}}},
		llm.Message{Role: "assistant", Content: "Checked."},
		llm.Message{Role: "assistant", Content: string(raw)},
	)
}

// runDiscussion runs one discussion job through the queue and returns it.
func runDiscussion(t *testing.T, f *fixture) jobs.Job {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, err := jobs.OpenStore(ctx, filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	// A realistic timeout: a short one leaves the agent no time to investigate.
	q := jobs.NewQueue(store, 1, 30*time.Minute, time.Hour, quiet)
	q.Register(DiscussionKind, f.reviewer.DiscussionHandler())
	job, _, err := q.Enqueue(ctx, jobs.NewJob{Kind: DiscussionKind, GroupKey: "g:discussion", DedupeKey: "d", KeepRunning: true,
		Payload: DiscussionRequest{Forge: "gitea", Owner: "o", Repo: "calc", PR: 4}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error)
	go func() { done <- q.Run(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got, err := store.Get(ctx, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != jobs.Queued && got.Status != jobs.Running {
			cancel()
			<-done
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("job still %s", got.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func discussionFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	s, err := skill.Load(skills.FS, "pr-discussion")
	if err != nil {
		t.Fatal(err)
	}
	f.reviewer.DiscussionSkill = s
	return f
}

func TestDiscussionAnswersRepliesAndMentions(t *testing.T) {
	f := discussionFixture(t)
	finding := f.forge.AddNote(forge.Note{Kind: forge.NoteInline, User: "agent-nexus", Body: "**[blocking]** Mod panics when b is 0\n\n<sub>agent-nexus</sub>", Path: "calc/calc.go", Line: 13, Thread: "t1"})
	reply := f.forge.AddNote(forge.Note{Kind: forge.NoteInline, User: "alice", Body: "Callers never pass 0.", Path: "calc/calc.go", Line: 13, Thread: "t1"})
	mention := f.forge.AddNote(forge.Note{Kind: forge.NoteComment, User: "bob", Body: "@agent-nexus does Div still return 0 for b == 0?"})
	f.forge.AddNote(forge.Note{Kind: forge.NoteComment, User: "carol", Body: "unrelated comment"})
	replyScript(f.llm, "No: `Mod` is exported and has no guard on line 13.")
	replyScript(f.llm, "Yes, line 5 returns 0.")

	job := runDiscussion(t, f)
	if job.Status != jobs.Succeeded || !strings.Contains(string(job.Result), `"answered":2,"answered_elsewhere":0`) || !strings.Contains(string(job.Result), `"remaining":0`) {
		t.Fatalf("job = %s %s %s", job.Status, job.Error, job.Result)
	}
	posted := f.forge.Replies()
	if len(posted) != 2 {
		t.Fatalf("posted %d answers, want 2: %+v", len(posted), posted)
	}
	if posted[0].Kind != forge.NoteInline || posted[0].Thread != finding.Thread ||
		!strings.HasPrefix(posted[0].Body, "No: `Mod` is exported") || !strings.Contains(posted[0].Body, replyMarker(reply)) {
		t.Errorf("thread answer = %+v", posted[0])
	}
	if posted[1].Kind != forge.NoteComment || !strings.HasPrefix(posted[1].Body, "> **@bob**:") || !strings.Contains(posted[1].Body, replyMarker(mention)) {
		t.Errorf("mention answer = %+v", posted[1])
	}

	// The answers' prompts carry the conversation; the thread's also has
	// the file's diff, the mention's the rest of the discussion.
	threadPrompt := f.llm.requests[0].Messages[1].Content
	for _, want := range []string{"Thread on calc/calc.go, line 13", "**agent-nexus (you)**:\n**[blocking]** Mod panics", "**alice**:\nCallers never pass 0.", `<file path="calc/calc.go">`, "Answer the latest message(s) from alice."} {
		if !strings.Contains(threadPrompt, want) {
			t.Errorf("thread prompt lacks %q:\n%s", want, threadPrompt)
		}
	}
	mentionPrompt := f.llm.requests[3].Messages[1].Content
	for _, want := range []string{"bob mentioned you in a pull request comment.", "For background", "unrelated comment"} {
		if !strings.Contains(mentionPrompt, want) {
			t.Errorf("mention prompt lacks %q:\n%s", want, mentionPrompt)
		}
	}
	if !strings.Contains(f.llm.requests[0].Messages[0].Content, "# PR Discussion") {
		t.Error("system prompt lacks the pr-discussion skill")
	}

	// Everything is answered: the next pass has nothing to do.
	job = runDiscussion(t, f)
	if job.Status != jobs.Skipped || !strings.Contains(string(job.Result), "nothing_to_answer") || len(f.forge.Replies()) != 2 {
		t.Errorf("second pass = %s %s, %d answers", job.Status, job.Result, len(f.forge.Replies()))
	}
}

func TestDiscussionFallsBackToReviewForComments(t *testing.T) {
	f := discussionFixture(t)
	f.forge.RejectComments = true
	f.forge.AddReview(forge.Review{ID: 77, User: "agent-nexus", State: "PENDING"})
	mention := f.forge.AddNote(forge.Note{Kind: forge.NoteComment, User: "bob", Body: "@agent-nexus ping"})
	replyScript(f.llm, "Pong.")
	job := runDiscussion(t, f)
	if job.Status != jobs.Succeeded {
		t.Fatalf("job = %s %s", job.Status, job.Error)
	}
	created := f.forge.Created()
	if len(created) != 1 || created[0].Event != forge.EventComment || created[0].CommitSHA != f.remote.HeadSHA ||
		!strings.Contains(created[0].Body, "Pong.") || !strings.Contains(created[0].Body, replyMarker(mention)) {
		t.Errorf("fallback review = %+v", created)
	}
	reviews, _ := f.forge.ListReviews(context.Background(), forge.Repo{}, 4)
	for _, r := range reviews {
		if r.State == "PENDING" {
			t.Error("a pending review was left behind before the fallback review")
		}
	}
	// The review counts as the answer.
	if asks := pendingAsks(mustNotes(t, f), "agent-nexus"); len(asks) != 0 {
		t.Errorf("still pending after the fallback: %+v", asks)
	}
}

func TestDiscussionSkipsClosedAndQuietPullRequests(t *testing.T) {
	f := discussionFixture(t)
	if job := runDiscussion(t, f); job.Status != jobs.Skipped || !strings.Contains(string(job.Result), "nothing_to_answer") {
		t.Errorf("quiet = %s %s", job.Status, job.Result)
	}
	f.forge.AddNote(forge.Note{Kind: forge.NoteComment, User: "bob", Body: "@agent-nexus ping"})
	f.forge.SetPR(forge.PR{Number: 4, State: "closed", BaseRef: "main", HeadSHA: f.remote.HeadSHA})
	if job := runDiscussion(t, f); job.Status != jobs.Skipped || !strings.Contains(string(job.Result), string(SkippedClosed)) {
		t.Errorf("closed = %s %s", job.Status, job.Result)
	}
	if len(f.llm.requests) != 0 {
		t.Error("skipped passes must not call the LLM")
	}
}

func TestDraftIncludesEarlierDiscussion(t *testing.T) {
	f := newFixture(t)
	f.forge.AddNote(forge.Note{Kind: forge.NoteReview, User: "agent-nexus", Body: "## Summary\n\nFirst pass.\n\n---\n<sub>agent-nexus · job 1</sub>\n" + marker("old")})
	f.forge.AddNote(forge.Note{Kind: forge.NoteInline, User: "agent-nexus", Body: "**[question]** Is 0 a valid divisor?", Path: "calc/calc.go", Line: 5, Thread: "t1"})
	f.forge.AddNote(forge.Note{Kind: forge.NoteInline, User: "alice", Body: "Yes, Div returns 0 by design.", Path: "calc/calc.go", Line: 5, Thread: "t1"})
	f.script(sampleOutput)
	if _, _, err := f.reviewer.Draft(context.Background(), f.req, 2, nil); err != nil {
		t.Fatal(err)
	}
	user := f.llm.requests[0].Messages[1].Content
	for _, want := range []string{"## Earlier discussion", "Do not repeat a finding that was answered convincingly", "**alice**:\nYes, Div returns 0 by design.", "### Your latest review\n\n## Summary\n\nFirst pass."} {
		if !strings.Contains(user, want) {
			t.Errorf("review prompt lacks %q:\n%s", want, user)
		}
	}
	if strings.Contains(user, "agent-nexus:sha=") {
		t.Error("markers leak into the prompt")
	}
}

func mustNotes(t *testing.T, f *fixture) []forge.Note {
	t.Helper()
	notes, err := f.forge.ListNotes(context.Background(), forge.Repo{}, 4)
	if err != nil {
		t.Fatal(err)
	}
	return notes
}

func TestDiscussionReviewCommand(t *testing.T) {
	f := discussionFixture(t)
	var queued []jobs.NewJob
	f.reviewer.Enqueue = func(_ context.Context, nj jobs.NewJob) (jobs.Job, bool, error) {
		queued = append(queued, nj)
		return jobs.Job{ID: 42}, true, nil
	}
	ask := f.forge.AddNote(forge.Note{Kind: forge.NoteComment, User: "mycroft", Body: "@agent-nexus review again"})
	job := runDiscussion(t, f)
	if job.Status != jobs.Succeeded {
		t.Fatalf("job = %s %s", job.Status, job.Error)
	}
	if len(queued) != 1 {
		t.Fatalf("queued %d jobs", len(queued))
	}
	var req Request
	raw, _ := json.Marshal(queued[0].Payload)
	_ = json.Unmarshal(raw, &req)
	if queued[0].Kind != Kind || !req.Force || req.HeadSHA != f.remote.HeadSHA || queued[0].APIKeyName != "comment:mycroft" {
		t.Errorf("queued %+v with %+v", queued[0], req)
	}
	posted := f.forge.Replies()
	if len(posted) != 1 || !strings.Contains(posted[0].Body, "Queued a new review of `"+f.remote.HeadSHA[:7]+"` (job 42).") || !strings.Contains(posted[0].Body, replyMarker(ask)) {
		t.Errorf("answer = %+v", posted)
	}
	if len(f.llm.requests) != 0 {
		t.Error("a review command must not call the LLM")
	}

	for body, want := range map[string]bool{
		"@agent-nexus review":                 true,
		"@agent-nexus: re-review please":      true,
		"@Agent-Nexus rereview":               true,
		"@agent-nexus what does review mean?": false,
		"please @agent-nexus":                 false,
	} {
		if got := reviewCommandPattern("agent-nexus").MatchString(body); got != want {
			t.Errorf("review command %q = %v, want %v", body, got, want)
		}
	}
}

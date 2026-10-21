package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func newJob(sha string) NewJob {
	return NewJob{Kind: "k", GroupKey: "g#1", DedupeKey: "g#1@" + sha, APIKeyName: "test", Payload: map[string]string{"sha": sha}}
}

func TestEnqueueDedupeAndSupersede(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	a, created, _, err := s.Enqueue(ctx, newJob("a"))
	if err != nil || !created || a.Status != Queued {
		t.Fatalf("first enqueue = %+v, %v, %v", a, created, err)
	}
	dup, created, _, err := s.Enqueue(ctx, newJob("a"))
	if err != nil || created || dup.ID != a.ID {
		t.Fatalf("duplicate enqueue = %+v, %v, %v", dup, created, err)
	}

	// Claim a, then a newer commit arrives: a is superseded and reported
	// for cancellation.
	claimed, err := s.Claim(ctx, ClaimSpec{})
	if err != nil || claimed.ID != a.ID || claimed.Status != Running || claimed.Attempts != 1 {
		t.Fatalf("Claim = %+v, %v", claimed, err)
	}
	b, created, cancel, err := s.Enqueue(ctx, newJob("b"))
	if err != nil || !created {
		t.Fatalf("enqueue b = %v, %v", created, err)
	}
	if len(cancel) != 1 || cancel[0] != a.ID {
		t.Errorf("cancel = %v, want [%d]", cancel, a.ID)
	}
	if got, _ := s.Get(ctx, a.ID); got.Status != Superseded {
		t.Errorf("a status = %s, want superseded", got.Status)
	}
	// The superseded worker can neither checkpoint nor overwrite the state.
	if err := s.Checkpoint(ctx, a.ID, []byte(`{}`)); !errors.Is(err, ErrNotRunning) {
		t.Errorf("Checkpoint on superseded job = %v, want ErrNotRunning", err)
	}
	if updated, err := s.Finish(ctx, a.ID, Failed, nil, "cancelled"); err != nil || updated {
		t.Errorf("Finish on superseded job = %v, %v; want no update", updated, err)
	}

	// A failed job does not block re-enqueueing the same commit.
	if _, err := s.Claim(ctx, ClaimSpec{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Finish(ctx, b.ID, Failed, nil, "boom"); err != nil {
		t.Fatal(err)
	}
	retry, created, _, err := s.Enqueue(ctx, newJob("b"))
	if err != nil || !created || retry.ID == b.ID {
		t.Fatalf("re-enqueue after failure = %+v, %v, %v", retry, created, err)
	}

	// A succeeded job does.
	if _, err := s.Claim(ctx, ClaimSpec{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Finish(ctx, retry.ID, Succeeded, []byte(`{"ok":true}`), ""); err != nil {
		t.Fatal(err)
	}
	again, created, _, err := s.Enqueue(ctx, newJob("b"))
	if err != nil || created || again.ID != retry.ID || again.Status != Succeeded {
		t.Fatalf("enqueue after success = %+v, %v, %v", again, created, err)
	}
}

func TestRecover(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	posting, _, _, _ := s.Enqueue(ctx, NewJob{Kind: "k", GroupKey: "g1", DedupeKey: "d1", Payload: 1})
	running, _, _, _ := s.Enqueue(ctx, NewJob{Kind: "k", GroupKey: "g2", DedupeKey: "d2", Payload: 2})
	if _, err := s.Claim(ctx, ClaimSpec{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Checkpoint(ctx, posting.ID, []byte(`{"draft":1}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(ctx, ClaimSpec{}); err != nil {
		t.Fatal(err)
	}

	requeued, failed, err := s.Recover(ctx, 3)
	if err != nil || requeued != 2 || failed != 0 {
		t.Fatalf("Recover = %d, %d, %v", requeued, failed, err)
	}
	got, _ := s.Get(ctx, posting.ID)
	if got.Status != Queued || string(got.Checkpoint) != `{"draft":1}` {
		t.Errorf("recovered posting job = %+v, want queued with its checkpoint", got)
	}

	// Two more interrupted runs exhaust the attempts of the running job.
	for range 2 {
		for range 2 {
			if _, err := s.Claim(ctx, ClaimSpec{}); err != nil {
				t.Fatal(err)
			}
		}
		if _, _, err := s.Recover(ctx, 3); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := s.Get(ctx, running.ID); got.Status != Failed {
		t.Errorf("job interrupted 3 times has status %s, want failed", got.Status)
	}
}

func TestClaimByModel(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	enqueue := func(kind, group string) Job {
		t.Helper()
		j, _, _, err := s.Enqueue(ctx, NewJob{Kind: kind, GroupKey: group, DedupeKey: group, Payload: group})
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	review1 := enqueue("pr_review", "a")
	chat := enqueue("chat_question", "b")
	review2 := enqueue("pr_review", "c")

	small := ClaimSpec{Model: "small", Limit: 1, Kinds: []string{"chat_question"}}
	big := ClaimSpec{Model: "big", Limit: 1, Kinds: []string{"pr_review", "chat_question"}}

	// A model only takes the kinds it serves, oldest first.
	if got, err := s.Claim(ctx, small); err != nil || got == nil || got.ID != chat.ID || got.Model != "small" {
		t.Fatalf("Claim(small) = %+v, %v; want the chat job", got, err)
	}
	if got, err := s.Claim(ctx, small); err != nil || got != nil {
		t.Fatalf("Claim(small) = %+v, %v; want nothing at its limit", got, err)
	}
	if got, err := s.Claim(ctx, big); err != nil || got == nil || got.ID != review1.ID || got.Model != "big" {
		t.Fatalf("Claim(big) = %+v, %v; want the first review", got, err)
	}
	// The limit counts the model's running jobs, whoever claimed them.
	if got, err := s.Claim(ctx, big); err != nil || got != nil {
		t.Fatalf("Claim(big) = %+v, %v; want nothing at its limit", got, err)
	}
	if usage, err := s.ModelUsage(ctx); err != nil || usage["big"] != 1 || usage["small"] != 1 || len(usage) != 2 {
		t.Errorf("ModelUsage = %v, %v", usage, err)
	}
	if _, err := s.Finish(ctx, review1.ID, Succeeded, nil, ""); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Claim(ctx, big); err != nil || got == nil || got.ID != review2.ID {
		t.Fatalf("Claim(big) = %+v, %v; want the second review once the first finished", got, err)
	}
}

func TestClaimCheckpointedJob(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	drafted, _, _, _ := s.Enqueue(ctx, NewJob{Kind: "k", GroupKey: "a", DedupeKey: "a", Payload: 1})
	spec := ClaimSpec{Model: "m", Limit: 1}
	if _, err := s.Claim(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if err := s.Checkpoint(ctx, drafted.ID, []byte(`{"draft":1}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Recover(ctx, 3); err != nil {
		t.Fatal(err)
	}
	busy, _, _, _ := s.Enqueue(ctx, NewJob{Kind: "k", GroupKey: "b", DedupeKey: "b", Payload: 2})
	if got, err := s.Claim(ctx, ClaimSpec{Model: "other", Limit: 1}); err != nil || got.ID != drafted.ID {
		t.Fatalf("Claim = %+v, %v; want the drafted job", got, err)
	}

	// The drafted job only has its review left to post: it is claimed as
	// posting, so it is neither superseded nor counted against the model,
	// and keeps the model that drafted it.
	got, _ := s.Get(ctx, drafted.ID)
	if got.Status != Posting || got.Model != "m" || string(got.Checkpoint) != `{"draft":1}` {
		t.Errorf("drafted job = %+v, want posting on m with its checkpoint", got)
	}
	if got, err := s.Claim(ctx, ClaimSpec{Model: "other", Limit: 1}); err != nil || got == nil || got.ID != busy.ID {
		t.Errorf("Claim = %+v, %v; want the other job, the posting one using no slot", got, err)
	}
}

func TestStepsAndPrune(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	j, _, _, _ := s.Enqueue(ctx, newJob("a"))
	for _, st := range []Step{{Role: "user", Content: "u"}, {Role: "assistant", Content: "a", PromptTokens: 7, CompletionTokens: 3}} {
		if err := s.AddStep(ctx, j.ID, st); err != nil {
			t.Fatal(err)
		}
	}
	steps, err := s.Steps(ctx, j.ID)
	if err != nil || len(steps) != 2 || steps[1].Content != "a" || steps[0].Seq != 1 || steps[1].Seq != 2 {
		t.Fatalf("Steps = %+v, %v", steps, err)
	}
	after, err := s.StepsAfter(ctx, j.ID, 1)
	if err != nil || len(after) != 1 || after[0].Seq != 2 {
		t.Errorf("StepsAfter(1) = %+v, %v", after, err)
	}
	if after, err := s.StepsAfter(ctx, j.ID, 2); err != nil || len(after) != 0 {
		t.Errorf("StepsAfter(2) = %+v, %v, want none", after, err)
	}
	p, c, err := s.Usage(ctx, j.ID)
	if err != nil || p != 7 || c != 3 {
		t.Errorf("Usage = %d, %d, %v", p, c, err)
	}
	n, err := s.PruneSteps(ctx, time.Now().Add(time.Hour))
	if err != nil || n != 2 {
		t.Errorf("PruneSteps = %d, %v", n, err)
	}
}

type handlerFunc func(ctx context.Context, job *Job, run *Run) (Result, error)

func (f handlerFunc) Handle(ctx context.Context, job *Job, run *Run) (Result, error) {
	return f(ctx, job, run)
}

func waitStatus(t *testing.T, s *Store, id int64, want Status) Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		j, err := s.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if j.Status == want {
			return j
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %d status = %s, want %s", id, j.Status, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestQueueRunsAndSupersedes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := openTestStore(t)
	log := slog.New(slog.DiscardHandler)
	started := make(chan int64, 4)
	h := handlerFunc(func(ctx context.Context, job *Job, run *Run) (Result, error) {
		var p map[string]string
		_ = json.Unmarshal(job.Payload, &p)
		run.Record(ctx, Step{Role: "user", Content: p["sha"]})
		started <- job.ID
		switch p["sha"] {
		case "slow":
			<-ctx.Done() // runs until superseded
			return Result{}, ctx.Err()
		case "fail":
			return Result{}, errors.New("boom")
		}
		if err := run.Checkpoint(ctx, map[string]string{"draft": p["sha"]}); err != nil {
			return Result{}, err
		}
		return Result{Status: Succeeded, Data: map[string]string{"sha": p["sha"]}}, nil
	})
	q := NewQueue(s, []Model{StaticModel("m", 1, map[string]Handler{"k": h})}, time.Minute, time.Hour, log)
	done := make(chan error)
	go func() { done <- q.Run(ctx) }()

	slow, _, err := q.Enqueue(ctx, newJob("slow"))
	if err != nil {
		t.Fatal(err)
	}
	<-started
	fast, _, err := q.Enqueue(ctx, newJob("fast"))
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, s, slow.ID, Superseded)
	got := waitStatus(t, s, fast.ID, Succeeded)
	if string(got.Result) != `{"sha":"fast"}` {
		t.Errorf("result = %s", got.Result)
	}

	failing, _, err := q.Enqueue(ctx, NewJob{Kind: "k", GroupKey: "other", DedupeKey: "other@1", Payload: map[string]string{"sha": "fail"}})
	if err != nil {
		t.Fatal(err)
	}
	if j := waitStatus(t, s, failing.ID, Failed); j.Error != "boom" {
		t.Errorf("error = %q", j.Error)
	}

	if _, _, err := q.Enqueue(ctx, NewJob{Kind: "unknown", GroupKey: "x", DedupeKey: "x"}); err == nil {
		t.Error("Enqueue accepted a kind no model serves")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestQueueModels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := openTestStore(t)
	log := slog.New(slog.DiscardHandler)

	// Each "k" job blocks until released, and reports where it ran.
	type start struct {
		id    int64
		model string
	}
	started := make(chan start, 8)
	release := make(chan struct{})
	block := handlerFunc(func(ctx context.Context, job *Job, _ *Run) (Result, error) {
		started <- start{job.ID, job.Model}
		select {
		case <-release:
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
		return Result{Status: Succeeded}, nil
	})
	// "slow" is not ready at first: its LLM is down.
	var attempts int
	slow := Model{Name: "slow", Concurrency: 1, Kinds: []string{"k"}, Handlers: func(context.Context) (map[string]Handler, error) {
		if attempts++; attempts == 1 {
			return nil, errors.New("connection refused")
		}
		return map[string]Handler{"k": block}, nil
	}}
	// "remote" has no handlers here: only runners would take its jobs.
	remote := Model{Name: "remote", Concurrency: 1, Kinds: []string{"r"}}
	q := NewQueue(s, []Model{StaticModel("fast", 1, map[string]Handler{"k": block}), slow, remote}, time.Minute, time.Hour, log)
	q.readyBackoff = 10 * time.Millisecond
	done := make(chan error)
	go func() { done <- q.Run(ctx) }()

	var ids []int64
	for i := range 3 {
		j, _, err := q.Enqueue(ctx, NewJob{Kind: "k", GroupKey: fmt.Sprint("g", i), DedupeKey: fmt.Sprint("d", i), Payload: i})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, j.ID)
	}
	// Two jobs start, one per model; the third waits for a free slot.
	seen := map[string]bool{}
	for range 2 {
		st := <-started
		seen[st.model] = true
	}
	if !seen["fast"] || !seen["slow"] {
		t.Errorf("jobs ran on %v, want one on each model", seen)
	}
	select {
	case st := <-started:
		t.Fatalf("job %d started on %s beyond the models' limits", st.id, st.model)
	case <-time.After(100 * time.Millisecond):
	}
	if got := waitStatus(t, s, ids[2], Queued); got.Model != "" {
		t.Errorf("queued job model = %q", got.Model)
	}
	close(release)
	waitStatus(t, s, ids[2], Succeeded)

	// A kind only a runner serves is accepted, and waits.
	r, _, err := q.Enqueue(ctx, NewJob{Kind: "r", GroupKey: "r", DedupeKey: "r", Payload: 1})
	if err != nil {
		t.Fatalf("Enqueue(r) = %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	waitStatus(t, s, r.ID, Queued)

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestClaimSerializesGroups(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	pass := func(n string) NewJob {
		return NewJob{Kind: "k", GroupKey: "pr1:discussion", DedupeKey: "pr1@" + n, KeepRunning: true, Payload: n}
	}
	first, _, _, _ := s.Enqueue(ctx, pass("1"))
	if got, err := s.Claim(ctx, ClaimSpec{}); err != nil || got.ID != first.ID {
		t.Fatalf("Claim = %+v, %v", got, err)
	}
	second, _, _, _ := s.Enqueue(ctx, pass("2"))
	other, _, _, _ := s.Enqueue(ctx, NewJob{Kind: "k", GroupKey: "pr2", DedupeKey: "pr2@1", Payload: 1})

	// The second pass of pr1 waits; another group's job does not.
	if got, err := s.Claim(ctx, ClaimSpec{}); err != nil || got == nil || got.ID != other.ID {
		t.Fatalf("Claim = %+v, %v; want the other group's job", got, err)
	}
	if got, err := s.Claim(ctx, ClaimSpec{}); err != nil || got != nil {
		t.Fatalf("Claim = %+v, %v; want nothing while pr1 runs", got, err)
	}
	if _, err := s.Finish(ctx, first.ID, Succeeded, nil, ""); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Claim(ctx, ClaimSpec{}); err != nil || got == nil || got.ID != second.ID {
		t.Fatalf("Claim = %+v, %v; want the second pass once the first finished", got, err)
	}
}

func TestKeepRunning(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	pass := func(n string) NewJob {
		return NewJob{Kind: "k", GroupKey: "g:discussion", DedupeKey: "g:discussion@" + n, KeepRunning: true, Payload: n}
	}
	running, _, _, _ := s.Enqueue(ctx, pass("1"))
	if _, err := s.Claim(ctx, ClaimSpec{}); err != nil {
		t.Fatal(err)
	}
	queued, _, _, _ := s.Enqueue(ctx, pass("2"))
	_, created, cancel, err := s.Enqueue(ctx, pass("3"))
	if err != nil || !created || len(cancel) != 0 {
		t.Fatalf("enqueue = %v, %v, cancel %v", created, err, cancel)
	}
	if got, _ := s.Get(ctx, running.ID); got.Status != Running {
		t.Errorf("running job = %s, want it kept running", got.Status)
	}
	if got, _ := s.Get(ctx, queued.ID); got.Status != Superseded {
		t.Errorf("queued job = %s, want superseded", got.Status)
	}
}

// seedJobs stores a running review with a transcript, a finished
// discussion pass and a queued review, a minute apart.
func seedJobs(t *testing.T) (*Store, time.Time, [3]int64) {
	t.Helper()
	ctx := context.Background()
	s := openTestStore(t)
	start := time.Date(2026, 10, 11, 8, 0, 0, 0, time.UTC)
	now := start
	s.now = func() time.Time { return now }

	review, _, _, _ := s.Enqueue(ctx, NewJob{Kind: "pr_review", GroupKey: "a", DedupeKey: "a@1", Payload: map[string]string{"repo": "o/a"}})
	now = now.Add(time.Minute)
	discussion, _, _, _ := s.Enqueue(ctx, NewJob{Kind: "pr_discussion", GroupKey: "b", DedupeKey: "b@1", Payload: 1})
	now = now.Add(time.Minute)
	queued, _, _, _ := s.Enqueue(ctx, NewJob{Kind: "pr_review", GroupKey: "c", DedupeKey: "c@1", Payload: 2})

	if got, _ := s.Claim(ctx, ClaimSpec{}); got.ID != review.ID {
		t.Fatalf("claimed %d", got.ID)
	}
	for _, st := range []Step{
		{Role: "user", Content: "u"},
		{Role: "assistant", Content: "a", PromptTokens: 100, CompletionTokens: 10},
		{Role: "assistant", Content: "b", PromptTokens: 300, CompletionTokens: 20},
	} {
		if err := s.AddStep(ctx, review.ID, st); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := s.Claim(ctx, ClaimSpec{}); got.ID != discussion.ID {
		t.Fatalf("claimed %d", got.ID)
	}
	if _, err := s.Finish(ctx, discussion.ID, Skipped, nil, ""); err != nil {
		t.Fatal(err)
	}
	return s, start, [3]int64{review.ID, discussion.ID, queued.ID}
}

func TestList(t *testing.T) {
	ctx := context.Background()
	s, _, ids := seedJobs(t)
	review, discussion, queued := ids[0], ids[1], ids[2]

	all, err := s.List(ctx, ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || all[0].ID != queued || all[2].ID != review {
		t.Fatalf("list = %+v, want newest first", all)
	}
	p := all[2].Progress
	if p.Turns != 2 || p.PeakPromptTokens != 300 || p.PromptTokens != 400 || p.CompletionTokens != 30 || p.LastActivity == nil {
		t.Errorf("progress = %+v", p)
	}
	if all[0].Turns != 0 || all[0].LastActivity != nil {
		t.Errorf("a queued job has no progress: %+v", all[0].Progress)
	}

	for _, tc := range []struct {
		f    ListFilter
		want []int64
	}{
		{ListFilter{Statuses: []Status{Running, Queued}}, []int64{queued, review}},
		{ListFilter{Kind: "pr_discussion"}, []int64{discussion}},
		{ListFilter{Kind: "pr_review", Statuses: []Status{Queued}}, []int64{queued}},
		{ListFilter{Limit: 1}, []int64{queued}},
		{ListFilter{BeforeID: queued}, []int64{discussion, review}},
		{ListFilter{BeforeID: discussion, Limit: 1}, []int64{review}},
		{ListFilter{BeforeID: review}, nil},
		{ListFilter{Statuses: []Status{Failed}}, nil},
	} {
		got, err := s.List(ctx, tc.f)
		if err != nil {
			t.Fatal(err)
		}
		var listed []int64
		for _, j := range got {
			listed = append(listed, j.ID)
		}
		if fmt.Sprint(listed) != fmt.Sprint(tc.want) {
			t.Errorf("List(%+v) = %v, want %v", tc.f, listed, tc.want)
		}
	}
}

func TestSummary(t *testing.T) {
	ctx := context.Background()
	s, _, ids := seedJobs(t)
	got, err := s.Summary(ctx, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != ids[0] || got.Status != Running || got.Turns != 2 || got.PeakPromptTokens != 300 {
		t.Errorf("Summary = %+v", got)
	}
	if got, err := s.Summary(ctx, ids[2]); err != nil || got.Turns != 0 || got.LastActivity != nil {
		t.Errorf("Summary of a queued job = %+v, %v", got, err)
	}
	if _, err := s.Summary(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("Summary(999) error = %v, want ErrNotFound", err)
	}
}

func TestActive(t *testing.T) {
	s, start, _ := seedJobs(t)
	active, err := s.Active(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byStatus := map[Status]ActiveStat{}
	for _, a := range active {
		byStatus[a.Status] = a
	}
	if q := byStatus[Queued]; q.Count != 1 || !q.Oldest.Equal(start.Add(2*time.Minute)) {
		t.Errorf("queued = %+v", q)
	}
	// A running job is dated from its start, not from when it was queued.
	if r := byStatus[Running]; r.Count != 1 || !r.Oldest.Equal(start.Add(2*time.Minute)) {
		t.Errorf("running = %+v", r)
	}
	if len(active) != 2 {
		t.Errorf("active = %+v, want only queued and running", active)
	}
}

func TestParseStatus(t *testing.T) {
	if _, err := ParseStatus("running"); err != nil {
		t.Error(err)
	}
	if _, err := ParseStatus("done"); err == nil {
		t.Error("ParseStatus accepted an unknown status")
	}
}

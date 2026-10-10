package jobs

import (
	"context"
	"encoding/json"
	"errors"
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
	claimed, err := s.Claim(ctx)
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
	if _, err := s.Claim(ctx); err != nil {
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
	if _, err := s.Claim(ctx); err != nil {
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
	if _, err := s.Claim(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Checkpoint(ctx, posting.ID, []byte(`{"draft":1}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(ctx); err != nil {
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
			if _, err := s.Claim(ctx); err != nil {
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
	if err != nil || len(steps) != 2 || steps[1].Content != "a" {
		t.Fatalf("Steps = %+v, %v", steps, err)
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
	q := NewQueue(s, 1, time.Minute, time.Hour, log)

	started := make(chan int64, 4)
	q.Register("k", handlerFunc(func(ctx context.Context, job *Job, run *Run) (Result, error) {
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
	}))
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
		t.Error("Enqueue accepted a kind without handler")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

package runner

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/mycroft/agent-nexus/internal/jobs"
)

type handlerFunc func(ctx context.Context, job *jobs.Job, run *jobs.Run) (jobs.Result, error)

func (f handlerFunc) Handle(ctx context.Context, job *jobs.Job, run *jobs.Run) (jobs.Result, error) {
	return f(ctx, job, run)
}

// startWorker runs a worker serving model gpu with h, until the test ends
// or the returned function stops it.
func startWorker(t *testing.T, c *controller, h jobs.Handler, unreachable time.Duration) func() {
	t.Helper()
	client, err := NewClient(c.srv.URL, token, "box-1", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	w := &Worker{
		Client: client, Logger: slog.New(slog.DiscardHandler), unreachableAfter: unreachable, releaseTimeout: 100 * time.Millisecond,
		Models: []jobs.Model{jobs.StaticModel("gpu", 1, map[string]jobs.Handler{"k": h})},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()
	stop := func() {
		cancel()
		<-done
	}
	t.Cleanup(stop)
	return stop
}

func waitStatus(t *testing.T, s *jobs.Store, id int64, want jobs.Status) jobs.Job {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
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

func TestWorkerRunsJobs(t *testing.T) {
	c := newController(t)
	ctx := context.Background()
	startWorker(t, c, handlerFunc(func(ctx context.Context, job *jobs.Job, run *jobs.Run) (jobs.Result, error) {
		run.Record(ctx, jobs.Step{Role: "assistant", Content: "looked at " + string(job.Payload), PromptTokens: 5})
		if err := run.Checkpoint(ctx, map[string]int{"draft": 1}); err != nil {
			return jobs.Result{}, err
		}
		return jobs.Result{Status: jobs.Succeeded, Data: map[string]string{"outcome": "posted"}}, nil
	}), 0)

	j := c.enqueue(t, "pr1", "pr1@a")
	got := waitStatus(t, c.store, j.ID, jobs.Succeeded)
	if got.Runner != "homelab/box-1" || got.Model != "gpu" || string(got.Result) != `{"outcome":"posted"}` ||
		string(got.Checkpoint) != `{"draft":1}` || got.Lease != "" {
		t.Errorf("job = %+v", got)
	}
	steps, _ := c.store.Steps(ctx, j.ID)
	if len(steps) != 1 || steps[0].Content != `looked at {"group":"pr1"}` {
		t.Errorf("steps = %+v", steps)
	}

	// A failing job is reported as such.
	if _, _, err := c.queue.Enqueue(ctx, jobs.NewJob{Kind: "other", GroupKey: "x", DedupeKey: "x", Payload: 1}); err != nil {
		t.Fatal(err)
	}
	j = c.enqueue(t, "pr2", "pr2@a")
	waitStatus(t, c.store, j.ID, jobs.Succeeded)
}

func TestWorkerHandlerFailure(t *testing.T) {
	c := newController(t)
	startWorker(t, c, handlerFunc(func(context.Context, *jobs.Job, *jobs.Run) (jobs.Result, error) {
		return jobs.Result{}, errors.New("forge down")
	}), 0)
	j := c.enqueue(t, "pr1", "pr1@a")
	if got := waitStatus(t, c.store, j.ID, jobs.Failed); got.Error != "forge down" {
		t.Errorf("error = %q", got.Error)
	}
}

// blocking is a handler that runs until its context ends, and reports
// when it starts.
func blocking(started chan<- int64) jobs.Handler {
	return handlerFunc(func(ctx context.Context, job *jobs.Job, _ *jobs.Run) (jobs.Result, error) {
		started <- job.ID
		<-ctx.Done()
		return jobs.Result{}, ctx.Err()
	})
}

func TestWorkerReleasesAtShutdown(t *testing.T) {
	c := newController(t)
	started := make(chan int64, 1)
	stop := startWorker(t, c, blocking(started), 0)
	j := c.enqueue(t, "pr1", "pr1@a")
	<-started
	stop()
	if got := waitStatus(t, c.store, j.ID, jobs.Queued); got.Lease != "" || got.Attempts != 1 {
		t.Errorf("job after the runner's shutdown = %+v, want queued at once", got)
	}
}

func TestWorkerStopsSupersededJob(t *testing.T) {
	c := newController(t)
	started := make(chan int64, 2)
	startWorker(t, c, blocking(started), 0)
	old := c.enqueue(t, "pr1", "pr1@a")
	<-started
	newer := c.enqueue(t, "pr1", "pr1@b")

	// The next heartbeat stops the old job, whose slot then goes to the
	// newer commit.
	waitStatus(t, c.store, old.ID, jobs.Superseded)
	select {
	case id := <-started:
		if id != newer.ID {
			t.Errorf("started job %d, want %d", id, newer.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the newer job did not start: the old one was not stopped")
	}
	if got, _ := c.store.Get(context.Background(), old.ID); got.Lease != "" {
		t.Errorf("superseded job still held: %+v", got)
	}
}

func TestWorkerStopsWithoutController(t *testing.T) {
	c := newController(t)
	started := make(chan int64, 1)
	stopped := make(chan error, 1)
	startWorker(t, c, handlerFunc(func(ctx context.Context, job *jobs.Job, _ *jobs.Run) (jobs.Result, error) {
		started <- job.ID
		<-ctx.Done()
		stopped <- context.Cause(ctx)
		return jobs.Result{}, ctx.Err()
	}), time.Millisecond)
	c.enqueue(t, "pr1", "pr1@a")
	<-started
	c.srv.CloseClientConnections()
	c.srv.Close()
	select {
	case cause := <-stopped:
		if !errors.Is(cause, errUnreachable) {
			t.Errorf("cause = %v, want errUnreachable", cause)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the job kept running without its controller")
	}
}

func TestClientEnqueue(t *testing.T) {
	c := newController(t)
	client, err := NewClient(c.srv.URL+"/", token, "box-1", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	nj := jobs.NewJob{Kind: "k", GroupKey: "pr1", DedupeKey: "pr1@forced", Payload: map[string]any{"pr": 1, "force": true}}
	job, created, err := client.Enqueue(context.Background(), nj)
	if err != nil || !created || job.Status != jobs.Queued {
		t.Fatalf("Enqueue = %+v, %v, %v", job, created, err)
	}
	var payload map[string]any
	if json.Unmarshal(job.Payload, &payload) != nil || payload["force"] != true {
		t.Errorf("payload = %s", job.Payload)
	}
	if again, created, err := client.Enqueue(context.Background(), nj); err != nil || created || again.ID != job.ID {
		t.Errorf("Enqueue again = %+v, %v, %v", again, created, err)
	}
	if _, _, err := client.Enqueue(context.Background(), jobs.NewJob{Kind: "nope", GroupKey: "g", DedupeKey: "d", Payload: 1}); err == nil || !strings.Contains(err.Error(), "400") {
		t.Errorf("Enqueue of an unknown kind = %v", err)
	}
	if _, err := NewClient("controller:8080", token, "box", "1"); err == nil {
		t.Error("NewClient accepted a relative URL")
	}
	if _, err := NewClient(c.srv.URL, token, "a box", "1"); err == nil {
		t.Error("NewClient accepted a bad runner name")
	}
}

package review

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mycroft/agent-nexus/internal/auth"
	"github.com/mycroft/agent-nexus/internal/jobs"
	"github.com/mycroft/agent-nexus/internal/runner"
)

// A review run by a runner: the controller only holds the queue, the
// runner drafts and posts, and the transcript and checkpoint land in the
// controller's store.
func TestReviewThroughRunner(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newFixture(t)
	f.script(sampleOutput)
	store, err := jobs.OpenStore(ctx, filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	queue := jobs.NewQueue(store, []jobs.Model{{Name: "gpu", Concurrency: 1, Kinds: []string{Kind}}}, 30*time.Minute, time.Hour, quiet)
	token := strings.Repeat("t", 32)
	tokens, err := auth.ParseKeys("homelab="+token, 0)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer((&runner.Server{Queue: queue, Tokens: tokens, Timeout: 30 * time.Minute, Version: "test", Logger: quiet}).Handler())
	defer srv.Close()

	client, err := runner.NewClient(srv.URL, token, "box-1", "test")
	if err != nil {
		t.Fatal(err)
	}
	worker := &runner.Worker{Client: client, Models: []jobs.Model{jobs.StaticModel("gpu", 1, map[string]jobs.Handler{Kind: f.reviewer})}, Logger: quiet}
	done := make(chan struct{})
	go func() {
		defer close(done)
		worker.Run(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()

	job, _, err := queue.Enqueue(ctx, jobs.NewJob{Kind: Kind, GroupKey: "g", DedupeKey: "g@1", Payload: f.req})
	if err != nil {
		t.Fatal(err)
	}
	got := waitFor(t, store, job.ID, jobs.Succeeded)
	if got.Runner != "homelab/box-1" || got.Model != "gpu" || len(got.Checkpoint) == 0 {
		t.Errorf("job = %+v, want run by the runner, with its draft checkpointed", got)
	}
	if created := f.forge.Created(); len(created) != 1 || !strings.Contains(created[0].Body, "Mod panics when b is 0") {
		t.Errorf("created reviews = %+v", created)
	}
	if steps, _ := store.Steps(ctx, job.ID); len(steps) < 5 {
		t.Errorf("transcript has %d steps at the controller", len(steps))
	}
}

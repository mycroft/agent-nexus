package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/mycroft/agent-nexus/internal/metrics"
)

const (
	// maxAttempts bounds how often a job is retried after the process
	// died while running it.
	maxAttempts  = 3
	pollInterval = 30 * time.Second
	pruneEvery   = 24 * time.Hour
	// maxReadyBackoff caps the wait between two attempts to set up a
	// model whose LLM is unreachable.
	maxReadyBackoff = 5 * time.Minute
)

// Result is a handler's outcome for a job that did not fail.
type Result struct {
	// Status is Succeeded, Skipped or Superseded.
	Status Status
	// Data is stored as the job's JSON result.
	Data any
}

// Handler runs jobs of one kind.
type Handler interface {
	Handle(ctx context.Context, job *Job, run *Run) (Result, error)
}

// Run gives a handler access to its job's persistence.
type Run struct {
	store *Store
	id    int64
	log   *slog.Logger
}

// Record appends a transcript step. Failures are logged, not returned: a
// missing transcript line must not fail the job.
func (r *Run) Record(ctx context.Context, st Step) {
	if err := r.store.AddStep(ctx, r.id, st); err != nil {
		r.log.WarnContext(ctx, "record step", "job", r.id, "error", err)
	}
}

// Checkpoint stores data and moves the job to posting. It returns
// ErrNotRunning if the job was superseded meanwhile.
func (r *Run) Checkpoint(ctx context.Context, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal checkpoint: %w", err)
	}
	return r.store.Checkpoint(ctx, r.id, raw)
}

// Model is an LLM jobs run on.
type Model struct {
	Name string
	// Concurrency bounds the model's jobs running at once.
	Concurrency int
	// Kinds are the job kinds the model serves.
	Kinds []string
	// Handlers, if set, runs the model's jobs in this process. It returns
	// each kind's handler once the model can be used, and is retried until
	// then. Without it, the model's jobs wait for a runner.
	Handlers func(context.Context) (map[string]Handler, error)
}

// StaticModel returns a model running handlers in this process, from the
// start.
func StaticModel(name string, concurrency int, handlers map[string]Handler) Model {
	return Model{
		Name: name, Concurrency: concurrency, Kinds: slices.Sorted(maps.Keys(handlers)),
		Handlers: func(context.Context) (map[string]Handler, error) { return handlers, nil },
	}
}

// Queue runs stored jobs on a pool of workers per model.
type Queue struct {
	store     *Store
	models    []Model
	kinds     map[string]bool
	timeout   time.Duration
	retention time.Duration
	log       *slog.Logger
	// readyBackoff is the first wait before retrying a model's Handlers.
	readyBackoff time.Duration

	mu      sync.Mutex
	running map[int64]context.CancelFunc
	// changed is closed, and replaced, when a job may have become
	// claimable.
	changed chan struct{}
}

// NewQueue returns a queue running the jobs of models. timeout bounds one
// job; retention is how long transcripts are kept.
func NewQueue(store *Store, models []Model, timeout, retention time.Duration, log *slog.Logger) *Queue {
	q := &Queue{
		store:        store,
		models:       models,
		kinds:        map[string]bool{},
		timeout:      timeout,
		retention:    retention,
		log:          log,
		readyBackoff: 5 * time.Second,
		running:      map[int64]context.CancelFunc{},
		changed:      make(chan struct{}),
	}
	for _, m := range models {
		for _, k := range m.Kinds {
			q.kinds[k] = true
		}
	}
	return q
}

// Store returns the underlying store.
func (q *Queue) Store() *Store { return q.store }

// Enqueue stores a job (see Store.Enqueue), cancels the running jobs it
// supersedes and wakes the workers.
func (q *Queue) Enqueue(ctx context.Context, nj NewJob) (Job, bool, error) {
	if !q.kinds[nj.Kind] {
		return Job{}, false, fmt.Errorf("no model serves job kind %q", nj.Kind)
	}
	job, created, cancelIDs, err := q.store.Enqueue(ctx, nj)
	if err != nil {
		return Job{}, false, err
	}
	q.mu.Lock()
	for _, id := range cancelIDs {
		if cancel, ok := q.running[id]; ok {
			q.log.InfoContext(ctx, "superseding running job", "job", id, "by", job.ID)
			cancel()
		}
	}
	q.mu.Unlock()
	if created {
		q.notify()
	}
	return job, created, nil
}

// notify wakes every waiting worker: with per-model workers, the one
// woken must be one that can take the job.
func (q *Queue) notify() {
	q.mu.Lock()
	close(q.changed)
	q.changed = make(chan struct{})
	q.mu.Unlock()
}

// Changed returns a channel closed the next time a job may become
// claimable. Take it before claiming, so a change in between is not
// missed.
func (q *Queue) Changed() <-chan struct{} {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.changed
}

// Run recovers interrupted jobs, then processes jobs until ctx is done.
// Jobs interrupted by shutdown stay running in the store and are requeued
// by the next Run.
func (q *Queue) Run(ctx context.Context) error {
	requeued, failed, err := q.store.Recover(ctx, maxAttempts)
	if err != nil {
		return err
	}
	if requeued > 0 || failed > 0 {
		q.log.InfoContext(ctx, "recovered interrupted jobs", "requeued", requeued, "failed", failed)
	}

	var wg sync.WaitGroup
	for _, m := range q.models {
		if m.Handlers != nil {
			wg.Go(func() { q.serve(ctx, m) })
		}
	}
	wg.Go(func() { q.prune(ctx) })
	wg.Wait()
	return nil
}

// serve runs the model's workers once its handlers are ready.
func (q *Queue) serve(ctx context.Context, m Model) {
	handlers := q.ready(ctx, m)
	if handlers == nil {
		return
	}
	spec := ClaimSpec{Model: m.Name, Limit: m.Concurrency}
	for _, k := range m.Kinds {
		if handlers[k] != nil {
			spec.Kinds = append(spec.Kinds, k)
		}
	}
	if len(spec.Kinds) == 0 {
		q.log.WarnContext(ctx, "model has no handler for the kinds it serves", "model", m.Name, "kinds", m.Kinds)
		return
	}
	var wg sync.WaitGroup
	for range m.Concurrency {
		wg.Go(func() { q.work(ctx, spec, handlers) })
	}
	wg.Wait()
}

// ready calls m.Handlers until it succeeds, backing off between attempts.
// It returns nil once ctx is done.
func (q *Queue) ready(ctx context.Context, m Model) map[string]Handler {
	wait := q.readyBackoff
	for {
		handlers, err := m.Handlers(ctx)
		if err == nil {
			return handlers
		}
		if ctx.Err() != nil {
			return nil
		}
		q.log.WarnContext(ctx, "model not ready; its jobs wait", "model", m.Name, "retry_in", wait, "error", err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
		wait = min(2*wait, maxReadyBackoff)
	}
}

func (q *Queue) work(ctx context.Context, spec ClaimSpec, handlers map[string]Handler) {
	for {
		changed := q.Changed()
		job, err := q.store.Claim(ctx, spec)
		if err != nil && ctx.Err() == nil {
			q.log.ErrorContext(ctx, "claim job", "model", spec.Model, "error", err)
		}
		if job != nil {
			q.process(ctx, job, handlers[job.Kind])
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
		case <-time.After(pollInterval):
		}
	}
}

func (q *Queue) process(ctx context.Context, job *Job, h Handler) {
	log := q.log.With("job", job.ID, "kind", job.Kind, "model", job.Model, "attempt", job.Attempts)
	jobCtx, cancel := context.WithTimeout(ctx, q.timeout)
	defer cancel()
	q.mu.Lock()
	q.running[job.ID] = cancel
	q.mu.Unlock()
	defer func() {
		q.mu.Lock()
		delete(q.running, job.ID)
		q.mu.Unlock()
	}()

	start := time.Now()
	log.InfoContext(ctx, "job started")
	// Jobs of the same group, and of the model, wait for this one: wake
	// the workers when it ends.
	defer q.notify()
	result, err := q.handle(jobCtx, h, job)
	if ctx.Err() != nil {
		log.WarnContext(ctx, "job interrupted by shutdown; it will be requeued on restart")
		return
	}

	// The job context may be cancelled (superseded or timed out); the
	// final state must still be written.
	writeCtx := context.WithoutCancel(ctx)
	status, data, errMsg := result.Status, result.Data, ""
	if err != nil {
		status, data, errMsg = Failed, nil, err.Error()
	}
	var raw []byte
	if data != nil {
		if raw, err = json.Marshal(data); err != nil {
			status, raw, errMsg = Failed, nil, fmt.Sprintf("marshal result: %v", err)
		}
	}
	updated, err := q.store.Finish(writeCtx, job.ID, status, raw, errMsg)
	if err != nil {
		log.ErrorContext(ctx, "finish job", "error", err)
		return
	}
	if !updated {
		status = Superseded
	}
	metrics.JobsTotal.WithLabelValues(job.Kind, string(status)).Inc()
	metrics.JobDuration.WithLabelValues(job.Kind).Observe(time.Since(start).Seconds())
	if status == Failed {
		log.ErrorContext(ctx, "job failed", "error", errMsg, "duration", time.Since(start))
		return
	}
	log.InfoContext(ctx, "job finished", "status", status, "duration", time.Since(start))
}

func (q *Queue) handle(ctx context.Context, h Handler, job *Job) (res Result, err error) { //nolint:nonamedreturns // needed to convert a handler panic into an error
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("handler panic: %v", p)
		}
	}()
	if h == nil {
		return Result{}, fmt.Errorf("no handler for job kind %q", job.Kind)
	}
	res, err = h.Handle(ctx, job, &Run{store: q.store, id: job.ID, log: q.log})
	if err == nil && res.Status != Succeeded && res.Status != Skipped && res.Status != Superseded {
		err = fmt.Errorf("handler returned invalid status %q", res.Status)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		err = fmt.Errorf("job timed out after %s: %w", q.timeout, err)
	}
	return res, err
}

func (q *Queue) prune(ctx context.Context) {
	ticker := time.NewTicker(pruneEvery)
	defer ticker.Stop()
	for {
		n, err := q.store.PruneSteps(ctx, time.Now().Add(-q.retention))
		switch {
		case err != nil && ctx.Err() == nil:
			q.log.ErrorContext(ctx, "prune transcripts", "error", err)
		case n > 0:
			q.log.InfoContext(ctx, "pruned transcripts", "steps", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

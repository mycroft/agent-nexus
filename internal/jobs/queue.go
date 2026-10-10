package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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

// Queue runs stored jobs on a pool of workers.
type Queue struct {
	store     *Store
	handlers  map[string]Handler
	workers   int
	timeout   time.Duration
	retention time.Duration
	log       *slog.Logger
	wake      chan struct{}

	mu      sync.Mutex
	running map[int64]context.CancelFunc
}

// NewQueue returns a queue with the given number of workers. timeout bounds
// one job; retention is how long transcripts are kept.
func NewQueue(store *Store, workers int, timeout, retention time.Duration, log *slog.Logger) *Queue {
	return &Queue{
		store:     store,
		handlers:  map[string]Handler{},
		workers:   workers,
		timeout:   timeout,
		retention: retention,
		log:       log,
		wake:      make(chan struct{}, 1),
		running:   map[int64]context.CancelFunc{},
	}
}

// Register sets the handler for a job kind. Call before Run.
func (q *Queue) Register(kind string, h Handler) { q.handlers[kind] = h }

// Store returns the underlying store.
func (q *Queue) Store() *Store { return q.store }

// Enqueue stores a job (see Store.Enqueue), cancels the running jobs it
// supersedes and wakes a worker.
func (q *Queue) Enqueue(ctx context.Context, nj NewJob) (Job, bool, error) {
	if _, ok := q.handlers[nj.Kind]; !ok {
		return Job{}, false, fmt.Errorf("no handler for job kind %q", nj.Kind)
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
		select {
		case q.wake <- struct{}{}:
		default:
		}
	}
	return job, created, nil
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
	for range q.workers {
		wg.Go(func() { q.work(ctx) })
	}
	wg.Go(func() { q.prune(ctx) })
	wg.Wait()
	return nil
}

func (q *Queue) work(ctx context.Context) {
	for {
		job, err := q.store.Claim(ctx)
		if err != nil && ctx.Err() == nil {
			q.log.ErrorContext(ctx, "claim job", "error", err)
		}
		if job != nil {
			q.process(ctx, job)
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-q.wake:
		case <-time.After(pollInterval):
		}
	}
}

func (q *Queue) process(ctx context.Context, job *Job) {
	log := q.log.With("job", job.ID, "kind", job.Kind, "attempt", job.Attempts)
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
	result, err := q.handle(jobCtx, job)
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

func (q *Queue) handle(ctx context.Context, job *Job) (res Result, err error) { //nolint:nonamedreturns // needed to convert a handler panic into an error
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("handler panic: %v", p)
		}
	}()
	h, ok := q.handlers[job.Kind]
	if !ok {
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

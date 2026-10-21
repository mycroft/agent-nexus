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
	// reapEvery is how often the leases of runners are checked.
	reapEvery = 15 * time.Second
)

// LeaseTTL is how long a runner's lease lasts unless renewed. The
// controller also gives runners that long to renew their leases after it
// restarts.
const LeaseTTL = 2 * time.Minute

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

// Writer persists what a running job records, under its lease: the store
// itself, or the controller as seen from a runner.
type Writer interface {
	AddStep(ctx context.Context, id int64, lease string, st Step) error
	Checkpoint(ctx context.Context, id int64, lease string, data []byte) error
}

// Run gives a handler access to its job's persistence.
type Run struct {
	w     Writer
	id    int64
	lease string
	log   *slog.Logger
}

// NewRun returns the run of job, claimed with its lease, writing to w.
func NewRun(w Writer, job *Job, log *slog.Logger) *Run {
	return &Run{w: w, id: job.ID, lease: job.Lease, log: log}
}

// Record appends a transcript step. Failures are logged, not returned: a
// missing transcript line must not fail the job.
func (r *Run) Record(ctx context.Context, st Step) {
	if err := r.w.AddStep(ctx, r.id, r.lease, st); err != nil {
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
	return r.w.Checkpoint(ctx, r.id, r.lease, raw)
}

// Execute runs job with h, turning a panic into an error. timeout, the
// bound of ctx, words the error of a job that ran out of time.
func Execute(ctx context.Context, h Handler, job *Job, run *Run, timeout time.Duration) (res Result, err error) { //nolint:nonamedreturns // needed to convert a handler panic into an error
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("handler panic: %v", p)
		}
	}()
	if h == nil {
		return Result{}, fmt.Errorf("no handler for job kind %q", job.Kind)
	}
	res, err = h.Handle(ctx, job, run)
	if err == nil && res.Status != Succeeded && res.Status != Skipped && res.Status != Superseded {
		err = fmt.Errorf("handler returned invalid status %q", res.Status)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		err = fmt.Errorf("job timed out after %s: %w", timeout, err)
	}
	return res, err
}

// Outcome returns what to store of a handler's return: the final status,
// the JSON result and the error message.
func Outcome(res Result, err error) (Status, []byte, string) {
	if err != nil {
		return Failed, nil, err.Error()
	}
	if res.Data == nil {
		return res.Status, nil, ""
	}
	raw, err := json.Marshal(res.Data)
	if err != nil {
		return Failed, nil, fmt.Sprintf("marshal result: %v", err)
	}
	return res.Status, raw, ""
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
		return Job{}, false, fmt.Errorf("%w: %q", ErrUnknownKind, nj.Kind)
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

// ErrUnknownKind is returned when enqueueing a job kind no model serves.
var ErrUnknownKind = errors.New("no model serves this job kind")

// ErrUnknownModel is returned when a runner claims for a model the
// controller does not know.
var ErrUnknownModel = errors.New("unknown model")

// ErrTimedOut is returned when a runner renews the lease of a job that
// outlived the job timeout: it no longer may run.
var ErrTimedOut = errors.New("job timed out")

// ClaimFor claims a job of model for runner, which can run kinds. The
// controller's configuration bounds the claim: the model's concurrency,
// and the kinds it serves. The lease lasts LeaseTTL unless renewed.
func (q *Queue) ClaimFor(ctx context.Context, runner, model string, kinds []string) (*Job, error) {
	i := slices.IndexFunc(q.models, func(m Model) bool { return m.Name == model })
	if i < 0 {
		return nil, fmt.Errorf("%w: %q", ErrUnknownModel, model)
	}
	m := q.models[i]
	spec := ClaimSpec{Model: m.Name, Limit: m.Concurrency, Runner: runner, LeaseTTL: LeaseTTL}
	for _, k := range m.Kinds {
		if slices.Contains(kinds, k) {
			spec.Kinds = append(spec.Kinds, k)
		}
	}
	if len(spec.Kinds) == 0 {
		return nil, nil //nolint:nilnil // nothing the runner can run is not an error
	}
	return q.store.Claim(ctx, spec)
}

// Heartbeat renews a runner's lease on a job. It returns ErrNotRunning
// when the job was superseded, and ErrTimedOut once it outlived the job
// timeout, without renewing it then: either way, the runner must stop.
func (q *Queue) Heartbeat(ctx context.Context, id int64, lease string) error {
	job, err := q.store.Get(ctx, id)
	if err != nil {
		return err
	}
	if job.Lease != lease {
		return ErrLeaseLost
	}
	now := q.store.now()
	if job.StartedAt != nil && now.Sub(*job.StartedAt) > q.timeout+LeaseTTL {
		return ErrTimedOut
	}
	status, err := q.store.Renew(ctx, id, lease, now.Add(LeaseTTL))
	if err != nil {
		return err
	}
	if status != Running && status != Posting {
		return ErrNotRunning
	}
	return nil
}

// Release gives back a job a runner stops holding (see Store.Release),
// and wakes the workers.
func (q *Queue) Release(ctx context.Context, id int64, lease string) error {
	if err := q.store.Release(ctx, id, lease, maxAttempts); err != nil {
		return err
	}
	q.notify()
	return nil
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
	requeued, failed, err := q.store.Recover(ctx, maxAttempts, LeaseTTL)
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
	wg.Go(func() { q.reap(ctx) })
	wg.Wait()
	return nil
}

// reap takes back the jobs of runners that stopped renewing their lease.
func (q *Queue) reap(ctx context.Context) {
	ticker := time.NewTicker(reapEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		requeued, failed, err := q.store.Reap(ctx, maxAttempts)
		switch {
		case err != nil && ctx.Err() == nil:
			q.log.ErrorContext(ctx, "reap runner leases", "error", err)
		case requeued > 0 || failed > 0:
			q.log.WarnContext(ctx, "took back jobs from runners that stopped answering", "requeued", requeued, "failed", failed)
			q.notify()
		}
	}
}

// serve runs the model's workers once its handlers are ready.
func (q *Queue) serve(ctx context.Context, m Model) {
	handlers := WaitReady(ctx, m, q.readyBackoff, q.log)
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

// WaitReady calls m.Handlers until it succeeds, backing off from wait
// between attempts. It returns nil once ctx is done.
func WaitReady(ctx context.Context, m Model, wait time.Duration, log *slog.Logger) map[string]Handler {
	for {
		handlers, err := m.Handlers(ctx)
		if err == nil {
			return handlers
		}
		if ctx.Err() != nil {
			return nil
		}
		log.WarnContext(ctx, "model not ready; its jobs wait", "model", m.Name, "retry_in", wait, "error", err)
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

	log.InfoContext(ctx, "job started")
	result, err := Execute(jobCtx, h, job, NewRun(q.store, job, q.log), q.timeout)
	if ctx.Err() != nil {
		// Jobs of the same group, and of the model, wait for it: on
		// shutdown, they wait for the restart.
		log.WarnContext(ctx, "job interrupted by shutdown; it will be requeued on restart")
		return
	}
	status, raw, errMsg := Outcome(result, err)
	// The job context may be cancelled (superseded or timed out); the
	// final state must still be written.
	if _, err := q.Finish(context.WithoutCancel(ctx), job.ID, job.Lease, status, raw, errMsg); err != nil {
		log.ErrorContext(ctx, "finish job", "error", err)
	}
}

// Finish records the final state of a job held by lease (see
// Store.Finish), counts it, and wakes the workers: jobs of its group and
// of its model may wait for it. It returns the status the job ended with,
// Superseded if a newer job replaced it meanwhile.
func (q *Queue) Finish(ctx context.Context, id int64, lease string, status Status, result []byte, errMsg string) (Status, error) {
	job, err := q.store.Get(ctx, id)
	if err != nil {
		return "", err
	}
	updated, err := q.store.Finish(ctx, id, lease, status, result, errMsg)
	if err != nil {
		return "", err
	}
	defer q.notify()
	if !updated {
		status = Superseded
	}
	log := q.log.With("job", id, "kind", job.Kind, "model", job.Model, "runner", job.Runner, "attempt", job.Attempts)
	var duration time.Duration
	if job.StartedAt != nil {
		duration = q.store.now().Sub(*job.StartedAt)
	}
	metrics.JobsTotal.WithLabelValues(job.Kind, string(status), job.Model).Inc()
	metrics.JobDuration.WithLabelValues(job.Kind, job.Model).Observe(duration.Seconds())
	if status == Failed {
		log.ErrorContext(ctx, "job failed", "error", errMsg, "duration", duration)
	} else {
		log.InfoContext(ctx, "job finished", "status", status, "duration", duration)
	}
	return status, nil
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

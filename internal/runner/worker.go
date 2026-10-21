package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/mycroft/agent-nexus/internal/jobs"
)

const (
	// unreachableAfter is how long a runner keeps a job without renewing
	// its lease before it stops on its own: well before the controller
	// takes the job back (jobs.LeaseTTL), so two runners never run it.
	unreachableAfter = 90 * time.Second
	// releaseTimeout bounds giving back a job at shutdown.
	releaseTimeout = 10 * time.Second
	// maxClaimBackoff caps the wait between claims the controller failed.
	maxClaimBackoff = time.Minute
)

// errUnreachable stops a job whose lease could not be renewed for
// unreachableAfter.
var errUnreachable = errors.New("the controller did not renew the job's lease")

// Worker runs, for each of its models, Concurrency workers taking the
// model's jobs from the controller.
type Worker struct {
	Client *Client
	// Models are the models this runner serves, with their handlers.
	Models []jobs.Model
	Logger *slog.Logger

	// Test hooks: zero values take the defaults.
	readyBackoff     time.Duration
	unreachableAfter time.Duration
	releaseTimeout   time.Duration
}

// Run serves the models until ctx is done. Jobs running then are given
// back to the controller, which queues them again.
func (w *Worker) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, m := range w.Models {
		if m.Handlers != nil {
			wg.Go(func() { w.serve(ctx, m) })
		}
	}
	wg.Wait()
}

func (w *Worker) serve(ctx context.Context, m jobs.Model) {
	backoff := w.readyBackoff
	if backoff == 0 {
		backoff = 5 * time.Second
	}
	handlers := jobs.WaitReady(ctx, m, backoff, w.Logger)
	if handlers == nil {
		return
	}
	var kinds []string
	for _, k := range m.Kinds {
		if handlers[k] != nil {
			kinds = append(kinds, k)
		}
	}
	if len(kinds) == 0 {
		w.Logger.WarnContext(ctx, "model has no handler for the kinds it serves", "model", m.Name, "kinds", m.Kinds)
		return
	}
	w.Logger.InfoContext(ctx, "taking jobs from the controller", "model", m.Name, "workers", m.Concurrency, "kinds", kinds)
	var wg sync.WaitGroup
	for range m.Concurrency {
		wg.Go(func() { w.work(ctx, m.Name, kinds, handlers) })
	}
	wg.Wait()
}

// work claims and runs jobs of model, one at a time.
func (w *Worker) work(ctx context.Context, model string, kinds []string, handlers map[string]jobs.Handler) {
	wait := time.Second
	for ctx.Err() == nil {
		l, err := w.Client.Claim(ctx, model, kinds)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			w.Logger.WarnContext(ctx, "claim a job from the controller", "model", model, "retry_in", wait, "error", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			wait = min(2*wait, maxClaimBackoff)
			continue
		}
		wait = time.Second
		if l != nil {
			w.run(ctx, l, handlers)
		}
	}
}

// run runs one claimed job, renewing its lease, and reports its end.
func (w *Worker) run(ctx context.Context, l *Lease, handlers map[string]jobs.Handler) {
	job := l.Job
	job.Checkpoint, job.Lease = l.Checkpoint, l.Lease
	log := w.Logger.With("job", job.ID, "kind", job.Kind, "model", job.Model, "attempt", job.Attempts)
	timeout := time.Duration(l.TimeoutSeconds) * time.Second
	jobCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	jobCtx, stop := context.WithTimeout(jobCtx, timeout)
	defer stop()

	beats := make(chan struct{})
	go func() {
		defer close(beats)
		w.heartbeat(jobCtx, log, &job, time.Duration(l.HeartbeatSeconds)*time.Second, cancel)
	}()
	log.InfoContext(ctx, "job started")
	res, err := jobs.Execute(jobCtx, handlers[job.Kind], &job, jobs.NewRun(w.Client, &job, w.Logger), timeout)
	stop()
	<-beats

	// Without a controller, or at shutdown, the job goes back to the
	// queue: another attempt, here or on another runner, may finish it.
	cause := context.Cause(jobCtx)
	if ctx.Err() != nil || errors.Is(cause, errUnreachable) {
		log.WarnContext(ctx, "giving the job back to the controller", "cause", cause)
		limit := w.releaseTimeout
		if limit == 0 {
			limit = releaseTimeout
		}
		releaseCtx, cancelRelease := context.WithTimeout(context.WithoutCancel(ctx), limit)
		defer cancelRelease()
		if err := w.Client.Release(releaseCtx, job.ID, job.Lease); err != nil {
			log.WarnContext(ctx, "release job; the controller takes it back once its lease expires", "error", err)
		}
		return
	}
	status, raw, errMsg := jobs.Outcome(res, err)
	if status == jobs.Failed && errors.Is(cause, ErrStop) {
		// The handler only saw its context cancelled.
		errMsg = cause.Error()
	}
	final, err := w.Client.Finish(context.WithoutCancel(ctx), job.ID, job.Lease, status, raw, errMsg)
	switch {
	case err != nil:
		log.ErrorContext(ctx, "finish job", "error", err)
	case final == jobs.Failed:
		log.ErrorContext(ctx, "job failed", "error", errMsg)
	default:
		log.InfoContext(ctx, "job finished", "status", final)
	}
}

// heartbeat renews the job's lease every interval until ctx is done. It
// cancels the job when the controller says to stop, or when it could not
// renew the lease for unreachableAfter.
func (w *Worker) heartbeat(ctx context.Context, log *slog.Logger, job *jobs.Job, interval time.Duration, cancel context.CancelCauseFunc) {
	if interval <= 0 {
		interval = HeartbeatInterval
	}
	limit := w.unreachableAfter
	if limit == 0 {
		limit = unreachableAfter
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	renewed := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		err := w.Client.Heartbeat(ctx, job.ID, job.Lease)
		switch {
		case err == nil:
			renewed = time.Now()
		case errors.Is(err, ErrStop):
			log.InfoContext(ctx, "the controller stopped the job", "reason", err)
			cancel(err)
			return
		case ctx.Err() != nil:
			return
		case time.Since(renewed) > limit:
			log.ErrorContext(ctx, "stopping the job: the controller does not answer", "error", err)
			cancel(fmt.Errorf("%w for %s: %w", errUnreachable, limit, err))
			return
		default:
			log.WarnContext(ctx, "renew the job's lease", "error", err)
		}
	}
}

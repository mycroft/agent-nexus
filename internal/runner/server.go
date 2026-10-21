package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/mycroft/agent-nexus/internal/auth"
	"github.com/mycroft/agent-nexus/internal/jobs"
)

const (
	// maxBodyBytes fits a checkpointed draft, or a long tool output.
	maxBodyBytes = 4 << 20
	// pollWait bounds a claim's long poll, well within the API's write
	// timeout.
	pollWait = 25 * time.Second
	// HeartbeatInterval is how often a runner renews its lease.
	HeartbeatInterval = 15 * time.Second
)

// namePattern is that of a runner instance name.
var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// Server is the controller's side of the runner API.
type Server struct {
	Queue *jobs.Queue
	// Tokens authenticate the runners. Their names prefix the runners'.
	Tokens *auth.Keys
	// Timeout is the job timeout, which runners apply.
	Timeout time.Duration
	// Version is the controller's; a runner of another version is logged.
	Version string
	// Done ends the long polls, at shutdown.
	Done   <-chan struct{}
	Logger *slog.Logger

	// pollWait, when set, replaces the default (tests).
	pollWait time.Duration
	warned   sync.Map // runners whose version was reported
}

// Handler returns the runner API routes, under /v1/runner/.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/runner/claim", s.claim)
	mux.HandleFunc("POST /v1/runner/jobs/{id}/heartbeat", s.heartbeat)
	mux.HandleFunc("POST /v1/runner/jobs/{id}/steps", s.step)
	mux.HandleFunc("POST /v1/runner/jobs/{id}/checkpoint", s.checkpoint)
	mux.HandleFunc("POST /v1/runner/jobs/{id}/finish", s.finish)
	mux.HandleFunc("POST /v1/runner/jobs/{id}/release", s.release)
	mux.HandleFunc("POST /v1/runner/enqueue", s.enqueue)
	return s.Tokens.Middleware(mux)
}

// claim long-polls for a job of the runner's model.
func (s *Server) claim(w http.ResponseWriter, r *http.Request) {
	var req ClaimRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Protocol != Protocol {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("runner protocol %d, the controller speaks %d", req.Protocol, Protocol))
		return
	}
	if !namePattern.MatchString(req.Runner) {
		writeError(w, http.StatusBadRequest, "runner must be a host-like name")
		return
	}
	runner := auth.KeyName(r.Context()) + "/" + req.Runner
	log := s.logger().With("runner", runner, "model", req.Model)
	if req.Version != s.Version {
		if _, seen := s.warned.LoadOrStore(runner+"@"+req.Version, true); !seen {
			log.WarnContext(r.Context(), "runner version differs from the controller's", "runner_version", req.Version, "version", s.Version)
		}
	}

	ctx := r.Context()
	wait := s.pollWait
	if wait == 0 {
		wait = pollWait
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		changed := s.Queue.Changed()
		job, err := s.Queue.ClaimFor(ctx, runner, req.Model, req.Kinds)
		switch {
		case errors.Is(err, jobs.ErrUnknownModel):
			writeError(w, http.StatusBadRequest, err.Error())
			return
		case err != nil:
			if ctx.Err() == nil {
				log.ErrorContext(ctx, "claim job for a runner", "error", err)
				writeError(w, http.StatusInternalServerError, "could not claim a job")
			}
			return
		case job != nil:
			s.hand(w, r, log, job)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-s.Done:
			w.WriteHeader(http.StatusNoContent)
			return
		case <-timer.C:
			w.WriteHeader(http.StatusNoContent)
			return
		case <-changed:
		}
	}
}

// hand gives a claimed job to the runner, or releases it at once if the
// runner went away meanwhile: otherwise it would wait for its lease to
// expire.
func (s *Server) hand(w http.ResponseWriter, r *http.Request, log *slog.Logger, job *jobs.Job) {
	l := Lease{
		Job: *job, Checkpoint: job.Checkpoint, Lease: job.Lease,
		TimeoutSeconds: int(s.Timeout / time.Second), HeartbeatSeconds: int(HeartbeatInterval / time.Second),
	}
	body, err := json.Marshal(l)
	if err == nil && r.Context().Err() == nil {
		w.Header().Set("Content-Type", "application/json")
		if _, err = w.Write(body); err == nil {
			log.InfoContext(r.Context(), "job handed to a runner", "job", job.ID, "kind", job.Kind)
			return
		}
	}
	if err == nil {
		err = r.Context().Err()
	}
	log.WarnContext(r.Context(), "runner went away with its claim; releasing the job", "job", job.ID, "error", err)
	if err := s.Queue.Release(context.WithoutCancel(r.Context()), job.ID, job.Lease); err != nil {
		log.ErrorContext(r.Context(), "release job", "job", job.ID, "error", err)
	}
}

func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request) {
	id, ok := jobID(w, r)
	var req leaseRequest
	if !ok || !decode(w, r, &req) {
		return
	}
	s.reply(w, r, s.Queue.Heartbeat(r.Context(), id, req.Lease), nil)
}

func (s *Server) step(w http.ResponseWriter, r *http.Request) {
	id, ok := jobID(w, r)
	var req stepRequest
	if !ok || !decode(w, r, &req) {
		return
	}
	s.reply(w, r, s.Queue.Store().AddStep(r.Context(), id, req.Lease, req.Step), nil)
}

func (s *Server) checkpoint(w http.ResponseWriter, r *http.Request) {
	id, ok := jobID(w, r)
	var req checkpointRequest
	if !ok || !decode(w, r, &req) {
		return
	}
	if len(req.Data) == 0 {
		writeError(w, http.StatusBadRequest, "data is required")
		return
	}
	s.reply(w, r, s.Queue.Store().Checkpoint(r.Context(), id, req.Lease, req.Data), nil)
}

func (s *Server) finish(w http.ResponseWriter, r *http.Request) {
	id, ok := jobID(w, r)
	var req finishRequest
	if !ok || !decode(w, r, &req) {
		return
	}
	switch req.Status {
	case jobs.Succeeded, jobs.Skipped, jobs.Superseded, jobs.Failed:
	default:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("status %q is not final", req.Status))
		return
	}
	var result []byte
	if len(req.Result) > 0 {
		result = req.Result
	}
	status, err := s.Queue.Finish(r.Context(), id, req.Lease, req.Status, result, req.Error)
	s.reply(w, r, err, finishResponse{Status: status})
}

func (s *Server) release(w http.ResponseWriter, r *http.Request) {
	id, ok := jobID(w, r)
	var req leaseRequest
	if !ok || !decode(w, r, &req) {
		return
	}
	s.reply(w, r, s.Queue.Release(r.Context(), id, req.Lease), nil)
}

// enqueue queues a job a handler starts, such as the review a discussion
// asks for.
func (s *Server) enqueue(w http.ResponseWriter, r *http.Request) {
	var req enqueueRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Kind == "" || req.GroupKey == "" || req.DedupeKey == "" || len(req.Payload) == 0 {
		writeError(w, http.StatusBadRequest, "kind, group_key, dedupe_key and payload are required")
		return
	}
	job, created, err := s.Queue.Enqueue(r.Context(), jobs.NewJob{
		Kind: req.Kind, GroupKey: req.GroupKey, DedupeKey: req.DedupeKey, KeepRunning: req.KeepRunning,
		APIKeyName: req.APIKeyName, Payload: req.Payload,
	})
	if errors.Is(err, jobs.ErrUnknownKind) {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.reply(w, r, err, enqueueResponse{Job: job, Created: created})
}

// reply answers a call: body, or no content without one; 409 when the
// runner must stop working on the job.
func (s *Server) reply(w http.ResponseWriter, r *http.Request, err error, body any) {
	switch {
	case errors.Is(err, jobs.ErrLeaseLost), errors.Is(err, jobs.ErrNotRunning), errors.Is(err, jobs.ErrTimedOut):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, jobs.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case err != nil:
		s.logger().ErrorContext(r.Context(), "runner call", "path", r.URL.Path, "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	case body == nil:
		w.WriteHeader(http.StatusNoContent)
	default:
		writeJSON(w, http.StatusOK, body)
	}
}

func (s *Server) logger() *slog.Logger {
	if s.Logger == nil {
		return slog.Default()
	}
	return s.Logger
}

func jobID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid job id")
		return 0, false
	}
	return id, true
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}

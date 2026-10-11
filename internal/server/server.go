// Package server exposes the HTTP API: events that enqueue jobs, and job
// status for debugging.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mycroft/agent-nexus/internal/auth"
	"github.com/mycroft/agent-nexus/internal/forge"
	"github.com/mycroft/agent-nexus/internal/jobs"
	"github.com/mycroft/agent-nexus/internal/review"
)

const (
	maxBodyBytes = 64 << 10
	forgeTimeout = 30 * time.Second
)

var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// Server holds the API dependencies.
type Server struct {
	Queue *jobs.Queue
	// Forges are matched against the event's server_url.
	Forges        []forge.Forge
	AllowedOwners []string
	Keys          *auth.Keys
	Logger        *slog.Logger
}

// Handler returns the API routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("POST /v1/events/pr-review", s.Keys.Middleware(http.HandlerFunc(s.prReviewEvent)))
	mux.Handle("POST /v1/events/pr-comment", s.Keys.Middleware(http.HandlerFunc(s.prCommentEvent)))
	mux.Handle("GET /v1/jobs", s.Keys.Middleware(http.HandlerFunc(s.listJobs)))
	mux.Handle("GET /v1/jobs/{id}", s.Keys.Middleware(http.HandlerFunc(s.getJob)))
	mux.Handle("GET /v1/jobs/{id}/steps", s.Keys.Middleware(http.HandlerFunc(s.getSteps)))
	return mux
}

// PREvent is the body of POST /v1/events/pr-review and
// /v1/events/pr-comment, as sent by the workflows. pr-comment events have
// no head_sha.
type PREvent struct {
	// ServerURL is the forge web root (${{ github.server_url }}).
	ServerURL string `json:"server_url"`
	// Repo is "owner/name" (${{ github.repository }}).
	Repo    string `json:"repo"`
	PR      int    `json:"pr"`
	HeadSHA string `json:"head_sha"`
	// Force reviews the commit again even if it was reviewed already
	// (a review was requested from the bot).
	Force bool `json:"force,omitempty"`
}

// checkedEvent is a validated event: its body, and its forge, repository and
// pull request as read from the forge.
type checkedEvent struct {
	body  PREvent
	forge forge.Forge
	repo  forge.Repo
	pr    forge.PR
	log   *slog.Logger
}

// readEvent decodes an event body, validates its identifiers and reads the
// pull request from the forge: only identifiers come from the caller. On
// failure it writes the response and returns nil.
func (s *Server) readEvent(w http.ResponseWriter, r *http.Request) *checkedEvent {
	ctx := r.Context()
	var ev PREvent
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&ev); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return nil
	}
	repo, err := forge.ParseRepo(ev.Repo)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return nil
	}
	if ev.PR <= 0 {
		writeError(w, http.StatusBadRequest, "pr must be a positive pull request number")
		return nil
	}
	f := s.forgeFor(ev.ServerURL)
	if f == nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown server_url %q", ev.ServerURL))
		return nil
	}
	if !slices.ContainsFunc(s.AllowedOwners, func(o string) bool { return strings.EqualFold(o, repo.Owner) }) {
		writeError(w, http.StatusForbidden, fmt.Sprintf("owner %q is not allowed", repo.Owner))
		return nil
	}
	log := s.Logger.With("forge", f.Name(), "repo", repo.String(), "pr", ev.PR, "key", auth.KeyName(ctx))

	forgeCtx, cancel := context.WithTimeout(ctx, forgeTimeout)
	defer cancel()
	pr, err := f.GetPR(forgeCtx, repo, ev.PR)
	switch {
	case errors.Is(err, forge.ErrNotFound):
		writeError(w, http.StatusNotFound, "pull request not found")
		return nil
	case err != nil:
		log.ErrorContext(ctx, "get pull request", "error", err)
		writeError(w, http.StatusBadGateway, "could not read the pull request from the forge")
		return nil
	}
	return &checkedEvent{body: ev, forge: f, repo: repo, pr: pr, log: log}
}

func (s *Server) prReviewEvent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ev := s.readEvent(w, r)
	if ev == nil {
		return
	}
	body := ev.body
	if !shaPattern.MatchString(body.HeadSHA) {
		writeError(w, http.StatusBadRequest, "head_sha must be a full lowercase hex commit SHA")
		return
	}
	log := ev.log.With("head_sha", body.HeadSHA)
	switch {
	case ev.pr.State != "open":
		writeJSON(w, http.StatusOK, map[string]any{"status": "ignored", "reason": "pull request is not open"})
		return
	case ev.pr.HeadSHA != body.HeadSHA:
		// A late event must not supersede the job of a newer commit.
		log.InfoContext(ctx, "ignoring stale event", "current_head_sha", ev.pr.HeadSHA)
		writeJSON(w, http.StatusOK, map[string]any{"status": "stale", "current_head_sha": ev.pr.HeadSHA})
		return
	case ev.pr.Draft:
		writeJSON(w, http.StatusOK, map[string]any{"status": "ignored", "reason": "pull request is a draft"})
		return
	}

	s.enqueue(w, r, log, "review", review.Job(review.Request{
		Forge:   ev.forge.Name(),
		Owner:   ev.repo.Owner,
		Repo:    ev.repo.Name,
		PR:      body.PR,
		HeadSHA: body.HeadSHA,
		Force:   body.Force,
	}, auth.KeyName(ctx)))
}

// prCommentEvent queues a pass over the pull request's discussion, which
// answers the replies and mentions waiting for the bot, closed pull
// requests included. Each event gets its own job: a newer one replaces a
// queued one, and a running one finishes.
func (s *Server) prCommentEvent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ev := s.readEvent(w, r)
	if ev == nil {
		return
	}
	body := ev.body
	group := review.GroupKey(ev.forge.Name(), ev.repo, body.PR) + ":discussion"
	s.enqueue(w, r, ev.log, "discussion", jobs.NewJob{
		Kind:        review.DiscussionKind,
		GroupKey:    group,
		DedupeKey:   fmt.Sprintf("%s@%d", group, time.Now().UnixNano()),
		KeepRunning: true,
		APIKeyName:  auth.KeyName(ctx),
		Payload: review.DiscussionRequest{
			Forge: ev.forge.Name(),
			Owner: ev.repo.Owner,
			Repo:  ev.repo.Name,
			PR:    body.PR,
		},
	})
}

func (s *Server) enqueue(w http.ResponseWriter, r *http.Request, log *slog.Logger, what string, nj jobs.NewJob) {
	ctx := r.Context()
	job, created, err := s.Queue.Enqueue(ctx, nj)
	if err != nil {
		log.ErrorContext(ctx, "enqueue "+what, "error", err)
		writeError(w, http.StatusInternalServerError, "could not enqueue the "+what)
		return
	}
	if !created {
		writeJSON(w, http.StatusOK, map[string]any{"status": job.Status, "job_id": job.ID, "duplicate": true})
		return
	}
	log.InfoContext(ctx, what+" queued", "job", job.ID)
	writeJSON(w, http.StatusAccepted, map[string]any{"status": job.Status, "job_id": job.ID})
}

func (s *Server) forgeFor(serverURL string) forge.Forge {
	serverURL = strings.TrimRight(strings.TrimSpace(serverURL), "/")
	for _, f := range s.Forges {
		if strings.EqualFold(f.ServerURL(), serverURL) {
			return f
		}
	}
	return nil
}

// listJobs lists recent jobs, newest first, with their progress:
// ?status=running,queued (comma-separated), ?kind=pr_review, ?limit=20.
func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := jobs.ListFilter{Kind: q.Get("kind")}
	for name := range strings.SplitSeq(q.Get("status"), ",") {
		if name = strings.TrimSpace(name); name == "" {
			continue
		}
		status, err := jobs.ParseStatus(name)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		filter.Statuses = append(filter.Statuses, status)
	}
	if v := q.Get("limit"); v != "" {
		limit, err := strconv.Atoi(v)
		if err != nil || limit <= 0 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		filter.Limit = limit
	}
	list, err := s.Queue.Store().List(r.Context(), filter)
	if err != nil {
		s.Logger.ErrorContext(r.Context(), "list jobs", "error", err)
		writeError(w, http.StatusInternalServerError, "could not list the jobs")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": list})
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	id, ok := jobID(w, r)
	if !ok {
		return
	}
	store := s.Queue.Store()
	job, err := store.Get(r.Context(), id)
	if errors.Is(err, jobs.ErrNotFound) {
		writeError(w, http.StatusNotFound, "job not found")
		return
	}
	if err != nil {
		s.Logger.ErrorContext(r.Context(), "get job", "job", id, "error", err)
		writeError(w, http.StatusInternalServerError, "could not read the job")
		return
	}
	prompt, completion, err := store.Usage(r.Context(), id)
	if err != nil {
		s.Logger.ErrorContext(r.Context(), "get job usage", "job", id, "error", err)
	}
	writeJSON(w, http.StatusOK, struct {
		jobs.Job
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	}{job, prompt, completion})
}

func (s *Server) getSteps(w http.ResponseWriter, r *http.Request) {
	id, ok := jobID(w, r)
	if !ok {
		return
	}
	steps, err := s.Queue.Store().Steps(r.Context(), id)
	if err != nil {
		s.Logger.ErrorContext(r.Context(), "get job steps", "job", id, "error", err)
		writeError(w, http.StatusInternalServerError, "could not read the job steps")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job_id": id, "steps": steps})
}

func jobID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid job id")
		return 0, false
	}
	return id, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

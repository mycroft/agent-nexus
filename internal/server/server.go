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
	mux.Handle("GET /v1/jobs/{id}", s.Keys.Middleware(http.HandlerFunc(s.getJob)))
	mux.Handle("GET /v1/jobs/{id}/steps", s.Keys.Middleware(http.HandlerFunc(s.getSteps)))
	return mux
}

// PREvent is the body of POST /v1/events/pr-review, as sent by the
// workflows.
type PREvent struct {
	// ServerURL is the forge web root (${{ github.server_url }}).
	ServerURL string `json:"server_url"`
	// Repo is "owner/name" (${{ github.repository }}).
	Repo    string `json:"repo"`
	PR      int    `json:"pr"`
	HeadSHA string `json:"head_sha"`
}

func (s *Server) prReviewEvent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var ev PREvent
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&ev); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	repo, err := forge.ParseRepo(ev.Repo)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if ev.PR <= 0 {
		writeError(w, http.StatusBadRequest, "pr must be a positive pull request number")
		return
	}
	if !shaPattern.MatchString(ev.HeadSHA) {
		writeError(w, http.StatusBadRequest, "head_sha must be a full lowercase hex commit SHA")
		return
	}
	f := s.forgeFor(ev.ServerURL)
	if f == nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown server_url %q", ev.ServerURL))
		return
	}
	if !slices.ContainsFunc(s.AllowedOwners, func(o string) bool { return strings.EqualFold(o, repo.Owner) }) {
		writeError(w, http.StatusForbidden, fmt.Sprintf("owner %q is not allowed", repo.Owner))
		return
	}
	log := s.Logger.With("forge", f.Name(), "repo", repo.String(), "pr", ev.PR, "head_sha", ev.HeadSHA, "key", auth.KeyName(ctx))

	// Only identifiers come from the caller; the pull request itself is
	// read from the forge. This also drops late events: an event for a
	// commit that is no longer the head must not supersede a newer job.
	forgeCtx, cancel := context.WithTimeout(ctx, forgeTimeout)
	defer cancel()
	pr, err := f.GetPR(forgeCtx, repo, ev.PR)
	switch {
	case errors.Is(err, forge.ErrNotFound):
		writeError(w, http.StatusNotFound, "pull request not found")
		return
	case err != nil:
		log.ErrorContext(ctx, "get pull request", "error", err)
		writeError(w, http.StatusBadGateway, "could not read the pull request from the forge")
		return
	}
	switch {
	case pr.HeadSHA != ev.HeadSHA:
		log.InfoContext(ctx, "ignoring stale event", "current_head_sha", pr.HeadSHA)
		writeJSON(w, http.StatusOK, map[string]any{"status": "stale", "current_head_sha": pr.HeadSHA})
		return
	case pr.State != "open":
		writeJSON(w, http.StatusOK, map[string]any{"status": "ignored", "reason": "pull request is not open"})
		return
	case pr.Draft:
		writeJSON(w, http.StatusOK, map[string]any{"status": "ignored", "reason": "pull request is a draft"})
		return
	}

	group := fmt.Sprintf("%s:%s#%d", f.Name(), strings.ToLower(repo.String()), ev.PR)
	job, created, err := s.Queue.Enqueue(ctx, jobs.NewJob{
		Kind:       review.Kind,
		GroupKey:   group,
		DedupeKey:  group + "@" + ev.HeadSHA,
		APIKeyName: auth.KeyName(ctx),
		Payload: review.Request{
			Forge:   f.Name(),
			Owner:   repo.Owner,
			Repo:    repo.Name,
			PR:      ev.PR,
			HeadSHA: ev.HeadSHA,
		},
	})
	if err != nil {
		log.ErrorContext(ctx, "enqueue review", "error", err)
		writeError(w, http.StatusInternalServerError, "could not enqueue the review")
		return
	}
	if !created {
		writeJSON(w, http.StatusOK, map[string]any{"status": job.Status, "job_id": job.ID, "duplicate": true})
		return
	}
	log.InfoContext(ctx, "review queued", "job", job.ID)
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

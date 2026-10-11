// Package web serves a small read-only UI over the job store: what is
// running, the job history, and each job's transcript. It has no
// authentication of its own: serve it on its own listener, behind an
// authenticating proxy.
package web

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mycroft/agent-nexus/internal/jobs"
)

//go:embed templates static
var assets embed.FS

const (
	historyPageSize = 30
	// activeLimit bounds the running and queued jobs shown at the top.
	activeLimit = 20
)

// Options configures the UI.
type Options struct {
	Version string
	// ForgeURLs maps forge names ("gitea", "github") to their web roots,
	// to link pull requests.
	ForgeURLs map[string]string
	Logger    *slog.Logger
}

// Server renders the UI.
type Server struct {
	store *jobs.Store
	opts  Options
	tmpl  *template.Template
}

// New parses the templates.
func New(store *jobs.Store, opts Options) (*Server, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	tmpl, err := template.New("").Funcs(funcs).ParseFS(assets, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	return &Server{store: store, opts: opts, tmpl: tmpl}, nil
}

// Handler returns the UI routes.
func (s *Server) Handler() http.Handler {
	static, err := fs.Sub(assets, "static")
	if err != nil {
		panic(err) // the embedded tree always has it
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /fragments/live", s.handleLive)
	mux.HandleFunc("GET /fragments/history", s.handleHistory)
	mux.HandleFunc("GET /jobs/{id}", s.handleJob)
	mux.HandleFunc("GET /fragments/jobs/{id}/steps", s.handleSteps)
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheStatic(http.FileServerFS(static))))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// cacheStatic lets browsers keep assets: their URLs carry the version.
func cacheStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		next.ServeHTTP(w, r)
	})
}

// page is the data of a full page.
type page struct {
	Title   string
	Version string
	Theme   string // "light" or "dark" when chosen with the toggle; empty follows the OS
	Live    live
	History history
	Job     *jobDetail
}

// live is the top of the main page: what runs and waits.
type live struct {
	Active []jobView
	Queued []jobView
	// Seen lists the shown unfinished jobs and their status; the next
	// poll sends it back, so the server can tell when the history needs
	// a refresh.
	Seen string
}

// history is a page of finished and unfinished jobs, newest first.
type history struct {
	Jobs    []jobView
	OlderID int64 // the next page starts below this job, 0 at the end
	// OOB swaps the "load more" button out of band, for appended rows. A
	// whole section must not: htmx would pull the button out of it.
	OOB bool
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	l, err := s.live(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	list, err := s.store.List(r.Context(), jobs.ListFilter{Limit: historyPageSize})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, http.StatusOK, "index", page{Title: "Jobs", Version: s.opts.Version, Theme: theme(r), Live: l, History: s.history(list)})
}

// handleLive refreshes the running and queued jobs. When they changed since
// the page last looked (?seen=), it also asks the page to reload its
// history, which is otherwise left alone so loaded older rows stay.
func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	l, err := s.live(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if r.URL.Query().Get("seen") != l.Seen {
		w.Header().Set("Hx-Trigger", "jobs-changed")
	}
	s.render(w, http.StatusOK, "live", l)
}

func (s *Server) live(r *http.Request) (live, error) {
	ctx := r.Context()
	active, err := s.store.List(ctx, jobs.ListFilter{Statuses: []jobs.Status{jobs.Running, jobs.Posting}, Limit: activeLimit})
	if err != nil {
		return live{}, err
	}
	queued, err := s.store.List(ctx, jobs.ListFilter{Statuses: []jobs.Status{jobs.Queued}, Limit: activeLimit})
	if err != nil {
		return live{}, err
	}
	l := live{Active: s.views(active), Queued: s.views(queued)}
	// Oldest first: that is the order they run in.
	for i, j := 0, len(l.Queued)-1; i < j; i, j = i+1, j-1 {
		l.Queued[i], l.Queued[j] = l.Queued[j], l.Queued[i]
	}
	var seen []string
	for _, j := range append(append([]jobView{}, l.Active...), l.Queued...) {
		seen = append(seen, fmt.Sprintf("%d:%s", j.ID, j.Status))
	}
	l.Seen = strings.Join(seen, ",")
	return l, nil
}

// handleHistory serves the history: its first page as a whole section, or
// the rows below ?before=<job id> to append.
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	var before int64
	if v := r.URL.Query().Get("before"); v != "" {
		var err error
		if before, err = strconv.ParseInt(v, 10, 64); err != nil || before <= 0 {
			http.Error(w, "before must be a job id", http.StatusBadRequest)
			return
		}
	}
	list, err := s.store.List(r.Context(), jobs.ListFilter{BeforeID: before, Limit: historyPageSize})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	h := s.history(list)
	name := "history"
	if before > 0 {
		name, h.OOB = "history-rows", true
	}
	s.render(w, http.StatusOK, name, h)
}

func (s *Server) history(list []jobs.JobSummary) history {
	h := history{Jobs: s.views(list)}
	if len(list) == historyPageSize {
		h.OlderID = list[len(list)-1].ID
	}
	return h
}

// jobDetail is a job page.
type jobDetail struct {
	jobView
	Steps   []stepView
	LastSeq int
	OOB     bool
}

func (s *Server) handleJob(w http.ResponseWriter, r *http.Request) {
	d, ok := s.jobDetail(w, r, 0)
	if !ok {
		return
	}
	s.render(w, http.StatusOK, "job", page{Title: fmt.Sprintf("Job %d", d.ID), Version: s.opts.Version, Theme: theme(r), Job: d})
}

// handleSteps serves the steps recorded after ?after=<seq>, with the job
// header out of band. While the job is unfinished, the response carries
// the poller asking for the next ones.
func (s *Server) handleSteps(w http.ResponseWriter, r *http.Request) {
	after, err := strconv.Atoi(r.URL.Query().Get("after"))
	if err != nil || after < 0 {
		http.Error(w, "after must be a step number", http.StatusBadRequest)
		return
	}
	d, ok := s.jobDetail(w, r, after)
	if !ok {
		return
	}
	d.OOB = true
	s.render(w, http.StatusOK, "steps", d)
}

func (s *Server) jobDetail(w http.ResponseWriter, r *http.Request, after int) (*jobDetail, bool) {
	ctx := r.Context()
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid job id", http.StatusBadRequest)
		return nil, false
	}
	summary, err := s.store.Summary(ctx, id)
	if errors.Is(err, jobs.ErrNotFound) {
		s.render(w, http.StatusNotFound, "not-found", page{Title: "Not found", Version: s.opts.Version, Theme: theme(r)})
		return nil, false
	}
	if err != nil {
		s.fail(w, r, err)
		return nil, false
	}
	steps, err := s.store.StepsAfter(ctx, id, after)
	if err != nil {
		s.fail(w, r, err)
		return nil, false
	}
	d := &jobDetail{jobView: newJobView(summary, s.opts.ForgeURLs), LastSeq: after}
	for i, st := range steps {
		d.Steps = append(d.Steps, newStepView(st, after == 0 && i < 2))
		d.LastSeq = st.Seq
	}
	return d, true
}

func (s *Server) views(list []jobs.JobSummary) []jobView {
	out := make([]jobView, 0, len(list))
	for _, j := range list {
		out = append(out, newJobView(j, s.opts.ForgeURLs))
	}
	return out
}

// render buffers the template, so a failure yields a clean error page.
func (s *Server) render(w http.ResponseWriter, status int, name string, data any) {
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		s.opts.Logger.Error("render template", "template", name, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	s.opts.Logger.ErrorContext(r.Context(), "ui request", "path", r.URL.Path, "error", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

// themeCookie holds the theme picked with the header toggle (set by
// app.js). Rendering it server-side avoids a flash of the wrong theme.
const themeCookie = "theme"

func theme(r *http.Request) string {
	c, err := r.Cookie(themeCookie)
	if err != nil || (c.Value != "light" && c.Value != "dark") {
		return ""
	}
	return c.Value
}

// jobView is a job as the templates show it.
type jobView struct {
	ID           int64
	Kind         string
	Status       jobs.Status
	Forge        string
	Repo         string // owner/name
	PR           int
	PRURL        string
	HeadSHA      string
	Forced       bool
	APIKeyName   string
	CreatedAt    time.Time
	StartedAt    *time.Time
	FinishedAt   *time.Time
	Attempts     int
	Error        string
	Progress     jobs.Progress
	Result       map[string]any
	ResultPretty string
}

func newJobView(j jobs.JobSummary, forgeURLs map[string]string) jobView {
	v := jobView{
		ID: j.ID, Kind: j.Kind, Status: j.Status, APIKeyName: j.APIKeyName,
		CreatedAt: j.CreatedAt, StartedAt: j.StartedAt, FinishedAt: j.FinishedAt,
		Attempts: j.Attempts, Error: j.Error, Progress: j.Progress,
	}
	var payload struct {
		Forge   string `json:"forge"`
		Owner   string `json:"owner"`
		Repo    string `json:"repo"`
		PR      int    `json:"pr"`
		HeadSHA string `json:"head_sha"`
		Force   bool   `json:"force"`
	}
	if json.Unmarshal(j.Payload, &payload) == nil && payload.Owner != "" {
		v.Forge, v.PR, v.HeadSHA, v.Forced = payload.Forge, payload.PR, payload.HeadSHA, payload.Force
		v.Repo = payload.Owner + "/" + payload.Repo
		if root := forgeURLs[payload.Forge]; root != "" {
			segment := "pulls"
			if payload.Forge == "github" {
				segment = "pull"
			}
			v.PRURL = fmt.Sprintf("%s/%s/%s/%d", root, v.Repo, segment, payload.PR)
		}
	}
	if len(j.Result) > 0 {
		_ = json.Unmarshal(j.Result, &v.Result)
		if pretty, err := json.MarshalIndent(v.Result, "", "  "); err == nil {
			v.ResultPretty = string(pretty)
		}
	}
	return v
}

// Active reports whether the job may still change.
func (v jobView) Active() bool {
	return v.Status == jobs.Queued || v.Status == jobs.Running || v.Status == jobs.Posting
}

// outcomeLabels words the outcome codes jobs report.
var outcomeLabels = map[string]string{
	"skipped_closed":    "pull request closed",
	"skipped_draft":     "draft",
	"superseded":        "newer commit",
	"already_posted":    "already posted",
	"nothing_to_answer": "nothing to answer",
	"approved":          "approved",
}

// Outcome is the job's result in a few words.
func (v jobView) Outcome() string {
	str := func(k string) string { s, _ := v.Result[k].(string); return s }
	num := func(k string) int { n, _ := v.Result[k].(float64); return int(n) }
	approved, _ := v.Result["approved"].(bool)
	switch {
	case v.Status == jobs.Failed:
		return "failed"
	case v.Status == jobs.Superseded:
		return outcomeLabels["superseded"]
	case v.Status == jobs.Skipped:
		// A review skipped at posting time still carries its verdict:
		// the reason it was not posted matters more.
		if label, ok := outcomeLabels[str("outcome")]; ok {
			return label
		}
		return str("outcome")
	case str("verdict") != "" && str("event") != "":
		return fmt.Sprintf("%s · %d finding%s", str("event"), num("findings"), plural(num("findings")))
	case str("verdict") != "":
		return fmt.Sprintf("%s · %d finding%s", str("verdict"), num("findings"), plural(num("findings")))
	case str("outcome") == "answered" && approved:
		return fmt.Sprintf("%d answered · approved", num("answered"))
	case str("outcome") == "answered":
		return fmt.Sprintf("%d answered", num("answered"))
	default:
		if label, ok := outcomeLabels[str("outcome")]; ok {
			return label
		}
		return str("outcome")
	}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

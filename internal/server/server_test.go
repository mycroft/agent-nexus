package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mycroft/agent-nexus/internal/auth"
	"github.com/mycroft/agent-nexus/internal/forge"
	"github.com/mycroft/agent-nexus/internal/forge/forgetest"
	"github.com/mycroft/agent-nexus/internal/jobs"
	"github.com/mycroft/agent-nexus/internal/review"
)

var (
	apiKey = strings.Repeat("k", 32)
	head   = strings.Repeat("a", 40)
	newer  = strings.Repeat("b", 40)
)

type noopHandler struct{}

func (noopHandler) Handle(context.Context, *jobs.Job, *jobs.Run) (jobs.Result, error) {
	return jobs.Result{Status: jobs.Succeeded}, nil
}

func newTestServer(t *testing.T) (*httptest.Server, *forgetest.Fake, *jobs.Store) {
	t.Helper()
	store, err := jobs.OpenStore(context.Background(), filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	log := slog.New(slog.DiscardHandler)
	q := jobs.NewQueue(store, 1, time.Minute, time.Hour, log)
	q.Register(review.Kind, noopHandler{})

	keys, err := auth.ParseKeys("test="+apiKey, 1000)
	if err != nil {
		t.Fatal(err)
	}
	fake := forgetest.New("gitea", "https://git.example.com")
	fake.SetPR(forge.PR{Number: 1, State: "open", HeadSHA: head, BaseRef: "main"})
	fake.SetPR(forge.PR{Number: 2, State: "open", Draft: true, HeadSHA: head})
	fake.SetPR(forge.PR{Number: 3, State: "closed", HeadSHA: head})

	s := &Server{Queue: q, Forges: []forge.Forge{fake}, AllowedOwners: []string{"Mycroft"}, Keys: keys, Logger: log}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv, fake, store
}

func post(t *testing.T, srv *httptest.Server, key string, body any) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/events/pr-review", strings.NewReader(string(raw)))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func event(pr int, sha string) PREvent {
	return PREvent{ServerURL: "https://git.example.com/", Repo: "mycroft/repo", PR: pr, HeadSHA: sha}
}

func TestPREvent(t *testing.T) {
	srv, fake, store := newTestServer(t)

	if code, _ := post(t, srv, "", event(1, head)); code != http.StatusUnauthorized {
		t.Errorf("no key → %d", code)
	}
	if code, _ := post(t, srv, "wrong", event(1, head)); code != http.StatusUnauthorized {
		t.Errorf("wrong key → %d", code)
	}

	for name, tc := range map[string]struct {
		ev   PREvent
		code int
	}{
		"bad repo":       {PREvent{ServerURL: "https://git.example.com", Repo: "../x", PR: 1, HeadSHA: head}, http.StatusBadRequest},
		"bad pr":         {event(0, head), http.StatusBadRequest},
		"short sha":      {event(1, "abc123"), http.StatusBadRequest},
		"unknown forge":  {PREvent{ServerURL: "https://evil.example.com", Repo: "mycroft/repo", PR: 1, HeadSHA: head}, http.StatusBadRequest},
		"other owner":    {PREvent{ServerURL: "https://git.example.com", Repo: "someone/repo", PR: 1, HeadSHA: head}, http.StatusForbidden},
		"missing pr":     {event(99, head), http.StatusNotFound},
		"stale sha":      {event(1, newer), http.StatusOK},
		"draft pr":       {event(2, head), http.StatusOK},
		"closed pr":      {event(3, head), http.StatusOK},
		"queued for pr1": {event(1, head), http.StatusAccepted},
	} {
		code, body := post(t, srv, apiKey, tc.ev)
		if code != tc.code {
			t.Errorf("%s → %d %v, want %d", name, code, body, tc.code)
		}
	}

	code, body := post(t, srv, apiKey, event(1, head))
	if code != http.StatusOK || body["duplicate"] != true {
		t.Fatalf("duplicate event → %d %v", code, body)
	}
	id := int64(body["job_id"].(float64))
	job, err := store.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var payload review.Request
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	want := review.Request{Forge: "gitea", Owner: "mycroft", Repo: "repo", PR: 1, HeadSHA: head}
	if payload != want || job.APIKeyName != "test" || job.GroupKey != "gitea:mycroft/repo#1" {
		t.Errorf("job = %+v, payload = %+v", job, payload)
	}

	// A push moves the head: the new event supersedes the queued job.
	fake.SetPR(forge.PR{Number: 1, State: "open", HeadSHA: newer, BaseRef: "main"})
	if code, _ := post(t, srv, apiKey, event(1, newer)); code != http.StatusAccepted {
		t.Fatalf("new head → %d", code)
	}
	if job, _ := store.Get(context.Background(), id); job.Status != jobs.Superseded {
		t.Errorf("old job status = %s, want superseded", job.Status)
	}
}

func TestGetJob(t *testing.T) {
	srv, _, _ := newTestServer(t)
	_, body := post(t, srv, apiKey, event(1, head))
	id := int64(body["job_id"].(float64))

	get := func(path, key string) (int, map[string]any) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	if code, _ := get("/v1/jobs/1", "wrong"); code != http.StatusUnauthorized {
		t.Errorf("job without key → %d", code)
	}
	code, job := get("/v1/jobs/"+jsonNumber(id), apiKey)
	if code != http.StatusOK || job["status"] != "queued" || job["kind"] != review.Kind {
		t.Errorf("get job → %d %v", code, job)
	}
	if code, _ := get("/v1/jobs/999", apiKey); code != http.StatusNotFound {
		t.Errorf("missing job → %d", code)
	}
	if code, _ := get("/v1/jobs/abc", apiKey); code != http.StatusBadRequest {
		t.Errorf("bad id → %d", code)
	}
	if code, steps := get("/v1/jobs/"+jsonNumber(id)+"/steps", apiKey); code != http.StatusOK || steps["job_id"] == nil {
		t.Errorf("steps → %d %v", code, steps)
	}

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz → %d", resp.StatusCode)
	}
}

func jsonNumber(n int64) string {
	raw, _ := json.Marshal(n)
	return string(raw)
}

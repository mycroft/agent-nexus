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
	"github.com/mycroft/agent-nexus/internal/zulip"
	"github.com/mycroft/agent-nexus/internal/zulip/zuliptest"
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
	q.Register(review.DiscussionKind, noopHandler{})

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
	return postTo(t, srv, "/v1/events/pr-review", key, body)
}

func postTo(t *testing.T, srv *httptest.Server, path, key string, body any) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(string(raw)))
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

func TestPRCommentEvent(t *testing.T) {
	srv, _, store := newTestServer(t)
	ctx := context.Background()
	comment := func(pr int) PREvent {
		return PREvent{ServerURL: "https://git.example.com", Repo: "mycroft/repo", PR: pr}
	}
	if code, _ := postTo(t, srv, "/v1/events/pr-comment", "", comment(1)); code != http.StatusUnauthorized {
		t.Errorf("no key → %d", code)
	}
	// Closed pull requests are discussed too (only reviews skip them).
	if code, body := postTo(t, srv, "/v1/events/pr-comment", apiKey, comment(3)); code != http.StatusAccepted || body["status"] != "queued" {
		t.Errorf("closed PR → %d %v", code, body)
	}
	if code, _ := postTo(t, srv, "/v1/events/pr-comment", apiKey, PREvent{ServerURL: "https://git.example.com", Repo: "someone/repo", PR: 1}); code != http.StatusForbidden {
		t.Errorf("other owner → %d", code)
	}

	code, body := postTo(t, srv, "/v1/events/pr-comment", apiKey, comment(1))
	if code != http.StatusAccepted {
		t.Fatalf("comment → %d %v", code, body)
	}
	first := int64(body["job_id"].(float64))
	job, err := store.Get(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	var payload review.DiscussionRequest
	_ = json.Unmarshal(job.Payload, &payload)
	if job.Kind != review.DiscussionKind || job.GroupKey != "gitea:mycroft/repo#1:discussion" ||
		payload != (review.DiscussionRequest{Forge: "gitea", Owner: "mycroft", Repo: "repo", PR: 1}) {
		t.Errorf("job = %+v, payload = %+v", job, payload)
	}

	// A second comment queues a new pass, which replaces the queued one;
	// the review of the same pull request is a different group.
	if code, _ := post(t, srv, apiKey, event(1, head)); code != http.StatusAccepted {
		t.Fatalf("review → %d", code)
	}
	if code, _ := postTo(t, srv, "/v1/events/pr-comment", apiKey, comment(1)); code != http.StatusAccepted {
		t.Fatalf("second comment → %d", code)
	}
	if job, _ := store.Get(ctx, first); job.Status != jobs.Superseded {
		t.Errorf("first pass status = %s, want superseded", job.Status)
	}
	var reviews int
	for id := first + 1; ; id++ {
		job, err := store.Get(ctx, id)
		if err != nil {
			break
		}
		if job.Kind == review.Kind && job.Status == jobs.Queued {
			reviews++
		}
	}
	if reviews != 1 {
		t.Errorf("queued reviews = %d, want 1 (a comment must not supersede it)", reviews)
	}
}

func TestListJobs(t *testing.T) {
	srv, _, _ := newTestServer(t)
	post(t, srv, apiKey, event(1, head))
	postTo(t, srv, "/v1/events/pr-comment", apiKey, PREvent{ServerURL: "https://git.example.com", Repo: "mycroft/repo", PR: 1})

	get := func(query, key string) (int, map[string]any) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/jobs"+query, nil)
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
	if code, _ := get("", "wrong"); code != http.StatusUnauthorized {
		t.Errorf("no key → %d", code)
	}
	code, body := get("", apiKey)
	list, _ := body["jobs"].([]any)
	if code != http.StatusOK || len(list) != 2 {
		t.Fatalf("list → %d %v", code, body)
	}
	newest, _ := list[0].(map[string]any)
	if newest["kind"] != review.DiscussionKind || newest["status"] != "queued" || newest["turns"] != float64(0) || newest["payload"] == nil {
		t.Errorf("newest job = %v", newest)
	}
	if code, body := get("?status=queued&kind=pr_review&limit=5", apiKey); code != http.StatusOK || len(body["jobs"].([]any)) != 1 {
		t.Errorf("filtered → %d %v", code, body)
	}
	if code, body := get("?status=running", apiKey); code != http.StatusOK || len(body["jobs"].([]any)) != 0 {
		t.Errorf("no running job → %d %v", code, body)
	}
	for _, bad := range []string{"?status=done", "?limit=0", "?limit=x"} {
		if code, _ := get(bad, apiKey); code != http.StatusBadRequest {
			t.Errorf("%s → %d", bad, code)
		}
	}
}

// A requested review runs again on an already reviewed commit.
func TestForcedPREvent(t *testing.T) {
	srv, _, store := newTestServer(t)
	ctx := context.Background()
	_, body := post(t, srv, apiKey, event(1, head))
	first := int64(body["job_id"].(float64))
	if _, err := store.Claim(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Finish(ctx, first, jobs.Succeeded, nil, ""); err != nil {
		t.Fatal(err)
	}
	if code, body := post(t, srv, apiKey, event(1, head)); code != http.StatusOK || body["duplicate"] != true {
		t.Errorf("same commit → %d %v, want a duplicate", code, body)
	}
	forced := event(1, head)
	forced.Force = true
	code, body := post(t, srv, apiKey, forced)
	if code != http.StatusAccepted {
		t.Fatalf("forced → %d %v", code, body)
	}
	job, _ := store.Get(ctx, int64(body["job_id"].(float64)))
	var req review.Request
	_ = json.Unmarshal(job.Payload, &req)
	if !req.Force || job.GroupKey != "gitea:mycroft/repo#1" {
		t.Errorf("forced job = %+v, payload %+v", job, req)
	}
}

func TestZulipEvent(t *testing.T) {
	ctx := context.Background()
	store, err := jobs.OpenStore(ctx, filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	log := slog.New(slog.DiscardHandler)
	q := jobs.NewQueue(store, 1, time.Minute, time.Hour, log)
	q.Register(review.ChatKind, noopHandler{})
	z := zuliptest.New(t)
	keys, err := auth.ParseKeys("test="+apiKey, 1000)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Queue: q, Keys: keys, Zulip: z.Client(), ZulipToken: "webhook-token", Logger: log}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	alice := zulip.User{ID: 7, FullName: "Alice"}
	m := z.Add(zuliptest.StreamMessage(3, "dev", "Calc", alice, "@**agent-nexus** what is o/calc?"))
	outgoing := func(token string, m zulip.Message) zulip.Outgoing {
		return zulip.Outgoing{BotEmail: zuliptest.Bot.Email, Token: token, Trigger: "mention", Message: m}
	}

	if code, _ := postTo(t, srv, "/v1/events/zulip", "", outgoing("wrong", m)); code != http.StatusUnauthorized {
		t.Errorf("wrong token → %d", code)
	}
	if code, _ := postTo(t, srv, "/v1/events/zulip", "", outgoing("", m)); code != http.StatusUnauthorized {
		t.Errorf("no token → %d", code)
	}
	if code, _ := postTo(t, srv, "/v1/events/zulip", "", outgoing("webhook-token", zulip.Message{ID: 9, Type: "other"})); code != http.StatusBadRequest {
		t.Errorf("unknown message type → %d", code)
	}

	// Zulip retries a message it got no answer for: it is one job.
	for range 2 {
		code, body := postTo(t, srv, "/v1/events/zulip", "", outgoing("webhook-token", m))
		if code != http.StatusOK || body["response_not_required"] != true {
			t.Fatalf("event → %d %v", code, body)
		}
	}
	list, err := store.List(ctx, jobs.ListFilter{Kind: review.ChatKind})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].GroupKey != "zulip:stream:3:calc" || list[0].Status != jobs.Queued || list[0].APIKeyName != "zulip:Alice" {
		t.Fatalf("jobs = %+v", list)
	}
	var payload review.ChatRequest
	if err := json.Unmarshal(list[0].Payload, &payload); err != nil || payload.Conversation.Topic != "Calc" || payload.Label != "#dev > Calc" {
		t.Errorf("payload = %+v, %v", payload, err)
	}
	if got := z.Message(m.ID); !got.ReactedBy(zuliptest.Bot.ID, review.ChatSeenEmoji) {
		t.Errorf("question not acknowledged: %+v", got.Reactions)
	}

	// Without Zulip, there is no such endpoint.
	plain, _, _ := newTestServer(t)
	if code, _ := postTo(t, plain, "/v1/events/zulip", "", outgoing("", m)); code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
		t.Errorf("without Zulip → %d", code)
	}
}

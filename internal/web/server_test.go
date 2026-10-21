package web

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mycroft/agent-nexus/internal/jobs"
)

func newTestUI(t *testing.T) (http.Handler, *jobs.Store) {
	t.Helper()
	store, err := jobs.OpenStore(context.Background(), filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ui, err := New(store, Options{
		Version:   "1.2.3",
		ForgeURLs: map[string]string{"gitea": "https://git.example", "github": "https://github.com"},
		Logger:    slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	return ui.Handler(), store
}

// enqueue adds a review of a distinct pull request.
func enqueue(t *testing.T, store *jobs.Store, forge string, pr int) int64 {
	t.Helper()
	key := fmt.Sprintf("%s:mycroft/app#%d", forge, pr)
	j, _, _, err := store.Enqueue(context.Background(), jobs.NewJob{
		Kind: "pr_review", GroupKey: key, DedupeKey: key + "@sha", APIKeyName: "gitea-actions",
		Payload: map[string]any{"forge": forge, "owner": "mycroft", "repo": "app", "pr": pr, "head_sha": strings.Repeat("c", 40)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return j.ID
}

func claim(t *testing.T, store *jobs.Store, want int64) {
	t.Helper()
	got, err := store.Claim(context.Background(), jobs.ClaimSpec{})
	if err != nil || got == nil || got.ID != want {
		t.Fatalf("Claim = %+v, %v, want job %d", got, err, want)
	}
}

func get(t *testing.T, h http.Handler, path string, cookies ...*http.Cookie) (*http.Response, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	res := rec.Result()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	return res, string(body)
}

func assertContains(t *testing.T, body string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(body, w) {
			t.Errorf("body lacks %q", w)
		}
	}
}

func assertLacks(t *testing.T, body string, unwanted ...string) {
	t.Helper()
	for _, u := range unwanted {
		if strings.Contains(body, u) {
			t.Errorf("body has %q", u)
		}
	}
}

func TestIndexEmpty(t *testing.T) {
	h, _ := newTestUI(t)
	res, body := get(t, h, "/")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if csp := res.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
		t.Errorf("CSP = %q", csp)
	}
	if res.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing nosniff")
	}
	assertContains(t, body, "No job running", "No jobs yet.", "v1.2.3", "/static/js/htmx.min.js?v=1.2.3")
	assertLacks(t, body, "data-theme=")
}

func TestIndexShowsRunningQueuedAndHistory(t *testing.T) {
	ctx := context.Background()
	h, store := newTestUI(t)
	running := enqueue(t, store, "gitea", 1)
	done := enqueue(t, store, "gitea", 2)
	queued := enqueue(t, store, "gitea", 3)
	claim(t, store, running)
	claim(t, store, done)
	if _, err := store.Finish(ctx, done, jobs.Succeeded, []byte(`{"verdict":"approve","event":"approve","findings":1}`), ""); err != nil {
		t.Fatal(err)
	}
	if err := store.AddStep(ctx, running, jobs.Step{Role: "assistant", Content: "x", PromptTokens: 12_300}); err != nil {
		t.Fatal(err)
	}

	_, body := get(t, h, "/")
	assertContains(t, body,
		fmt.Sprintf(`<a href="/jobs/%d" class="block rounded-xl`, running), "Review of mycroft/app#1", "running for", "12.3k",
		"Queued", fmt.Sprintf(`#%d</span> Review mycroft/app#3`, queued),
		fmt.Sprintf(`data-href="/jobs/%d"`, done), "approve · 1 finding<",
		fmt.Sprintf(`hx-get="/fragments/live?seen=%d:running,%d:queued"`, running, queued),
	)
	assertLacks(t, body, "No job running")
}

func TestModels(t *testing.T) {
	ctx := context.Background()
	_, store := newTestUI(t)
	ui, err := New(store, Options{Models: []Model{{Name: "big", Concurrency: 1}, {Name: "small", Concurrency: 2}}, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	h := ui.Handler()
	running := enqueue(t, store, "gitea", 1)
	if got, err := store.Claim(ctx, jobs.ClaimSpec{Model: "big", Limit: 1}); err != nil || got.ID != running {
		t.Fatalf("Claim = %+v, %v", got, err)
	}

	// The live section shows each model's slots, a full one highlighted,
	// and the model of the running job.
	_, body := get(t, h, "/fragments/live")
	assertContains(t, body, `title="1 of 1 slots used"`, `big <span class="tabular-nums text-slate-500 dark:text-slate-400">1/1</span>`,
		`title="0 of 2 slots used"`, `<dt class="inline">model</dt> <dd class="inline">big</dd>`)
	if !strings.Contains(body, `border-amber-200 bg-amber-50/70 dark:border-amber-500/25 dark:bg-amber-500/5" title="1 of 1`) {
		t.Error("the full model is not highlighted")
	}
	_, body = get(t, h, fmt.Sprintf("/jobs/%d", running))
	assertContains(t, body, `Model</dt><dd>big</dd>`)

	// Without models, there is no slot line.
	h, _ = newTestUI(t)
	_, body = get(t, h, "/fragments/live")
	assertLacks(t, body, "Models</span>")
}

func TestLiveSignalsChanges(t *testing.T) {
	h, store := newTestUI(t)
	running := enqueue(t, store, "gitea", 1)
	queued := enqueue(t, store, "gitea", 2)
	claim(t, store, running)
	seen := fmt.Sprintf("%d:running,%d:queued", running, queued)

	res, body := get(t, h, "/fragments/live?seen=")
	if res.Header.Get("Hx-Trigger") != "jobs-changed" {
		t.Errorf("HX-Trigger = %q, want jobs-changed after a change", res.Header.Get("Hx-Trigger"))
	}
	assertContains(t, body, `<section id="live"`, "seen="+seen)
	assertLacks(t, body, "<html")

	res, _ = get(t, h, "/fragments/live?seen="+seen)
	if got := res.Header.Get("Hx-Trigger"); got != "" {
		t.Errorf("HX-Trigger = %q, want none while nothing changed", got)
	}
}

func TestHistoryPaging(t *testing.T) {
	h, store := newTestUI(t)
	var ids []int64
	for pr := 1; pr <= historyPageSize+5; pr++ {
		ids = append(ids, enqueue(t, store, "github", pr))
	}
	oldestShown := ids[5] // newest first: the first page ends 30 jobs down

	res, body := get(t, h, "/fragments/history")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	assertContains(t, body, `<section id="history"`, `<div id="load-more" class="flex justify-center">`,
		fmt.Sprintf(`hx-get="/fragments/history?before=%d"`, oldestShown))
	if n := strings.Count(body, "<tr data-href="); n != historyPageSize {
		t.Errorf("first page has %d rows, want %d", n, historyPageSize)
	}

	_, body = get(t, h, fmt.Sprintf("/fragments/history?before=%d", oldestShown))
	if n := strings.Count(body, "<tr data-href="); n != 5 {
		t.Errorf("second page has %d rows, want 5", n)
	}
	// The appended rows move the button out of band; at the end, it goes.
	assertContains(t, body, `<div id="load-more" hx-swap-oob="true"`)
	assertLacks(t, body, "<section", "Load older jobs")

	for _, bad := range []string{"abc", "0", "-3"} {
		if res, _ := get(t, h, "/fragments/history?before="+bad); res.StatusCode != http.StatusBadRequest {
			t.Errorf("before=%s: status = %d, want 400", bad, res.StatusCode)
		}
	}
}

func addSteps(t *testing.T, store *jobs.Store, id int64) {
	t.Helper()
	for _, st := range []jobs.Step{
		{Role: "system", Content: "You review pull requests."},
		{Role: "user", Content: "Review PR #1."},
		{Role: "assistant", Content: "<reasoning>\nLook at main.go first.\n</reasoning>\nChecking.\n→ read_file({\"path\":\"main.go\"})", PromptTokens: 900, CompletionTokens: 40},
		{Role: "tool", ToolName: "read_file", Content: "1\tpackage main"},
	} {
		if err := store.AddStep(context.Background(), id, st); err != nil {
			t.Fatal(err)
		}
	}
}

func TestJobPage(t *testing.T) {
	ctx := context.Background()
	h, store := newTestUI(t)
	id := enqueue(t, store, "gitea", 7)
	claim(t, store, id)
	addSteps(t, store, id)

	res, body := get(t, h, fmt.Sprintf("/jobs/%d", id))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	assertContains(t, body,
		`href="https://git.example/mycroft/app/pulls/7"`, "gitea-actions", "cccccc",
		"System prompt", "Request", "Model", "900 in · 40 out", "Look at main.go first.", "Checking.",
		`→ read_file({&#34;path&#34;:&#34;main.go&#34;})`, ">read_file</span>", "package main",
		fmt.Sprintf(`hx-get="/fragments/jobs/%d/steps?after=4"`, id),
	)

	if _, err := store.Finish(ctx, id, jobs.Failed, nil, "boom"); err != nil {
		t.Fatal(err)
	}
	_, body = get(t, h, fmt.Sprintf("/jobs/%d", id))
	assertContains(t, body, "boom")
	assertLacks(t, body, `id="poller"`)
}

func TestJobPageWithoutSteps(t *testing.T) {
	h, store := newTestUI(t)
	id := enqueue(t, store, "github", 8)
	_, body := get(t, h, fmt.Sprintf("/jobs/%d", id))
	assertContains(t, body, "No step yet.", `id="poller"`, `href="https://github.com/mycroft/app/pull/8"`)
}

func TestStepsFragment(t *testing.T) {
	h, store := newTestUI(t)
	id := enqueue(t, store, "gitea", 7)
	claim(t, store, id)
	addSteps(t, store, id)

	_, body := get(t, h, fmt.Sprintf("/fragments/jobs/%d/steps?after=2", id))
	assertContains(t, body, `id="step-3"`, `id="step-4"`, `<header id="job-header" hx-swap-oob="true"`,
		fmt.Sprintf(`hx-get="/fragments/jobs/%d/steps?after=4"`, id))
	assertLacks(t, body, `id="step-2"`, "<html", `id="no-steps"`)

	// The first steps replace the placeholder.
	_, body = get(t, h, fmt.Sprintf("/fragments/jobs/%d/steps?after=0", id))
	assertContains(t, body, `<p id="no-steps" hx-swap-oob="delete">`, `id="step-1"`)

	// Nothing new: the poller asks again from the same place.
	_, body = get(t, h, fmt.Sprintf("/fragments/jobs/%d/steps?after=4", id))
	assertContains(t, body, `steps?after=4"`)
	assertLacks(t, body, "<article")

	for _, bad := range []string{"", "x", "-1"} {
		if res, _ := get(t, h, fmt.Sprintf("/fragments/jobs/%d/steps?after=%s", id, bad)); res.StatusCode != http.StatusBadRequest {
			t.Errorf("after=%q: status = %d, want 400", bad, res.StatusCode)
		}
	}
}

func TestJobNotFound(t *testing.T) {
	h, _ := newTestUI(t)
	res, body := get(t, h, "/jobs/999")
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", res.StatusCode)
	}
	assertContains(t, body, "No such job")
	if res, _ := get(t, h, "/fragments/jobs/999/steps?after=0"); res.StatusCode != http.StatusNotFound {
		t.Errorf("steps of a missing job: status = %d, want 404", res.StatusCode)
	}
	if res, _ := get(t, h, "/jobs/abc"); res.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", res.StatusCode)
	}
	if res, _ := get(t, h, "/nope"); res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown path: status = %d, want 404", res.StatusCode)
	}
}

func TestThemeCookie(t *testing.T) {
	h, _ := newTestUI(t)
	_, body := get(t, h, "/", &http.Cookie{Name: "theme", Value: "dark"})
	assertContains(t, body, `<html lang="en" class="h-full" data-theme="dark">`)
	_, body = get(t, h, "/", &http.Cookie{Name: "theme", Value: `"><script>`})
	assertLacks(t, body, "data-theme=")
}

func TestStaticAndHealth(t *testing.T) {
	h, _ := newTestUI(t)
	res, body := get(t, h, "/static/js/app.js")
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "data-theme-toggle") {
		t.Errorf("app.js: status = %d", res.StatusCode)
	}
	if cc := res.Header.Get("Cache-Control"); !strings.Contains(cc, "max-age") {
		t.Errorf("Cache-Control = %q", cc)
	}
	if res, body := get(t, h, "/healthz"); res.StatusCode != http.StatusOK || body != "ok\n" {
		t.Errorf("healthz = %d %q", res.StatusCode, body)
	}
}

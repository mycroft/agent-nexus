package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mycroft/agent-nexus/internal/auth"
	"github.com/mycroft/agent-nexus/internal/jobs"
)

var token = strings.Repeat("r", 32)

type controller struct {
	queue *jobs.Queue
	store *jobs.Store
	srv   *httptest.Server
	done  chan struct{}
}

// newController serves the runner API over a queue whose model "gpu"
// runs one "k" job at a time, on runners only.
func newController(t *testing.T) *controller {
	t.Helper()
	store, err := jobs.OpenStore(context.Background(), filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	log := slog.New(slog.DiscardHandler)
	queue := jobs.NewQueue(store, []jobs.Model{{Name: "gpu", Concurrency: 1, Kinds: []string{"k", "other"}}}, 10*time.Minute, time.Hour, log)
	tokens, err := auth.ParseKeys("homelab="+token, 0)
	if err != nil {
		t.Fatal(err)
	}
	c := &controller{queue: queue, store: store, done: make(chan struct{})}
	s := &Server{Queue: queue, Tokens: tokens, Timeout: 10 * time.Minute, Version: "1.0.0", Done: c.done, Logger: log, pollWait: 200 * time.Millisecond}
	c.srv = httptest.NewServer(s.Handler())
	t.Cleanup(c.srv.Close)
	return c
}

func (c *controller) call(t *testing.T, path string, body any) (int, []byte) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, c.srv.URL+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := c.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(res.Body)
	return res.StatusCode, out
}

func (c *controller) enqueue(t *testing.T, group, dedupe string) jobs.Job {
	t.Helper()
	j, _, err := c.queue.Enqueue(context.Background(), jobs.NewJob{Kind: "k", GroupKey: group, DedupeKey: dedupe, Payload: map[string]string{"group": group}})
	if err != nil {
		t.Fatal(err)
	}
	return j
}

var claimGPU = ClaimRequest{Protocol: Protocol, Version: "1.0.0", Runner: "box-1", Model: "gpu", Kinds: []string{"k"}}

func (c *controller) claim(t *testing.T) Lease {
	t.Helper()
	code, body := c.call(t, "/v1/runner/claim", claimGPU)
	if code != http.StatusOK {
		t.Fatalf("claim = %d %s", code, body)
	}
	var l Lease
	if err := json.Unmarshal(body, &l); err != nil {
		t.Fatal(err)
	}
	return l
}

func TestClaimAndFinish(t *testing.T) {
	c := newController(t)
	ctx := context.Background()

	// No job: the poll ends empty.
	if code, _ := c.call(t, "/v1/runner/claim", claimGPU); code != http.StatusNoContent {
		t.Fatalf("empty claim = %d", code)
	}

	j := c.enqueue(t, "pr1", "pr1@a")
	l := c.claim(t)
	if l.Job.ID != j.ID || l.Lease == "" || l.TimeoutSeconds != 600 || l.HeartbeatSeconds != 15 ||
		l.Job.Runner != "homelab/box-1" || l.Job.Model != "gpu" || string(l.Job.Payload) != `{"group":"pr1"}` {
		t.Fatalf("lease = %+v", l)
	}
	// The model runs one job at a time, across runners.
	c.enqueue(t, "pr2", "pr2@a")
	if code, _ := c.call(t, "/v1/runner/claim", claimGPU); code != http.StatusNoContent {
		t.Errorf("claim beyond the model's limit = %d", code)
	}

	id := "/v1/runner/jobs/" + jsonID(j.ID)
	if code, body := c.call(t, id+"/heartbeat", leaseRequest{Lease: l.Lease}); code != http.StatusNoContent {
		t.Errorf("heartbeat = %d %s", code, body)
	}
	if code, body := c.call(t, id+"/steps", stepRequest{Lease: l.Lease, Step: jobs.Step{Role: "assistant", Content: "hi", PromptTokens: 10}}); code != http.StatusNoContent {
		t.Errorf("step = %d %s", code, body)
	}
	if code, body := c.call(t, id+"/checkpoint", checkpointRequest{Lease: l.Lease, Data: json.RawMessage(`{"draft":1}`)}); code != http.StatusNoContent {
		t.Errorf("checkpoint = %d %s", code, body)
	}
	if code, body := c.call(t, id+"/steps", stepRequest{Lease: "stale", Step: jobs.Step{Role: "user"}}); code != http.StatusConflict {
		t.Errorf("step with a stale lease = %d %s", code, body)
	}
	if code, body := c.call(t, id+"/finish", finishRequest{Lease: l.Lease, Status: jobs.Running}); code != http.StatusBadRequest {
		t.Errorf("finish as running = %d %s", code, body)
	}
	code, body := c.call(t, id+"/finish", finishRequest{Lease: l.Lease, Status: jobs.Succeeded, Result: json.RawMessage(`{"ok":true}`)})
	if code != http.StatusOK || !strings.Contains(string(body), `"status":"succeeded"`) {
		t.Errorf("finish = %d %s", code, body)
	}
	got, _ := c.store.Get(ctx, j.ID)
	if got.Status != jobs.Succeeded || string(got.Result) != `{"ok":true}` || string(got.Checkpoint) != `{"draft":1}` {
		t.Errorf("finished job = %+v", got)
	}
	if steps, _ := c.store.Steps(ctx, j.ID); len(steps) != 1 || steps[0].Content != "hi" || steps[0].PromptTokens != 10 {
		t.Errorf("steps = %+v", steps)
	}
	// Finishing released the job: the runner holds it no more.
	if code, _ := c.call(t, id+"/heartbeat", leaseRequest{Lease: l.Lease}); code != http.StatusConflict {
		t.Errorf("heartbeat after finish = %d", code)
	}
	// And the model's slot went to the next job.
	if next := c.claim(t); next.Job.GroupKey != "pr2" {
		t.Errorf("next claim = %+v", next.Job)
	}
}

func jsonID(id int64) string { b, _ := json.Marshal(id); return string(b) }

func TestClaimWaitsForAJob(t *testing.T) {
	c := newController(t)
	codes := make(chan int, 1)
	go func() {
		code, _ := c.call(t, "/v1/runner/claim", claimGPU)
		codes <- code
	}()
	time.Sleep(50 * time.Millisecond)
	c.enqueue(t, "pr1", "pr1@a")
	select {
	case code := <-codes:
		if code != http.StatusOK {
			t.Errorf("claim = %d, want the job enqueued while it waited", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the claim did not wake up")
	}
}

func TestClaimEndsAtShutdown(t *testing.T) {
	c := newController(t)
	codes := make(chan int, 1)
	go func() {
		code, _ := c.call(t, "/v1/runner/claim", claimGPU)
		codes <- code
	}()
	time.Sleep(20 * time.Millisecond)
	close(c.done)
	if code := <-codes; code != http.StatusNoContent {
		t.Errorf("claim at shutdown = %d", code)
	}
}

func TestClaimRefusals(t *testing.T) {
	c := newController(t)
	for name, req := range map[string]ClaimRequest{
		"protocol": {Protocol: Protocol + 1, Runner: "box", Model: "gpu", Kinds: []string{"k"}},
		"model":    {Protocol: Protocol, Runner: "box", Model: "cpu", Kinds: []string{"k"}},
		"runner":   {Protocol: Protocol, Runner: "a box", Model: "gpu", Kinds: []string{"k"}},
	} {
		if code, body := c.call(t, "/v1/runner/claim", req); code != http.StatusBadRequest {
			t.Errorf("%s: claim = %d %s, want 400", name, code, body)
		}
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, c.srv.URL+"/v1/runner/claim", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+strings.Repeat("x", 32))
	res, err := c.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("claim with a wrong token = %d", res.StatusCode)
	}
}

func TestSupersededJob(t *testing.T) {
	c := newController(t)
	j := c.enqueue(t, "pr1", "pr1@a")
	l := c.claim(t)
	c.enqueue(t, "pr1", "pr1@b")

	// The runner learns at its next heartbeat that it must stop, and
	// finishing then only releases the job.
	id := "/v1/runner/jobs/" + jsonID(j.ID)
	if code, body := c.call(t, id+"/heartbeat", leaseRequest{Lease: l.Lease}); code != http.StatusConflict || !strings.Contains(string(body), "no longer running") {
		t.Errorf("heartbeat = %d %s", code, body)
	}
	code, body := c.call(t, id+"/finish", finishRequest{Lease: l.Lease, Status: jobs.Failed, Error: "context canceled"})
	if code != http.StatusOK || !strings.Contains(string(body), `"status":"superseded"`) {
		t.Errorf("finish = %d %s", code, body)
	}
	if next := c.claim(t); next.Job.DedupeKey != "pr1@b" {
		t.Errorf("next claim = %+v", next.Job)
	}
}

func TestReleaseAndCheckpointResume(t *testing.T) {
	c := newController(t)
	j := c.enqueue(t, "pr1", "pr1@a")
	l := c.claim(t)
	id := "/v1/runner/jobs/" + jsonID(j.ID)
	if code, _ := c.call(t, id+"/checkpoint", checkpointRequest{Lease: l.Lease, Data: json.RawMessage(`{"draft":1}`)}); code != http.StatusNoContent {
		t.Fatalf("checkpoint = %d", code)
	}
	// The runner shuts down: the job is requeued at once, and handed back
	// with its checkpoint, as posting.
	if code, _ := c.call(t, id+"/release", leaseRequest{Lease: l.Lease}); code != http.StatusNoContent {
		t.Fatalf("release = %d", code)
	}
	again := c.claim(t)
	if again.Job.ID != j.ID || string(again.Checkpoint) != `{"draft":1}` || again.Job.Status != jobs.Posting || again.Lease == l.Lease {
		t.Errorf("claim after release = %+v", again)
	}
}

func TestEnqueue(t *testing.T) {
	c := newController(t)
	req := enqueueRequest{Kind: "k", GroupKey: "pr1", DedupeKey: "pr1@forced@1", APIKeyName: "discussion", Payload: json.RawMessage(`{"pr":1,"force":true}`)}
	code, body := c.call(t, "/v1/runner/enqueue", req)
	var res enqueueResponse
	if code != http.StatusOK || json.Unmarshal(body, &res) != nil || !res.Created || string(res.Job.Payload) != `{"pr":1,"force":true}` {
		t.Fatalf("enqueue = %d %s", code, body)
	}
	// A retry of the same request finds the job.
	code, body = c.call(t, "/v1/runner/enqueue", req)
	var again enqueueResponse
	if code != http.StatusOK || json.Unmarshal(body, &again) != nil || again.Created || again.Job.ID != res.Job.ID {
		t.Errorf("enqueue again = %d %s", code, body)
	}
	req.Kind = "unknown"
	req.DedupeKey = "x"
	if code, _ := c.call(t, "/v1/runner/enqueue", req); code != http.StatusBadRequest {
		t.Errorf("enqueue of an unknown kind = %d", code)
	}
}

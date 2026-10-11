package tools

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mycroft/agent-nexus/internal/remote"
)

func webTools(t *testing.T, policy WebPolicy) (map[string]Tool, *httptest.Server, *atomic.Int32) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /docs/upgrade", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<title>Upgrade</title><h2>3.0</h2><p>auth moved</p>")
	})
	// ghcr.io images resolve to a fake GitHub API here.
	mux.HandleFunc("GET /repos/o/app/releases", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[{"tag_name":"v3.0.0","body":"Breaking: auth moved"},{"tag_name":"v2.0.0","body":"old"}]`)
	})
	registryHits := &atomic.Int32{}
	mux.HandleFunc("GET /v2/", func(w http.ResponseWriter, r *http.Request) {
		registryHits.Add(1)
		http.NotFound(w, r) // no labels: the ghcr.io name is the fallback
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	// Every host, ghcr.io included, is served by srv.
	client := &http.Client{Transport: toServer{srv}}
	c := remote.New(client, remote.WithGitHub(srv.URL, srv.URL+"/raw"))
	byName := map[string]Tool{}
	for _, tool := range Web(c, policy) {
		byName[tool.Name] = tool
	}
	return byName, srv, registryHits
}

// toServer sends every request to one test server, whatever its host.
type toServer struct{ srv *httptest.Server }

func (s toServer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.URL.Host = strings.TrimPrefix(s.srv.URL, "https://")
	return s.srv.Client().Transport.RoundTrip(r)
}

func TestWebTools(t *testing.T) {
	allow := func(context.Context, string) error { return nil }
	tools, srv, registryHits := webTools(t, WebPolicy{Page: allow, Image: allow})

	out, err := call(t, tools["fetch_url"], map[string]any{"url": srv.URL + "/docs/upgrade"})
	if err != nil || out != "Title: Upgrade\nURL: "+srv.URL+"/docs/upgrade\n\n    1  ## 3.0\n    2  auth moved\n" {
		t.Errorf("fetch_url = %q, %v", out, err)
	}

	// ghcr.io/o/app has no labels: its ghcr.io name identifies the project.
	out, err = call(t, tools["release_notes"], map[string]any{"image": "ghcr.io/o/app", "from_version": "2.0.0", "to_version": "3.0.0"})
	if err != nil || !strings.Contains(out, "Releases of github.com/o/app after 2.0.0, up to 3.0.0:\n\n## v3.0.0\n\nBreaking: auth moved") || strings.Contains(out, "old") {
		t.Errorf("release_notes = %q, %v", out, err)
	}
	if registryHits.Load() == 0 {
		t.Error("the registry was never asked for the image labels")
	}
	out, err = call(t, tools["release_notes"], map[string]any{"image": "ghcr.io/o/app", "from_version": "3.0.0", "to_version": "3.1.0"})
	if err != nil || !strings.Contains(out, "has no release after 3.0.0 up to 3.1.0; its latest release tags: v3.0.0, v2.0.0") {
		t.Errorf("no release in range = %q, %v", out, err)
	}
	if _, err := call(t, tools["release_notes"], map[string]any{"image": "ghcr.io/o/app", "from_version": "2.0.0", "to_version": "3.0.0/../x"}); err == nil {
		t.Error("an invalid tag must be refused")
	}
}

func TestWebToolsAskThePolicy(t *testing.T) {
	deny := func(context.Context, string) error { return errors.New("not referenced") }
	tools, srv, _ := webTools(t, WebPolicy{Page: deny, Image: deny})
	for name, args := range map[string]map[string]any{
		"fetch_url":     {"url": srv.URL + "/docs/upgrade"},
		"release_notes": {"image": "ghcr.io/o/app", "from_version": "1", "to_version": "2"},
	} {
		if _, err := call(t, tools[name], args); err == nil || err.Error() != "not referenced" {
			t.Errorf("%s: err = %v, want the policy's", name, err)
		}
	}
}

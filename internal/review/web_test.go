package review

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/mycroft/agent-nexus/internal/charts"
	"github.com/mycroft/agent-nexus/internal/remote"
)

func TestWebPolicies(t *testing.T) {
	f := newFixtureWith(t,
		map[string]string{"versions.yaml": "images:\n  ghcr.io/gethomepage/homepage: v2.4\n  jhaals/yopass: 15.0.0\n",
			"repos.go": `const prom = "https://prometheus-community.github.io/helm-charts"` + "\n"},
		map[string]string{"new.md": "See https://evil.example/upgrade and image evil/app:1\n"})
	f.reviewer.Remote = remote.New(nil)
	ctx := context.Background()
	pr, _ := f.forge.GetPR(ctx, f.req.repo(), 4)
	ws, _, err := f.reviewer.prepare(ctx, f.forge, f.req.repo(), pr, f.remote.HeadSHA)
	if err != nil {
		t.Fatal(err)
	}
	for url, want := range map[string]string{
		"https://prometheus-community.github.io/helm-charts/upgrade.html": "",
		"https://community.github.io/x":                                   "does not appear", // a suffix of a referenced host
		"https://evil.example/upgrade":                                    "does not appear", // only at the head
		"https://prometheus-community.github.io/x?leak=1":                 "query strings are not allowed",
		"http://prometheus-community.github.io/x":                         "want https://",
	} {
		err := ws.pagePolicy(ctx, url)
		if (want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), want)) {
			t.Errorf("pagePolicy(%q) = %v, want %q", url, err, want)
		}
	}
	for image, want := range map[string]string{
		"ghcr.io/gethomepage/homepage": "",
		"jhaals/yopass":                "",
		"haals/yopass":                 "does not appear",
		"gethomepage/homepage":         "does not appear", // the Docker Hub name is another image
		"evil/app":                     "does not appear",
		"jhaals/yopass:15.0.0":         "invalid image name",
	} {
		err := ws.imagePolicy(ctx, image)
		if (want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), want)) {
			t.Errorf("imagePolicy(%q) = %v, want %q", image, err, want)
		}
	}
	ws.fetchHosts = []string{"evil.example", "registry-1.docker.io"}
	if err := ws.pagePolicy(ctx, "https://evil.example/upgrade"); err != nil {
		t.Errorf("an allowed page host: %v", err)
	}
	if err := ws.imagePolicy(ctx, "evil/app"); err != nil {
		t.Errorf("an image on an allowed registry: %v", err)
	}
	if !strings.Contains(ws.toolsSentence(), "fetch_url reads documentation pages") {
		t.Error("the tools sentence does not mention the web tools")
	}
}

func TestBareWorkspace(t *testing.T) {
	f := newFixture(t)
	f.reviewer.Remote = remote.New(nil)
	fetcher, err := charts.New(t.TempDir(), f.reviewer.Remote)
	if err != nil {
		t.Fatal(err)
	}
	f.reviewer.Charts = fetcher

	// Without allowed hosts, nothing can be read.
	ws := f.reviewer.bareWorkspace()
	if got := ws.tools(); len(got) != 0 || ws.toolsSentence() != "" {
		t.Errorf("tools = %d, sentence %q, want none", len(got), ws.toolsSentence())
	}

	f.reviewer.FetchHosts = []string{"docs.example.com", "ghcr.io"}
	ws = f.reviewer.bareWorkspace()
	var names []string
	for _, tool := range ws.tools() {
		names = append(names, tool.Name)
	}
	for _, want := range []string{"chart_files", "chart_read", "chart_diff", "fetch_url", "release_notes"} {
		if !slices.Contains(names, want) {
			t.Errorf("tools = %v, lack %s", names, want)
		}
	}
	for _, unwanted := range []string{"read_file", "repo_read"} {
		if slices.Contains(names, unwanted) {
			t.Errorf("tools = %v, want no %s", names, unwanted)
		}
	}
	if s := ws.toolsSentence(); !strings.Contains(s, "from these hosts only: docs.example.com, ghcr.io.") {
		t.Errorf("tools sentence = %q", s)
	}

	ctx := context.Background()
	if err := ws.pagePolicy(ctx, "https://docs.example.com/upgrade"); err != nil {
		t.Errorf("an allowed page host: %v", err)
	}
	if err := ws.imagePolicy(ctx, "ghcr.io/gethomepage/homepage"); err != nil {
		t.Errorf("an image on an allowed registry: %v", err)
	}
	for name, err := range map[string]error{
		"page":  ws.pagePolicy(ctx, "https://evil.example/upgrade"),
		"image": ws.imagePolicy(ctx, "evil/app"),
		"chart": ws.chartPolicy(ctx, "https://evil.example/charts"),
	} {
		if err == nil || !strings.Contains(err.Error(), "without a repository, only") {
			t.Errorf("%s policy = %v, want a refusal", name, err)
		}
	}
}

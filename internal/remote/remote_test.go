package remote

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPublic(t *testing.T) {
	for addr, want := range map[string]bool{
		"51.15.191.121": true, "2001:bc8:6005:122::1": true, "185.199.108.153": true,
		"127.0.0.1": false, "::1": false, "10.43.0.1": false, "10.42.3.188": false, "192.168.1.1": false,
		"172.16.0.1": false, "169.254.169.254": false, "100.64.0.1": false, "0.0.0.0": false,
		"fd00::1": false, "fe80::1": false, "::ffff:10.0.0.1": false, "64:ff9b::a00:1": false, "224.0.0.1": false,
	} {
		if got := Public(netip.MustParseAddr(addr)); got != want {
			t.Errorf("Public(%s) = %v, want %v", addr, got, want)
		}
	}
}

func TestClientRefusesPrivateAddresses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the request reached a loopback server")
	}))
	defer srv.Close()
	_, err := New(nil).Get(context.Background(), Request{URL: srv.URL, Limit: 10})
	if !errors.Is(err, ErrNotPublic) {
		t.Errorf("err = %v, want ErrNotPublic", err)
	}
}

// testClient talks to srv for every host it is asked about.
func testClient(srv *httptest.Server) *Client {
	return New(srv.Client(), WithGitHub(srv.URL, srv.URL+"/raw"))
}

func TestParseImage(t *testing.T) {
	for image, want := range map[string]string{
		"ghcr.io/gethomepage/homepage":      "ghcr.io gethomepage/homepage",
		"jhaals/yopass":                     "registry-1.docker.io jhaals/yopass",
		"nginx":                             "registry-1.docker.io library/nginx",
		"docker.io/nginx":                   "registry-1.docker.io library/nginx",
		"docker.io/linuxserver/heimdall":    "registry-1.docker.io linuxserver/heimdall",
		"registry.mkz.me:5000/mycroft/app":  "registry.mkz.me:5000 mycroft/app",
		"registry.gitlab.com/timvisee/send": "registry.gitlab.com timvisee/send",
	} {
		host, repo, err := ParseImage(image)
		if err != nil || host+" "+repo != want {
			t.Errorf("ParseImage(%q) = %q %q, %v; want %q", image, host, repo, err, want)
		}
	}
	for _, bad := range []string{"jhaals/yopass:15.0.0", "Upper/case", "../etc", "a//b", "evil.com/x?y", ""} {
		if _, _, err := ParseImage(bad); err == nil {
			t.Errorf("ParseImage(%q) accepted", bad)
		}
	}
}

func TestImageLabels(t *testing.T) {
	config := `{"config":{"Labels":{"org.opencontainers.image.source":"https://github.com/gethomepage/homepage"}}}`
	sum := sha256.Sum256([]byte(config))
	configDigest := "sha256:" + hex.EncodeToString(sum[:])
	amd64 := "sha256:" + strings.Repeat("a", 64)
	var tokens atomic.Int32
	var srv *httptest.Server
	mux := http.NewServeMux()
	auth := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") == "Bearer anon" {
			return true
		}
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="ghcr.io",scope="repository:o/app:pull"`, srv.URL))
		w.WriteHeader(http.StatusUnauthorized)
		return false
	}
	mux.HandleFunc("GET /token", func(w http.ResponseWriter, _ *http.Request) {
		tokens.Add(1)
		_, _ = io.WriteString(w, `{"token":"anon"}`)
	})
	mux.HandleFunc("GET /v2/o/app/manifests/{ref}", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		switch r.PathValue("ref") {
		case "v2.5":
			fmt.Fprintf(w, `{"manifests":[{"digest":"sha256:%s","platform":{"os":"linux","architecture":"arm64"}},{"digest":"%s","platform":{"os":"linux","architecture":"amd64"}}]}`, strings.Repeat("b", 64), amd64)
		case amd64:
			fmt.Fprintf(w, `{"config":{"digest":"%s"}}`, configDigest)
		default:
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("GET /v2/o/app/blobs/{digest}", func(w http.ResponseWriter, r *http.Request) {
		if auth(w, r) && r.PathValue("digest") == configDigest {
			_, _ = io.WriteString(w, config)
		}
	})
	srv = httptest.NewTLSServer(mux)
	defer srv.Close()
	c := testClient(srv)
	image := strings.TrimPrefix(srv.URL, "https://") + "/o/app"

	labels, err := c.ImageLabels(context.Background(), image, "v2.5")
	if err != nil {
		t.Fatal(err)
	}
	if labels[SourceLabel] != "https://github.com/gethomepage/homepage" {
		t.Errorf("labels = %v", labels)
	}
	if tokens.Load() != 1 {
		t.Errorf("fetched %d tokens, want 1 reused across the requests", tokens.Load())
	}
	if _, err := c.ImageLabels(context.Background(), image, "v9"); !IsNotFound(err) {
		t.Errorf("missing tag: err = %v, want a 404", err)
	}
	if _, err := c.ImageLabels(context.Background(), image, "v2.5/../x"); err == nil {
		t.Error("an invalid tag must be refused")
	}
}

func TestGitHubRepo(t *testing.T) {
	for source, want := range map[string]string{
		"https://github.com/gethomepage/homepage":             "gethomepage/homepage",
		"https://github.com/linuxserver/docker-bookstack.git": "linuxserver/docker-bookstack",
		"git@github.com:jhaals/yopass.git":                    "jhaals/yopass",
		"https://github.com/o/r/tree/main/sub":                "o/r",
		"https://gitlab.com/timvisee/send":                    "",
		"https://github.com/onlyowner":                        "",
	} {
		owner, repo, ok := GitHubRepo(source)
		if got := owner + "/" + repo; ok != (want != "") || (ok && got != want) {
			t.Errorf("GitHubRepo(%q) = %q, %v; want %q", source, got, ok, want)
		}
	}
}

func TestReleases(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/gethomepage/homepage/releases" || r.Header.Get("Authorization") != "" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `[
			{"tag_name":"v2.6.0-rc1","prerelease":true,"body":"rc"},
			{"tag_name":"v2.5.1","body":"next"},
			{"tag_name":"v2.5.0","name":"Homepage 2.5","body":"big release","published_at":"2026-10-01T00:00:00Z"},
			{"tag_name":"v2.4.2","body":"fix"},
			{"tag_name":"v2.4.1","body":"fix 1"},
			{"tag_name":"v2.4.0","body":"base"},
			{"tag_name":"v2.3.9","draft":true,"body":"draft"}]`)
	}))
	defer srv.Close()
	c := testClient(srv)
	ctx := context.Background()

	got, latest, err := c.Releases(ctx, "gethomepage", "homepage", "v2.4", "v2.5")
	if err != nil {
		t.Fatal(err)
	}
	var tags []string
	for _, r := range got {
		tags = append(tags, r.Tag)
	}
	// "v2.4" is "v2.4.0" (excluded), "v2.5" is "v2.5.0" (included).
	if strings.Join(tags, ",") != "v2.5.0,v2.4.2,v2.4.1" || len(latest) != 6 {
		t.Errorf("releases = %v, latest = %v", tags, latest)
	}
	if got, _, _ := c.Releases(ctx, "gethomepage", "homepage", "v2.5.1", "v2.6.0-rc1"); len(got) != 1 || got[0].Tag != "v2.6.0-rc1" {
		t.Errorf("a pre-release target keeps pre-releases: %+v", got)
	}
	if _, _, err := c.Releases(ctx, "gethomepage", "homepage", "latest", "v2.5"); err == nil {
		t.Error("a version without numbers must be refused")
	}
	if _, _, err := c.Releases(ctx, "../x", "homepage", "1", "2"); err == nil {
		t.Error("an invalid owner must be refused")
	}

	text := FormatReleases(got, 1<<10, 4)
	for _, want := range []string{"## v2.5.0 — Homepage 2.5 (2026-10-01)\n\nbig \n[…release notes cut]", "## v2.4.2\n\nfix"} {
		if !strings.Contains(text, want) {
			t.Errorf("FormatReleases lacks %q:\n%s", want, text)
		}
	}
	if text := FormatReleases(got, 60, 100); !strings.Contains(text, "(left out for space: v2.4.2, v2.4.1)") {
		t.Errorf("over budget:\n%s", text)
	}
}

func TestVersionKey(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"v2.5", "v2.5.0", 0},
		{"v25.12.8-ls249", "v25.12.8-ls248", 1},
		{"kube-prometheus-stack-92.3.0", "92.2.0", 1},
		{"1.0.0rc1", "1.0.0", 0},
		{"source-1.2", "1.2", 0},
	} {
		if got := compareVersions(versionKey(tc.a), versionKey(tc.b)); (got > 0) != (tc.want > 0) || (got == 0) != (tc.want == 0) {
			t.Errorf("compare(%s, %s) = %d, want sign %d", tc.a, tc.b, got, tc.want)
		}
	}
	if isPrerelease("source-1.2") || !isPrerelease("v1.0.0-beta.2") || !isPrerelease("1.0.0rc1") {
		t.Error("isPrerelease misjudges")
	}
}

func TestFetchPage(t *testing.T) {
	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /docs/upgrade", func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<html><head><title>Upgrading</title><style>body{}</style></head><body>
<nav><a href="/">Home</a> <a href="/docs">Docs</a></nav>
<h1>Upgrade to 3.0</h1>
<p>The <code>auth.enabled</code> key   moved
 to <b>security.auth</b>.</p>
<ul><li>Removed <code>legacy</code></li><li><p>Renamed values</p></li></ul>
<p>Follow us on&nbsp;<a href="/t">Twitter</a>&nbsp;and <a href="/m">Mastodon</a>.</p>
<pre>helm upgrade x
  --version 3.0.0</pre>
<table><tr><th>Key</th><th>Default</th></tr><tr><td>replicas</td><td>2</td></tr></table>
<script>track()</script><footer>© someone</footer></body></html>`)
	})
	mux.HandleFunc("GET /raw/o/r/main/UPGRADE.md", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "# Upgrade\n\nRead this.\n")
	})
	mux.HandleFunc("GET /logo.png", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte{0x89, 'P', 'N', 'G'})
	})
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()
	c := testClient(srv)
	ctx := context.Background()

	page, err := c.FetchPage(ctx, srv.URL+"/docs/upgrade")
	if err != nil {
		t.Fatal(err)
	}
	want := "# Upgrade to 3.0\nThe `auth.enabled` key moved to security.auth.\n- Removed `legacy`\n- Renamed values\nFollow us on Twitter and Mastodon.\n```\nhelm upgrade x\n  --version 3.0.0\n```\n | Key | Default\n | replicas | 2\n"
	if page.Title != "Upgrading" || page.Text != want {
		t.Errorf("page = %q\n%s\nwant\n%s", page.Title, page.Text, want)
	}
	if _, err := c.FetchPage(ctx, srv.URL+"/docs/upgrade"); err != nil || hits.Load() != 1 {
		t.Errorf("a page read again within the TTL must come from the cache: %d hits, %v", hits.Load(), err)
	}
	c.now = func() time.Time { return time.Now().Add(2 * pageTTL) }
	if _, err := c.FetchPage(ctx, srv.URL+"/docs/upgrade"); err != nil || hits.Load() != 2 {
		t.Errorf("an expired page must be fetched again: %d hits, %v", hits.Load(), err)
	}

	page, err = c.FetchPage(ctx, "https://github.com/o/r/blob/main/UPGRADE.md")
	if err != nil || page.Text != "# Upgrade\n\nRead this.\n" || !strings.HasSuffix(page.URL, "/raw/o/r/main/UPGRADE.md") {
		t.Errorf("GitHub file = %+v, %v", page, err)
	}
	if _, err := c.FetchPage(ctx, srv.URL+"/logo.png"); err == nil || !strings.Contains(err.Error(), "not a text page") {
		t.Errorf("an image: err = %v", err)
	}
}

func TestPageHost(t *testing.T) {
	for raw, want := range map[string]string{
		"https://Artifacthub.io/packages/helm/x#values":   "artifacthub.io",
		"https://github.com/o/r/blob/main/UPGRADE.md":     "github.com",
		"http://example.com/":                             "",
		"https://example.com/?leak=1":                     "",
		"https://example.com/?":                           "",
		"https://user@example.com/":                       "",
		"https://example.com/" + strings.Repeat("a", 600): "",
	} {
		got, err := PageHost(raw)
		if (err != nil) != (want == "") || got != want {
			t.Errorf("PageHost(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
}

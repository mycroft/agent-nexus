package charts

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mycroft/agent-nexus/internal/remote"
)

// tgz builds a chart package: files under root/, and extra raw entries.
func tgz(t *testing.T, root string, files map[string]string, extra ...*tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: root + "/" + name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, content); err != nil {
			t.Fatal(err)
		}
	}
	for _, h := range extra {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			if _, err := tw.Write(bytes.Repeat([]byte("x"), int(h.Size))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestNormalizeRepoURL(t *testing.T) {
	for raw, want := range map[string]string{
		"https://charts.longhorn.io/":          "https://charts.longhorn.io",
		"oci://registry.mkz.me/mycroft/charts": "oci://registry.mkz.me/mycroft/charts",
		"http://charts.example.com":            "",
		"https://user:pw@charts.example.com":   "",
		"https://charts.example.com/?x=1":      "",
		"file:///etc":                          "",
		"oci:registry.mkz.me":                  "",
	} {
		got, _, err := NormalizeRepoURL(raw)
		if (err != nil) != (want == "") || got != want {
			t.Errorf("NormalizeRepoURL(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	if got, host, err := NormalizeRepoURL("  https://Charts.Example.com/stable/ \n"); err != nil || got != "https://Charts.Example.com/stable" || host != "Charts.Example.com" {
		t.Errorf("surrounding space: %q, %q, %v", got, host, err)
	}
}

func newFetcher(t *testing.T, srv *httptest.Server) *Fetcher {
	t.Helper()
	f, err := New(t.TempDir(), remote.New(srv.Client()))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestLoadFromHTTPRepository(t *testing.T) {
	sub := tgz(t, "redis", map[string]string{"Chart.yaml": "name: redis\n", "values.yaml": "port: 6379\n"})
	pkg1 := tgz(t, "demo", map[string]string{"Chart.yaml": "name: demo\nversion: 1.0.0\n", "values.yaml": "replicas: 1\n", "charts/redis-1.0.0.tgz": string(sub)},
		&tar.Header{Name: "demo/../../escape", Size: 1, Typeflag: tar.TypeReg},
		&tar.Header{Name: "demo/link", Linkname: "/etc/passwd", Typeflag: tar.TypeSymlink})
	pkg2 := tgz(t, "demo", map[string]string{"Chart.yaml": "name: demo\nversion: 1.1.0\n", "values.yaml": "replicas: 2\n"})
	var indexHits, pkgHits atomic.Int32
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("GET /charts/index.yaml", func(w http.ResponseWriter, _ *http.Request) {
		indexHits.Add(1)
		fmt.Fprintf(w, "apiVersion: v1\nentries:\n  demo:\n"+
			"  - version: 1.1.0\n    urls: [%s/dl/demo-1.1.0.tgz]\n    digest: %s\n"+
			"  - version: v1.0.0\n    urls: [pkgs/demo-1.0.0.tgz]\n    digest: %s\n"+
			"  - version: 0.9.0\n    urls: [pkgs/demo-0.9.0.tgz]\n    digest: %s\n",
			srv.URL, digest(pkg2), digest(pkg1), strings.Repeat("0", 64))
	})
	mux.HandleFunc("GET /charts/pkgs/demo-1.0.0.tgz", func(w http.ResponseWriter, _ *http.Request) {
		pkgHits.Add(1)
		_, _ = w.Write(pkg1)
	})
	mux.HandleFunc("GET /dl/demo-1.1.0.tgz", func(w http.ResponseWriter, _ *http.Request) {
		pkgHits.Add(1)
		_, _ = w.Write(pkg2)
	})
	mux.HandleFunc("GET /charts/pkgs/demo-0.9.0.tgz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(pkg1)
	})
	srv = httptest.NewTLSServer(mux)
	defer srv.Close()
	f := newFetcher(t, srv)
	ctx := context.Background()
	repo := srv.URL + "/charts/"

	c, err := f.Load(ctx, repo, "demo", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(c.Paths(), ","); got != "Chart.yaml,charts/redis/Chart.yaml,charts/redis/values.yaml,values.yaml" {
		t.Errorf("paths = %s (want the subchart unpacked, no escape or link)", got)
	}
	if string(c.Files["values.yaml"]) != "replicas: 1\n" {
		t.Errorf("values.yaml = %q", c.Files["values.yaml"])
	}
	// An absolute package URL, and the index reused within the hour.
	if c, err := f.Load(ctx, repo, "demo", "1.1.0"); err != nil || string(c.Files["values.yaml"]) != "replicas: 2\n" {
		t.Fatalf("Load 1.1.0 = %v", err)
	}
	// Cached packages need no request.
	if _, err := f.Load(ctx, repo, "demo", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	if indexHits.Load() != 1 || pkgHits.Load() != 2 {
		t.Errorf("index fetched %d times, packages %d times; want 1 and 2", indexHits.Load(), pkgHits.Load())
	}
	// A stale index is fetched again.
	f.now = func() time.Time { return time.Now().Add(2 * indexTTL) }
	if _, err := f.Load(ctx, repo, "demo", "0.9.0"); err == nil || !strings.Contains(err.Error(), "digest does not match") {
		t.Errorf("tampered package: err = %v", err)
	}
	if indexHits.Load() != 2 {
		t.Errorf("index fetched %d times after it went stale, want 2", indexHits.Load())
	}

	for _, tc := range []struct{ name, version, want string }{
		{"demo", "2.0.0", "recent versions: 1.1.0, v1.0.0, 0.9.0"},
		{"other", "1.0.0", "no chart other"},
		{"../demo", "1.0.0", "invalid chart name"},
		{"demo", "1.0.0/../x", "invalid chart version"},
	} {
		if _, err := f.Load(ctx, repo, tc.name, tc.version); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Load(%s, %s) err = %v, want %q", tc.name, tc.version, err, tc.want)
		}
	}
}

func TestLoadFromOCIRegistry(t *testing.T) {
	pkg := tgz(t, "demo", map[string]string{"Chart.yaml": "name: demo\n", "values.yaml": "a: 1\n"})
	var srv *httptest.Server
	mux := http.NewServeMux()
	authorized := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") == "Bearer tok" {
			return true
		}
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="reg",scope="repository:charts/demo:pull"`, srv.URL))
		w.WriteHeader(http.StatusUnauthorized)
		return false
	}
	mux.HandleFunc("GET /token", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("service") != "reg" || r.URL.Query().Get("scope") != "repository:charts/demo:pull" || r.Header.Get("Authorization") != "" {
			t.Errorf("token request %s with %q", r.URL, r.Header.Get("Authorization"))
		}
		_, _ = io.WriteString(w, `{"token":"tok"}`)
	})
	mux.HandleFunc("GET /v2/charts/demo/manifests/{tag}", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(w, r) {
			return
		}
		if r.PathValue("tag") != "1.0.0_build.1" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `{"layers":[{"mediaType":"application/vnd.cncf.helm.config.v1+json","digest":"sha256:%s","size":2},`+
			`{"mediaType":"%s","digest":"sha256:%s","size":%d}]}`, strings.Repeat("a", 64), helmLayerType, digest(pkg), len(pkg))
	})
	mux.HandleFunc("GET /v2/charts/demo/blobs/{digest}", func(w http.ResponseWriter, r *http.Request) {
		if authorized(w, r) && r.PathValue("digest") == "sha256:"+digest(pkg) {
			_, _ = w.Write(pkg)
		}
	})
	srv = httptest.NewTLSServer(mux)
	defer srv.Close()
	f := newFetcher(t, srv)
	repo := "oci://" + strings.TrimPrefix(srv.URL, "https://") + "/charts"

	c, err := f.Load(context.Background(), repo, "demo", "1.0.0+build.1")
	if err != nil {
		t.Fatal(err)
	}
	if string(c.Files["values.yaml"]) != "a: 1\n" {
		t.Errorf("values.yaml = %q", c.Files["values.yaml"])
	}
	if _, err := f.Load(context.Background(), repo, "demo", "9.9.9"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("missing tag: err = %v", err)
	}
}

func TestUnpackLimits(t *testing.T) {
	files := map[string]string{"a": "1", "b": "2", "c": "3"}
	if _, _, err := untar(tgz(t, "big", files), &budget{bytes: 100, files: 2}); err == nil {
		t.Error("too many files: want an error")
	}
	if _, _, err := untar(tgz(t, "big", files), &budget{bytes: 2, files: 10}); err == nil {
		t.Error("too many bytes: want an error")
	}
	if _, _, err := untar(tgz(t, "big", files), &budget{bytes: 3, files: 3}); err != nil {
		t.Errorf("within the budget: %v", err)
	}
	if _, err := unpack([]byte("not gzip")); err == nil {
		t.Error("garbage: want an error")
	}
}

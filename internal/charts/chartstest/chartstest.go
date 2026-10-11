// Package chartstest serves Helm chart repositories for tests.
package chartstest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Package builds a chart package holding files under name/.
func Package(t *testing.T, name string, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for p, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name + "/" + p, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
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

// NewRepo serves a classic Helm repository over TLS holding the versions
// of one chart, each a set of files. It returns the server, whose Client
// trusts it, and the repository URL.
func NewRepo(t *testing.T, name string, versions map[string]map[string]string) (*httptest.Server, string) {
	t.Helper()
	packages := map[string][]byte{}
	var index strings.Builder
	fmt.Fprintf(&index, "apiVersion: v1\nentries:\n  %s:\n", name)
	for version, files := range versions {
		pkg := Package(t, name, files)
		file := fmt.Sprintf("%s-%s.tgz", name, version)
		packages["/charts/"+file] = pkg
		sum := sha256.Sum256(pkg)
		fmt.Fprintf(&index, "  - version: %s\n    urls: [%s]\n    digest: %s\n", version, file, hex.EncodeToString(sum[:]))
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/charts/index.yaml" {
			_, _ = w.Write([]byte(index.String()))
			return
		}
		pkg, ok := packages[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(pkg)
	}))
	t.Cleanup(srv.Close)
	return srv, srv.URL + "/charts"
}

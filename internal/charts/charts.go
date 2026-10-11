// Package charts reads Helm chart packages from classic (HTTP) and OCI
// repositories, through remote.Client, so a review can see what a chart
// version change brings. Packages are cached on disk and read in memory:
// their paths never reach the filesystem.
package charts

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/mycroft/agent-nexus/internal/remote"
)

const (
	// maxIndexBytes bounds a repository index; prometheus-community's is
	// about 6.5 MB.
	maxIndexBytes    = 32 << 20
	maxPackageBytes  = 16 << 20
	maxManifestBytes = 1 << 20
	// maxChartBytes and maxChartFiles bound a package once unpacked,
	// subcharts included.
	maxChartBytes = 64 << 20
	maxChartFiles = 5000
	indexTTL      = time.Hour

	helmLayerType = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"
)

var (
	namePattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
	versionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)
)

// Chart is one version of a chart: its files, by path relative to the
// chart's directory. Subcharts shipped as archives are unpacked under
// charts/<name>/.
type Chart struct {
	Name    string
	Version string
	Files   map[string][]byte
}

// Paths returns the chart's file paths, sorted.
func (c *Chart) Paths() []string {
	out := make([]string, 0, len(c.Files))
	for p := range c.Files {
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}

// Fetcher downloads chart packages and caches them on disk. A published
// chart version does not change, so packages are kept for good; indexes
// are refreshed hourly.
type Fetcher struct {
	remote *remote.Client
	dir    string
	now    func() time.Time
	// mu serializes downloads, so concurrent reads of one chart fetch it
	// once.
	mu sync.Mutex
}

// New returns a fetcher caching under dir.
func New(dir string, client *remote.Client) (*Fetcher, error) {
	for _, sub := range []string{"packages", "index"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o750); err != nil {
			return nil, fmt.Errorf("chart cache: %w", err)
		}
	}
	return &Fetcher{remote: client, dir: dir, now: time.Now}, nil
}

// NormalizeRepoURL checks a chart repository URL, https:// or oci://, and
// returns it without a trailing slash, with its host.
func NormalizeRepoURL(raw string) (string, string, error) {
	s := strings.TrimRight(strings.TrimSpace(raw), "/")
	u, err := url.Parse(s)
	if err != nil {
		return "", "", fmt.Errorf("invalid repository URL: %w", err)
	}
	switch {
	case u.Scheme != "https" && u.Scheme != "oci":
		return "", "", fmt.Errorf("repository URL %q: want https:// or oci://", raw)
	case u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "":
		return "", "", fmt.Errorf("repository URL %q: want scheme://host/path only", raw)
	}
	return s, u.Hostname(), nil
}

// Load returns a chart version from a repository.
func (f *Fetcher) Load(ctx context.Context, repoURL, name, version string) (*Chart, error) {
	repo, _, err := NormalizeRepoURL(repoURL)
	if err != nil {
		return nil, err
	}
	if !namePattern.MatchString(name) {
		return nil, fmt.Errorf("invalid chart name %q", name)
	}
	if !versionPattern.MatchString(version) {
		return nil, fmt.Errorf("invalid chart version %q", version)
	}
	data, err := f.cachedPackage(ctx, repo, name, version)
	if err != nil {
		return nil, err
	}
	files, err := unpack(data)
	if err != nil {
		return nil, fmt.Errorf("chart %s %s: %w", name, version, err)
	}
	return &Chart{Name: name, Version: version, Files: files}, nil
}

func (f *Fetcher) cachedPackage(ctx context.Context, repo, name, version string) ([]byte, error) {
	sum := sha256.Sum256([]byte(repo + "\x00" + name + "\x00" + version))
	file := filepath.Join(f.dir, "packages", hex.EncodeToString(sum[:])+".tgz")
	f.mu.Lock()
	defer f.mu.Unlock()
	data, err := os.ReadFile(file)
	if err == nil {
		return data, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("chart cache: %w", err)
	}
	if strings.HasPrefix(repo, "oci://") {
		data, err = f.ociPackage(ctx, repo, name, version)
	} else {
		data, err = f.httpPackage(ctx, repo, name, version)
	}
	if err != nil {
		return nil, err
	}
	if err := writeFile(file, data); err != nil {
		return nil, err
	}
	return data, nil
}

// indexEntry is a chart version in a repository index.
type indexEntry struct {
	Version string   `yaml:"version"`
	URLs    []string `yaml:"urls"`
	Digest  string   `yaml:"digest"`
}

func (f *Fetcher) httpPackage(ctx context.Context, repo, name, version string) ([]byte, error) {
	entries, err := f.indexEntries(ctx, repo, name)
	if err != nil {
		return nil, err
	}
	var entry *indexEntry
	for i, e := range entries {
		if e.Version == version || strings.TrimPrefix(e.Version, "v") == strings.TrimPrefix(version, "v") {
			entry = &entries[i]
			break
		}
	}
	if entry == nil || len(entry.URLs) == 0 {
		return nil, fmt.Errorf("chart %s has no version %s in %s; recent versions: %s", name, version, repo, recentVersions(entries))
	}
	base, err := url.Parse(repo + "/")
	if err != nil {
		return nil, err
	}
	pkgURL, err := base.Parse(entry.URLs[0])
	if err != nil {
		return nil, fmt.Errorf("chart %s %s: invalid package URL: %w", name, version, err)
	}
	if pkgURL.Scheme != "https" {
		return nil, fmt.Errorf("chart %s %s: refusing a package URL over %s", name, version, pkgURL.Scheme)
	}
	resp, err := f.remote.Get(ctx, remote.Request{URL: pkgURL.String(), Limit: maxPackageBytes})
	if err != nil {
		return nil, err
	}
	data := resp.Body
	if entry.Digest != "" {
		if sum := sha256.Sum256(data); !strings.EqualFold(hex.EncodeToString(sum[:]), entry.Digest) {
			return nil, fmt.Errorf("chart %s %s: package digest does not match the index", name, version)
		}
	}
	return data, nil
}

// indexEntries returns a chart's entries in a repository index, from a
// cached copy when it is recent.
func (f *Fetcher) indexEntries(ctx context.Context, repo, name string) ([]indexEntry, error) {
	sum := sha256.Sum256([]byte(repo))
	file := filepath.Join(f.dir, "index", hex.EncodeToString(sum[:])+".yaml")
	data, err := os.ReadFile(file)
	if info, statErr := os.Stat(file); err != nil || statErr != nil || f.now().Sub(info.ModTime()) > indexTTL {
		resp, err := f.remote.Get(ctx, remote.Request{URL: repo + "/index.yaml", Limit: maxIndexBytes})
		if err != nil {
			return nil, err
		}
		data = resp.Body
		if err := writeFile(file, data); err != nil {
			return nil, err
		}
	}
	var index struct {
		Entries map[string][]indexEntry `yaml:"entries"`
	}
	if err := yaml.Unmarshal(data, &index); err != nil {
		return nil, fmt.Errorf("parse the index of %s: %w", repo, err)
	}
	entries, ok := index.Entries[name]
	if !ok {
		return nil, fmt.Errorf("no chart %s in %s", name, repo)
	}
	return entries, nil
}

func recentVersions(entries []indexEntry) string {
	var out []string
	for _, e := range entries[:min(len(entries), 10)] {
		out = append(out, e.Version)
	}
	if len(out) == 0 {
		return "none"
	}
	return strings.Join(out, ", ")
}

// ociPackage pulls a chart from an OCI registry, anonymously.
func (f *Fetcher) ociPackage(ctx context.Context, repo, name, version string) ([]byte, error) {
	u, err := url.Parse(repo)
	if err != nil {
		return nil, err
	}
	repository := strings.Trim(u.Path+"/"+name, "/")
	// OCI tags cannot hold "+"; Helm pushes it as "_".
	tag := strings.ReplaceAll(version, "+", "_")
	resp, err := f.remote.RegistryGet(ctx, u.Host, repository, "manifests/"+tag, "application/vnd.oci.image.manifest.v1+json", maxManifestBytes)
	if err != nil {
		return nil, fmt.Errorf("chart %s %s: %w", name, version, err)
	}
	raw := resp.Body
	var manifest struct {
		Layers []struct {
			MediaType string `json:"mediaType"`
			Digest    string `json:"digest"`
			Size      int64  `json:"size"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, fmt.Errorf("chart %s %s: invalid manifest: %w", name, version, err)
	}
	for _, l := range manifest.Layers {
		if l.MediaType != helmLayerType {
			continue
		}
		digest, ok := strings.CutPrefix(l.Digest, "sha256:")
		if !ok || len(digest) != 64 {
			return nil, fmt.Errorf("chart %s %s: unsupported layer digest %q", name, version, l.Digest)
		}
		if l.Size > maxPackageBytes {
			return nil, fmt.Errorf("chart %s %s: package of %d bytes exceeds %d", name, version, l.Size, maxPackageBytes)
		}
		blob, err := f.remote.RegistryGet(ctx, u.Host, repository, "blobs/"+l.Digest, "", maxPackageBytes)
		if err != nil {
			return nil, fmt.Errorf("chart %s %s: %w", name, version, err)
		}
		data := blob.Body
		if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != strings.ToLower(digest) {
			return nil, fmt.Errorf("chart %s %s: package digest does not match the manifest", name, version)
		}
		return data, nil
	}
	return nil, fmt.Errorf("chart %s %s: %s is not a Helm chart", name, version, repo)
}

// unpack reads a chart package; nested chart archives under charts/ are
// unpacked in place of the archive.
func unpack(data []byte) (map[string][]byte, error) {
	budget := &budget{bytes: maxChartBytes, files: maxChartFiles}
	_, files, err := untar(data, budget)
	if err != nil {
		return nil, err
	}
	for _, p := range slices.Collect(maps.Keys(files)) {
		dir, file := path.Split(p)
		if dir != "charts/" || !strings.HasSuffix(file, ".tgz") {
			continue
		}
		root, sub, err := untar(files[p], budget)
		if err != nil {
			return nil, fmt.Errorf("subchart %s: %w", file, err)
		}
		delete(files, p)
		for sp, sc := range sub {
			files["charts/"+root+"/"+sp] = sc
		}
	}
	return files, nil
}

// budget bounds what unpacking may produce.
type budget struct {
	bytes int64
	files int
}

// untar reads the regular files of a gzipped tar archive whose entries sit
// under one top directory, which it returns with the files relative to it.
func untar(data []byte, b *budget) (string, map[string][]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return "", nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	root := ""
	files := map[string][]byte{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", nil, err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		clean := path.Clean(strings.TrimPrefix(h.Name, "/"))
		top, rel, ok := strings.Cut(clean, "/")
		if !ok || top == ".." {
			continue
		}
		if root == "" {
			root = top
		}
		if top != root {
			continue
		}
		b.files--
		b.bytes -= h.Size
		if b.files < 0 || b.bytes < 0 {
			return "", nil, fmt.Errorf("more than %d files or %d bytes unpacked", maxChartFiles, maxChartBytes)
		}
		content, err := io.ReadAll(io.LimitReader(tr, h.Size))
		if err != nil {
			return "", nil, err
		}
		files[rel] = content
	}
	if root == "" {
		return "", nil, errors.New("empty chart package")
	}
	return root, files, nil
}

// writeFile writes atomically, so a crash never leaves a partial package
// in the cache.
func writeFile(file string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(file), ".tmp-*")
	if err != nil {
		return fmt.Errorf("chart cache: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("chart cache: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("chart cache: %w", err)
	}
	if err := os.Rename(tmp.Name(), file); err != nil {
		return fmt.Errorf("chart cache: %w", err)
	}
	return nil
}

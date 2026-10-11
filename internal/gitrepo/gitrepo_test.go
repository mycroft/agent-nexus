package gitrepo_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mycroft/agent-nexus/internal/gitrepo"
	"github.com/mycroft/agent-nexus/internal/gitrepo/gitrepotest"
)

func syncedRepo(t *testing.T) (*gitrepo.Repo, *gitrepotest.Remote, string) {
	t.Helper()
	remote := gitrepotest.New(t, 7,
		map[string]string{
			"main.go":         "package main\n\nfunc main() {}\n",
			"docs/readme.md":  "# Readme\n",
			"old/removed.txt": "gone\n",
		},
		map[string]string{
			"main.go":         "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"hi\") }\n",
			"pkg/new.go":      "package pkg\n\n// Answer is the answer.\nconst Answer = 42\n",
			"old/removed.txt": "",
		})
	cacheDir := t.TempDir()
	cache, err := gitrepo.NewCache(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := cache.Sync(context.Background(), gitrepo.Remote{Key: "test/owner/repo", URL: remote.Dir}, "main", 7)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	return repo, remote, cacheDir
}

func TestSyncAndRead(t *testing.T) {
	ctx := context.Background()
	repo, remote, cacheDir := syncedRepo(t)

	if _, err := os.Stat(filepath.Join(cacheDir, "test", "owner", "repo.git", "HEAD")); err != nil {
		t.Fatalf("bare repo not created where expected: %v", err)
	}
	if !repo.HasCommit(ctx, remote.HeadSHA) {
		t.Fatal("head commit missing after sync")
	}
	if repo.HasCommit(ctx, strings.Repeat("0", 40)) {
		t.Fatal("HasCommit reported a nonexistent commit")
	}
	base, err := repo.ResolveBranch(ctx, "main")
	if err != nil || base != remote.BaseSHA {
		t.Fatalf("ResolveBranch = %q, %v; want %q", base, err, remote.BaseSHA)
	}

	d, err := repo.Diff(ctx, base, remote.HeadSHA)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"diff --git a/main.go b/main.go", "new file mode", "deleted file mode", `+import "fmt"`} {
		if !strings.Contains(d, want) {
			t.Errorf("diff lacks %q:\n%s", want, d)
		}
	}

	content, err := repo.ReadFile(ctx, remote.HeadSHA, "/./pkg/new.go")
	if err != nil || !strings.Contains(string(content), "Answer = 42") {
		t.Fatalf("ReadFile = %q, %v", content, err)
	}
	if _, err := repo.ReadFile(ctx, remote.HeadSHA, "pkg"); !errors.Is(err, gitrepo.ErrInvalidPath) {
		t.Errorf("ReadFile(dir) error = %v, want ErrInvalidPath", err)
	}
	if _, err := repo.ReadFile(ctx, remote.HeadSHA, "missing.go"); !errors.Is(err, gitrepo.ErrNotFound) {
		t.Errorf("ReadFile(missing) error = %v, want ErrNotFound", err)
	}

	entries, err := repo.ListDir(ctx, remote.HeadSHA, "")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		n := e.Name
		if e.IsDir {
			n += "/"
		}
		names = append(names, n)
	}
	if got := strings.Join(names, ","); got != "docs/,main.go,pkg/" {
		t.Errorf("ListDir(root) = %s", got)
	}

	out, err := repo.Grep(ctx, remote.HeadSHA, "Answer", "", false)
	if err != nil || !strings.HasPrefix(out, "pkg/new.go:3:") {
		t.Errorf("Grep = %q, %v", out, err)
	}
	out, err = repo.Grep(ctx, remote.HeadSHA, "Answer", "docs", false)
	if err != nil || out != "" {
		t.Errorf("Grep limited to docs = %q, %v; want no match", out, err)
	}

	log, err := repo.Log(ctx, remote.HeadSHA, "main.go", 5)
	if err != nil || !strings.Contains(log, "change") || !strings.Contains(log, "base") {
		t.Errorf("Log = %q, %v", log, err)
	}
}

func TestResyncFetchesNewHead(t *testing.T) {
	ctx := context.Background()
	remote := gitrepotest.New(t, 1, map[string]string{"a": "1\n"}, map[string]string{"a": "2\n"})
	cache, err := gitrepo.NewCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := gitrepo.Remote{Key: "k/o/r", URL: remote.Dir}
	if _, err := cache.Sync(ctx, r, "main", 1); err != nil {
		t.Fatal(err)
	}
	gitrepotest.Git(t, remote.Dir, "checkout", "--quiet", "feature")
	if err := os.WriteFile(filepath.Join(remote.Dir, "a"), []byte("3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitrepotest.Git(t, remote.Dir, "commit", "--quiet", "-am", "more")
	newHead := gitrepotest.Git(t, remote.Dir, "rev-parse", "HEAD")
	gitrepotest.Git(t, remote.Dir, "update-ref", "refs/pull/1/head", newHead)

	repo, err := cache.Sync(ctx, r, "main", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !repo.HasCommit(ctx, newHead) {
		t.Fatal("second sync did not fetch the new head")
	}
}

// Model-supplied strings must never be interpreted as git options or
// escape the tree.
func TestArgumentHardening(t *testing.T) {
	ctx := context.Background()
	repo, remote, _ := syncedRepo(t)
	marker := filepath.Join(t.TempDir(), "pwned")

	for _, pattern := range []string{"--output=" + marker, "-O/bin/touch " + marker, "--open-files-in-pager=touch " + marker} {
		if _, err := repo.Grep(ctx, remote.HeadSHA, pattern, "", false); err != nil {
			t.Errorf("Grep(%q) error = %v, want a plain no-match", pattern, err)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a grep pattern was interpreted as an option")
	}

	for _, p := range []string{"../etc/passwd", "a/../../b", "-O/bin/sh", "x\x00y", `a\b`} {
		if _, err := repo.ReadFile(ctx, remote.HeadSHA, p); !errors.Is(err, gitrepo.ErrInvalidPath) {
			t.Errorf("ReadFile(%q) error = %v, want ErrInvalidPath", p, err)
		}
		if _, err := repo.Grep(ctx, remote.HeadSHA, "x", p, false); !errors.Is(err, gitrepo.ErrInvalidPath) {
			t.Errorf("Grep path %q error = %v, want ErrInvalidPath", p, err)
		}
	}

	for _, rev := range []string{"HEAD", "--all", "main", remote.HeadSHA[:12]} {
		if _, err := repo.ReadFile(ctx, rev, "main.go"); err == nil {
			t.Errorf("ReadFile accepted non-SHA revision %q", rev)
		}
	}
}

func TestSyncRejectsSuspiciousInput(t *testing.T) {
	cache, err := gitrepo.NewCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, branch := range []string{"", "--upload-pack=x", "a:b", "a..b", "a b"} {
		if _, err := cache.Sync(ctx, gitrepo.Remote{Key: "k/o/r", URL: "/nonexistent"}, branch, 1); err == nil {
			t.Errorf("Sync accepted base branch %q", branch)
		}
	}
	for _, key := range []string{"../escape", "a//b", "a/./b"} {
		if _, err := cache.Sync(ctx, gitrepo.Remote{Key: key, URL: "/nonexistent"}, "main", 1); err == nil {
			t.Errorf("Sync accepted key %q", key)
		}
	}
}

func TestCleanPath(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"/", ""},
		{".", ""},
		{"./a/b.go", "a/b.go"},
		{"/a//b/../c", "a/c"},
		{"\tdocs/x.md ", "docs/x.md"},
	} {
		got, err := gitrepo.CleanPath(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("CleanPath(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}

func TestContains(t *testing.T) {
	repo, remote, _ := syncedRepo(t)
	ctx := context.Background()
	for literal, want := range map[string]bool{
		"func main() {}":    true,
		"const Answer":      false, // only at the head
		"-e":                false, // a literal, never an option
		"[a-z]+ main\\(\\)": false,
	} {
		got, err := repo.Contains(ctx, remote.BaseSHA, literal)
		if err != nil || got != want {
			t.Errorf("Contains(base, %q) = %v, %v; want %v", literal, got, err, want)
		}
	}
	if got, err := repo.Contains(ctx, remote.HeadSHA, "const Answer"); err != nil || !got {
		t.Errorf("Contains(head) = %v, %v", got, err)
	}
	if _, err := repo.Contains(ctx, "main", "x"); err == nil {
		t.Error("a branch name must be rejected: revisions are SHAs")
	}
}

func TestDiffBlobs(t *testing.T) {
	ctx := context.Background()
	got, err := gitrepo.DiffBlobs(ctx, "templates/../values.yaml", []byte("a: 1\nb: 2\n"), []byte("a: 1\nb: 3\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := "--- a/templates/../values.yaml\n+++ b/templates/../values.yaml\n@@ -1,2 +1,2 @@\n a: 1\n-b: 2\n+b: 3\n"
	if got != want {
		t.Errorf("DiffBlobs =\n%q\nwant\n%q", got, want)
	}
	if got, err := gitrepo.DiffBlobs(ctx, "x", []byte("same\n"), []byte("same\n")); err != nil || got != "" {
		t.Errorf("equal blobs = %q, %v", got, err)
	}
	if got, err := gitrepo.DiffBlobs(ctx, "logo.png", []byte("\x00\x01"), []byte("\x00\x02")); err != nil || got != "Binary files a/logo.png and b/logo.png differ\n" {
		t.Errorf("binary blobs = %q, %v", got, err)
	}
}

func TestContainsBounded(t *testing.T) {
	remote := gitrepotest.New(t, 1,
		map[string]string{"values.yaml": "repo: https://prometheus-community.github.io/helm-charts\nimage: jhaals/yopass:15.0.0\n"},
		map[string]string{"values.yaml": "x\n"})
	cache, err := gitrepo.NewCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo, err := cache.Sync(context.Background(), gitrepo.Remote{Key: "t/o/r", URL: remote.Dir}, "main", 1)
	if err != nil {
		t.Fatal(err)
	}
	const host, image = "A-Za-z0-9.-", "A-Za-z0-9._/-"
	for _, tc := range []struct {
		literal, class string
		want           bool
	}{
		{"prometheus-community.github.io", host, true},
		{"community.github.io", host, false},
		{"github.io", host, false},
		{"prometheus-community.githubXio", host, false}, // "." is literal
		{"jhaals/yopass", image, true},
		{"haals/yopass", image, false},
		{"jhaals/yopas", image, false},
	} {
		got, err := repo.ContainsBounded(context.Background(), remote.BaseSHA, tc.literal, tc.class)
		if err != nil || got != tc.want {
			t.Errorf("ContainsBounded(%q) = %v, %v; want %v", tc.literal, got, err, tc.want)
		}
	}
	if _, err := repo.ContainsBounded(context.Background(), remote.BaseSHA, "a|b", host); err == nil {
		t.Error("a literal with regexp syntax must be refused")
	}
}

func TestFetchRefs(t *testing.T) {
	remote := gitrepotest.New(t, 1,
		map[string]string{"app.go": "v1\n", "CHANGELOG.md": "# 1.0.0\n"},
		map[string]string{"app.go": "v2\n", "CHANGELOG.md": "# 1.1.0\n"})
	// The head commit becomes a release on main, as after a merge.
	gitrepotest.Git(t, remote.Dir, "update-ref", "refs/heads/main", remote.HeadSHA)
	gitrepotest.Git(t, remote.Dir, "tag", "v1.0.0", remote.BaseSHA)
	gitrepotest.Git(t, remote.Dir, "tag", "-a", "-m", "release", "v1.1.0", remote.HeadSHA)
	cache, err := gitrepo.NewCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r := gitrepo.Remote{Key: "test/o/app", URL: remote.Dir}

	repo, commits, tags, err := cache.FetchRefs(ctx, r, []string{"v1.0.0", "v1.1.0", "main", "v9.9.9"})
	if err != nil {
		t.Fatal(err)
	}
	// An annotated tag resolves to its commit; a missing name is left out.
	if commits["v1.0.0"] != remote.BaseSHA || commits["v1.1.0"] != remote.HeadSHA || commits["main"] != remote.HeadSHA || len(commits) != 3 {
		t.Errorf("commits = %v", commits)
	}
	if strings.Join(tags, ",") != "v1.0.0,v1.1.0" {
		t.Errorf("tags = %v", tags)
	}
	log, err := repo.LogRange(ctx, commits["v1.0.0"], commits["v1.1.0"], 10)
	if err != nil || !strings.HasPrefix(log, remote.HeadSHA[:7]+" ") || !strings.Contains(log, " test: change\n") {
		t.Errorf("LogRange = %q, %v", log, err)
	}
	stat, err := repo.DiffStat(ctx, commits["v1.0.0"], commits["v1.1.0"])
	if err != nil || !strings.Contains(stat, "CHANGELOG.md") || !strings.Contains(stat, "2 files changed") {
		t.Errorf("DiffStat = %q, %v", stat, err)
	}
	if content, err := repo.ReadFile(ctx, commits["v1.1.0"], "CHANGELOG.md"); err != nil || string(content) != "# 1.1.0\n" {
		t.Errorf("ReadFile = %q, %v", content, err)
	}
	for _, bad := range []string{"-upload-pack=x", "a..b", "x.lock", "a b"} {
		if _, _, _, err := cache.FetchRefs(ctx, r, []string{bad}); err == nil {
			t.Errorf("FetchRefs(%q) accepted", bad)
		}
	}
}

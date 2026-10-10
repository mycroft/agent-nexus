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

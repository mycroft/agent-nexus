package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/mycroft/agent-nexus/internal/gitrepo"
	"github.com/mycroft/agent-nexus/internal/gitrepo/gitrepotest"
)

func repoTools(t *testing.T) (map[string]Tool, *gitrepotest.Remote, *[]string) {
	t.Helper()
	remote := gitrepotest.New(t, 1,
		map[string]string{"app.go": "v1\n", "CHANGELOG.md": "# 1.0.0\n"},
		map[string]string{"app.go": "v2\n", "CHANGELOG.md": "# 1.1.0\n- faster\n"})
	gitrepotest.Git(t, remote.Dir, "update-ref", "refs/heads/main", remote.HeadSHA)
	gitrepotest.Git(t, remote.Dir, "tag", "v1.0.0", remote.BaseSHA)
	gitrepotest.Git(t, remote.Dir, "tag", "v1.1.0", remote.HeadSHA)
	gitrepotest.Git(t, remote.Dir, "tag", "v0.9.0", remote.BaseSHA)
	cache, err := gitrepo.NewCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var opened []string
	open := func(ctx context.Context, repo string, refs []string) (*gitrepo.Repo, map[string]string, []string, error) {
		opened = append(opened, repo)
		if repo != "o/app" {
			return nil, nil, nil, errors.New("not referenced")
		}
		return cache.FetchRefs(ctx, gitrepo.Remote{Key: "test/o/app", URL: remote.Dir}, refs)
	}
	byName := map[string]Tool{}
	for _, tool := range Repos(open) {
		byName[tool.Name] = tool
	}
	return byName, remote, &opened
}

func TestRepoTools(t *testing.T) {
	tools, remote, opened := repoTools(t)

	// Versions match their "v" tags.
	out, err := call(t, tools["repo_log"], map[string]any{"repo": "o/app", "from": "1.0.0", "to": "1.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Commits of o/app after 1.0.0, up to 1.1.0, newest first:\n\n" + remote.HeadSHA[:7], "test: change", "Files changed:\n", "CHANGELOG.md", "2 files changed"} {
		if !strings.Contains(out, want) {
			t.Errorf("repo_log lacks %q:\n%s", want, out)
		}
	}
	out, err = call(t, tools["repo_read"], map[string]any{"repo": "o/app", "ref": "v1.1.0", "path": "CHANGELOG.md"})
	if err != nil || out != "    1  # 1.1.0\n    2  - faster\n" {
		t.Errorf("repo_read = %q, %v", out, err)
	}
	if out, err := call(t, tools["repo_read"], map[string]any{"repo": "o/app", "ref": "main", "path": "app.go"}); err != nil || out != "    1  v2\n" {
		t.Errorf("repo_read at a branch = %q, %v", out, err)
	}
	if _, err := call(t, tools["repo_log"], map[string]any{"repo": "o/app", "from": "1.0.0", "to": "2.0.0"}); err == nil ||
		!strings.Contains(err.Error(), `o/app has no tag or branch "2.0.0"; its latest tags: v1.1.0, v1.0.0, v0.9.0`) {
		t.Errorf("missing version: %v", err)
	}
	if _, err := call(t, tools["repo_log"], map[string]any{"repo": "o/secret", "from": "1", "to": "2"}); err == nil || err.Error() != "not referenced" {
		t.Errorf("the opener's refusal must reach the model: %v", err)
	}
	if len(*opened) != 5 {
		t.Errorf("opened %d times, want once per call", len(*opened))
	}
}

func TestCut(t *testing.T) {
	if got := cut("short", 10); got != "short" {
		t.Errorf("cut under the limit = %q", got)
	}
	if got := cut(strings.Repeat("x", 20), 8); got != "xxxxxxxx\n[…cut; narrow the range for the rest]\n" {
		t.Errorf("cut over the limit = %q", got)
	}
}

func TestRepoLogCutsLongLogs(t *testing.T) {
	remote := gitrepotest.New(t, 1, map[string]string{"a": "1\n"}, map[string]string{"a": "2\n"})
	// A release whose log exceeds the tool's budget.
	for i := range 40 {
		gitrepotest.Git(t, remote.Dir, "commit", "--allow-empty", "-q", "-m", fmt.Sprintf("change %d\n\n%s", i, strings.Repeat("detail ", 60)))
	}
	gitrepotest.Git(t, remote.Dir, "tag", "v1.0.0", remote.BaseSHA)
	gitrepotest.Git(t, remote.Dir, "tag", "v2.0.0", "HEAD")
	cache, err := gitrepo.NewCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	open := func(ctx context.Context, _ string, refs []string) (*gitrepo.Repo, map[string]string, []string, error) {
		return cache.FetchRefs(ctx, gitrepo.Remote{Key: "test/o/big", URL: remote.Dir}, refs)
	}
	out, err := call(t, Repos(open)[0], map[string]any{"repo": "o/big", "from": "1.0.0", "to": "2.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > maxRepoLogBytes+maxRepoStatBytes+512 || !strings.Contains(out, "[…cut; narrow the range for the rest]") || !strings.Contains(out, "change 39") {
		t.Errorf("repo_log of a long range: %d bytes, cut marker or newest commit missing", len(out))
	}
}

func TestRefCandidates(t *testing.T) {
	for v, want := range map[string]string{"1.2.3": "1.2.3,v1.2.3", "v1.2.3": "v1.2.3,1.2.3", "main": "main,vmain"} {
		if got := strings.Join(refCandidates(v), ","); got != want {
			t.Errorf("refCandidates(%q) = %s, want %s", v, got, want)
		}
	}
}

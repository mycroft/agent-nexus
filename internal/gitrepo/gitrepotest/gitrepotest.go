// Package gitrepotest builds small git repositories for tests: a "main"
// branch and a pull request ref on top of it, like a forge serves them.
package gitrepotest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Remote is a repository that can be fetched from by path.
type Remote struct {
	Dir     string
	BaseSHA string
	HeadSHA string
}

// New creates a repository whose main branch holds base, and whose
// refs/pull/<pr>/head commit applies head on top (an empty value deletes
// the file).
func New(t *testing.T, pr int, base, head map[string]string) *Remote {
	t.Helper()
	dir := t.TempDir()
	Git(t, dir, "init", "--quiet", "--initial-branch=main")
	write(t, dir, base)
	Git(t, dir, "add", "-A")
	Git(t, dir, "commit", "--quiet", "-m", "base")
	r := &Remote{Dir: dir, BaseSHA: Git(t, dir, "rev-parse", "HEAD")}

	Git(t, dir, "checkout", "--quiet", "-b", "feature")
	write(t, dir, head)
	Git(t, dir, "add", "-A")
	Git(t, dir, "commit", "--quiet", "-m", "change")
	r.HeadSHA = Git(t, dir, "rev-parse", "HEAD")
	Git(t, dir, "update-ref", "refs/pull/"+strconv.Itoa(pr)+"/head", r.HeadSHA)
	Git(t, dir, "checkout", "--quiet", "main")
	return r
}

func write(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if content == "" {
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// Git runs git in dir with an isolated configuration and returns its
// trimmed output.
func Git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	gitArgs := append([]string{"-C", dir, "-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false"}, args...)
	cmd := exec.Command("git", gitArgs...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

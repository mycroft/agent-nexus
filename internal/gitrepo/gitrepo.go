// Package gitrepo keeps a cache of bare git repositories and reads from them
// through a hardened wrapper around the git CLI. Nothing is ever checked
// out: every read goes through git objects at a commit chosen by
// agent-nexus, so repository content cannot plant symlinks or hooks.
package gitrepo

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	fetchTimeout = 5 * time.Minute
	readTimeout  = 30 * time.Second
	// MaxOutput caps what any single read returns.
	MaxOutput = 8 << 20
)

var (
	shaPattern = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

	// ErrNotFound is returned when a path does not exist at the revision.
	ErrNotFound = errors.New("path not found")
	// ErrInvalidPath is returned for paths that try to leave the tree.
	ErrInvalidPath = errors.New("invalid path")
	// ErrTruncated is returned (with the partial output) when output
	// exceeded the cap.
	ErrTruncated = errors.New("output truncated")
)

// Cache stores one bare repository per key under a directory.
type Cache struct {
	dir   string
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// NewCache returns a cache rooted at dir, creating it if needed.
func NewCache(dir string) (*Cache, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create repo cache dir: %w", err)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve repo cache dir: %w", err)
	}
	return &Cache{dir: abs, locks: map[string]*sync.Mutex{}}, nil
}

// Remote describes where to fetch from.
type Remote struct {
	// Key identifies the repository in the cache, e.g. "gitea/owner/repo".
	// Each segment must be a plain name.
	Key string
	// URL is the clone URL.
	URL string
	// AuthHeader, if set, is sent as the HTTP Authorization header value.
	AuthHeader string
}

// BasicAuth builds an Authorization header value for HTTP basic auth.
func BasicAuth(user, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+password))
}

// Sync fetches the given branch and pull-request head into the cached bare
// repository for remote, creating it on first use, and returns it.
func (c *Cache) Sync(ctx context.Context, remote Remote, baseBranch string, pr int) (*Repo, error) {
	dir, err := c.repoDir(remote.Key)
	if err != nil {
		return nil, err
	}
	if baseBranch == "" || strings.HasPrefix(baseBranch, "-") || strings.ContainsAny(baseBranch, ": \t\n\\^~?*[") || strings.Contains(baseBranch, "..") {
		return nil, fmt.Errorf("refusing suspicious base branch name %q", baseBranch)
	}
	if pr <= 0 {
		return nil, fmt.Errorf("invalid pull request number %d", pr)
	}

	lock := c.lock(remote.Key)
	lock.Lock()
	defer lock.Unlock()

	repo := &Repo{dir: dir}
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("create repo dir: %w", err)
		}
		if _, err := repo.run(ctx, readTimeout, nil, "init", "--bare", "--quiet"); err != nil {
			return nil, err
		}
	}

	var env []string
	if remote.AuthHeader != "" {
		env = []string{
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http.extraHeader",
			"GIT_CONFIG_VALUE_0=Authorization: " + remote.AuthHeader,
		}
	}
	args := []string{
		"fetch", "--quiet", "--no-tags", "--force", "--no-write-fetch-head",
		"--end-of-options", remote.URL,
		"+refs/heads/" + baseBranch + ":refs/remotes/origin/" + baseBranch,
		"+refs/pull/" + strconv.Itoa(pr) + "/head:refs/pr/" + strconv.Itoa(pr),
	}
	if _, err := repo.run(ctx, fetchTimeout, env, args...); err != nil {
		return nil, fmt.Errorf("fetch %s: %w", remote.Key, err)
	}
	return repo, nil
}

func (c *Cache) repoDir(key string) (string, error) {
	parts := strings.Split(key, "/")
	for _, p := range parts {
		if p == "" || p == "." || p == ".." || strings.ContainsAny(p, `\`) {
			return "", fmt.Errorf("invalid repository key %q", key)
		}
	}
	return filepath.Join(c.dir, filepath.Join(parts...)+".git"), nil
}

func (c *Cache) lock(key string) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	l, ok := c.locks[key]
	if !ok {
		l = &sync.Mutex{}
		c.locks[key] = l
	}
	return l
}

// Repo is a bare repository. All revisions passed to its methods must be
// full commit SHAs.
type Repo struct {
	dir string
}

// Open returns the repository at dir without fetching (used in tests).
func Open(dir string) *Repo { return &Repo{dir: dir} }

// ResolveBranch returns the commit SHA of the fetched base branch.
func (r *Repo) ResolveBranch(ctx context.Context, branch string) (string, error) {
	out, err := r.run(ctx, readTimeout, nil, "rev-parse", "--verify", "--quiet", "--end-of-options", "refs/remotes/origin/"+branch+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve base branch %q: %w", branch, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// HasCommit reports whether sha is a commit present in the repository.
func (r *Repo) HasCommit(ctx context.Context, sha string) bool {
	if !shaPattern.MatchString(sha) {
		return false
	}
	_, err := r.run(ctx, readTimeout, nil, "cat-file", "-e", "--end-of-options", sha+"^{commit}")
	return err == nil
}

// Diff returns the diff between the merge-base of base and head, and head
// (what a pull request page shows).
func (r *Repo) Diff(ctx context.Context, base, head string) (string, error) {
	if err := checkSHAs(base, head); err != nil {
		return "", err
	}
	out, err := r.run(ctx, readTimeout, nil,
		"-c", "core.quotePath=false",
		"diff", "--no-ext-diff", "--no-textconv", "--no-color", "--find-renames",
		"--end-of-options", base+"..."+head, "--")
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// ReadFile returns the content of p at rev.
func (r *Repo) ReadFile(ctx context.Context, rev, p string) ([]byte, error) {
	if err := checkSHAs(rev); err != nil {
		return nil, err
	}
	clean, err := CleanPath(p)
	if err != nil {
		return nil, err
	}
	if clean == "" {
		return nil, fmt.Errorf("%w: %q is a directory", ErrInvalidPath, p)
	}
	kind, err := r.objectType(ctx, rev, clean)
	if err != nil {
		return nil, err
	}
	if kind != "blob" {
		return nil, fmt.Errorf("%w: %q is a %s, not a file", ErrInvalidPath, clean, kind)
	}
	return r.run(ctx, readTimeout, nil, "cat-file", "blob", rev+":"+clean)
}

// Entry is one directory entry.
type Entry struct {
	Name  string
	IsDir bool
}

// ListDir lists the directory p at rev ("" for the root).
func (r *Repo) ListDir(ctx context.Context, rev, p string) ([]Entry, error) {
	if err := checkSHAs(rev); err != nil {
		return nil, err
	}
	clean, err := CleanPath(p)
	if err != nil {
		return nil, err
	}
	kind, err := r.objectType(ctx, rev, clean)
	if err != nil {
		return nil, err
	}
	if kind != "tree" {
		return nil, fmt.Errorf("%w: %q is a %s, not a directory", ErrInvalidPath, clean, kind)
	}
	out, err := r.run(ctx, readTimeout, nil, "-c", "core.quotePath=false", "ls-tree", "-z", rev+":"+clean)
	if err != nil {
		return nil, err
	}
	var entries []Entry
	for rec := range strings.SplitSeq(strings.TrimSuffix(string(out), "\x00"), "\x00") {
		meta, name, ok := strings.Cut(rec, "\t")
		if !ok {
			continue
		}
		fields := strings.Fields(meta)
		entries = append(entries, Entry{Name: name, IsDir: len(fields) > 1 && fields[1] == "tree"})
	}
	return entries, nil
}

// Grep searches tracked files at rev for an extended regular expression,
// optionally limited to a path prefix. It returns "path:line:text" lines.
func (r *Repo) Grep(ctx context.Context, rev, pattern, prefix string, ignoreCase bool) (string, error) {
	if err := checkSHAs(rev); err != nil {
		return "", err
	}
	if pattern == "" {
		return "", errors.New("empty pattern")
	}
	clean, err := CleanPath(prefix)
	if err != nil {
		return "", err
	}
	args := []string{"-c", "core.quotePath=false", "grep", "-n", "-I", "-E", "--no-color", "--full-name"}
	if ignoreCase {
		args = append(args, "-i")
	}
	args = append(args, "-e", pattern, "--end-of-options", rev, "--")
	if clean != "" {
		args = append(args, clean)
	}
	out, err := r.run(ctx, readTimeout, nil, args...)
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return "", nil // no match
	}
	lines := strings.SplitAfter(string(out), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimPrefix(l, rev+":")
	}
	return strings.Join(lines, ""), err
}

// Contains reports whether a tracked text file at rev contains literal.
func (r *Repo) Contains(ctx context.Context, rev, literal string) (bool, error) {
	if err := checkSHAs(rev); err != nil {
		return false, err
	}
	if literal == "" {
		return false, errors.New("empty literal")
	}
	_, err := r.run(ctx, readTimeout, nil, "grep", "-q", "-I", "-F", "--no-color", "-e", literal, "--end-of-options", rev, "--")
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return err == nil, err
}

// DiffBlobs returns the unified diff (3 lines of context) turning a into
// b, under the a/<label> and b/<label> names, or "" when they are equal.
// The contents are written to temporary files with fixed names: label
// never reaches the filesystem or git's arguments.
func DiffBlobs(ctx context.Context, label string, a, b []byte) (string, error) {
	dir, err := os.MkdirTemp("", "agent-nexus-diff-")
	if err != nil {
		return "", fmt.Errorf("diff %s: %w", label, err)
	}
	defer os.RemoveAll(dir)
	pa, pb := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	if err := os.WriteFile(pa, a, 0o600); err != nil {
		return "", fmt.Errorf("diff %s: %w", label, err)
	}
	if err := os.WriteFile(pb, b, 0o600); err != nil {
		return "", fmt.Errorf("diff %s: %w", label, err)
	}
	out, err := execGit(ctx, readTimeout, nil, "diff", "--no-index", "--no-color", "--no-ext-diff", "--no-textconv", "-U3", "--", pa, pb)
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return "", nil // equal
	case errors.As(err, &exitErr) && exitErr.ExitCode() == 1, errors.Is(err, ErrTruncated):
	default:
		return "", err
	}
	text := string(out)
	// Replace git's header, which names the temporary files.
	if i := strings.Index(text, "\n@@"); i >= 0 {
		text = text[i+1:]
	} else {
		return fmt.Sprintf("Binary files a/%s and b/%s differ\n", label, label), nil
	}
	return fmt.Sprintf("--- a/%s\n+++ b/%s\n%s", label, label, text), nil
}

// Log returns up to n one-line commits touching p, newest first, reachable
// from rev.
func (r *Repo) Log(ctx context.Context, rev, p string, n int) (string, error) {
	if err := checkSHAs(rev); err != nil {
		return "", err
	}
	clean, err := CleanPath(p)
	if err != nil {
		return "", err
	}
	args := []string{"log", "--no-color", "--format=%h %ad %an: %s", "--date=short", "-n", strconv.Itoa(n), "--end-of-options", rev, "--"}
	if clean != "" {
		args = append(args, clean)
	}
	out, err := r.run(ctx, readTimeout, nil, args...)
	return string(out), err
}

func (r *Repo) objectType(ctx context.Context, rev, clean string) (string, error) {
	out, err := r.run(ctx, readTimeout, nil, "cat-file", "-t", rev+":"+clean)
	if err != nil {
		return "", fmt.Errorf("%w: %q", ErrNotFound, clean)
	}
	return strings.TrimSpace(string(out)), nil
}

// CleanPath normalizes a repository-relative path supplied by a model and
// rejects anything that would leave the tree. It returns "" for the root.
func CleanPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	p = strings.TrimLeft(p, "/")
	if p == "" || p == "." {
		return "", nil
	}
	if strings.ContainsAny(p, "\x00\n") || strings.Contains(p, `\`) {
		return "", fmt.Errorf("%w: %q", ErrInvalidPath, p)
	}
	clean := path.Clean(p)
	if clean == "." {
		return "", nil
	}
	if clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "-") {
		return "", fmt.Errorf("%w: %q", ErrInvalidPath, p)
	}
	return clean, nil
}

func checkSHAs(shas ...string) error {
	for _, s := range shas {
		if !shaPattern.MatchString(s) {
			return fmt.Errorf("invalid commit SHA %q", s)
		}
	}
	return nil
}

// run executes git against the bare repository with an isolated
// configuration: no system/global config, no prompts, literal pathspecs.
func (r *Repo) run(ctx context.Context, timeout time.Duration, extraEnv []string, args ...string) ([]byte, error) {
	return execGit(ctx, timeout, extraEnv, append([]string{"--git-dir=" + r.dir}, args...)...)
}

// execGit runs git with an isolated configuration.
func execGit(ctx context.Context, timeout time.Duration, extraEnv []string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Arguments are fixed by this package; model input only ever arrives
	// after --end-of-options / -e / --, or inside a validated rev:path.
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.TempDir(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_LITERAL_PATHSPECS=1",
		"GIT_OPTIONAL_LOCKS=0",
		"LC_ALL=C",
	}, extraEnv...)
	stdout := &capped{max: MaxOutput}
	stderr := &capped{max: 4096}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(stderr.buf.String())
		if stdout.truncated && msg == "" {
			return stdout.buf.Bytes(), ErrTruncated
		}
		return stdout.buf.Bytes(), fmt.Errorf("git %s: %w: %s", subcommand(args), err, msg)
	}
	if stdout.truncated {
		return stdout.buf.Bytes(), ErrTruncated
	}
	return stdout.buf.Bytes(), nil
}

// subcommand names the git command in args, after the global options.
func subcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-c":
			i++
		case strings.HasPrefix(args[i], "-"):
		default:
			return args[i]
		}
	}
	return "command"
}

// capped buffers up to max bytes and silently discards the rest.
type capped struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room < len(p) {
		c.truncated = true
		if room > 0 {
			c.buf.Write(p[:room])
		}
		return len(p), nil
	}
	c.buf.Write(p)
	return len(p), nil
}

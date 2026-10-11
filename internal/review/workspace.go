package review

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/mycroft/agent-nexus/internal/charts"
	"github.com/mycroft/agent-nexus/internal/diff"
	"github.com/mycroft/agent-nexus/internal/forge"
	"github.com/mycroft/agent-nexus/internal/gitrepo"
	"github.com/mycroft/agent-nexus/internal/remote"
	"github.com/mycroft/agent-nexus/internal/tools"
)

// workspace is a pull request ready for the agent: the repository at the
// head commit, the diff against the merge-base, and the base branch's
// repository guidance. A branch workspace (prepareBranch) is a branch's
// head commit instead, without a diff.
type workspace struct {
	repo *gitrepo.Repo
	// branch is the branch of a branch workspace, "" for a pull request.
	branch        string
	baseSHA       string
	headSHA       string
	files         []*diff.File
	byPath        map[string]*diff.File
	guidanceName  string
	guidance      string
	contextTokens int
	charts        *charts.Fetcher
	remote        *remote.Client
	fetchHosts    []string
	// forge, repos and repoID let the agent read other repositories of
	// the forge, with the forge's own credentials.
	forge  forge.Forge
	repos  *gitrepo.Cache
	repoID forge.Repo
}

// prepare fetches the pull request at headSHA and computes its diff. It
// returns Stale when headSHA is not in the fetched pull request (it was
// force-pushed since it was read).
func (r *Reviewer) prepare(ctx context.Context, f forge.Forge, repoID forge.Repo, pr forge.PR, headSHA string) (*workspace, Outcome, error) {
	remote, err := f.GitRemote(ctx, repoID)
	if err != nil {
		return nil, "", err
	}
	repo, err := r.Repos.Sync(ctx, remote, pr.BaseRef, pr.Number)
	if err != nil {
		return nil, "", err
	}
	if !repo.HasCommit(ctx, headSHA) {
		return nil, Stale, nil
	}
	baseSHA, err := repo.ResolveBranch(ctx, pr.BaseRef)
	if err != nil {
		return nil, "", err
	}
	// Once merged, the base branch holds the pull request: diffing against
	// it would show nothing. The forge's recorded merge base still tells
	// the pull request's changes apart.
	if pr.State != "open" && pr.MergeBase != "" && repo.HasCommit(ctx, pr.MergeBase) {
		baseSHA = pr.MergeBase
	}
	rawDiff, err := repo.Diff(ctx, baseSHA, headSHA)
	if errors.Is(err, gitrepo.ErrTruncated) {
		return nil, "", fmt.Errorf("diff exceeds %d bytes; too large to review", gitrepo.MaxOutput)
	}
	if err != nil {
		return nil, "", err
	}
	files, err := diff.Parse(rawDiff)
	if err != nil {
		return nil, "", fmt.Errorf("parse diff: %w", err)
	}

	w := r.newWorkspace(ctx, f, repoID, repo, baseSHA, headSHA)
	w.files = files
	for _, file := range files {
		w.byPath[file.Path()] = file
		if file.Status == diff.Renamed {
			w.byPath[file.OldPath] = file
		}
	}
	return w, "", nil
}

// prepareBranch fetches the head of the repository's default branch, for
// questions about the repository rather than a pull request. That commit
// is also the base of the URL policies.
func (r *Reviewer) prepareBranch(ctx context.Context, f forge.Forge, repoID forge.Repo) (*workspace, error) {
	branch, err := f.DefaultBranch(ctx, repoID)
	if err != nil {
		return nil, err
	}
	remote, err := f.GitRemote(ctx, repoID)
	if err != nil {
		return nil, err
	}
	repo, commits, _, err := r.Repos.FetchRefs(ctx, remote, []string{branch})
	if err != nil {
		return nil, err
	}
	sha, ok := commits[branch]
	if !ok {
		return nil, fmt.Errorf("branch %s of %s not found", branch, repoID)
	}
	w := r.newWorkspace(ctx, f, repoID, repo, sha, sha)
	w.branch = branch
	return w, nil
}

// newWorkspace returns a workspace without a diff, with the repository
// guidance of baseSHA.
func (r *Reviewer) newWorkspace(ctx context.Context, f forge.Forge, repoID forge.Repo, repo *gitrepo.Repo, baseSHA, headSHA string) *workspace {
	w := &workspace{
		repo: repo, baseSHA: baseSHA, headSHA: headSHA, byPath: map[string]*diff.File{},
		contextTokens: r.ContextTokens, charts: r.Charts, remote: r.Remote, fetchHosts: r.FetchHosts,
		forge: f, repos: r.Repos, repoID: repoID,
	}
	for _, name := range guidanceFiles {
		if content, err := repo.ReadFile(ctx, baseSHA, name); err == nil {
			w.guidanceName, w.guidance = name, string(content)
			break
		}
	}
	return w
}

// fileDiff returns the annotated diff of one changed file.
func (w *workspace) fileDiff(path string) (string, bool) {
	file, ok := w.byPath[path]
	if !ok {
		return "", false
	}
	if file.Binary {
		return "binary file; no textual diff", true
	}
	return file.Annotated(), true
}

func (w *workspace) tools() []tools.Tool {
	out := tools.Repo(w.repo, w.headSHA, w.fileDiff)
	if w.branch != "" {
		out = tools.Commit(w.repo, w.headSHA, "the head of the "+w.branch+" branch")
	}
	if w.charts != nil {
		out = append(out, tools.Charts(w.charts, w.chartPolicy)...)
	}
	if w.remote != nil {
		out = append(out, tools.Web(w.remote, tools.WebPolicy{Page: w.pagePolicy, Image: w.imagePolicy})...)
	}
	if w.forge != nil && w.repos != nil {
		out = append(out, tools.Repos(w.openRepo)...)
	}
	return out
}

// toolsSentence tells the model which tools it has.
func (w *workspace) toolsSentence() string {
	s := "You have read-only tools over the repository at the pull request's head commit: read_file, list_dir, grep, git_log and get_file_diff."
	base := "base branch"
	if w.branch != "" {
		s = fmt.Sprintf("You have read-only tools over the repository at the head of its %s branch (%s): read_file, list_dir, grep and git_log.", w.branch, short(w.headSHA))
		base = w.branch + " branch"
	}
	if w.charts != nil {
		s += " chart_files, chart_read and chart_diff read published Helm chart versions, from the chart repositories the repository's " + base + " references."
	}
	if w.remote != nil {
		s += " fetch_url reads documentation pages on hosts the " + base + " references, and release_notes the GitHub release notes of a container image it references."
	}
	if w.forge != nil && w.repos != nil {
		s += " repo_log and repo_read read other repositories of this forge that the " + base + " references, such as the source of a self-built image or chart."
	}
	return s
}

// repoNameChars are those of an owner or repository name: the class
// around "git.example.com/owner/name" in the base commit.
const repoNameChars = "A-Za-z0-9._-"

var repoPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}/[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// openRepo fetches the given tags or branches of another repository of the
// forge, with the forge's credentials: the bot reads what it may read on
// the forge. The repository must be the one under review, or appear in
// the base commit as "<forge host and path>/<owner>/<name>" (a chart's
// sources, a link), so the model can only name repositories already in use.
func (w *workspace) openRepo(ctx context.Context, repo string, refs []string) (*gitrepo.Repo, map[string]string, []string, error) {
	repo = strings.TrimSuffix(strings.Trim(strings.TrimSpace(repo), "/"), ".git")
	if !repoPattern.MatchString(repo) || strings.Contains(repo, "..") {
		return nil, nil, nil, fmt.Errorf("invalid repository %q: want owner/name", repo)
	}
	owner, name, _ := strings.Cut(repo, "/")
	id := forge.Repo{Owner: owner, Name: name}
	if !strings.EqualFold(id.String(), w.repoID.String()) {
		u, err := url.Parse(w.forge.ServerURL())
		if err != nil {
			return nil, nil, nil, fmt.Errorf("forge URL: %w", err)
		}
		// A forge may be served under a path: git.example.com/gitea/o/r.
		root := strings.Trim(u.Host+u.Path, "/")
		found := false
		for _, literal := range []string{root + "/" + repo, root + "/" + repo + ".git"} {
			ok, err := w.repo.ContainsBounded(ctx, w.baseSHA, literal, repoNameChars)
			if err != nil {
				return nil, nil, nil, err
			}
			found = found || ok
		}
		if !found {
			return nil, nil, nil, fmt.Errorf("%s/%s does not appear in the repository's base branch: only repositories it references can be read", root, repo)
		}
	}
	remote, err := w.forge.GitRemote(ctx, id)
	if err != nil {
		return nil, nil, nil, err
	}
	return w.repos.FetchRefs(ctx, remote, refs)
}

// Character classes around a hostname or an image name in the base
// commit: "community.github.io" is not "prometheus-community.github.io",
// "haals/yopass" is not "jhaals/yopass".
const (
	hostChars  = "A-Za-z0-9.-"
	imageChars = "A-Za-z0-9._/-"
)

// pagePolicy admits a page whose host appears in the base commit, or is
// on fetchHosts. Query strings are refused, so the model can only choose
// a path on a host already in use.
func (w *workspace) pagePolicy(ctx context.Context, rawURL string) error {
	host, err := remote.PageHost(rawURL)
	if err != nil {
		return err
	}
	if w.allowedHost(host) {
		return nil
	}
	found, err := w.repo.ContainsBounded(ctx, w.baseSHA, host, hostChars)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%s does not appear in the repository's base branch and is not an allowed host: only hosts the repository already references can be read", host)
	}
	return nil
}

// imagePolicy admits an image named in the base commit, or hosted on a
// registry on fetchHosts.
func (w *workspace) imagePolicy(ctx context.Context, image string) error {
	host, _, err := remote.ParseImage(image)
	if err != nil {
		return err
	}
	if w.allowedHost(host) {
		return nil
	}
	found, err := w.repo.ContainsBounded(ctx, w.baseSHA, image, imageChars)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("image %s does not appear in the repository's base branch: only images the repository already uses can be looked up", image)
	}
	return nil
}

func (w *workspace) allowedHost(host string) bool {
	return slices.ContainsFunc(w.fetchHosts, func(h string) bool { return strings.EqualFold(h, host) })
}

// chartPolicy admits a chart repository URL that appears in the base
// commit, which a pull request cannot change, or whose host is on
// fetchHosts. The model can then only name repositories already in use,
// and cannot smuggle data out in a URL.
func (w *workspace) chartPolicy(ctx context.Context, repoURL string) error {
	norm, host, err := charts.NormalizeRepoURL(repoURL)
	if err != nil {
		return err
	}
	if w.allowedHost(host) {
		return nil
	}
	found, err := w.repo.Contains(ctx, w.baseSHA, norm)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%s does not appear in the repository's base branch, and %s is not an allowed host: only chart repositories the repository already uses can be read", norm, host)
	}
	return nil
}

// changed lists the changed files, with the diffs that fit the prompt's
// share of the context window.
func (w *workspace) changed(ctx context.Context) []changedFile {
	peek := func(path string) []byte {
		content, err := w.repo.ReadFile(ctx, w.headSHA, path)
		if err != nil {
			return nil
		}
		return content[:min(len(content), headPeekBytes)]
	}
	budget := int(float64(w.contextTokens) * diffBudgetShare * charsPerToken)
	return selectDiffs(w.files, budget, peek)
}

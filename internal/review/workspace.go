package review

import (
	"context"
	"errors"
	"fmt"
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
// repository guidance.
type workspace struct {
	repo          *gitrepo.Repo
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

	w := &workspace{
		repo: repo, baseSHA: baseSHA, headSHA: headSHA, files: files, byPath: map[string]*diff.File{},
		contextTokens: r.ContextTokens, charts: r.Charts, remote: r.Remote, fetchHosts: r.FetchHosts,
	}
	for _, file := range files {
		w.byPath[file.Path()] = file
		if file.Status == diff.Renamed {
			w.byPath[file.OldPath] = file
		}
	}
	for _, name := range guidanceFiles {
		if content, err := repo.ReadFile(ctx, baseSHA, name); err == nil {
			w.guidanceName, w.guidance = name, string(content)
			break
		}
	}
	return w, "", nil
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
	if w.charts != nil {
		out = append(out, tools.Charts(w.charts, w.chartPolicy)...)
	}
	if w.remote != nil {
		out = append(out, tools.Web(w.remote, tools.WebPolicy{Page: w.pagePolicy, Image: w.imagePolicy})...)
	}
	return out
}

// toolsSentence tells the model which tools it has.
func (w *workspace) toolsSentence() string {
	s := "You have read-only tools over the repository at the pull request's head commit: read_file, list_dir, grep, git_log and get_file_diff."
	if w.charts != nil {
		s += " chart_files, chart_read and chart_diff read published Helm chart versions, from the chart repositories the repository's base branch references."
	}
	if w.remote != nil {
		s += " fetch_url reads documentation pages on hosts the base branch references, and release_notes the GitHub release notes of a container image it references."
	}
	return s
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

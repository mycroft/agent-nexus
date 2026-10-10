package review

import (
	"context"
	"errors"
	"fmt"

	"github.com/mycroft/agent-nexus/internal/diff"
	"github.com/mycroft/agent-nexus/internal/forge"
	"github.com/mycroft/agent-nexus/internal/gitrepo"
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

	w := &workspace{repo: repo, baseSHA: baseSHA, headSHA: headSHA, files: files, byPath: map[string]*diff.File{}, contextTokens: r.ContextTokens}
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
	return tools.Repo(w.repo, w.headSHA, w.fileDiff)
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

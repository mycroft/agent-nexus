package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/mycroft/agent-nexus/internal/gitrepo"
	"github.com/mycroft/agent-nexus/internal/remote"
)

const (
	maxRepoLogCommits = 100
	maxRepoLogBytes   = 10 << 10
	maxRepoStatBytes  = 3 << 10
)

// RepoOpener opens another repository of the forge under review, with the
// given tags or branches fetched: it returns the repository, the commit of
// each name found, and the repository's tags.
type RepoOpener func(ctx context.Context, repo string, refs []string) (*gitrepo.Repo, map[string]string, []string, error)

const repoParam = `"repo":{"type":"string","description":"owner/name of a repository on this forge that the repository under review references, e.g. in a chart's sources"}`

// Repos returns tools reading other repositories of the forge: the source
// of a self-built image or chart, for instance.
func Repos(open RepoOpener) []Tool {
	// resolve opens repo at the given versions, each as a tag or branch,
	// with or without a "v" prefix.
	resolve := func(ctx context.Context, repo string, versions ...string) (*gitrepo.Repo, []string, error) {
		var names []string
		for _, v := range versions {
			names = append(names, refCandidates(v)...)
		}
		r, commits, tags, err := open(ctx, repo, names)
		if err != nil {
			return nil, nil, err
		}
		out := make([]string, len(versions))
		for i, v := range versions {
			for _, name := range refCandidates(v) {
				if sha := commits[name]; sha != "" {
					out[i] = sha
					break
				}
			}
			if out[i] == "" {
				remote.SortVersions(tags)
				return nil, nil, fmt.Errorf("%s has no tag or branch %q; its latest tags: %s", repo, v, strings.Join(tags[:min(len(tags), 10)], ", "))
			}
		}
		return r, out, nil
	}
	return []Tool{
		{
			Name:        "repo_log",
			Description: "List the commits of another repository on this forge between two tags or branches (after from, up to to), with their messages and the files changed: the changelog of a self-built image or chart between two versions. A version also matches its tag with a \"v\" prefix.",
			Parameters: json.RawMessage(`{"type":"object","properties":{` + repoParam + `,` +
				`"from":{"type":"string","description":"Old version, tag or branch"},` +
				`"to":{"type":"string","description":"New version, tag or branch"}},` +
				`"required":["repo","from","to"]}`),
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Repo string `json:"repo"`
					From string `json:"from"`
					To   string `json:"to"`
				}
				if err := decode(raw, &args); err != nil {
					return "", err
				}
				r, shas, err := resolve(ctx, args.Repo, args.From, args.To)
				if err != nil {
					return "", err
				}
				log, err := r.LogRange(ctx, shas[0], shas[1], maxRepoLogCommits)
				if err != nil && !errors.Is(err, gitrepo.ErrTruncated) {
					return "", err
				}
				stat, err := r.DiffStat(ctx, shas[0], shas[1])
				if err != nil && !errors.Is(err, gitrepo.ErrTruncated) {
					return "", err
				}
				if strings.TrimSpace(log) == "" {
					return fmt.Sprintf("%s has no commit after %s up to %s", args.Repo, args.From, args.To), nil
				}
				return fmt.Sprintf("Commits of %s after %s, up to %s, newest first:\n\n%s\nFiles changed:\n%s",
					args.Repo, args.From, args.To, cut(log, maxRepoLogBytes), cut(stat, maxRepoStatBytes)), nil
			},
		},
		{
			Name:        "repo_read",
			Description: "Read a file of another repository on this forge at a tag or branch, with line numbers (e.g. its CHANGELOG.md at a release). Defaults to the first 400 lines; use start_line/end_line for more.",
			Parameters: json.RawMessage(`{"type":"object","properties":{` + repoParam + `,` +
				`"ref":{"type":"string","description":"Version, tag or branch"},` +
				`"path":{"type":"string","description":"File path in that repository"},` +
				`"start_line":{"type":"integer","description":"First line (1-based), default 1"},` +
				`"end_line":{"type":"integer","description":"Last line, inclusive"}},` +
				`"required":["repo","ref","path"]}`),
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Repo      string `json:"repo"`
					Ref       string `json:"ref"`
					Path      string `json:"path"`
					StartLine int    `json:"start_line"`
					EndLine   int    `json:"end_line"`
				}
				if err := decode(raw, &args); err != nil {
					return "", err
				}
				r, shas, err := resolve(ctx, args.Repo, args.Ref)
				if err != nil {
					return "", err
				}
				content, err := r.ReadFile(ctx, shas[0], args.Path)
				if err != nil {
					return "", err
				}
				return numberLines(string(content), args.StartLine, args.EndLine), nil
			},
		},
	}
}

// refCandidates names the refs a version may be: as given, and with or
// without its "v" prefix.
func refCandidates(v string) []string {
	v = strings.TrimSpace(v)
	if trimmed, ok := strings.CutPrefix(v, "v"); ok && trimmed != "" {
		return []string{v, trimmed}
	}
	return []string{v, "v" + v}
}

func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n[…cut; narrow the range for the rest]\n"
}

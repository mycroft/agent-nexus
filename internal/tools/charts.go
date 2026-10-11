package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/mycroft/agent-nexus/internal/charts"
	"github.com/mycroft/agent-nexus/internal/gitrepo"
)

const (
	maxChartListing = 400
	// chartDiffBudget keeps a whole-chart diff under the agent's tool
	// output cap, so it ends with the list of omitted files.
	chartDiffBudget = 12 << 10
)

// ChartPolicy returns an error when the model may not read a chart
// repository: the URL ends up in requests, so it must not be free text.
type ChartPolicy func(ctx context.Context, repoURL string) error

// chartPriority orders a chart diff: metadata and defaults first.
var chartPriority = []string{"Chart.yaml", "values.yaml", "values.schema.json", "Chart.lock", "CHANGELOG.md", "README.md"}

const chartParams = `"repo_url":{"type":"string","description":"Chart repository as the repository under review declares it: https://… for a classic Helm repository, oci://… for an OCI registry path (without the chart name)"},` +
	`"chart":{"type":"string","description":"Chart name"}`

// Charts returns tools reading Helm chart packages from the repositories
// that allow admits.
func Charts(f *charts.Fetcher, allow ChartPolicy) []Tool {
	load := func(ctx context.Context, repoURL, name, version string) (*charts.Chart, error) {
		if err := allow(ctx, repoURL); err != nil {
			return nil, err
		}
		return f.Load(ctx, repoURL, name, version)
	}
	return []Tool{
		{
			Name:        "chart_files",
			Description: "List the files of a Helm chart version, with their sizes. Subcharts appear under charts/<name>/.",
			Parameters: json.RawMessage(`{"type":"object","properties":{` + chartParams + `,` +
				`"version":{"type":"string","description":"Chart version"}},` +
				`"required":["repo_url","chart","version"]}`),
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					RepoURL string `json:"repo_url"`
					Chart   string `json:"chart"`
					Version string `json:"version"`
				}
				if err := decode(raw, &args); err != nil {
					return "", err
				}
				c, err := load(ctx, args.RepoURL, args.Chart, args.Version)
				if err != nil {
					return "", err
				}
				var b strings.Builder
				paths := c.Paths()
				fmt.Fprintf(&b, "%s %s: %d files\n", c.Name, c.Version, len(paths))
				for _, p := range paths[:min(len(paths), maxChartListing)] {
					fmt.Fprintf(&b, "%s (%d bytes)\n", p, len(c.Files[p]))
				}
				if len(paths) > maxChartListing {
					fmt.Fprintf(&b, "(%d more files)\n", len(paths)-maxChartListing)
				}
				return b.String(), nil
			},
		},
		{
			Name:        "chart_read",
			Description: "Read a file of a Helm chart version, with line numbers. Defaults to the first 400 lines; use start_line/end_line for other ranges.",
			Parameters: json.RawMessage(`{"type":"object","properties":{` + chartParams + `,` +
				`"version":{"type":"string","description":"Chart version"},` +
				`"path":{"type":"string","description":"File path in the chart, e.g. values.yaml or templates/deployment.yaml"},` +
				`"start_line":{"type":"integer","description":"First line (1-based), default 1"},` +
				`"end_line":{"type":"integer","description":"Last line, inclusive"}},` +
				`"required":["repo_url","chart","version","path"]}`),
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					RepoURL   string `json:"repo_url"`
					Chart     string `json:"chart"`
					Version   string `json:"version"`
					Path      string `json:"path"`
					StartLine int    `json:"start_line"`
					EndLine   int    `json:"end_line"`
				}
				if err := decode(raw, &args); err != nil {
					return "", err
				}
				c, err := load(ctx, args.RepoURL, args.Chart, args.Version)
				if err != nil {
					return "", err
				}
				p := chartPath(args.Path)
				content, ok := c.Files[p]
				if !ok {
					return "", fmt.Errorf("%s %s has no file %q; list them with chart_files", c.Name, c.Version, p)
				}
				return numberLines(string(content), args.StartLine, args.EndLine), nil
			},
		},
		{
			Name:        "chart_diff",
			Description: "Diff two versions of a Helm chart: which files changed, then their unified diffs, Chart.yaml and values.yaml first. Pass path for a single file's full diff.",
			Parameters: json.RawMessage(`{"type":"object","properties":{` + chartParams + `,` +
				`"from_version":{"type":"string","description":"Old chart version"},` +
				`"to_version":{"type":"string","description":"New chart version"},` +
				`"path":{"type":"string","description":"Optional file path in the chart"}},` +
				`"required":["repo_url","chart","from_version","to_version"]}`),
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					RepoURL     string `json:"repo_url"`
					Chart       string `json:"chart"`
					FromVersion string `json:"from_version"`
					ToVersion   string `json:"to_version"`
					Path        string `json:"path"`
				}
				if err := decode(raw, &args); err != nil {
					return "", err
				}
				from, err := load(ctx, args.RepoURL, args.Chart, args.FromVersion)
				if err != nil {
					return "", err
				}
				to, err := load(ctx, args.RepoURL, args.Chart, args.ToVersion)
				if err != nil {
					return "", err
				}
				if args.Path != "" {
					p := chartPath(args.Path)
					d, err := gitrepo.DiffBlobs(ctx, p, from.Files[p], to.Files[p])
					if err != nil {
						return "", err
					}
					if d == "" {
						return fmt.Sprintf("%s is the same in %s and %s", p, from.Version, to.Version), nil
					}
					return d, nil
				}
				return chartDiff(ctx, from, to)
			},
		},
	}
}

func chartPath(p string) string {
	return strings.TrimPrefix(path.Clean("/"+strings.TrimSpace(p)), "/")
}

// chartDiff lists the files that differ between two chart versions, then
// their diffs in priority order while they fit the budget.
func chartDiff(ctx context.Context, from, to *charts.Chart) (string, error) {
	var changed []string
	status := map[string]string{}
	for p, content := range to.Files {
		old, ok := from.Files[p]
		switch {
		case !ok:
			status[p] = "added"
		case string(old) != string(content):
			status[p] = "changed"
		default:
			continue
		}
		changed = append(changed, p)
	}
	for p := range from.Files {
		if _, ok := to.Files[p]; !ok {
			status[p] = "removed"
			changed = append(changed, p)
		}
	}
	if len(changed) == 0 {
		return fmt.Sprintf("%s %s and %s have the same files", to.Name, from.Version, to.Version), nil
	}
	rank := func(p string) int {
		if i := slices.Index(chartPriority, p); i >= 0 {
			return i
		}
		if strings.HasPrefix(p, "templates/") {
			return len(chartPriority)
		}
		return len(chartPriority) + 1
	}
	slices.SortFunc(changed, func(a, b string) int {
		if d := rank(a) - rank(b); d != 0 {
			return d
		}
		return strings.Compare(a, b)
	})

	var b strings.Builder
	fmt.Fprintf(&b, "%s %s → %s: %d files differ\n", to.Name, from.Version, to.Version, len(changed))
	for _, p := range changed[:min(len(changed), 100)] {
		fmt.Fprintf(&b, "  %s %s\n", status[p], p)
	}
	if len(changed) > 100 {
		fmt.Fprintf(&b, "  (%d more)\n", len(changed)-100)
	}
	b.WriteString("\n")
	// A diff too large for what is left is skipped, not the ones after it.
	var omitted []string
	for _, p := range changed {
		if chartDiffBudget-b.Len() < 256 {
			omitted = append(omitted, p)
			continue
		}
		d, err := gitrepo.DiffBlobs(ctx, p, from.Files[p], to.Files[p])
		if err != nil {
			return "", err
		}
		if b.Len()+len(d) > chartDiffBudget {
			omitted = append(omitted, p)
			continue
		}
		b.WriteString(d)
	}
	if len(omitted) > 0 {
		fmt.Fprintf(&b, "\n(diffs omitted, call chart_diff with path for them: %s)\n", strings.Join(omitted[:min(len(omitted), 30)], ", "))
	}
	return b.String(), nil
}

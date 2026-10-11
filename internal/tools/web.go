package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mycroft/agent-nexus/internal/remote"
)

const (
	releaseNotesBudget = 12 << 10
	releaseNotesEach   = 4 << 10
)

// WebPolicy decides what the model may fetch: the hosts of pages, and the
// images whose release notes it may read. Requests carry these, so they
// must not be free text.
type WebPolicy struct {
	Page  func(ctx context.Context, rawURL string) error
	Image func(ctx context.Context, image string) error
}

// Web returns tools reading documentation pages and release notes.
func Web(c *remote.Client, policy WebPolicy) []Tool {
	return []Tool{
		{
			Name:        "fetch_url",
			Description: "Read a documentation page (upgrade guide, changelog, README) as text, with line numbers. Defaults to the first 400 lines; use start_line/end_line for more. Only hosts the repository's base branch references can be read, without query strings.",
			Parameters: json.RawMessage(`{"type":"object","properties":{` +
				`"url":{"type":"string","description":"https:// URL of the page"},` +
				`"start_line":{"type":"integer","description":"First line (1-based), default 1"},` +
				`"end_line":{"type":"integer","description":"Last line, inclusive"}},` +
				`"required":["url"]}`),
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					URL       string `json:"url"`
					StartLine int    `json:"start_line"`
					EndLine   int    `json:"end_line"`
				}
				if err := decode(raw, &args); err != nil {
					return "", err
				}
				args.URL = strings.TrimSpace(args.URL)
				if err := policy.Page(ctx, args.URL); err != nil {
					return "", err
				}
				page, err := c.FetchPage(ctx, args.URL)
				if err != nil {
					return "", err
				}
				var b strings.Builder
				if page.Title != "" {
					fmt.Fprintf(&b, "Title: %s\n", page.Title)
				}
				fmt.Fprintf(&b, "URL: %s\n\n", page.URL)
				b.WriteString(numberLines(page.Text, args.StartLine, args.EndLine))
				return b.String(), nil
			},
		},
		{
			Name:        "release_notes",
			Description: "Read the GitHub release notes of a container image's project between two versions: the releases after from_version, up to to_version. The project comes from the image's org.opencontainers.image.source label, or its ghcr.io name.",
			Parameters: json.RawMessage(`{"type":"object","properties":{` +
				`"image":{"type":"string","description":"Image name as the repository writes it, without tag, e.g. ghcr.io/gethomepage/homepage or jhaals/yopass"},` +
				`"from_version":{"type":"string","description":"Old image tag"},` +
				`"to_version":{"type":"string","description":"New image tag"}},` +
				`"required":["image","from_version","to_version"]}`),
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Image       string `json:"image"`
					FromVersion string `json:"from_version"`
					ToVersion   string `json:"to_version"`
				}
				if err := decode(raw, &args); err != nil {
					return "", err
				}
				image := strings.TrimSpace(args.Image)
				if err := policy.Image(ctx, image); err != nil {
					return "", err
				}
				for _, v := range []string{args.FromVersion, args.ToVersion} {
					if !remote.ValidTag(v) {
						return "", fmt.Errorf("invalid image tag %q", v)
					}
				}
				owner, repo, err := imageProject(ctx, c, image, args.ToVersion)
				if err != nil {
					return "", err
				}
				releases, latest, err := c.Releases(ctx, owner, repo, args.FromVersion, args.ToVersion)
				if err != nil {
					return "", err
				}
				if len(releases) == 0 {
					return fmt.Sprintf("github.com/%s/%s has no release after %s up to %s; its latest release tags: %s",
						owner, repo, args.FromVersion, args.ToVersion, strings.Join(latest, ", ")), nil
				}
				header := fmt.Sprintf("Releases of github.com/%s/%s after %s, up to %s:\n\n", owner, repo, args.FromVersion, args.ToVersion)
				return header + remote.FormatReleases(releases, releaseNotesBudget, releaseNotesEach), nil
			},
		},
	}
}

// imageProject finds the GitHub project of an image: its source label, or
// for ghcr.io its owner and name.
func imageProject(ctx context.Context, c *remote.Client, image, tag string) (string, string, error) {
	// Without labels, a ghcr.io name still identifies the project.
	labels, err := c.ImageLabels(ctx, image, tag)
	if source := labels[remote.SourceLabel]; source != "" {
		if owner, repo, ok := remote.GitHubRepo(source); ok {
			return owner, repo, nil
		}
		return "", "", fmt.Errorf("%s's source is %s: release notes are only read from GitHub", image, source)
	}
	host, repository, perr := remote.ParseImage(image)
	if perr == nil && host == "ghcr.io" {
		if parts := strings.Split(repository, "/"); len(parts) >= 2 {
			return parts[0], parts[1], nil
		}
	}
	if err != nil {
		return "", "", err
	}
	return "", "", fmt.Errorf("%s:%s has no %s label: its project is unknown", image, tag, remote.SourceLabel)
}

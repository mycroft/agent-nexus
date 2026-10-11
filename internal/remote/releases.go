package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const maxReleasesBytes = 8 << 20

var (
	numberRun = regexp.MustCompile(`\d+`)
	// preRelease matches a pre-release marker between separators or
	// digits: "-rc1", ".beta.2", "0rc1", but not the "rc" of "source".
	preRelease = regexp.MustCompile(`(?i)(^|[-._+\d])(alpha|beta|rc|pre|preview|dev|nightly)([-._]?\d+)?($|[-._+])`)
)

// Release is a GitHub release.
type Release struct {
	Tag        string    `json:"tag_name"`
	Name       string    `json:"name"`
	Body       string    `json:"body"`
	Published  time.Time `json:"published_at"`
	Prerelease bool      `json:"prerelease"`
	Draft      bool      `json:"draft"`
}

// Releases returns the published releases of a GitHub repository whose
// version is after from and up to to, newest first. Pre-releases count
// only when to is one. Versions compare by their numbers, so "v2.5"
// matches the release "v2.5.0" and "v25.12.8-ls249" follows "-ls248". The
// second result lists the latest release tags, to help when none match.
func (c *Client) Releases(ctx context.Context, owner, repo, from, to string) ([]Release, []string, error) {
	if !githubName.MatchString(owner) || !githubName.MatchString(repo) {
		return nil, nil, fmt.Errorf("invalid GitHub repository %s/%s", owner, repo)
	}
	resp, err := c.Get(ctx, Request{
		URL:    fmt.Sprintf("%s/repos/%s/%s/releases?per_page=100", c.githubAPI, owner, repo),
		Accept: "application/vnd.github+json",
		Header: map[string]string{"X-GitHub-Api-Version": "2022-11-28"},
		Limit:  maxReleasesBytes,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("releases of %s/%s: %w", owner, repo, err)
	}
	var all []Release
	if err := json.Unmarshal(resp.Body, &all); err != nil {
		return nil, nil, fmt.Errorf("releases of %s/%s: %w", owner, repo, err)
	}
	lo, hi := versionKey(from), versionKey(to)
	if len(lo) == 0 || len(hi) == 0 {
		return nil, nil, fmt.Errorf("versions %q and %q need numbers to compare", from, to)
	}
	wantPre := isPrerelease(to)
	var out []Release
	var latest []string
	for _, r := range all {
		if r.Draft {
			continue
		}
		if len(latest) < 10 {
			latest = append(latest, r.Tag)
		}
		if (r.Prerelease || isPrerelease(r.Tag)) && !wantPre {
			continue
		}
		v := versionKey(r.Tag)
		if len(v) > 0 && compareVersions(v, lo) > 0 && compareVersions(v, hi) <= 0 {
			out = append(out, r)
		}
	}
	slices.SortStableFunc(out, func(a, b Release) int { return compareVersions(versionKey(b.Tag), versionKey(a.Tag)) })
	return out, latest, nil
}

// versionKey returns the numbers of a version or tag, pre-release
// markers aside: "kube-prometheus-stack-92.3.0" → [92 3 0].
func versionKey(s string) []int {
	s = preRelease.ReplaceAllString(s, "${1}${4}")
	var key []int
	for _, m := range numberRun.FindAllString(s, 8) {
		n, err := strconv.Atoi(m)
		if err != nil {
			return nil
		}
		key = append(key, n)
	}
	return key
}

func isPrerelease(s string) bool { return preRelease.MatchString(s) }

// SortVersions orders tags newest version first, as Releases compares them.
func SortVersions(tags []string) {
	slices.SortStableFunc(tags, func(a, b string) int { return compareVersions(versionKey(b), versionKey(a)) })
}

// compareVersions compares number lists, missing numbers counting as 0.
func compareVersions(a, b []int) int {
	for i := range max(len(a), len(b)) {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			return x - y
		}
	}
	return 0
}

// FormatReleases renders releases as markdown within budget bytes, each
// body cut to perRelease bytes, and names the ones left out.
func FormatReleases(releases []Release, budget, perRelease int) string {
	var b strings.Builder
	var omitted []string
	for _, r := range releases {
		body := strings.TrimSpace(strings.ReplaceAll(r.Body, "\r\n", "\n"))
		if len(body) > perRelease {
			body = body[:perRelease] + "\n[…release notes cut]"
		}
		var entry strings.Builder
		fmt.Fprintf(&entry, "## %s", r.Tag)
		if name := strings.TrimSpace(r.Name); name != "" && name != r.Tag {
			fmt.Fprintf(&entry, " — %s", name)
		}
		if !r.Published.IsZero() {
			fmt.Fprintf(&entry, " (%s)", r.Published.Format("2006-01-02"))
		}
		entry.WriteString("\n\n" + body + "\n\n")
		if b.Len()+entry.Len() > budget {
			omitted = append(omitted, r.Tag)
			continue
		}
		b.WriteString(entry.String())
	}
	if len(omitted) > 0 {
		fmt.Fprintf(&b, "(left out for space: %s)\n", strings.Join(omitted, ", "))
	}
	return b.String()
}

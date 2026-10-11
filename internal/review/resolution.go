package review

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/mycroft/agent-nexus/internal/forge"
)

// A review lists its findings in a marker of its body. Discussion answers
// record the findings they settle in markers of their own, and once every
// finding of the latest review is settled, a verdict-mode repository gets
// an approval: the verdict follows the discussion without a new review.

var (
	findingsMarkerPattern = regexp.MustCompile(`<!-- agent-nexus:findings=(\[.*?\]) -->`)
	resolvesMarkerPattern = regexp.MustCompile(`<!-- agent-nexus:resolves=(review:\d+)#(\d+(?:,\d+)*) -->`)
)

// listedFinding is a finding as its review's marker lists it.
type listedFinding struct {
	Severity string `json:"severity"`
	Title    string `json:"title"`
	Path     string `json:"path,omitempty"`
	Line     int    `json:"line,omitempty"`
}

// findingsMarker lists findings, in the order they are numbered from 1.
// encoding/json escapes '>', so the list cannot end the HTML comment.
func findingsMarker(findings []Finding) string {
	listed := make([]listedFinding, 0, len(findings))
	for _, f := range findings {
		listed = append(listed, listedFinding{Severity: f.Severity, Title: oneLine(f.Title), Path: f.Path, Line: f.Line})
	}
	raw, err := json.Marshal(listed)
	if err != nil {
		panic(err) // strings and ints always marshal
	}
	return "<!-- agent-nexus:findings=" + string(raw) + " -->"
}

// resolvesMarker records the findings, numbered in the review with the
// given note key, that an answer settles.
func resolvesMarker(reviewKey string, numbers []int) string {
	parts := make([]string, 0, len(numbers))
	for _, n := range numbers {
		parts = append(parts, strconv.Itoa(n))
	}
	return "<!-- agent-nexus:resolves=" + reviewKey + "#" + strings.Join(parts, ",") + " -->"
}

// reviewState is the bot's latest review of a commit, and how far the
// discussion settled its findings.
type reviewState struct {
	// Key is the review's note key, which answers refer to.
	Key      string
	HeadSHA  string
	Approved bool
	Findings []listedFinding
	// Resolved holds the numbers (from 1) of the settled findings.
	Resolved map[int]bool
}

// open returns the numbers of the findings still open.
func (s *reviewState) open() []int {
	var out []int
	for i := range s.Findings {
		if !s.Resolved[i+1] {
			out = append(out, i+1)
		}
	}
	return out
}

// settled reports whether the review has findings and the discussion
// settled them all, without the review approving already.
func (s *reviewState) settled() bool {
	return !s.Approved && len(s.Findings) > 0 && len(s.open()) == 0
}

// reviewsCommit reports whether a review body carries the marker of a
// review of headSHA: the commit's, a forced review's or a verdict update's.
func reviewsCommit(body, headSHA string) bool {
	return strings.Contains(body, marker(headSHA)) || strings.Contains(body, "<!-- agent-nexus:sha="+headSHA+":")
}

// latestReview returns the state of the bot's latest review of headSHA,
// or nil when there is none.
func latestReview(notes []forge.Note, login, headSHA string) *reviewState {
	var latest *forge.Note
	for i, n := range notes {
		if n.Kind == forge.NoteReview && isBot(n.User, login) && reviewsCommit(n.Body, headSHA) {
			latest = &notes[i]
		}
	}
	if latest == nil {
		return nil
	}
	s := &reviewState{Key: latest.Key(), HeadSHA: headSHA, Approved: latest.Approved, Resolved: map[int]bool{}}
	if m := findingsMarkerPattern.FindStringSubmatch(latest.Body); m != nil {
		// A list that does not parse leaves the review without findings
		// to settle: it is never approved by a verdict update.
		_ = json.Unmarshal([]byte(m[1]), &s.Findings)
	}
	for _, n := range notes {
		if !isBot(n.User, login) {
			continue
		}
		for _, m := range resolvesMarkerPattern.FindAllStringSubmatch(n.Body, -1) {
			if m[1] != s.Key {
				continue
			}
			for num := range strings.SplitSeq(m[2], ",") {
				if i, err := strconv.Atoi(num); err == nil && i >= 1 && i <= len(s.Findings) {
					s.Resolved[i] = true
				}
			}
		}
	}
	return s
}

// openFindings keeps the numbers that name open findings of s, once each.
func (s *reviewState) openFindings(numbers []int) []int {
	var out []int
	for _, n := range numbers {
		if n >= 1 && n <= len(s.Findings) && !s.Resolved[n] && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}

// findingsSection lists the findings of the latest review for a
// discussion prompt, or returns "" when there is nothing open to settle.
func findingsSection(s *reviewState) string {
	if s == nil || s.Approved || len(s.open()) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n## Findings of your latest review\n\nYour latest review of `%s` listed these findings:\n\n", short(s.HeadSHA))
	for i, f := range s.Findings {
		status := "open"
		if s.Resolved[i+1] {
			status = "settled"
		}
		loc := ""
		if f.Path != "" {
			loc = " (" + f.Path
			if f.Line > 0 {
				loc += ":" + strconv.Itoa(f.Line)
			}
			loc += ")"
		}
		fmt.Fprintf(&b, "%d. [%s] %s%s: %s\n", i+1, f.Severity, f.Title, loc, status)
	}
	return b.String()
}

// approveIfSettled submits an approval when the discussion settled every
// finding of s, the bot's latest review of the pull request's head, in a
// repository whose reviews carry a verdict. It reports whether it
// approved. It is idempotent: the approval becomes the latest review.
func (r *Reviewer) approveIfSettled(ctx context.Context, f forge.Forge, repoID forge.Repo, pr forge.PR, s *reviewState, checkout func() (*workspace, error), jobID int64) (bool, error) {
	if s == nil || !s.settled() {
		return false, nil
	}
	// The pull request may have moved on while the answers were written.
	current, err := f.GetPR(ctx, repoID, pr.Number)
	if err != nil {
		return false, err
	}
	if checkPR(current, s.HeadSHA) != "" {
		return false, nil
	}
	ws, err := checkout()
	if err != nil {
		return false, err
	}
	if mode, _, _ := r.reviewMode(ctx, ws.repo, ws.baseSHA, repoID); mode != ModeVerdict {
		return false, nil
	}
	if err := f.DeletePendingReviews(ctx, repoID, pr.Number); err != nil {
		return false, err
	}
	err = f.CreateReview(ctx, repoID, pr.Number, forge.NewReview{CommitSHA: s.HeadSHA, Event: forge.EventApprove, Body: approvalBody(s, jobID)})
	if err != nil {
		return false, fmt.Errorf("submit the verdict update: %w", err)
	}
	r.logger().InfoContext(ctx, "verdict updated", "repo", repoID.String(), "pr", pr.Number, "review", s.Key)
	return true, nil
}

func approvalBody(s *reviewState, jobID int64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Verdict update\n\nThe discussion settled every finding of my review of `%s`:\n\n", short(s.HeadSHA))
	for _, f := range s.Findings {
		fmt.Fprintf(&b, "- **[%s]** %s\n", f.Severity, f.Title)
	}
	b.WriteString("\n")
	writeVerdict(&b, &verdict{Event: forge.EventApprove})
	fmt.Fprintf(&b, "\n---\n<sub>agent-nexus · verdict update for %s · job %d</sub>\n<!-- agent-nexus:sha=%s:settled=%s -->\n",
		short(s.HeadSHA), jobID, s.HeadSHA, s.Key)
	return b.String()
}

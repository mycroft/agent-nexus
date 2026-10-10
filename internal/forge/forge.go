// Package forge talks to code-hosting platforms (Gitea, GitHub) for the few
// operations agent-nexus needs: read a pull request, list and create reviews,
// and get credentials for fetching the repository.
package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/mycroft/agent-nexus/internal/gitrepo"
	"github.com/mycroft/agent-nexus/internal/httpclient"
	"github.com/mycroft/agent-nexus/internal/metrics"
)

// ErrNotFound is returned when the forge answers 404.
var ErrNotFound = errors.New("not found")

var namePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// Repo identifies a repository.
type Repo struct {
	Owner string
	Name  string
}

// ParseRepo parses and validates "owner/name".
func ParseRepo(s string) (Repo, error) {
	owner, name, ok := strings.Cut(s, "/")
	if !ok || !validName(owner) || !validName(name) {
		return Repo{}, fmt.Errorf("invalid repository %q, want owner/name", s)
	}
	return Repo{Owner: owner, Name: name}, nil
}

func validName(s string) bool {
	return namePattern.MatchString(s) && s != "." && s != ".."
}

func (r Repo) String() string { return r.Owner + "/" + r.Name }

// PR is the pull request data a review needs.
type PR struct {
	Number  int
	Title   string
	Body    string
	Author  string
	State   string // "open" or "closed"
	Draft   bool
	BaseRef string
	HeadSHA string
	URL     string
}

// Review is an existing review on a pull request.
type Review struct {
	ID       int64
	User     string
	Body     string
	State    string
	CommitID string
}

// Comment is an inline review comment on a new-side line.
type Comment struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Body string `json:"body"`
}

// NewReview is a review to create. It is always submitted as a plain
// comment review: agent-nexus never approves or requests changes.
type NewReview struct {
	CommitSHA string    `json:"commit_sha"`
	Body      string    `json:"body"`
	Comments  []Comment `json:"comments"`
}

// Forge is a code-hosting platform.
type Forge interface {
	// Name is the short platform name ("gitea", "github").
	Name() string
	// ServerURL is the web root workflows report as github.server_url.
	ServerURL() string
	// BotLogin is the login reviews are posted under.
	BotLogin(ctx context.Context) (string, error)
	GetPR(ctx context.Context, repo Repo, number int) (PR, error)
	ListReviews(ctx context.Context, repo Repo, number int) ([]Review, error)
	// DeletePendingReviews removes unsubmitted reviews left by the bot
	// after a failed submission.
	DeletePendingReviews(ctx context.Context, repo Repo, number int) error
	CreateReview(ctx context.Context, repo Repo, number int, review NewReview) error
	// GitRemote returns fetch coordinates and credentials for repo.
	GitRemote(ctx context.Context, repo Repo) (gitrepo.Remote, error)
}

// apiClient is the JSON-over-HTTP plumbing shared by both forges.
type apiClient struct {
	forge   string
	baseURL string
	http    *httpclient.Client
}

// do sends a JSON request and decodes a JSON response into out (if not nil).
// auth sets the Authorization header (and any other headers).
func (c *apiClient) do(ctx context.Context, method, path string, auth func(*http.Request), in, out any) error {
	err := c.doRaw(ctx, method, path, auth, in, out)
	if err != nil && !errors.Is(err, ErrNotFound) {
		metrics.ForgeErrorsTotal.WithLabelValues(c.forge).Inc()
	}
	return err
}

func (c *apiClient) doRaw(ctx context.Context, method, path string, auth func(*http.Request), in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("marshal %s %s: %w", method, path, err)
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return fmt.Errorf("build %s %s: %w", method, path, err)
	}
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	auth(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := httpclient.Read(resp, 32<<20)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%s %s: %w", method, path, ErrNotFound)
	}
	if err := httpclient.EnsureOK(resp, raw); err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode %s %s: %w", method, path, err)
	}
	return nil
}

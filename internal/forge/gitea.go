package forge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mycroft/agent-nexus/internal/gitrepo"
	"github.com/mycroft/agent-nexus/internal/httpclient"
)

const giteaPageSize = 50

// Gitea is a Gitea instance accessed with a bot user's token.
type Gitea struct {
	api   apiClient
	url   string
	user  string
	token string

	mu    sync.Mutex
	login string
}

// NewGitea returns a Gitea forge. url is the web root (https://git.mkz.me).
func NewGitea(url, user, token string, opts ...httpclient.Option) *Gitea {
	url = strings.TrimRight(url, "/")
	return &Gitea{
		api:   apiClient{forge: "gitea", baseURL: url + "/api/v1", http: httpclient.New(opts...)},
		url:   url,
		user:  user,
		token: token,
	}
}

// Name implements Forge.
func (g *Gitea) Name() string { return "gitea" }

// ServerURL implements Forge.
func (g *Gitea) ServerURL() string { return g.url }

func (g *Gitea) auth(req *http.Request) {
	req.Header.Set("Authorization", "token "+g.token)
}

type giteaUser struct {
	Login string `json:"login"`
}

// BotLogin implements Forge.
func (g *Gitea) BotLogin(ctx context.Context) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.login != "" {
		return g.login, nil
	}
	var u giteaUser
	if err := g.api.do(ctx, http.MethodGet, "/user", g.auth, nil, &u); err != nil {
		return "", fmt.Errorf("gitea: get bot user: %w", err)
	}
	g.login = u.Login
	return g.login, nil
}

// GetPR implements Forge.
func (g *Gitea) GetPR(ctx context.Context, repo Repo, number int) (PR, error) {
	var pr struct {
		Number  int       `json:"number"`
		Title   string    `json:"title"`
		Body    string    `json:"body"`
		User    giteaUser `json:"user"`
		State   string    `json:"state"`
		Draft   bool      `json:"draft"`
		HTMLURL string    `json:"html_url"`
		Base    struct {
			Ref string `json:"ref"`
		} `json:"base"`
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	path := fmt.Sprintf("/repos/%s/pulls/%d", repo, number)
	if err := g.api.do(ctx, http.MethodGet, path, g.auth, nil, &pr); err != nil {
		return PR{}, fmt.Errorf("gitea: %w", err)
	}
	return PR{
		Number:  pr.Number,
		Title:   pr.Title,
		Body:    pr.Body,
		Author:  pr.User.Login,
		State:   pr.State,
		Draft:   pr.Draft,
		BaseRef: pr.Base.Ref,
		HeadSHA: pr.Head.SHA,
		URL:     pr.HTMLURL,
	}, nil
}

type giteaReview struct {
	ID            int64     `json:"id"`
	User          giteaUser `json:"user"`
	Body          string    `json:"body"`
	State         string    `json:"state"`
	CommitID      string    `json:"commit_id"`
	CommentsCount int       `json:"comments_count"`
	SubmittedAt   time.Time `json:"submitted_at"`
}

func (g *Gitea) listReviews(ctx context.Context, repo Repo, number int) ([]giteaReview, error) {
	var all []giteaReview
	for page := 1; ; page++ {
		var reviews []giteaReview
		path := fmt.Sprintf("/repos/%s/pulls/%d/reviews?page=%d&limit=%d", repo, number, page, giteaPageSize)
		if err := g.api.do(ctx, http.MethodGet, path, g.auth, nil, &reviews); err != nil {
			return nil, fmt.Errorf("gitea: %w", err)
		}
		all = append(all, reviews...)
		if len(reviews) < giteaPageSize {
			return all, nil
		}
	}
}

// ListReviews implements Forge.
func (g *Gitea) ListReviews(ctx context.Context, repo Repo, number int) ([]Review, error) {
	reviews, err := g.listReviews(ctx, repo, number)
	if err != nil {
		return nil, err
	}
	all := make([]Review, 0, len(reviews))
	for _, r := range reviews {
		all = append(all, Review{ID: r.ID, User: r.User.Login, Body: r.Body, State: r.State, CommitID: r.CommitID})
	}
	return all, nil
}

// ListNotes implements Forge. Gitea stores a reply to an inline comment
// in the same review, on the same path and line, so those three identify
// a thread.
func (g *Gitea) ListNotes(ctx context.Context, repo Repo, number int) ([]Note, error) {
	reviews, err := g.listReviews(ctx, repo, number)
	if err != nil {
		return nil, err
	}
	var notes []Note
	for _, r := range reviews {
		if r.State == "PENDING" {
			continue
		}
		if strings.TrimSpace(r.Body) != "" {
			notes = append(notes, Note{Kind: NoteReview, ID: r.ID, User: r.User.Login, Body: r.Body, Approved: r.State == "APPROVED", CreatedAt: r.SubmittedAt})
		}
		if r.CommentsCount == 0 {
			continue
		}
		var comments []struct {
			ID               int64     `json:"id"`
			User             giteaUser `json:"user"`
			Body             string    `json:"body"`
			Path             string    `json:"path"`
			Position         int       `json:"position"`
			OriginalPosition int       `json:"original_position"`
			CreatedAt        time.Time `json:"created_at"`
		}
		path := fmt.Sprintf("/repos/%s/pulls/%d/reviews/%d/comments", repo, number, r.ID)
		if err := g.api.do(ctx, http.MethodGet, path, g.auth, nil, &comments); err != nil {
			return nil, fmt.Errorf("gitea: %w", err)
		}
		for _, c := range comments {
			// position is the new-side line; original_position the
			// old-side one, for comments on removed lines.
			side := c.Position
			if side == 0 {
				side = -c.OriginalPosition
			}
			notes = append(notes, Note{
				Kind: NoteInline, ID: c.ID, User: c.User.Login, Body: c.Body,
				Path: c.Path, Line: c.Position,
				Thread:    fmt.Sprintf("%d:%s:%d", r.ID, c.Path, side),
				CreatedAt: c.CreatedAt,
			})
		}
	}
	var comments []struct {
		ID        int64     `json:"id"`
		User      giteaUser `json:"user"`
		Body      string    `json:"body"`
		CreatedAt time.Time `json:"created_at"`
	}
	err = g.api.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/issues/%d/comments", repo, number), g.auth, nil, &comments)
	var statusErr *httpclient.StatusError
	switch {
	case errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusForbidden:
		// Pull request comments need the read:issue token scope. Review
		// threads still work without them.
		slog.WarnContext(ctx, "gitea: cannot read pull request comments; give the token the read:issue scope", "repo", repo.String(), "pr", number)
	case err != nil:
		return nil, fmt.Errorf("gitea: %w", err)
	}
	for _, c := range comments {
		notes = append(notes, Note{Kind: NoteComment, ID: c.ID, User: c.User.Login, Body: c.Body, CreatedAt: c.CreatedAt})
	}
	sortNotes(notes)
	return notes, nil
}

// ReplyInline implements Forge.
func (g *Gitea) ReplyInline(ctx context.Context, repo Repo, number int, root Note, body string) error {
	path := fmt.Sprintf("/repos/%s/pulls/%d/comments/%d/replies", repo, number, root.ID)
	if err := g.api.do(ctx, http.MethodPost, path, g.auth, map[string]string{"body": body}, nil); err != nil {
		return fmt.Errorf("gitea: reply to comment %d: %w", root.ID, err)
	}
	return nil
}

// Comment implements Forge. It needs the write:issue token scope.
func (g *Gitea) Comment(ctx context.Context, repo Repo, number int, body string) error {
	path := fmt.Sprintf("/repos/%s/issues/%d/comments", repo, number)
	if err := g.api.do(ctx, http.MethodPost, path, g.auth, map[string]string{"body": body}, nil); err != nil {
		return fmt.Errorf("gitea: comment: %w", err)
	}
	return nil
}

// DeletePendingReviews implements Forge. A failed review submission on
// Gitea can leave a pending review behind, which the next submission would
// silently append to.
func (g *Gitea) DeletePendingReviews(ctx context.Context, repo Repo, number int) error {
	login, err := g.BotLogin(ctx)
	if err != nil {
		return err
	}
	reviews, err := g.ListReviews(ctx, repo, number)
	if err != nil {
		return err
	}
	for _, r := range reviews {
		if r.State != "PENDING" || !strings.EqualFold(r.User, login) {
			continue
		}
		path := fmt.Sprintf("/repos/%s/pulls/%d/reviews/%d", repo, number, r.ID)
		if err := g.api.do(ctx, http.MethodDelete, path, g.auth, nil, nil); err != nil {
			return fmt.Errorf("gitea: delete pending review: %w", err)
		}
	}
	return nil
}

// CreateReview implements Forge.
func (g *Gitea) CreateReview(ctx context.Context, repo Repo, number int, review NewReview) error {
	type comment struct {
		Path        string `json:"path"`
		Body        string `json:"body"`
		NewPosition int    `json:"new_position"`
	}
	in := struct {
		Event    string    `json:"event"`
		Body     string    `json:"body"`
		CommitID string    `json:"commit_id"`
		Comments []comment `json:"comments"`
	}{Event: giteaEvent(review.Event), Body: review.Body, CommitID: review.CommitSHA, Comments: []comment{}}
	for _, c := range review.Comments {
		in.Comments = append(in.Comments, comment{Path: c.Path, Body: c.Body, NewPosition: c.Line})
	}
	path := fmt.Sprintf("/repos/%s/pulls/%d/reviews", repo, number)
	if err := g.api.do(ctx, http.MethodPost, path, g.auth, in, nil); err != nil {
		return fmt.Errorf("gitea: create review: %w", err)
	}
	return nil
}

func giteaEvent(e Event) string {
	switch e {
	case EventApprove:
		return "APPROVED"
	case EventRequestChanges:
		return "REQUEST_CHANGES"
	default:
		return "COMMENT"
	}
}

// GitRemote implements Forge.
func (g *Gitea) GitRemote(_ context.Context, repo Repo) (gitrepo.Remote, error) {
	return gitrepo.Remote{
		Key:        "gitea/" + repo.Owner + "/" + repo.Name,
		URL:        g.url + "/" + repo.String() + ".git",
		AuthHeader: gitrepo.BasicAuth(g.user, g.token),
	}, nil
}

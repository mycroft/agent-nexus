package forge

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"

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

// ListReviews implements Forge.
func (g *Gitea) ListReviews(ctx context.Context, repo Repo, number int) ([]Review, error) {
	var all []Review
	for page := 1; ; page++ {
		var reviews []struct {
			ID       int64     `json:"id"`
			User     giteaUser `json:"user"`
			Body     string    `json:"body"`
			State    string    `json:"state"`
			CommitID string    `json:"commit_id"`
		}
		path := fmt.Sprintf("/repos/%s/pulls/%d/reviews?page=%d&limit=%d", repo, number, page, giteaPageSize)
		if err := g.api.do(ctx, http.MethodGet, path, g.auth, nil, &reviews); err != nil {
			return nil, fmt.Errorf("gitea: %w", err)
		}
		for _, r := range reviews {
			all = append(all, Review{ID: r.ID, User: r.User.Login, Body: r.Body, State: r.State, CommitID: r.CommitID})
		}
		if len(reviews) < giteaPageSize {
			return all, nil
		}
	}
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
	}{Event: "COMMENT", Body: review.Body, CommitID: review.CommitSHA, Comments: []comment{}}
	for _, c := range review.Comments {
		in.Comments = append(in.Comments, comment{Path: c.Path, Body: c.Body, NewPosition: c.Line})
	}
	path := fmt.Sprintf("/repos/%s/pulls/%d/reviews", repo, number)
	if err := g.api.do(ctx, http.MethodPost, path, g.auth, in, nil); err != nil {
		return fmt.Errorf("gitea: create review: %w", err)
	}
	return nil
}

// GitRemote implements Forge.
func (g *Gitea) GitRemote(_ context.Context, repo Repo) (gitrepo.Remote, error) {
	return gitrepo.Remote{
		Key:        "gitea/" + repo.Owner + "/" + repo.Name,
		URL:        g.url + "/" + repo.String() + ".git",
		AuthHeader: gitrepo.BasicAuth(g.user, g.token),
	}, nil
}

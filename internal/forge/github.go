package forge

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mycroft/agent-nexus/internal/gitrepo"
	"github.com/mycroft/agent-nexus/internal/httpclient"
)

const (
	githubPageSize = 100
	// Installation tokens live one hour; refresh well before expiry so a
	// long fetch never runs with a dying token.
	tokenRefreshMargin = 5 * time.Minute
)

// GitHub is github.com (or GHES) accessed as a GitHub App.
type GitHub struct {
	api       apiClient
	serverURL string
	appID     int64
	key       *rsa.PrivateKey
	now       func() time.Time

	mu            sync.Mutex
	login         string
	installations map[string]int64 // owner/name → installation ID
	tokens        map[int64]installationToken
}

type installationToken struct {
	token     string
	expiresAt time.Time
}

// NewGitHub returns a GitHub forge authenticating as the given App.
// privateKeyPEM is the App's private key (PKCS#1 or PKCS#8).
func NewGitHub(apiURL, serverURL string, appID int64, privateKeyPEM string, opts ...httpclient.Option) (*GitHub, error) {
	key, err := parseRSAKey(privateKeyPEM)
	if err != nil {
		return nil, err
	}
	return &GitHub{
		api:           apiClient{forge: "github", baseURL: strings.TrimRight(apiURL, "/"), http: httpclient.New(opts...)},
		serverURL:     strings.TrimRight(serverURL, "/"),
		appID:         appID,
		key:           key,
		now:           time.Now,
		installations: map[string]int64{},
		tokens:        map[int64]installationToken{},
	}, nil
}

func parseRSAKey(pemData string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemData))
	if block == nil {
		return nil, errors.New("github: app private key is not PEM")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("github: parse app private key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("github: app private key is not RSA")
	}
	return key, nil
}

// Name implements Forge.
func (g *GitHub) Name() string { return "github" }

// ServerURL implements Forge.
func (g *GitHub) ServerURL() string { return g.serverURL }

func githubHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-Github-Api-Version", "2022-11-28")
}

// appJWT signs a short-lived JWT identifying the App (RS256).
func (g *GitHub) appJWT() (string, error) {
	now := g.now()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{
		// Backdated to tolerate clock drift, as GitHub recommends.
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": strconv.FormatInt(g.appID, 10),
	})
	if err != nil {
		return "", fmt.Errorf("github: marshal jwt claims: %w", err)
	}
	signingInput := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, g.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("github: sign jwt: %w", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func (g *GitHub) appAuth() (func(*http.Request), error) {
	jwt, err := g.appJWT()
	if err != nil {
		return nil, err
	}
	return func(req *http.Request) {
		githubHeaders(req)
		req.Header.Set("Authorization", "Bearer "+jwt)
	}, nil
}

// installationToken returns a cached or fresh installation token for repo.
func (g *GitHub) installationToken(ctx context.Context, repo Repo) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	key := strings.ToLower(repo.String())
	id, ok := g.installations[key]
	if !ok {
		auth, err := g.appAuth()
		if err != nil {
			return "", err
		}
		var inst struct {
			ID int64 `json:"id"`
		}
		if err := g.api.do(ctx, http.MethodGet, "/repos/"+repo.String()+"/installation", auth, nil, &inst); err != nil {
			return "", fmt.Errorf("github: app is not installed on %s: %w", repo, err)
		}
		id = inst.ID
		g.installations[key] = id
	}

	if t, ok := g.tokens[id]; ok && g.now().Add(tokenRefreshMargin).Before(t.expiresAt) {
		return t.token, nil
	}
	auth, err := g.appAuth()
	if err != nil {
		return "", err
	}
	var tok struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	path := fmt.Sprintf("/app/installations/%d/access_tokens", id)
	if err := g.api.do(ctx, http.MethodPost, path, auth, nil, &tok); err != nil {
		if errors.Is(err, ErrNotFound) {
			delete(g.installations, key)
		}
		return "", fmt.Errorf("github: create installation token: %w", err)
	}
	g.tokens[id] = installationToken{token: tok.Token, expiresAt: tok.ExpiresAt}
	return tok.Token, nil
}

func (g *GitHub) repoAuth(ctx context.Context, repo Repo) (func(*http.Request), error) {
	token, err := g.installationToken(ctx, repo)
	if err != nil {
		return nil, err
	}
	return func(req *http.Request) {
		githubHeaders(req)
		req.Header.Set("Authorization", "Bearer "+token)
	}, nil
}

// BotLogin implements Forge: an App posts as "<slug>[bot]".
func (g *GitHub) BotLogin(ctx context.Context) (string, error) {
	g.mu.Lock()
	login := g.login
	g.mu.Unlock()
	if login != "" {
		return login, nil
	}
	auth, err := g.appAuth()
	if err != nil {
		return "", err
	}
	var app struct {
		Slug string `json:"slug"`
	}
	if err := g.api.do(ctx, http.MethodGet, "/app", auth, nil, &app); err != nil {
		return "", fmt.Errorf("github: get app: %w", err)
	}
	g.mu.Lock()
	g.login = app.Slug + "[bot]"
	g.mu.Unlock()
	return app.Slug + "[bot]", nil
}

type githubUser struct {
	Login string `json:"login"`
}

// GetPR implements Forge.
func (g *GitHub) GetPR(ctx context.Context, repo Repo, number int) (PR, error) {
	auth, err := g.repoAuth(ctx, repo)
	if err != nil {
		return PR{}, err
	}
	var pr struct {
		Number  int        `json:"number"`
		Title   string     `json:"title"`
		Body    string     `json:"body"`
		User    githubUser `json:"user"`
		State   string     `json:"state"`
		Draft   bool       `json:"draft"`
		HTMLURL string     `json:"html_url"`
		Base    struct {
			Ref string `json:"ref"`
		} `json:"base"`
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	path := fmt.Sprintf("/repos/%s/pulls/%d", repo, number)
	if err := g.api.do(ctx, http.MethodGet, path, auth, nil, &pr); err != nil {
		return PR{}, fmt.Errorf("github: %w", err)
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
func (g *GitHub) ListReviews(ctx context.Context, repo Repo, number int) ([]Review, error) {
	auth, err := g.repoAuth(ctx, repo)
	if err != nil {
		return nil, err
	}
	var all []Review
	for page := 1; ; page++ {
		var reviews []struct {
			ID       int64      `json:"id"`
			User     githubUser `json:"user"`
			Body     string     `json:"body"`
			State    string     `json:"state"`
			CommitID string     `json:"commit_id"`
		}
		path := fmt.Sprintf("/repos/%s/pulls/%d/reviews?per_page=%d&page=%d", repo, number, githubPageSize, page)
		if err := g.api.do(ctx, http.MethodGet, path, auth, nil, &reviews); err != nil {
			return nil, fmt.Errorf("github: %w", err)
		}
		for _, r := range reviews {
			all = append(all, Review{ID: r.ID, User: r.User.Login, Body: r.Body, State: r.State, CommitID: r.CommitID})
		}
		if len(reviews) < githubPageSize {
			return all, nil
		}
	}
}

// DeletePendingReviews implements Forge. GitHub validates a review as a
// whole and never leaves a pending one behind after a failed submission.
func (g *GitHub) DeletePendingReviews(context.Context, Repo, int) error { return nil }

// CreateReview implements Forge.
func (g *GitHub) CreateReview(ctx context.Context, repo Repo, number int, review NewReview) error {
	auth, err := g.repoAuth(ctx, repo)
	if err != nil {
		return err
	}
	type comment struct {
		Path string `json:"path"`
		Line int    `json:"line"`
		Side string `json:"side"`
		Body string `json:"body"`
	}
	in := struct {
		CommitID string    `json:"commit_id"`
		Body     string    `json:"body"`
		Event    string    `json:"event"`
		Comments []comment `json:"comments"`
	}{CommitID: review.CommitSHA, Body: review.Body, Event: "COMMENT", Comments: []comment{}}
	for _, c := range review.Comments {
		in.Comments = append(in.Comments, comment{Path: c.Path, Line: c.Line, Side: "RIGHT", Body: c.Body})
	}
	path := fmt.Sprintf("/repos/%s/pulls/%d/reviews", repo, number)
	if err := g.api.do(ctx, http.MethodPost, path, auth, in, nil); err != nil {
		return fmt.Errorf("github: create review: %w", err)
	}
	return nil
}

// GitRemote implements Forge.
func (g *GitHub) GitRemote(ctx context.Context, repo Repo) (gitrepo.Remote, error) {
	token, err := g.installationToken(ctx, repo)
	if err != nil {
		return gitrepo.Remote{}, err
	}
	return gitrepo.Remote{
		Key:        "github/" + repo.Owner + "/" + repo.Name,
		URL:        g.serverURL + "/" + repo.String() + ".git",
		AuthHeader: gitrepo.BasicAuth("x-access-token", token),
	}, nil
}

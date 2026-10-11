// Package remote reads artifacts from the public internet for the agent:
// documentation pages, container image metadata from OCI registries, and
// GitHub release notes. Every request goes through NewHTTPClient, which
// only dials public addresses, and none carries credentials.
package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const userAgent = "agent-nexus"

// Client reads remote artifacts.
type Client struct {
	http *http.Client
	// githubAPI and rawGitHub are the GitHub roots; tests replace them.
	githubAPI string
	rawGitHub string
	now       func() time.Time

	mu sync.Mutex
	// tokens holds anonymous registry tokens by host and repository.
	tokens map[string]cachedToken
	// pages holds recently fetched pages, so reading one in parts does
	// not fetch it again.
	pages map[string]cachedPage
}

type cachedToken struct {
	token   string
	expires time.Time
}

// Option configures a Client.
type Option func(*Client)

// WithGitHub replaces the GitHub API and raw content roots.
func WithGitHub(apiURL, rawURL string) Option {
	return func(c *Client) { c.githubAPI, c.rawGitHub = apiURL, rawURL }
}

// New returns a client. A nil httpClient means NewHTTPClient.
func New(httpClient *http.Client, opts ...Option) *Client {
	if httpClient == nil {
		httpClient = NewHTTPClient()
	}
	c := &Client{
		http:      httpClient,
		githubAPI: "https://api.github.com",
		rawGitHub: "https://raw.githubusercontent.com",
		now:       time.Now,
		tokens:    map[string]cachedToken{},
		pages:     map[string]cachedPage{},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Request describes a GET.
type Request struct {
	URL    string
	Accept string
	// Token is a bearer token, for registries.
	Token string
	// Limit is the most bytes read; a larger body is an error.
	Limit int64
	// Header holds extra request headers.
	Header map[string]string
}

// Response is a body read in full.
type Response struct {
	Body        []byte
	ContentType string
	// URL is where the body came from, after redirects.
	URL string
}

// ChallengeError is a 401 response carrying a WWW-Authenticate challenge.
type ChallengeError struct{ Challenge string }

func (e *ChallengeError) Error() string { return "unauthorized" }

// Get reads a URL, up to req.Limit bytes.
func (c *Client) Get(ctx context.Context, req Request) (*Response, error) {
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, req.URL, nil)
	if err != nil {
		return nil, err
	}
	r.Header.Set("User-Agent", userAgent)
	if req.Accept != "" {
		r.Header.Set("Accept", req.Accept)
	}
	if req.Token != "" {
		r.Header.Set("Authorization", "Bearer "+req.Token)
	}
	for k, v := range req.Header {
		r.Header.Set(k, v)
	}
	resp, err := c.http.Do(r)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", Redact(req.URL), err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized && resp.Header.Get("WWW-Authenticate") != "":
		return nil, &ChallengeError{Challenge: resp.Header.Get("WWW-Authenticate")}
	case resp.StatusCode != http.StatusOK:
		return nil, &StatusError{URL: Redact(req.URL), Code: resp.StatusCode, Status: resp.Status}
	case resp.ContentLength > req.Limit:
		return nil, fmt.Errorf("GET %s: %d bytes exceeds %d", Redact(req.URL), resp.ContentLength, req.Limit)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, req.Limit+1))
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", Redact(req.URL), err)
	}
	if int64(len(body)) > req.Limit {
		return nil, fmt.Errorf("GET %s: more than %d bytes", Redact(req.URL), req.Limit)
	}
	return &Response{Body: body, ContentType: resp.Header.Get("Content-Type"), URL: resp.Request.URL.String()}, nil
}

// Redact drops a URL's query, which may carry a registry's token
// parameters.
func Redact(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "URL"
	}
	u.RawQuery = ""
	return u.String()
}

// StatusError is a response other than 200 OK.
type StatusError struct {
	URL    string
	Code   int
	Status string
}

func (e *StatusError) Error() string { return fmt.Sprintf("GET %s: %s", e.URL, e.Status) }

// IsNotFound reports whether err is a 404 from Get.
func IsNotFound(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Code == http.StatusNotFound
}

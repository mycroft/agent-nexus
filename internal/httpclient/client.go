// Package httpclient wraps http.Client with retry/backoff for the transient
// failures that forge and LLM APIs routinely hit (network blips, 429, 5xx).
package httpclient

import (
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"time"
)

// Client wraps http.Client with retry/backoff.
type Client struct {
	httpClient *http.Client
	maxRetries int
	baseDelay  time.Duration
	maxDelay   time.Duration
	retryPOST  bool
}

// Option configures a Client constructed via New.
type Option func(*Client)

// WithRetry overrides the retry policy: up to maxRetries attempts beyond the
// first, with exponential backoff starting at baseDelay and capped at maxDelay.
func WithRetry(maxRetries int, baseDelay, maxDelay time.Duration) Option {
	return func(c *Client) {
		c.maxRetries = maxRetries
		c.baseDelay = baseDelay
		c.maxDelay = maxDelay
	}
}

// WithRetryPOST also retries POST requests. Only use it for endpoints where
// a POST has no side effects, such as chat completions: a forge POST that
// failed with a 5xx may still have been partially applied.
func WithRetryPOST() Option {
	return func(c *Client) { c.retryPOST = true }
}

// WithHTTPClient overrides the underlying *http.Client.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) { c.httpClient = hc }
}

// New constructs a Client: up to 3 retries with exponential backoff from 1s
// to 30s, and a 30s per-request timeout.
func New(opts ...Option) *Client {
	c := &Client{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		maxRetries: 3,
		baseDelay:  time.Second,
		maxDelay:   30 * time.Second,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Do executes req, retrying network errors (except timeouts), HTTP 429 and HTTP 5xx with
// exponential backoff when the method is retryable. Other responses are
// returned for the caller to handle; the caller must close the body.
//
// For retries to work with request bodies, build req with a body type that
// populates req.GetBody (bytes.Reader, strings.Reader).
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	retries := c.maxRetries
	if !c.retryable(req.Method) {
		retries = 0
	}

	var lastErr error
	for attempt := 0; attempt <= retries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(c.backoff(attempt)):
			}
		}

		attemptReq, err := prepareAttempt(req, attempt)
		if err != nil {
			return nil, err
		}

		resp, err := c.httpClient.Do(attemptReq) //nolint:gosec // G704: request URLs are built from operator configuration, not request input
		if err != nil {
			// A timed-out request already consumed its whole budget;
			// repeating it would multiply the wait.
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				return nil, err
			}
			lastErr = err
			continue
		}
		if attempt == retries || !isRetryableStatus(resp.StatusCode) {
			return resp, nil
		}

		lastErr = fmt.Errorf("received retryable status %d", resp.StatusCode)
		io.Copy(io.Discard, resp.Body) //nolint:errcheck // draining for connection reuse; failure is harmless here
		resp.Body.Close()
	}

	return nil, fmt.Errorf("giving up after %d attempt(s): %w", retries+1, lastErr)
}

func (c *Client) retryable(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodDelete:
		return true
	case http.MethodPost:
		return c.retryPOST
	default:
		return false
	}
}

// prepareAttempt rewinds the request body via GetBody for retries.
func prepareAttempt(req *http.Request, attempt int) (*http.Request, error) {
	if attempt == 0 || req.GetBody == nil {
		return req, nil
	}
	body, err := req.GetBody()
	if err != nil {
		return nil, fmt.Errorf("rewind request body for retry: %w", err)
	}
	clone := req.Clone(req.Context())
	clone.Body = body
	return clone, nil
}

func (c *Client) backoff(attempt int) time.Duration {
	d := time.Duration(float64(c.baseDelay) * math.Pow(2, float64(attempt-1)))
	if d > c.maxDelay {
		return c.maxDelay
	}
	return d
}

func isRetryableStatus(status int) bool {
	return status == http.StatusTooManyRequests || (status >= 500 && status <= 599)
}

// Read reads up to limit bytes of resp.Body. The caller closes the body.
func Read(resp *http.Response, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, fmt.Errorf("read response body (status %d): %w", resp.StatusCode, err)
	}
	return b, nil
}

// StatusError describes a non-2xx response.
type StatusError struct {
	StatusCode int
	Status     string
	Body       string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("unexpected status %s: %s", e.Status, e.Body)
}

// EnsureOK returns a *StatusError if the response status is not 2xx,
// including a snippet of the body for diagnostics.
func EnsureOK(resp *http.Response, body []byte) error {
	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		return nil
	}
	snippet := body
	if len(snippet) > 500 {
		snippet = snippet[:500]
	}
	return &StatusError{StatusCode: resp.StatusCode, Status: resp.Status, Body: string(snippet)}
}

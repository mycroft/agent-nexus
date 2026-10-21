package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mycroft/agent-nexus/internal/httpclient"
	"github.com/mycroft/agent-nexus/internal/jobs"
)

// ErrStop is returned by Heartbeat when the runner must stop working on
// the job: superseded, timed out, or no longer its own.
var ErrStop = errors.New("the controller stopped the job")

// maxResponseBytes bounds a response of the controller: a claimed job.
const maxResponseBytes = 4 << 20

// Client calls the controller's runner API. It implements jobs.Writer.
type Client struct {
	base    *url.URL
	token   string
	name    string
	version string
	// once sends the calls that are not retried: claims, which a retry
	// could duplicate, and heartbeats and steps, which the next one
	// replaces or which are best effort.
	once *httpclient.Client
	// retrying sends the calls the lease or the dedupe key make
	// idempotent.
	retrying *httpclient.Client
}

// NewClient returns a client of the controller at controllerURL (its API
// root), authenticated by token, for the runner instance name.
func NewClient(controllerURL, token, name, version string) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(controllerURL, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("controller URL %q: want an absolute http or https URL", controllerURL)
	}
	if !namePattern.MatchString(name) {
		return nil, fmt.Errorf("runner name %q: want letters, digits, dots, dashes or underscores", name)
	}
	// A claim waits up to pollWait at the controller.
	timeout := pollWait + 30*time.Second
	return &Client{
		base: u, token: token, name: name, version: version,
		once: httpclient.New(httpclient.WithHTTPClient(&http.Client{Timeout: timeout}), httpclient.WithRetry(0, 0, 0)),
		retrying: httpclient.New(httpclient.WithHTTPClient(&http.Client{Timeout: timeout}),
			httpclient.WithRetry(5, time.Second, 30*time.Second), httpclient.WithRetryPOST()),
	}, nil
}

// Claim waits for a job of model, among kinds. It returns nil when none
// came within the controller's poll.
func (c *Client) Claim(ctx context.Context, model string, kinds []string) (*Lease, error) {
	req := ClaimRequest{Protocol: Protocol, Version: c.version, Runner: c.name, Model: model, Kinds: kinds}
	var l Lease
	status, err := c.call(ctx, c.once, "/v1/runner/claim", req, &l)
	if err != nil || status == http.StatusNoContent {
		return nil, err
	}
	return &l, nil
}

// Heartbeat renews the lease on job id. It returns ErrStop when the
// runner must stop working on it.
func (c *Client) Heartbeat(ctx context.Context, id int64, lease string) error {
	return c.leaseCall(ctx, c.once, id, "heartbeat", leaseRequest{Lease: lease}, nil, ErrStop)
}

// AddStep records a transcript step (jobs.Writer). It is not retried: a
// missing transcript line does not fail the job.
func (c *Client) AddStep(ctx context.Context, id int64, lease string, st jobs.Step) error {
	return c.leaseCall(ctx, c.once, id, "steps", stepRequest{Lease: lease, Step: st}, nil, jobs.ErrLeaseLost)
}

// Checkpoint stores the job's checkpoint (jobs.Writer). It returns
// jobs.ErrNotRunning when the job no longer runs.
func (c *Client) Checkpoint(ctx context.Context, id int64, lease string, data []byte) error {
	return c.leaseCall(ctx, c.retrying, id, "checkpoint", checkpointRequest{Lease: lease, Data: data}, nil, jobs.ErrNotRunning)
}

// Finish records the job's final state, and returns the status it ended
// with: Superseded if a newer job replaced it meanwhile.
func (c *Client) Finish(ctx context.Context, id int64, lease string, status jobs.Status, result []byte, errMsg string) (jobs.Status, error) {
	var res finishResponse
	err := c.leaseCall(ctx, c.retrying, id, "finish", finishRequest{Lease: lease, Status: status, Result: result, Error: errMsg}, &res, jobs.ErrLeaseLost)
	return res.Status, err
}

// Release gives the job back to the controller, which queues it again.
func (c *Client) Release(ctx context.Context, id int64, lease string) error {
	return c.leaseCall(ctx, c.retrying, id, "release", leaseRequest{Lease: lease}, nil, jobs.ErrLeaseLost)
}

// Enqueue queues a job at the controller, as jobs.Queue.Enqueue does
// locally. Retries send the same dedupe key, so they find the job.
func (c *Client) Enqueue(ctx context.Context, nj jobs.NewJob) (jobs.Job, bool, error) {
	payload, err := json.Marshal(nj.Payload)
	if err != nil {
		return jobs.Job{}, false, fmt.Errorf("marshal job payload: %w", err)
	}
	req := enqueueRequest{
		Kind: nj.Kind, GroupKey: nj.GroupKey, DedupeKey: nj.DedupeKey, KeepRunning: nj.KeepRunning,
		APIKeyName: nj.APIKeyName, Payload: payload,
	}
	var res enqueueResponse
	if _, err := c.call(ctx, c.retrying, "/v1/runner/enqueue", req, &res); err != nil {
		return jobs.Job{}, false, err
	}
	return res.Job, res.Created, nil
}

// leaseCall calls a route of job id, returning conflict on a 409.
func (c *Client) leaseCall(ctx context.Context, hc *httpclient.Client, id int64, route string, body, out any, conflict error) error {
	_, err := c.call(ctx, hc, "/v1/runner/jobs/"+strconv.FormatInt(id, 10)+"/"+route, body, out)
	var se *httpclient.StatusError
	if errors.As(err, &se) && se.StatusCode == http.StatusConflict {
		return fmt.Errorf("%w: %s", conflict, conflictReason(se.Body))
	}
	return err
}

// call POSTs body to path, decoding a JSON answer into out.
func (c *Client) call(ctx context.Context, hc *httpclient.Client, path string, body, out any) (int, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, fmt.Errorf("marshal %s request: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base.String()+path, bytes.NewReader(raw))
	if err != nil {
		return 0, fmt.Errorf("build %s request: %w", path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", path, err)
	}
	defer resp.Body.Close()
	data, err := httpclient.Read(resp, maxResponseBytes)
	if err != nil {
		return resp.StatusCode, err
	}
	if err := httpclient.EnsureOK(resp, data); err != nil {
		return resp.StatusCode, fmt.Errorf("%s: %w", path, err)
	}
	if out != nil && resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, fmt.Errorf("decode %s response: %w", path, err)
		}
	}
	return resp.StatusCode, nil
}

// conflictReason returns the controller's explanation of a 409.
func conflictReason(body string) string {
	var e errorResponse
	if json.Unmarshal([]byte(body), &e) == nil && e.Error != "" {
		return e.Error
	}
	return body
}

// Package runner lets processes other than the controller run jobs: the
// controller serves the runner API under /v1/runner/ (Server), and a
// runner leases jobs from it and runs them with the usual handlers
// (Client, Worker). A runner only dials the controller.
package runner

import (
	"encoding/json"

	"github.com/mycroft/agent-nexus/internal/jobs"
)

// Protocol is the version of the runner API. The controller refuses
// runners speaking another one.
const Protocol = 1

// ClaimRequest is the body of POST /v1/runner/claim.
type ClaimRequest struct {
	Protocol int    `json:"protocol"`
	Version  string `json:"version"`
	// Runner names the runner instance (its host name, by default); the
	// controller prefixes it with the name of its token.
	Runner string `json:"runner"`
	// Model is the model the runner claims a job for, and Kinds the job
	// kinds it can run.
	Model string   `json:"model"`
	Kinds []string `json:"kinds"`
}

// Lease is a claimed job, as the claim returns it.
type Lease struct {
	Job jobs.Job `json:"job"`
	// Checkpoint and Lease are left out of the job's own JSON.
	Checkpoint json.RawMessage `json:"checkpoint,omitempty"`
	Lease      string          `json:"lease"`
	// TimeoutSeconds bounds the job, from when the runner gets it: a
	// duration, as the clocks of both ends may differ.
	TimeoutSeconds int `json:"timeout_seconds"`
	// HeartbeatSeconds is how often the runner renews the lease.
	HeartbeatSeconds int `json:"heartbeat_seconds"`
}

// leaseRequest is the body of the calls holding a job.
type leaseRequest struct {
	Lease string `json:"lease"`
}

type stepRequest struct {
	Lease string    `json:"lease"`
	Step  jobs.Step `json:"step"`
}

type checkpointRequest struct {
	Lease string          `json:"lease"`
	Data  json.RawMessage `json:"data"`
}

type finishRequest struct {
	Lease  string          `json:"lease"`
	Status jobs.Status     `json:"status"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

type finishResponse struct {
	// Status is the job's final status: Superseded if a newer job
	// replaced it meanwhile.
	Status jobs.Status `json:"status"`
}

// enqueueRequest is a jobs.NewJob with its payload as JSON.
type enqueueRequest struct {
	Kind        string          `json:"kind"`
	GroupKey    string          `json:"group_key"`
	DedupeKey   string          `json:"dedupe_key"`
	KeepRunning bool            `json:"keep_running,omitempty"`
	APIKeyName  string          `json:"api_key_name,omitempty"`
	Payload     json.RawMessage `json:"payload"`
}

type enqueueResponse struct {
	Job     jobs.Job `json:"job"`
	Created bool     `json:"created"`
}

// errorResponse explains a refusal.
type errorResponse struct {
	Error string `json:"error"`
}

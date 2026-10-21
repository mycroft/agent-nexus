// Package metrics declares the Prometheus metrics exported on the metrics
// listener.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// JobsTotal counts finished jobs by kind, final status and model.
	JobsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "agent_nexus_jobs_total",
		Help: "Finished jobs by kind, final status and model.",
	}, []string{"kind", "status", "model"})

	// JobDuration observes job run time, from claim to completion.
	JobDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "agent_nexus_job_duration_seconds",
		Help:    "Job run time from claim to completion, by kind and model.",
		Buckets: []float64{10, 30, 60, 120, 300, 600, 1200, 1800, 3600},
	}, []string{"kind", "model"})

	// LLMRequestDuration observes chat-completion latency by outcome and
	// model.
	LLMRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "agent_nexus_llm_request_duration_seconds",
		Help:    "Chat-completion request latency, by outcome and model.",
		Buckets: []float64{1, 5, 10, 30, 60, 120, 300, 600},
	}, []string{"outcome", "model"})

	// LLMTokensTotal counts tokens reported by the LLM server.
	LLMTokensTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "agent_nexus_llm_tokens_total",
		Help: "Tokens reported by the LLM server, by type (prompt, completion) and model.",
	}, []string{"type", "model"})

	// ToolCallsTotal counts agent tool invocations by tool and outcome.
	ToolCallsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "agent_nexus_tool_calls_total",
		Help: "Agent tool invocations by tool and outcome (ok, error).",
	}, []string{"tool", "outcome"})

	// ForgeErrorsTotal counts failed forge API requests.
	ForgeErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "agent_nexus_forge_errors_total",
		Help: "Failed forge API requests by forge.",
	}, []string{"forge"})

	// MCPErrorsTotal counts MCP servers a job could not open: a failed
	// connection or tool list.
	MCPErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "agent_nexus_mcp_errors_total",
		Help: "MCP servers a job could not open, by server.",
	}, []string{"server"})
)

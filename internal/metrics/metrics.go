// Package metrics declares the Prometheus metrics exported on the metrics
// listener.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// JobsTotal counts finished jobs by kind and final status.
	JobsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "agent_nexus_jobs_total",
		Help: "Finished jobs by kind and final status.",
	}, []string{"kind", "status"})

	// JobDuration observes job run time, from claim to completion.
	JobDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "agent_nexus_job_duration_seconds",
		Help:    "Job run time from claim to completion.",
		Buckets: []float64{10, 30, 60, 120, 300, 600, 1200, 1800, 3600},
	}, []string{"kind"})

	// LLMRequestDuration observes chat-completion latency by outcome.
	LLMRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "agent_nexus_llm_request_duration_seconds",
		Help:    "Chat-completion request latency.",
		Buckets: []float64{1, 5, 10, 30, 60, 120, 300, 600},
	}, []string{"outcome"})

	// LLMTokensTotal counts tokens reported by the LLM server.
	LLMTokensTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "agent_nexus_llm_tokens_total",
		Help: "Tokens reported by the LLM server, by type (prompt, completion).",
	}, []string{"type"})

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
)

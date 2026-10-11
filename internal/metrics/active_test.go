package metrics

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestActiveJobsCollector(t *testing.T) {
	now := time.Date(2026, 10, 11, 8, 0, 0, 0, time.UTC)
	c := NewActiveJobsCollector(func(context.Context) ([]ActiveJobs, error) {
		return []ActiveJobs{
			{Status: "running", Count: 1, Oldest: now.Add(-90 * time.Second)},
			{Status: "queued", Count: 3, Oldest: now.Add(-10 * time.Minute)},
		}, nil
	}).(*activeJobsCollector)
	c.now = func() time.Time { return now }

	want := `
# HELP agent_nexus_job_oldest_seconds Age of the oldest unfinished job by status: since it was queued, or since it started.
# TYPE agent_nexus_job_oldest_seconds gauge
agent_nexus_job_oldest_seconds{status="posting"} 0
agent_nexus_job_oldest_seconds{status="queued"} 600
agent_nexus_job_oldest_seconds{status="running"} 90
# HELP agent_nexus_jobs Unfinished jobs by status.
# TYPE agent_nexus_jobs gauge
agent_nexus_jobs{status="posting"} 0
agent_nexus_jobs{status="queued"} 3
agent_nexus_jobs{status="running"} 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want)); err != nil {
		t.Error(err)
	}
}

func TestActiveJobsCollectorError(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(NewActiveJobsCollector(func(context.Context) ([]ActiveJobs, error) { return nil, errors.New("db down") }))
	if _, err := reg.Gather(); err == nil || !strings.Contains(err.Error(), "db down") {
		t.Errorf("Gather error = %v, want the read error", err)
	}
}

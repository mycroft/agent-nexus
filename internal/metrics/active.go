package metrics

import (
	"context"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// activeStatuses are the unfinished job statuses, always exported so a
// quiet queue reads 0 rather than no data.
var activeStatuses = []string{"queued", "running", "posting"}

// ActiveJobs is the state of one unfinished job status.
type ActiveJobs struct {
	Status string
	Count  int
	// Oldest is when the oldest job entered the status: queued at, or
	// started at.
	Oldest time.Time
}

type activeJobsCollector struct {
	read   func(context.Context) ([]ActiveJobs, error)
	now    func() time.Time
	jobs   *prometheus.Desc
	oldest *prometheus.Desc
}

// NewActiveJobsCollector exports the unfinished jobs, read at each scrape:
// agent_nexus_jobs (count by status) and agent_nexus_job_oldest_seconds
// (age of the oldest job by status, to alert on stuck jobs).
func NewActiveJobsCollector(read func(context.Context) ([]ActiveJobs, error)) prometheus.Collector {
	return &activeJobsCollector{
		read: read,
		now:  time.Now,
		jobs: prometheus.NewDesc("agent_nexus_jobs",
			"Unfinished jobs by status.", []string{"status"}, nil),
		oldest: prometheus.NewDesc("agent_nexus_job_oldest_seconds",
			"Age of the oldest unfinished job by status: since it was queued, or since it started.", []string{"status"}, nil),
	}
}

func (c *activeJobsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.jobs
	ch <- c.oldest
}

func (c *activeJobsCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stats, err := c.read(ctx)
	if err != nil {
		slog.Warn("read active jobs for metrics", "error", err)
		ch <- prometheus.NewInvalidMetric(c.jobs, err)
		return
	}
	byStatus := map[string]ActiveJobs{}
	for _, s := range stats {
		byStatus[s.Status] = s
	}
	for _, status := range activeStatuses {
		s := byStatus[status]
		ch <- prometheus.MustNewConstMetric(c.jobs, prometheus.GaugeValue, float64(s.Count), status)
		age := 0.0
		if s.Count > 0 && !s.Oldest.IsZero() {
			age = c.now().Sub(s.Oldest).Seconds()
		}
		ch <- prometheus.MustNewConstMetric(c.oldest, prometheus.GaugeValue, age, status)
	}
}

// ModelSlots is the use of one model's slots.
type ModelSlots struct {
	Model       string
	Running     int
	Concurrency int
}

type modelSlotsCollector struct {
	read        func(context.Context) ([]ModelSlots, error)
	running     *prometheus.Desc
	concurrency *prometheus.Desc
}

// NewModelSlotsCollector exports each model's slots, read at each scrape:
// agent_nexus_model_jobs (its running jobs) and
// agent_nexus_model_concurrency (how many it may run at once). Jobs that
// wait while the first stays at the second need another model or more
// slots.
func NewModelSlotsCollector(read func(context.Context) ([]ModelSlots, error)) prometheus.Collector {
	return &modelSlotsCollector{
		read: read,
		running: prometheus.NewDesc("agent_nexus_model_jobs",
			"Jobs running on each model.", []string{"model"}, nil),
		concurrency: prometheus.NewDesc("agent_nexus_model_concurrency",
			"Jobs each model may run at once.", []string{"model"}, nil),
	}
}

func (c *modelSlotsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.running
	ch <- c.concurrency
}

func (c *modelSlotsCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	slots, err := c.read(ctx)
	if err != nil {
		slog.Warn("read model slots for metrics", "error", err)
		ch <- prometheus.NewInvalidMetric(c.running, err)
		return
	}
	for _, s := range slots {
		ch <- prometheus.MustNewConstMetric(c.running, prometheus.GaugeValue, float64(s.Running), s.Model)
		ch <- prometheus.MustNewConstMetric(c.concurrency, prometheus.GaugeValue, float64(s.Concurrency), s.Model)
	}
}

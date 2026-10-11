package jobs

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const (
	defaultListLimit = 20
	maxListLimit     = 100
)

// Progress summarizes a job's transcript. It is empty once the
// transcript is pruned.
type Progress struct {
	// Turns counts the model's answers.
	Turns            int        `json:"turns"`
	PeakPromptTokens int        `json:"peak_prompt_tokens"`
	PromptTokens     int        `json:"prompt_tokens"`
	CompletionTokens int        `json:"completion_tokens"`
	LastActivity     *time.Time `json:"last_activity,omitempty"`
}

// JobSummary is a job with its progress.
type JobSummary struct {
	Job
	Progress
}

// ListFilter selects the jobs to list. Zero values select everything,
// with a default limit.
type ListFilter struct {
	Statuses []Status
	Kind     string
	Limit    int
}

// List returns the most recent jobs matching f, newest first.
func (s *Store) List(ctx context.Context, f ListFilter) ([]JobSummary, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	limit = min(limit, maxListLimit)

	var where []string
	var args []any
	if len(f.Statuses) > 0 {
		where = append(where, "status IN ("+placeholders(len(f.Statuses))+")")
		for _, st := range f.Statuses {
			args = append(args, string(st))
		}
	}
	if f.Kind != "" {
		where = append(where, "kind = ?")
		args = append(args, f.Kind)
	}
	query := `SELECT ` + jobColumns + ` FROM jobs`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)

	out, index, err := s.queryJobs(ctx, query, args...)
	if err != nil || len(out) == 0 {
		return out, err
	}
	if err := s.addProgress(ctx, out, index); err != nil {
		return nil, err
	}
	return out, nil
}

// queryJobs runs a job query and indexes the results by ID.
func (s *Store) queryJobs(ctx context.Context, query string, args ...any) ([]JobSummary, map[int64]int, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("list jobs: %w", err)
	}
	defer rows.Close()
	out := []JobSummary{}
	index := map[int64]int{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, nil, fmt.Errorf("scan job: %w", err)
		}
		index[j.ID] = len(out)
		out = append(out, JobSummary{Job: j})
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("list jobs: %w", err)
	}
	return out, index, nil
}

// addProgress fills in the progress of the listed jobs from their steps.
func (s *Store) addProgress(ctx context.Context, out []JobSummary, index map[int64]int) error {
	ids := make([]any, 0, len(out))
	for _, j := range out {
		ids = append(ids, j.ID)
	}
	//nolint:gosec // G202: only "?" placeholders are concatenated
	query := `SELECT job_id, SUM(role = 'assistant'), MAX(prompt_tokens),
		SUM(prompt_tokens), SUM(completion_tokens), MAX(created_at)
		FROM job_steps WHERE job_id IN (` + placeholders(len(ids)) + `)
		GROUP BY job_id`
	rows, err := s.db.QueryContext(ctx, query, ids...)
	if err != nil {
		return fmt.Errorf("summarize job steps: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var p Progress
		var last string
		if err := rows.Scan(&id, &p.Turns, &p.PeakPromptTokens, &p.PromptTokens, &p.CompletionTokens, &last); err != nil {
			return fmt.Errorf("scan job progress: %w", err)
		}
		t := parseTime(last)
		p.LastActivity = &t
		out[index[id]].Progress = p
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("summarize job steps: %w", err)
	}
	return nil
}

// placeholders returns n comma-separated "?".
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// ActiveStat counts the jobs in an unfinished status, and dates the
// oldest: since it was queued, or since it started.
type ActiveStat struct {
	Status Status
	Count  int
	Oldest time.Time
}

// Active summarizes the queued, running and posting jobs.
func (s *Store) Active(ctx context.Context) ([]ActiveStat, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT status, COUNT(*),
		MIN(CASE WHEN status = 'queued' THEN created_at ELSE started_at END)
		FROM jobs WHERE status IN ('queued', 'running', 'posting') GROUP BY status`)
	if err != nil {
		return nil, fmt.Errorf("summarize active jobs: %w", err)
	}
	defer rows.Close()
	var out []ActiveStat
	for rows.Next() {
		var st ActiveStat
		var status string
		var oldest sql.NullString
		if err := rows.Scan(&status, &st.Count, &oldest); err != nil {
			return nil, fmt.Errorf("scan active jobs: %w", err)
		}
		st.Status = Status(status)
		if oldest.Valid {
			st.Oldest = parseTime(oldest.String)
		}
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("summarize active jobs: %w", err)
	}
	return out, nil
}

// ParseStatus validates a status name.
func ParseStatus(s string) (Status, error) {
	switch st := Status(s); st {
	case Queued, Running, Posting, Succeeded, Failed, Skipped, Superseded:
		return st, nil
	default:
		return "", fmt.Errorf("unknown job status %q", s)
	}
}

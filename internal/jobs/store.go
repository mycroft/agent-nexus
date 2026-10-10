// Package jobs persists jobs in SQLite and runs them on a worker pool.
package jobs

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite" // database/sql driver
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Status is a job's lifecycle state.
type Status string

// Job statuses. Queued → running → (posting →) one of the final states.
const (
	Queued     Status = "queued"
	Running    Status = "running"
	Posting    Status = "posting"
	Succeeded  Status = "succeeded"
	Failed     Status = "failed"
	Skipped    Status = "skipped"
	Superseded Status = "superseded"
)

// ErrNotRunning is returned when a job changed state under its worker,
// typically because a newer job superseded it.
var ErrNotRunning = errors.New("job is no longer running")

// ErrNotFound is returned for an unknown job ID.
var ErrNotFound = errors.New("job not found")

// Job is a stored job.
type Job struct {
	ID         int64           `json:"id"`
	Kind       string          `json:"kind"`
	GroupKey   string          `json:"group_key"`
	DedupeKey  string          `json:"dedupe_key"`
	Status     Status          `json:"status"`
	Payload    json.RawMessage `json:"payload"`
	Checkpoint json.RawMessage `json:"-"`
	Result     json.RawMessage `json:"result,omitempty"`
	Error      string          `json:"error,omitempty"`
	Attempts   int             `json:"attempts"`
	APIKeyName string          `json:"api_key_name,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
	StartedAt  *time.Time      `json:"started_at,omitempty"`
	FinishedAt *time.Time      `json:"finished_at,omitempty"`
}

// NewJob describes a job to enqueue.
type NewJob struct {
	Kind string
	// GroupKey groups jobs about the same subject (e.g. one pull request);
	// a new job supersedes the group's older unfinished jobs.
	GroupKey string
	// DedupeKey identifies identical work (e.g. one pull request commit).
	DedupeKey string
	// KeepRunning limits superseding to queued jobs: a running job of the
	// group finishes.
	KeepRunning bool
	APIKeyName  string
	Payload     any
}

// Step is one transcript entry of a job.
type Step struct {
	Role             string    `json:"role"`
	ToolName         string    `json:"tool_name,omitempty"`
	Content          string    `json:"content"`
	PromptTokens     int       `json:"prompt_tokens,omitempty"`
	CompletionTokens int       `json:"completion_tokens,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
}

// Store is the SQLite job store.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// OpenStore opens (creating if necessary) the database at path and applies
// pending migrations. path may be ":memory:" for tests.
func OpenStore(ctx context.Context, path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	if path != ":memory:" {
		dsn += "&_pragma=journal_mode(WAL)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}
	// SQLite allows one writer; a single connection serializes access
	// instead of surfacing SQLITE_BUSY.
	db.SetMaxOpenConns(1)

	s := &Store{db: db, now: func() time.Time { return time.Now().UTC() }}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".up.sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, name).Scan(&n); err != nil {
			return fmt.Errorf("check migration %s: %w", name, err)
		}
		if n > 0 {
			continue
		}
		contents, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, string(contents)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version) VALUES (?)`, name); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", name, err)
		}
	}
	return nil
}

const jobColumns = `id, kind, group_key, dedupe_key, status, payload, checkpoint, result, error,
	attempts, api_key_name, created_at, started_at, finished_at`

// Enqueue inserts a job unless a live job (queued, running, posting or
// succeeded) has the same dedupe key, in which case that job is returned
// with created=false. Older unfinished jobs of the same group are marked
// superseded; the IDs of those that were running are returned so their
// workers can be cancelled.
func (s *Store) Enqueue(ctx context.Context, nj NewJob) (Job, bool, []int64, error) {
	payload, err := json.Marshal(nj.Payload)
	if err != nil {
		return Job{}, false, nil, fmt.Errorf("marshal job payload: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, false, nil, fmt.Errorf("begin enqueue: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	existing, err := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs
		WHERE dedupe_key = ? AND status IN ('queued', 'running', 'posting', 'succeeded')`, nj.DedupeKey))
	switch {
	case err == nil:
		return existing, false, nil, nil
	case !errors.Is(err, sql.ErrNoRows):
		return Job{}, false, nil, fmt.Errorf("find existing job: %w", err)
	}

	now := s.now()
	cancel, err := supersede(ctx, tx, nj.GroupKey, nj.KeepRunning, now)
	if err != nil {
		return Job{}, false, nil, err
	}

	job, err := scanJob(tx.QueryRowContext(ctx, `INSERT INTO jobs
		(kind, group_key, dedupe_key, status, payload, api_key_name, created_at)
		VALUES (?, ?, ?, 'queued', ?, ?, ?) RETURNING `+jobColumns,
		nj.Kind, nj.GroupKey, nj.DedupeKey, payload, nj.APIKeyName, formatTime(now)))
	if err != nil {
		return Job{}, false, nil, fmt.Errorf("insert job: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Job{}, false, nil, fmt.Errorf("commit enqueue: %w", err)
	}
	return job, true, cancel, nil
}

// supersede marks the group's queued jobs, and running ones unless
// keepRunning, superseded. It returns the IDs of those that had started.
func supersede(ctx context.Context, tx *sql.Tx, group string, keepRunning bool, now time.Time) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `UPDATE jobs SET status = 'superseded', finished_at = ?
		WHERE group_key = ? AND (status = 'queued' OR (status = 'running' AND NOT ?))
		RETURNING id, (started_at IS NOT NULL)`,
		formatTime(now), group, keepRunning)
	if err != nil {
		return nil, fmt.Errorf("supersede older jobs: %w", err)
	}
	defer rows.Close()
	var started []int64
	for rows.Next() {
		var id int64
		var wasStarted bool
		if err := rows.Scan(&id, &wasStarted); err != nil {
			return nil, fmt.Errorf("scan superseded job: %w", err)
		}
		if wasStarted {
			started = append(started, id)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("supersede older jobs: %w", err)
	}
	return started, nil
}

// Claim marks the oldest queued job as running and returns it, or nil
// when the queue is empty.
func (s *Store) Claim(ctx context.Context) (*Job, error) {
	job, err := scanJob(s.db.QueryRowContext(ctx, `UPDATE jobs
		SET status = 'running', started_at = ?, attempts = attempts + 1
		WHERE id = (SELECT id FROM jobs WHERE status = 'queued' ORDER BY id LIMIT 1)
		RETURNING `+jobColumns, formatTime(s.now())))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil //nolint:nilnil // an empty queue is not an error
	}
	if err != nil {
		return nil, fmt.Errorf("claim job: %w", err)
	}
	return &job, nil
}

// Checkpoint stores data on a running job and moves it to posting. A job
// recovered after a crash keeps its checkpoint, so its handler can resume
// from it instead of redoing the work.
func (s *Store) Checkpoint(ctx context.Context, id int64, data []byte) error {
	res, err := s.db.ExecContext(ctx, `UPDATE jobs SET checkpoint = ?, status = 'posting'
		WHERE id = ? AND status IN ('running', 'posting')`, data, id)
	if err != nil {
		return fmt.Errorf("checkpoint job %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotRunning
	}
	return nil
}

// Finish records a job's final state. It does nothing (and returns false)
// if the job was already finished, e.g. superseded while running.
func (s *Store) Finish(ctx context.Context, id int64, status Status, result []byte, errMsg string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE jobs SET status = ?, result = ?, error = ?, finished_at = ?
		WHERE id = ? AND status IN ('running', 'posting')`,
		string(status), result, errMsg, formatTime(s.now()), id)
	if err != nil {
		return false, fmt.Errorf("finish job %d: %w", id, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// Recover requeues jobs left running or posting by a previous process,
// and fails those that already used maxAttempts.
func (s *Store) Recover(ctx context.Context, maxAttempts int) (int64, int64, error) {
	failed, err := s.db.ExecContext(ctx, `UPDATE jobs SET status = 'failed', finished_at = ?,
		error = 'interrupted too many times'
		WHERE status IN ('running', 'posting') AND attempts >= ?`, formatTime(s.now()), maxAttempts)
	if err != nil {
		return 0, 0, fmt.Errorf("fail interrupted jobs: %w", err)
	}
	requeued, err := s.db.ExecContext(ctx, `UPDATE jobs SET status = 'queued' WHERE status IN ('running', 'posting')`)
	if err != nil {
		return 0, 0, fmt.Errorf("requeue interrupted jobs: %w", err)
	}
	r, _ := requeued.RowsAffected()
	f, _ := failed.RowsAffected()
	return r, f, nil
}

// Get returns a job by ID.
func (s *Store) Get(ctx context.Context, id int64) (Job, error) {
	job, err := scanJob(s.db.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, fmt.Errorf("get job %d: %w", id, err)
	}
	return job, nil
}

// AddStep appends a transcript step to a job.
func (s *Store) AddStep(ctx context.Context, id int64, st Step) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO job_steps
		(job_id, seq, role, tool_name, content, prompt_tokens, completion_tokens, created_at)
		VALUES (?, (SELECT COALESCE(MAX(seq), 0) + 1 FROM job_steps WHERE job_id = ?), ?, ?, ?, ?, ?, ?)`,
		id, id, st.Role, st.ToolName, st.Content, st.PromptTokens, st.CompletionTokens, formatTime(s.now()))
	if err != nil {
		return fmt.Errorf("add step to job %d: %w", id, err)
	}
	return nil
}

// Usage sums the token counts recorded in a job's transcript.
func (s *Store) Usage(ctx context.Context, id int64) (int, int, error) {
	var prompt, completion int
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(prompt_tokens), 0), COALESCE(SUM(completion_tokens), 0)
		FROM job_steps WHERE job_id = ?`, id).Scan(&prompt, &completion)
	if err != nil {
		return 0, 0, fmt.Errorf("sum usage of job %d: %w", id, err)
	}
	return prompt, completion, nil
}

// Steps returns a job's transcript in order.
func (s *Store) Steps(ctx context.Context, id int64) ([]Step, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT role, tool_name, content, prompt_tokens, completion_tokens, created_at
		FROM job_steps WHERE job_id = ? ORDER BY seq`, id)
	if err != nil {
		return nil, fmt.Errorf("list steps of job %d: %w", id, err)
	}
	defer rows.Close()
	var steps []Step
	for rows.Next() {
		var st Step
		var created string
		if err := rows.Scan(&st.Role, &st.ToolName, &st.Content, &st.PromptTokens, &st.CompletionTokens, &created); err != nil {
			return nil, fmt.Errorf("scan step: %w", err)
		}
		st.CreatedAt = parseTime(created)
		steps = append(steps, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list steps of job %d: %w", id, err)
	}
	return steps, nil
}

// PruneSteps deletes transcript steps created before cutoff.
func (s *Store) PruneSteps(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM job_steps WHERE created_at < ?`, formatTime(cutoff))
	if err != nil {
		return 0, fmt.Errorf("prune steps: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanJob(row rowScanner) (Job, error) {
	var (
		j                  Job
		status, created    string
		started, finished  sql.NullString
		checkpoint, result []byte
	)
	err := row.Scan(&j.ID, &j.Kind, &j.GroupKey, &j.DedupeKey, &status, &j.Payload, &checkpoint, &result,
		&j.Error, &j.Attempts, &j.APIKeyName, &created, &started, &finished)
	if err != nil {
		return Job{}, err
	}
	j.Status = Status(status)
	j.Checkpoint = checkpoint
	j.Result = result
	j.CreatedAt = parseTime(created)
	if started.Valid {
		t := parseTime(started.String)
		j.StartedAt = &t
	}
	if finished.Valid {
		t := parseTime(finished.String)
		j.FinishedAt = &t
	}
	return j, nil
}

// timeLayout is fixed-width so stored timestamps sort as text.
const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"

func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

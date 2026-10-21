// Package jobs persists jobs in SQLite and runs them on a worker pool.
package jobs

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/hex"
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

// ErrLeaseLost is returned for a write whose lease no longer holds the
// job: the job finished, or its lease expired.
var ErrLeaseLost = errors.New("job lease lost")

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
	// Model is the model the job ran on, set when it is claimed.
	Model string `json:"model,omitempty"`
	// Runner names who runs the job: empty for the controller's own
	// workers.
	Runner string `json:"runner,omitempty"`
	// Lease is the token of the claim holding the job, set by Claim.
	// Every write of the holder carries it.
	Lease      string     `json:"-"`
	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
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
	// Seq numbers the steps of a job from 1; AddStep assigns it.
	Seq              int       `json:"seq"`
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
	attempts, api_key_name, model, runner, COALESCE(lease, ''), created_at, started_at, finished_at`

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

// ClaimSpec restricts the jobs Claim may start. Zero fields restrict
// nothing.
type ClaimSpec struct {
	// Model is the model the claimed job runs on. Limit, if positive,
	// bounds the jobs running on it at once.
	Model string
	Limit int
	// Kinds are the job kinds the claimer can run.
	Kinds []string
	// Runner names the claimer: empty for the controller's own workers.
	Runner string
	// LeaseTTL, if positive, is how long the lease lasts unless renewed:
	// a runner's. The controller's own leases last until it restarts.
	LeaseTTL time.Duration
}

// Claim starts the oldest queued job spec allows, and returns it with its
// lease, or nil when no job can start. A job waits while another job of
// its group is running, posting or still held by its worker, so jobs about
// one subject never run concurrently.
//
// A model's limit counts the jobs it holds, superseded ones included
// until their worker lets go. A job with a checkpoint only has its result
// left to post, without the model: it is claimed as posting, outside the
// model's limit, and keeps the model that drafted it.
func (s *Store) Claim(ctx context.Context, spec ClaimSpec) (*Job, error) {
	lease, err := newLease()
	if err != nil {
		return nil, err
	}
	now := s.now()
	var until any
	if spec.LeaseTTL > 0 {
		until = formatTime(now.Add(spec.LeaseTTL))
	}
	args := []any{formatTime(now), spec.Model, spec.Runner, lease, until}
	query := `UPDATE jobs
		SET status = CASE WHEN checkpoint IS NULL THEN 'running' ELSE 'posting' END,
			started_at = ?, attempts = attempts + 1,
			model = CASE WHEN checkpoint IS NULL THEN ? ELSE model END,
			runner = ?, lease = ?, lease_until = ?
		WHERE id = (SELECT q.id FROM jobs q WHERE q.status = 'queued'
			AND NOT EXISTS (SELECT 1 FROM jobs b WHERE b.group_key = q.group_key
				AND (b.status IN ('running', 'posting') OR b.lease IS NOT NULL))`
	if len(spec.Kinds) > 0 {
		query += ` AND q.kind IN (` + placeholders(len(spec.Kinds)) + `)`
		for _, k := range spec.Kinds {
			args = append(args, k)
		}
	}
	if spec.Limit > 0 {
		query += ` AND (q.checkpoint IS NOT NULL OR
			(SELECT COUNT(*) FROM jobs r WHERE r.model = ? AND ` + holdsSlot + `) < ?)`
		args = append(args, spec.Model, spec.Limit)
	}
	query += ` ORDER BY q.id LIMIT 1) RETURNING ` + jobColumns
	job, err := scanJob(s.db.QueryRowContext(ctx, query, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil //nolint:nilnil // an empty queue is not an error
	}
	if err != nil {
		return nil, fmt.Errorf("claim job: %w", err)
	}
	return &job, nil
}

// holdsSlot selects, in table r, the jobs using a slot of their model:
// held by a worker, and not only posting.
const holdsSlot = `r.lease IS NOT NULL AND r.status != 'posting'`

// newLease returns a random claim token.
func newLease() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate job lease: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// Checkpoint stores data on a running job and moves it to posting. A job
// recovered after a crash keeps its checkpoint, so its handler can resume
// from it instead of redoing the work. It returns ErrNotRunning if the job
// was superseded meanwhile, or lease no longer holds it.
func (s *Store) Checkpoint(ctx context.Context, id int64, lease string, data []byte) error {
	res, err := s.db.ExecContext(ctx, `UPDATE jobs SET checkpoint = ?, status = 'posting'
		WHERE id = ? AND lease = ? AND status IN ('running', 'posting')`, data, id, lease)
	if err != nil {
		return fmt.Errorf("checkpoint job %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotRunning
	}
	return nil
}

// Finish records a job's final state and releases its lease. It records
// nothing (and returns false) if the job was already finished, e.g.
// superseded while running, and returns ErrLeaseLost if lease no longer
// holds it.
func (s *Store) Finish(ctx context.Context, id int64, lease string, status Status, result []byte, errMsg string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE jobs SET status = ?, result = ?, error = ?, finished_at = ?,
			lease = NULL, lease_until = NULL
		WHERE id = ? AND lease = ? AND status IN ('running', 'posting')`,
		string(status), result, errMsg, formatTime(s.now()), id, lease)
	if err != nil {
		return false, fmt.Errorf("finish job %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return true, nil
	}
	// Superseded meanwhile: the worker only lets go of the job.
	res, err = s.db.ExecContext(ctx, `UPDATE jobs SET lease = NULL, lease_until = NULL WHERE id = ? AND lease = ?`, id, lease)
	if err != nil {
		return false, fmt.Errorf("release job %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, ErrLeaseLost
	}
	return false, nil
}

// Recover runs at the controller's startup. It requeues the jobs its own
// workers left running or posting, fails those that already used
// maxAttempts, and releases the jobs they held. Runners' leases are
// extended by grace, so a runner can renew them after the restart.
func (s *Store) Recover(ctx context.Context, maxAttempts int, grace time.Duration) (int64, int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("begin recover: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit
	failed, err := tx.ExecContext(ctx, `UPDATE jobs SET status = 'failed', finished_at = ?,
		error = 'interrupted too many times'
		WHERE status IN ('running', 'posting') AND lease_until IS NULL AND attempts >= ?`, formatTime(s.now()), maxAttempts)
	if err != nil {
		return 0, 0, fmt.Errorf("fail interrupted jobs: %w", err)
	}
	requeued, err := tx.ExecContext(ctx, `UPDATE jobs SET status = 'queued'
		WHERE status IN ('running', 'posting') AND lease_until IS NULL`)
	if err != nil {
		return 0, 0, fmt.Errorf("requeue interrupted jobs: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET lease = NULL WHERE lease IS NOT NULL AND lease_until IS NULL`); err != nil {
		return 0, 0, fmt.Errorf("release interrupted jobs: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET lease_until = MAX(lease_until, ?) WHERE lease_until IS NOT NULL`,
		formatTime(s.now().Add(grace))); err != nil {
		return 0, 0, fmt.Errorf("extend runner leases: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("commit recover: %w", err)
	}
	r, _ := requeued.RowsAffected()
	f, _ := failed.RowsAffected()
	return r, f, nil
}

// Reap takes back the jobs of runners whose lease expired: it requeues
// those left running or posting, or fails them once they used
// maxAttempts, and releases the others.
func (s *Store) Reap(ctx context.Context, maxAttempts int) (int64, int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("begin reap: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit
	now := formatTime(s.now())
	failed, err := tx.ExecContext(ctx, `UPDATE jobs SET status = 'failed', finished_at = ?,
		error = 'its runner stopped answering too many times'
		WHERE status IN ('running', 'posting') AND lease_until < ? AND attempts >= ?`, now, now, maxAttempts)
	if err != nil {
		return 0, 0, fmt.Errorf("fail abandoned jobs: %w", err)
	}
	requeued, err := tx.ExecContext(ctx, `UPDATE jobs SET status = 'queued'
		WHERE status IN ('running', 'posting') AND lease_until < ?`, now)
	if err != nil {
		return 0, 0, fmt.Errorf("requeue abandoned jobs: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET lease = NULL, lease_until = NULL WHERE lease_until < ?`, now); err != nil {
		return 0, 0, fmt.Errorf("release abandoned jobs: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("commit reap: %w", err)
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

// AddStep appends a transcript step to a job held by lease. It returns
// ErrLeaseLost once lease no longer holds the job.
func (s *Store) AddStep(ctx context.Context, id int64, lease string, st Step) error {
	res, err := s.db.ExecContext(ctx, `INSERT INTO job_steps
		(job_id, seq, role, tool_name, content, prompt_tokens, completion_tokens, created_at)
		SELECT ?, (SELECT COALESCE(MAX(seq), 0) + 1 FROM job_steps WHERE job_id = ?), ?, ?, ?, ?, ?, ?
		WHERE EXISTS (SELECT 1 FROM jobs WHERE id = ? AND lease = ?)`,
		id, id, st.Role, st.ToolName, st.Content, st.PromptTokens, st.CompletionTokens, formatTime(s.now()), id, lease)
	if err != nil {
		return fmt.Errorf("add step to job %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrLeaseLost
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
	return s.StepsAfter(ctx, id, 0)
}

// StepsAfter returns the steps of a job numbered after seq, in order: the
// ones recorded since a reader last looked.
func (s *Store) StepsAfter(ctx context.Context, id int64, seq int) ([]Step, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq, role, tool_name, content, prompt_tokens, completion_tokens, created_at
		FROM job_steps WHERE job_id = ? AND seq > ? ORDER BY seq`, id, seq)
	if err != nil {
		return nil, fmt.Errorf("list steps of job %d: %w", id, err)
	}
	defer rows.Close()
	var steps []Step
	for rows.Next() {
		var st Step
		var created string
		if err := rows.Scan(&st.Seq, &st.Role, &st.ToolName, &st.Content, &st.PromptTokens, &st.CompletionTokens, &created); err != nil {
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
		&j.Error, &j.Attempts, &j.APIKeyName, &j.Model, &j.Runner, &j.Lease, &created, &started, &finished)
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

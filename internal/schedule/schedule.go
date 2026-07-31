// Package schedule stores scheduled tasks — a user id, a free-text prompt,
// and either a one-off run time or a recurrence rule — in an embedded
// SQLite database (via the pure-Go modernc.org/sqlite driver, so
// cross-compilation never needs a C toolchain). It owns no scheduling logic
// of its own: internal/httpapi computes NextRunAt (using
// github.com/robfig/cron/v3 for recurring tasks) and decides what a due
// task's prompt means by replaying it through the ordinary agent loop —
// this package only persists what it's given, the same separation
// internal/history keeps from the agent loop that populates it.
package schedule

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

// ErrNotFound is returned by Delete when id doesn't exist or doesn't belong
// to the given userID — the same error either way, so a caller can't probe
// for the existence of another user's task id (see Delete).
var ErrNotFound = errors.New("schedule: task not found")

// Status values recorded by RecordRun. StatusError is used whenever
// Orchestrator.Handle returned an error for that firing; StatusSent
// otherwise (the model ran to completion — regardless of which, if any,
// output tool it chose to call — see RunScheduledTasks).
const (
	StatusSent  = "sent"
	StatusError = "error"
)

// Task is one scheduled task. Exactly one of CronExpr/RunAt is set: RunAt
// for a one-off task, CronExpr (a 5-field robfig/cron/v3 standard
// expression) for a recurring one.
type Task struct {
	ID          string
	UserID      string
	Prompt      string
	CronExpr    string
	RunAt       *time.Time
	NextRunAt   time.Time
	CreatedAt   time.Time
	LastFiredAt *time.Time
}

// TaskRun is one historical record of a scheduled task firing, written by
// RecordRun into scheduled_task_history — a table separate from
// scheduled_tasks so that a recurring task's many firings, and a one-off
// task's single firing (right before its scheduled_tasks row is removed via
// DeleteFired), both leave a permanent, queryable trace of what ran and
// whether it succeeded.
type TaskRun struct {
	ID       string
	TaskID   string
	UserID   string
	Prompt   string
	CronExpr string
	RunAt    *time.Time
	FiredAt  time.Time
	Status   string
	Error    string
}

// Store is a SQLite-backed scheduled-task database.
type Store struct {
	db *sql.DB
}

// Open creates (if needed) and opens the SQLite database at path, applying
// the schema. Mirrors internal/history.Open's DSN/pragma/connection-limit
// choices, since the same single-writer-process reasoning applies here.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("schedule: create dir %s: %w", dir, err)
		}
	}

	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("schedule: open %s: %w", path, err)
	}
	// SQLite only supports one writer at a time; a single connection avoids
	// SQLITE_BUSY errors from this process racing against itself.
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS scheduled_tasks (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL,
			prompt TEXT NOT NULL,
			cron_expr TEXT,
			run_at DATETIME,
			next_run_at DATETIME NOT NULL,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			last_fired_at DATETIME
		)`,
		`CREATE INDEX IF NOT EXISTS idx_scheduled_tasks_next_run ON scheduled_tasks(next_run_at)`,
		`CREATE INDEX IF NOT EXISTS idx_scheduled_tasks_user ON scheduled_tasks(user_id)`,
		`CREATE TABLE IF NOT EXISTS scheduled_task_history (
			id TEXT PRIMARY KEY,
			task_id TEXT NOT NULL,
			user_id TEXT NOT NULL,
			prompt TEXT NOT NULL,
			cron_expr TEXT,
			run_at DATETIME,
			fired_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			status TEXT NOT NULL,
			error TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_scheduled_task_history_user ON scheduled_task_history(user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_scheduled_task_history_task ON scheduled_task_history(task_id)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("schedule: migrate: %w", err)
		}
	}
	return nil
}

// Create records a new task and returns its generated id. Callers are
// expected to have already computed task.NextRunAt (task.RunAt itself for a
// one-off task, or the cron schedule's first Next(time.Now()) for a
// recurring one) — the store has no opinion on how that's derived.
func (s *Store) Create(ctx context.Context, task Task) (string, error) {
	id := uuid.NewString()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO scheduled_tasks (id, user_id, prompt, cron_expr, run_at, next_run_at) VALUES (?, ?, ?, ?, ?, ?)`,
		id, task.UserID, task.Prompt, nullString(task.CronExpr), nullTime(task.RunAt), utc(task.NextRunAt),
	); err != nil {
		return "", fmt.Errorf("schedule: create task: %w", err)
	}
	return id, nil
}

// ListForUser returns userID's scheduled tasks, soonest-next-run first.
func (s *Store) ListForUser(ctx context.Context, userID string) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, user_id, prompt, cron_expr, run_at, next_run_at, created_at, last_fired_at
		 FROM scheduled_tasks WHERE user_id = ? ORDER BY next_run_at ASC`, userID)
	if err != nil {
		return nil, fmt.Errorf("schedule: list tasks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanTasks(rows)
}

// DueTasks returns every task whose next_run_at is at or before now — what
// the background sweep (internal/httpapi.Orchestrator.RunScheduledTasks)
// polls for.
func (s *Store) DueTasks(ctx context.Context, now time.Time) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, user_id, prompt, cron_expr, run_at, next_run_at, created_at, last_fired_at
		 FROM scheduled_tasks WHERE next_run_at <= ? ORDER BY next_run_at ASC`, utc(now))
	if err != nil {
		return nil, fmt.Errorf("schedule: query due tasks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanTasks(rows)
}

// Delete removes id, but only if it belongs to userID — the ownership check
// that keeps the delete_scheduled_task tool from letting one household
// member delete another's task. Returns ErrNotFound identically whether id
// doesn't exist at all or belongs to someone else, so a caller can't use
// the error to probe which is true.
func (s *Store) Delete(ctx context.Context, id, userID string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM scheduled_tasks WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return fmt.Errorf("schedule: delete task: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("schedule: delete task: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteFired removes id unconditionally — used internally by the sweep
// right after it fires a one-off task, as opposed to Delete's user-facing,
// ownership-checked deletion.
func (s *Store) DeleteFired(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM scheduled_tasks WHERE id = ?`, id); err != nil {
		return fmt.Errorf("schedule: delete fired task: %w", err)
	}
	return nil
}

// Reschedule advances a recurring task to its next occurrence, called by the
// sweep right after firing it, and stamps last_fired_at with the same
// moment.
func (s *Store) Reschedule(ctx context.Context, id string, nextRunAt time.Time) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE scheduled_tasks SET next_run_at = ?, last_fired_at = CURRENT_TIMESTAMP WHERE id = ?`,
		utc(nextRunAt), id,
	); err != nil {
		return fmt.Errorf("schedule: reschedule task: %w", err)
	}
	return nil
}

// RecordRun writes one history row for a firing of task — called by
// RunScheduledTasks right after Orchestrator.Handle returns, whether it
// succeeded (status StatusSent) or failed (StatusError, with errMsg from
// Handle's returned error). This is what makes "which scheduled tasks
// actually ran" answerable after the fact: a recurring task's
// scheduled_tasks row survives and keeps getting reused, and a one-off
// task's row is removed entirely by DeleteFired once it succeeds, so
// scheduled_task_history is the only place either kind of firing is kept
// permanently.
func (s *Store) RecordRun(ctx context.Context, task Task, status, errMsg string) error {
	id := uuid.NewString()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO scheduled_task_history (id, task_id, user_id, prompt, cron_expr, run_at, status, error) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, task.ID, task.UserID, task.Prompt, nullString(task.CronExpr), nullTime(task.RunAt), status, nullString(errMsg),
	); err != nil {
		return fmt.Errorf("schedule: record task run: %w", err)
	}
	return nil
}

// HistoryForUser returns userID's recorded task firings, most recent first.
// fired_at is only second-precision (SQLite's CURRENT_TIMESTAMP), so two
// runs recorded within the same second would otherwise tie; rowid — which
// SQLite assigns in insertion order even for a TEXT PRIMARY KEY table like
// this one — breaks the tie deterministically.
func (s *Store) HistoryForUser(ctx context.Context, userID string) ([]TaskRun, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, task_id, user_id, prompt, cron_expr, run_at, fired_at, status, error
		 FROM scheduled_task_history WHERE user_id = ? ORDER BY fired_at DESC, rowid DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("schedule: list task history: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []TaskRun
	for rows.Next() {
		var r TaskRun
		var cronExpr, errMsg sql.NullString
		var runAt sql.NullTime
		if err := rows.Scan(&r.ID, &r.TaskID, &r.UserID, &r.Prompt, &cronExpr, &runAt, &r.FiredAt, &r.Status, &errMsg); err != nil {
			return nil, fmt.Errorf("schedule: scan task run: %w", err)
		}
		r.CronExpr = cronExpr.String
		r.Error = errMsg.String
		if runAt.Valid {
			r.RunAt = &runAt.Time
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func scanTasks(rows *sql.Rows) ([]Task, error) {
	var out []Task
	for rows.Next() {
		var t Task
		var cronExpr sql.NullString
		var runAt, lastFiredAt sql.NullTime
		if err := rows.Scan(&t.ID, &t.UserID, &t.Prompt, &cronExpr, &runAt, &t.NextRunAt, &t.CreatedAt, &lastFiredAt); err != nil {
			return nil, fmt.Errorf("schedule: scan task: %w", err)
		}
		t.CronExpr = cronExpr.String
		if runAt.Valid {
			t.RunAt = &runAt.Time
		}
		if lastFiredAt.Valid {
			t.LastFiredAt = &lastFiredAt.Time
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func nullString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

func nullTime(t *time.Time) sql.NullTime {
	if t == nil {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: utc(*t), Valid: true}
}

// utc normalizes t before it's ever bound as a query parameter.
// modernc.org/sqlite serializes a time.Time parameter via t.String(), and
// reads it back by re-parsing that same text — but a time.Time whose
// Location is an unnamed offset (e.g. one produced by time.Parse(RFC3339,
// ...) on an offset like "+03:00", as create_scheduled_task's run_at
// argument is) makes t.String() print the numeric offset twice with no
// letter-based zone abbreviation (e.g. "2026-07-31 09:01:00 +0300 +0300")
// — a string the driver's own read-back parser can't recognize, so it
// silently hands database/sql the raw text instead of a parsed time.Time,
// which then fails to Scan into *time.Time/sql.NullTime. UTC always has a
// proper "UTC" abbreviation, so it round-trips correctly regardless of
// which zone the caller's time.Time originally carried.
func utc(t time.Time) time.Time {
	return t.UTC()
}

package memory

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// ─── Scheduler Tasks (v2 — extension-based) ────────────────────────
//
// These methods back the new scheduler package (`scheduler/`). They
// operate on the `scheduler_tasks` table, which is distinct from the
// legacy `scheduled_tasks` table powering the zombie reminder code in
// `memory/store_tasks.go`. See docs/plans/PLAN-mood-tracking-redesign.md
// for the full design.
//
// The scheduler itself is dumb — it polls for due tasks and dispatches
// them by kind. All payload shape, retry logic interpretation, and
// per-task YAML config handling lives in the scheduler package.

// SchedulerTask is one row in the scheduler_tasks table. The scheduler
// package operates directly on these structs; there's no separate
// wrapper type.
//
// Go note: embedding `sql.NullString` and friends is clunky for app code,
// so nullable columns are represented with pointer-or-empty-string
// depending on which is more ergonomic for the field (same convention
// as the rest of the codebase).
type SchedulerTask struct {
	ID               int64
	Kind             string          // unique; matches the Handler.Kind() string
	CronExpr         string          // empty when task is one-shot (next_fire only)
	NextFire         time.Time       // absolute UTC time the task should execute next
	Payload          json.RawMessage // free-form JSON blob; shape known only by the handler
	RetryMaxAttempts int             // 0 = no retry
	RetryBackoff     string          // "none" | "linear" | "exponential"
	RetryInitialWait time.Duration   // starting wait between retries
	LastRunAt        *time.Time      // nil until first run
	LastError        string          // empty after a successful run
	AttemptCount     int             // current failing-run count; reset after success
	CreatedAt        time.Time
}

// UpsertSchedulerTask inserts a new scheduler task or updates the existing
// one keyed by kind. Kind has a UNIQUE index, so each kind maps to exactly
// one row — this matches how scheduler extensions register themselves at
// startup (one handler → one kind → one scheduled entry).
//
// When updating, we only change the scheduling config (cron, next_fire,
// payload, retry) — we leave last_run_at, last_error, and attempt_count
// alone so historical state isn't lost when task.yaml is edited.
func (s *SQLiteStore) UpsertSchedulerTask(t *SchedulerTask) error {
	cron := nullableString(t.CronExpr)

	_, err := s.db.Exec(
		`INSERT INTO scheduler_tasks
		   (kind, cron_expr, next_fire, payload_json,
		    retry_max_attempts, retry_backoff, retry_initial_wait)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(kind) DO UPDATE SET
		   cron_expr          = excluded.cron_expr,
		   next_fire          = excluded.next_fire,
		   payload_json       = excluded.payload_json,
		   retry_max_attempts = excluded.retry_max_attempts,
		   retry_backoff      = excluded.retry_backoff,
		   retry_initial_wait = excluded.retry_initial_wait`,
		t.Kind,
		cron,
		t.NextFire.UTC(),
		string(t.Payload),
		t.RetryMaxAttempts,
		t.RetryBackoff,
		int64(t.RetryInitialWait),
	)
	if err != nil {
		return fmt.Errorf("upserting scheduler task %q: %w", t.Kind, err)
	}
	return nil
}

// DueSchedulerTasks returns every task whose next_fire is at or before
// `now`. The runner calls this on every tick.
func (s *SQLiteStore) DueSchedulerTasks(now time.Time) ([]SchedulerTask, error) {
	rows, err := s.db.Query(
		`SELECT id, kind, cron_expr, next_fire, payload_json,
		        retry_max_attempts, retry_backoff, retry_initial_wait,
		        last_run_at, last_error, attempt_count, created_at
		 FROM scheduler_tasks
		 WHERE next_fire <= ?
		 ORDER BY next_fire ASC`,
		now.UTC(),
	)
	if err != nil {
		return nil, fmt.Errorf("querying due scheduler tasks: %w", err)
	}
	defer rows.Close()

	return scanSchedulerTasks(rows)
}

// SchedulerTaskByKind looks up the row for a given kind. Returns
// (nil, nil) when no row exists.
func (s *SQLiteStore) SchedulerTaskByKind(kind string) (*SchedulerTask, error) {
	rows, err := s.db.Query(
		`SELECT id, kind, cron_expr, next_fire, payload_json,
		        retry_max_attempts, retry_backoff, retry_initial_wait,
		        last_run_at, last_error, attempt_count, created_at
		 FROM scheduler_tasks
		 WHERE kind = ?
		 LIMIT 1`,
		kind,
	)
	if err != nil {
		return nil, fmt.Errorf("querying scheduler task %q: %w", kind, err)
	}
	defer rows.Close()

	tasks, err := scanSchedulerTasks(rows)
	if err != nil {
		return nil, err
	}
	if len(tasks) == 0 {
		return nil, nil
	}
	return &tasks[0], nil
}

// MarkSchedulerSuccess records a successful run: bumps next_fire, sets
// last_run_at, clears last_error, resets attempt_count.
func (s *SQLiteStore) MarkSchedulerSuccess(id int64, nextFire time.Time) error {
	_, err := s.db.Exec(
		`UPDATE scheduler_tasks
		   SET last_run_at   = ?,
		       next_fire     = ?,
		       last_error    = '',
		       attempt_count = 0
		 WHERE id = ?`,
		time.Now().UTC(),
		nextFire.UTC(),
		id,
	)
	if err != nil {
		return fmt.Errorf("marking scheduler task %d success: %w", id, err)
	}
	return nil
}

// MarkSchedulerFailure records a failed run. The caller computes the new
// next_fire according to the retry policy and passes it here, so this
// method is pure SQL with no policy logic.
func (s *SQLiteStore) MarkSchedulerFailure(id int64, nextFire time.Time, errMsg string, attempts int) error {
	_, err := s.db.Exec(
		`UPDATE scheduler_tasks
		   SET last_run_at   = ?,
		       next_fire     = ?,
		       last_error    = ?,
		       attempt_count = ?
		 WHERE id = ?`,
		time.Now().UTC(),
		nextFire.UTC(),
		errMsg,
		attempts,
		id,
	)
	if err != nil {
		return fmt.Errorf("marking scheduler task %d failure: %w", id, err)
	}
	return nil
}

// DeleteSchedulerTask removes a task row. Used for one-shot tasks after
// they've fired successfully (nothing registered yet, but the scheduler
// supports it).
func (s *SQLiteStore) DeleteSchedulerTask(id int64) error {
	_, err := s.db.Exec(`DELETE FROM scheduler_tasks WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("deleting scheduler task %d: %w", id, err)
	}
	return nil
}

// scanSchedulerTasks reads rows from a SELECT into SchedulerTask structs.
// Kept private — callers use the typed helpers above.
func scanSchedulerTasks(rows *sql.Rows) ([]SchedulerTask, error) {
	var tasks []SchedulerTask
	for rows.Next() {
		var (
			t        SchedulerTask
			cron     sql.NullString
			payload  string
			initWait int64
			lastErr  sql.NullString
		)
		err := rows.Scan(
			&t.ID, &t.Kind, &cron, &t.NextFire, &payload,
			&t.RetryMaxAttempts, &t.RetryBackoff, &initWait,
			&t.LastRunAt, &lastErr, &t.AttemptCount, &t.CreatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("scanning scheduler task: %w", err)
		}
		if cron.Valid {
			t.CronExpr = cron.String
		}
		if lastErr.Valid {
			t.LastError = lastErr.String
		}
		t.Payload = json.RawMessage(payload)
		t.RetryInitialWait = time.Duration(initWait)
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

// nullableString returns a sql.NullString that's NULL when s == "".
// Keeps "no cron expression" rows as SQL NULL instead of empty string,
// which is semantically cleaner and matches Go's zero-value conventions.
func nullableString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

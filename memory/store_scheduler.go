package memory

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
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
	Kind             string          // matches the Handler.Kind() string
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
	Source           string // "yaml" (loader-managed) or "user" (created via tools)
	Name             string // human-readable label; empty for system tasks
	Enabled          bool   // false = paused / soft-deleted
}

// UpsertSchedulerTask inserts or updates a YAML-managed scheduler task.
// This method is used exclusively by the loader at startup — it hard-codes
// source='yaml' so user-created rows are never touched.
//
// Uses explicit check-then-update instead of ON CONFLICT because the
// kind column is no longer UNIQUE (multiple user rows can share a kind).
// When updating, scheduling config changes but historical state
// (last_run_at, last_error, attempt_count) is preserved.
func (s *SQLiteStore) UpsertSchedulerTask(t *SchedulerTask) error {
	cron := nullableString(t.CronExpr)

	// Try updating the existing yaml-source row first.
	res, err := s.db.Exec(
		`UPDATE scheduler_tasks
		   SET cron_expr          = ?,
		       next_fire          = ?,
		       payload_json       = ?,
		       retry_max_attempts = ?,
		       retry_backoff      = ?,
		       retry_initial_wait = ?
		 WHERE kind = ? AND source = 'yaml'`,
		cron,
		t.NextFire.UTC(),
		string(t.Payload),
		t.RetryMaxAttempts,
		t.RetryBackoff,
		int64(t.RetryInitialWait),
		t.Kind,
	)
	if err != nil {
		return fmt.Errorf("upserting scheduler task %q: %w", t.Kind, err)
	}

	rows, _ := res.RowsAffected()
	if rows > 0 {
		return nil
	}

	// No existing yaml row — insert one.
	_, err = s.db.Exec(
		`INSERT INTO scheduler_tasks
		   (kind, cron_expr, next_fire, payload_json,
		    retry_max_attempts, retry_backoff, retry_initial_wait, source)
		 VALUES (?, ?, ?, ?, ?, ?, ?, 'yaml')`,
		t.Kind,
		cron,
		t.NextFire.UTC(),
		string(t.Payload),
		t.RetryMaxAttempts,
		t.RetryBackoff,
		int64(t.RetryInitialWait),
	)
	if err != nil {
		return fmt.Errorf("inserting scheduler task %q: %w", t.Kind, err)
	}
	return nil
}

// DueSchedulerTasks returns every enabled task whose next_fire is at or
// before `now`. The runner calls this on every tick.
func (s *SQLiteStore) DueSchedulerTasks(now time.Time) ([]SchedulerTask, error) {
	rows, err := s.db.Query(
		`SELECT id, kind, cron_expr, next_fire, payload_json,
		        retry_max_attempts, retry_backoff, retry_initial_wait,
		        last_run_at, last_error, attempt_count, created_at,
		        source, name, enabled
		 FROM scheduler_tasks
		 WHERE next_fire <= ? AND enabled = 1
		 ORDER BY next_fire ASC`,
		now.UTC(),
	)
	if err != nil {
		return nil, fmt.Errorf("querying due scheduler tasks: %w", err)
	}
	defer rows.Close()

	return scanSchedulerTasks(rows)
}

// ListAllSchedulerTasks returns every scheduler task (both yaml and user,
// enabled and disabled). Used by the /schedule command to show a
// complete overview.
func (s *SQLiteStore) ListAllSchedulerTasks() ([]SchedulerTask, error) {
	rows, err := s.db.Query(
		`SELECT id, kind, cron_expr, next_fire, payload_json,
		        retry_max_attempts, retry_backoff, retry_initial_wait,
		        last_run_at, last_error, attempt_count, created_at,
		        source, name, enabled
		 FROM scheduler_tasks
		 ORDER BY enabled DESC, next_fire ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("listing all scheduler tasks: %w", err)
	}
	defer rows.Close()

	return scanSchedulerTasks(rows)
}

// SchedulerTaskByKind looks up a row for a given kind. With multiple
// rows per kind now possible, this returns the first match — prefer
// SchedulerTaskByKindAndSource when you need a specific source.
func (s *SQLiteStore) SchedulerTaskByKind(kind string) (*SchedulerTask, error) {
	rows, err := s.db.Query(
		`SELECT id, kind, cron_expr, next_fire, payload_json,
		        retry_max_attempts, retry_backoff, retry_initial_wait,
		        last_run_at, last_error, attempt_count, created_at,
		        source, name, enabled
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

// SchedulerTaskByKindAndSource looks up the row for a given kind and
// source. Used by the loader to find only yaml-managed rows without
// accidentally matching user-created ones.
func (s *SQLiteStore) SchedulerTaskByKindAndSource(kind, source string) (*SchedulerTask, error) {
	rows, err := s.db.Query(
		`SELECT id, kind, cron_expr, next_fire, payload_json,
		        retry_max_attempts, retry_backoff, retry_initial_wait,
		        last_run_at, last_error, attempt_count, created_at,
		        source, name, enabled
		 FROM scheduler_tasks
		 WHERE kind = ? AND source = ?
		 LIMIT 1`,
		kind, source,
	)
	if err != nil {
		return nil, fmt.Errorf("querying scheduler task %q (source=%s): %w", kind, source, err)
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
// Kept private — callers use the typed helpers above. Column order must
// match every SELECT in this file: the 12 original columns followed by
// source, name, enabled (added by migration 000019).
func scanSchedulerTasks(rows *sql.Rows) ([]SchedulerTask, error) {
	var tasks []SchedulerTask
	for rows.Next() {
		var (
			t        SchedulerTask
			cron     sql.NullString
			payload  string
			initWait int64
			lastErr  sql.NullString
			name     sql.NullString
			enabled  int
		)
		err := rows.Scan(
			&t.ID, &t.Kind, &cron, &t.NextFire, &payload,
			&t.RetryMaxAttempts, &t.RetryBackoff, &initWait,
			&t.LastRunAt, &lastErr, &t.AttemptCount, &t.CreatedAt,
			&t.Source, &name, &enabled,
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
		if name.Valid {
			t.Name = name.String
		}
		t.Payload = json.RawMessage(payload)
		t.RetryInitialWait = time.Duration(initWait)
		t.Enabled = enabled == 1
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

// ─── Agent-Managed Schedule CRUD ───────────────────────────────────
//
// These methods manage named scheduler tasks — both user-created rows
// (source='user') and yaml-loaded tasks that have a user-facing name.
// Deletion is soft — set enabled=0 via UpdateSchedulerTask.

// CreateUserSchedulerTask inserts a new user-created scheduler task.
// The Source field is forced to "user" regardless of what the caller sets.
func (s *SQLiteStore) CreateUserSchedulerTask(t *SchedulerTask) (int64, error) {
	cron := nullableString(t.CronExpr)
	name := nullableString(t.Name)

	res, err := s.db.Exec(
		`INSERT INTO scheduler_tasks
		   (kind, cron_expr, next_fire, payload_json,
		    retry_max_attempts, retry_backoff, retry_initial_wait,
		    source, name, enabled)
		 VALUES (?, ?, ?, ?, ?, ?, ?, 'user', ?, 1)`,
		t.Kind,
		cron,
		t.NextFire.UTC(),
		string(t.Payload),
		t.RetryMaxAttempts,
		t.RetryBackoff,
		int64(t.RetryInitialWait),
		name,
	)
	if err != nil {
		return 0, fmt.Errorf("creating user scheduler task %q: %w", t.Kind, err)
	}
	return res.LastInsertId()
}

// GetSchedulerTaskByID fetches any scheduler task by ID, regardless of source.
// Returns (nil, nil) when not found.
func (s *SQLiteStore) GetSchedulerTaskByID(id int64) (*SchedulerTask, error) {
	rows, err := s.db.Query(
		`SELECT id, kind, cron_expr, next_fire, payload_json,
		        retry_max_attempts, retry_backoff, retry_initial_wait,
		        last_run_at, last_error, attempt_count, created_at,
		        source, name, enabled
		 FROM scheduler_tasks
		 WHERE id = ?`,
		id,
	)
	if err != nil {
		return nil, fmt.Errorf("querying scheduler task %d: %w", id, err)
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

// ListManagedSchedulerTasks returns scheduler tasks the agent can manage:
// worker_briefing, send_message, and send_prompt. Internal system tasks
// (mood_daily_rollup, etc.) are excluded. When includeDisabled is false,
// only enabled tasks are returned.
func (s *SQLiteStore) ListManagedSchedulerTasks(includeDisabled bool) ([]SchedulerTask, error) {
	query := `SELECT id, kind, cron_expr, next_fire, payload_json,
	                 retry_max_attempts, retry_backoff, retry_initial_wait,
	                 last_run_at, last_error, attempt_count, created_at,
	                 source, name, enabled
	          FROM scheduler_tasks
	          WHERE kind IN ('worker_briefing', 'send_message', 'send_prompt')`
	if !includeDisabled {
		query += ` AND enabled = 1`
	}
	query += ` ORDER BY next_fire ASC`

	rows, err := s.db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("listing managed scheduler tasks: %w", err)
	}
	defer rows.Close()

	return scanSchedulerTasks(rows)
}

// UpdateSchedulerTask applies partial updates to any scheduler task by ID.
// Only keys present in updates are changed. Supported keys: "name" (string),
// "cron_expr" (string), "next_fire" (time.Time), "enabled" (bool),
// "payload_json" (string). Returns an error if the task doesn't exist.
func (s *SQLiteStore) UpdateSchedulerTask(id int64, updates map[string]any) error {
	if len(updates) == 0 {
		return fmt.Errorf("no updates provided for scheduler task %d", id)
	}

	setClauses := make([]string, 0, len(updates))
	args := make([]any, 0, len(updates)+1)

	for key, val := range updates {
		switch key {
		case "name", "cron_expr", "payload_json":
			setClauses = append(setClauses, key+" = ?")
			args = append(args, val)
		case "next_fire":
			setClauses = append(setClauses, "next_fire = ?")
			if t, ok := val.(time.Time); ok {
				args = append(args, t.UTC())
			} else {
				args = append(args, val)
			}
		case "enabled":
			setClauses = append(setClauses, "enabled = ?")
			if b, ok := val.(bool); ok {
				if b {
					args = append(args, 1)
				} else {
					args = append(args, 0)
				}
			} else {
				args = append(args, val)
			}
		default:
			return fmt.Errorf("unsupported update key %q for scheduler task", key)
		}
	}

	query := "UPDATE scheduler_tasks SET " + strings.Join(setClauses, ", ") +
		" WHERE id = ?"
	args = append(args, id)

	res, err := s.db.Exec(query, args...)
	if err != nil {
		return fmt.Errorf("updating scheduler task %d: %w", id, err)
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return fmt.Errorf("scheduler task %d not found", id)
	}
	return nil
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

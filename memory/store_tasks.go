package memory

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// ─── Scheduled Tasks ────────────────────────────────────────────────
//
// These methods power the scheduler (Section 6 of SPEC.md).
// v0.2 uses only schedule_type="once" + task_type="send_message".
// The full schema supports recurring cron jobs and conditional tasks
// for v0.6.

// ScheduledTask represents a row in the scheduled_tasks table.
// Nullable fields use pointers — in Go, a *string can be nil while
// a plain string always has a value (its zero value is ""). This is
// how Go handles SQL NULLs without the sql.NullXxx wrapper types
// (which are clunkier to work with in application code).
type ScheduledTask struct {
	ID              int64
	Name            *string         // human-readable label, nullable
	ScheduleType    string          // "once", "recurring", "conditional"
	CronExpr        *string         // cron expression for recurring tasks
	TriggerAt       *time.Time      // for one-shot tasks: when to fire
	TaskType        string          // "send_message", "run_prompt", etc.
	Payload         json.RawMessage // task-specific config as raw JSON
	Enabled         bool
	LastRun         *time.Time
	NextRun         *time.Time
	RunCount        int
	MaxRuns         *int   // nil = unlimited
	Priority        string // "normal", "high", "critical" — controls damping behavior
	CreatedBy       string // "user", "system", "agent"
	SourceMessageID *int64
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// CreateScheduledTask inserts a new task and returns its ID.
// The caller sets next_run before inserting — for one-shot tasks this
// is just trigger_at, for recurring tasks (v0.6) it's computed from
// the cron expression.
func (s *Store) CreateScheduledTask(task *ScheduledTask) (int64, error) {
	// Default priority to "normal" if not set.
	priority := task.Priority
	if priority == "" {
		priority = "normal"
	}

	result, err := s.db.Exec(
		`INSERT INTO scheduled_tasks
		 (name, schedule_type, cron_expr, trigger_at, task_type, payload,
		  enabled, next_run, max_runs, priority, created_by, source_message_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		task.Name,
		task.ScheduleType,
		task.CronExpr,
		task.TriggerAt,
		task.TaskType,
		string(task.Payload),
		task.Enabled,
		task.NextRun,
		task.MaxRuns,
		priority,
		task.CreatedBy,
		task.SourceMessageID,
	)
	if err != nil {
		return 0, fmt.Errorf("creating scheduled task: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("getting scheduled task ID: %w", err)
	}
	return id, nil
}

// GetDueTasks returns all enabled tasks whose next_run is at or before
// the given time. This is the scheduler's polling query — called every
// minute by the ticker loop.
func (s *Store) GetDueTasks(now time.Time) ([]ScheduledTask, error) {
	rows, err := s.db.Query(
		`SELECT id, name, schedule_type, cron_expr, trigger_at, task_type,
		        payload, enabled, last_run, next_run, run_count, max_runs,
		        priority, created_by, source_message_id, created_at, updated_at
		 FROM scheduled_tasks
		 WHERE enabled = 1 AND next_run <= ?
		 ORDER BY next_run ASC`,
		now,
	)
	if err != nil {
		return nil, fmt.Errorf("querying due tasks: %w", err)
	}
	// defer rows.Close() ensures the database cursor is released even
	// if we return early due to an error. Same idea as Python's "with"
	// statement — cleanup runs no matter what.
	defer rows.Close()

	return scanScheduledTasks(rows)
}

// ListActiveTasks returns all enabled tasks, ordered by next run time.
// Used by the /schedule command to show what's coming up.
func (s *Store) ListActiveTasks() ([]ScheduledTask, error) {
	rows, err := s.db.Query(
		`SELECT id, name, schedule_type, cron_expr, trigger_at, task_type,
		        payload, enabled, last_run, next_run, run_count, max_runs,
		        priority, created_by, source_message_id, created_at, updated_at
		 FROM scheduled_tasks
		 WHERE enabled = 1
		 ORDER BY next_run ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("listing active tasks: %w", err)
	}
	defer rows.Close()

	return scanScheduledTasks(rows)
}

// MarkTaskRun updates a task after execution: sets last_run to now,
// increments run_count, and sets the new next_run. If the task has
// reached max_runs, it gets disabled instead.
func (s *Store) MarkTaskRun(taskID int64, nextRun *time.Time) error {
	now := time.Now()

	// First, increment run_count and set last_run.
	_, err := s.db.Exec(
		`UPDATE scheduled_tasks
		 SET last_run = ?, run_count = run_count + 1,
		     next_run = ?, updated_at = ?
		 WHERE id = ?`,
		now, nextRun, now, taskID,
	)
	if err != nil {
		return fmt.Errorf("marking task run: %w", err)
	}

	// Auto-disable if max_runs reached. This is a separate query to
	// keep the logic clear — SQLite is fast enough that two queries
	// to the same row is negligible.
	_, err = s.db.Exec(
		`UPDATE scheduled_tasks
		 SET enabled = 0, updated_at = ?
		 WHERE id = ? AND max_runs IS NOT NULL AND run_count >= max_runs`,
		now, taskID,
	)
	if err != nil {
		return fmt.Errorf("auto-disabling task: %w", err)
	}

	return nil
}

// UpdateScheduledTaskEnabled toggles a task's enabled state.
// Used by /schedule pause and /schedule resume.
func (s *Store) UpdateScheduledTaskEnabled(taskID int64, enabled bool) error {
	result, err := s.db.Exec(
		`UPDATE scheduled_tasks SET enabled = ?, updated_at = ? WHERE id = ?`,
		enabled, time.Now(), taskID,
	)
	if err != nil {
		return fmt.Errorf("updating task enabled: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("task %d not found", taskID)
	}
	return nil
}

// DeleteScheduledTask removes a task by ID.
func (s *Store) DeleteScheduledTask(taskID int64) error {
	result, err := s.db.Exec(
		`DELETE FROM scheduled_tasks WHERE id = ?`,
		taskID,
	)
	if err != nil {
		return fmt.Errorf("deleting task: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("task %d not found", taskID)
	}
	return nil
}

// scanScheduledTasks is a helper that reads rows into ScheduledTask structs.
// Factored out because both GetDueTasks and ListActiveTasks need the
// same scanning logic. In Go, sql.Rows.Scan fills variables by position —
// the order must match your SELECT columns exactly.
func scanScheduledTasks(rows *sql.Rows) ([]ScheduledTask, error) {
	var tasks []ScheduledTask
	for rows.Next() {
		var t ScheduledTask
		var payload string
		err := rows.Scan(
			&t.ID, &t.Name, &t.ScheduleType, &t.CronExpr, &t.TriggerAt,
			&t.TaskType, &payload, &t.Enabled, &t.LastRun, &t.NextRun,
			&t.RunCount, &t.MaxRuns, &t.Priority, &t.CreatedBy, &t.SourceMessageID,
			&t.CreatedAt, &t.UpdatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("scanning scheduled task: %w", err)
		}
		t.Payload = json.RawMessage(payload)
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

// --- Scheduler Helpers (v0.6 damping + defaults) ---

// CountTasksRunToday returns the number of non-once tasks that have
// executed since midnight in the given timezone. Used by the scheduler
// to enforce the max_proactive_per_day limit.
func (s *Store) CountTasksRunToday(now time.Time) (int, error) {
	startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	var count int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM scheduled_tasks
		 WHERE schedule_type != 'once' AND last_run >= ?`,
		startOfDay,
	).Scan(&count)
	return count, err
}

// DeferTask pushes a task's next_run to a later time. Used by the
// scheduler for quiet hours (defer to end of quiet window) and
// conversation-aware deferral (defer by 30 min if user is active).
func (s *Store) DeferTask(taskID int64, until time.Time) error {
	_, err := s.db.Exec(
		`UPDATE scheduled_tasks SET next_run = ?, updated_at = ? WHERE id = ?`,
		until, time.Now(), taskID,
	)
	return err
}

// GetTaskByName finds an enabled task by name and creator. Used by
// the scheduler's ensureDefaults() to check if a system-created
// default task already exists before creating it (idempotent).
func (s *Store) GetTaskByName(name string, createdBy string) (*ScheduledTask, error) {
	rows, err := s.db.Query(
		`SELECT id, name, schedule_type, cron_expr, trigger_at, task_type,
		        payload, enabled, last_run, next_run, run_count, max_runs,
		        priority, created_by, source_message_id, created_at, updated_at
		 FROM scheduled_tasks
		 WHERE name = ? AND created_by = ?
		 LIMIT 1`,
		name, createdBy,
	)
	if err != nil {
		return nil, fmt.Errorf("querying task by name: %w", err)
	}
	defer rows.Close()

	tasks, err := scanScheduledTasks(rows)
	if err != nil {
		return nil, err
	}
	if len(tasks) == 0 {
		return nil, nil // not found — callers check for nil
	}
	return &tasks[0], nil
}

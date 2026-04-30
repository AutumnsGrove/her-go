package memory

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newSchedulerTestStore opens a fresh temp SQLite with all tables
// created. embedDim=0 so vec_memories / vec_moods virtual tables are
// skipped — these tests only touch scheduler_tasks.
func newSchedulerTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "scheduler_test.db")
	store, err := NewStore(dbPath, 0)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// newTestTask returns a SchedulerTask filled with sensible defaults.
// Tests override only the fields they care about — keeps call sites
// focused on the behavior being verified.
func newTestTask(kind string) *SchedulerTask {
	return &SchedulerTask{
		Kind:             kind,
		CronExpr:         "0 21 * * *",
		NextFire:         time.Now().Add(1 * time.Hour),
		Payload:          json.RawMessage(`{"key":"value"}`),
		RetryMaxAttempts: 2,
		RetryBackoff:     "exponential",
		RetryInitialWait: 60 * time.Second,
	}
}

func TestUpsertSchedulerTask_Insert(t *testing.T) {
	store := newSchedulerTestStore(t)

	task := newTestTask("unit_insert")
	if err := store.UpsertSchedulerTask(task); err != nil {
		t.Fatalf("UpsertSchedulerTask: %v", err)
	}

	got, err := store.SchedulerTaskByKind("unit_insert")
	if err != nil {
		t.Fatalf("SchedulerTaskByKind: %v", err)
	}
	if got == nil {
		t.Fatal("SchedulerTaskByKind returned nil after insert")
	}
	if got.Kind != "unit_insert" {
		t.Errorf("Kind = %q, want %q", got.Kind, "unit_insert")
	}
	if got.CronExpr != "0 21 * * *" {
		t.Errorf("CronExpr = %q, want %q", got.CronExpr, "0 21 * * *")
	}
	if got.RetryMaxAttempts != 2 {
		t.Errorf("RetryMaxAttempts = %d, want 2", got.RetryMaxAttempts)
	}
	if got.RetryBackoff != "exponential" {
		t.Errorf("RetryBackoff = %q, want exponential", got.RetryBackoff)
	}
	if got.RetryInitialWait != 60*time.Second {
		t.Errorf("RetryInitialWait = %v, want 60s", got.RetryInitialWait)
	}
	if string(got.Payload) != `{"key":"value"}` {
		t.Errorf("Payload = %q, want %q", got.Payload, `{"key":"value"}`)
	}
}

// TestUpsertSchedulerTask_UpdatesScheduleButKeepsHistory verifies the
// key behavior: editing task.yaml updates the cron/retry/payload, but
// the runtime history (last_run_at, last_error, attempt_count) is
// preserved. This matters because a user tweaking the rollup time
// shouldn't wipe the "last run" clock.
func TestUpsertSchedulerTask_UpdatesScheduleButKeepsHistory(t *testing.T) {
	store := newSchedulerTestStore(t)

	// Insert, then mark a failure so last_run_at / last_error / attempts are set.
	orig := newTestTask("unit_update")
	if err := store.UpsertSchedulerTask(orig); err != nil {
		t.Fatalf("initial upsert: %v", err)
	}
	first, _ := store.SchedulerTaskByKind("unit_update")
	if err := store.MarkSchedulerFailure(first.ID, time.Now().Add(1*time.Hour), "boom", 1); err != nil {
		t.Fatalf("MarkSchedulerFailure: %v", err)
	}

	// Now simulate a task.yaml edit: different cron and retry.
	updated := &SchedulerTask{
		Kind:             "unit_update",
		CronExpr:         "0 9 * * *",
		NextFire:         time.Now().Add(3 * time.Hour),
		Payload:          json.RawMessage(`{"new":"payload"}`),
		RetryMaxAttempts: 5,
		RetryBackoff:     "linear",
		RetryInitialWait: 30 * time.Second,
	}
	if err := store.UpsertSchedulerTask(updated); err != nil {
		t.Fatalf("update upsert: %v", err)
	}

	got, err := store.SchedulerTaskByKind("unit_update")
	if err != nil {
		t.Fatalf("SchedulerTaskByKind after update: %v", err)
	}

	// Schedule fields should be updated.
	if got.CronExpr != "0 9 * * *" {
		t.Errorf("CronExpr = %q, want %q", got.CronExpr, "0 9 * * *")
	}
	if got.RetryMaxAttempts != 5 {
		t.Errorf("RetryMaxAttempts = %d, want 5", got.RetryMaxAttempts)
	}
	if got.RetryBackoff != "linear" {
		t.Errorf("RetryBackoff = %q, want linear", got.RetryBackoff)
	}
	if string(got.Payload) != `{"new":"payload"}` {
		t.Errorf("Payload = %q, want %q", got.Payload, `{"new":"payload"}`)
	}

	// History fields should be untouched — that's the whole point.
	if got.LastError != "boom" {
		t.Errorf("LastError = %q, want %q (upsert should preserve error)", got.LastError, "boom")
	}
	if got.AttemptCount != 1 {
		t.Errorf("AttemptCount = %d, want 1 (upsert should preserve attempts)", got.AttemptCount)
	}
	if got.LastRunAt == nil {
		t.Error("LastRunAt = nil, want preserved from MarkSchedulerFailure")
	}
}

// TestUpsertSchedulerTask_EmptyCronStoredAsNull — the CronExpr field is
// a plain string in Go but nullable in SQL. Empty string must be stored
// as SQL NULL so future queries that filter on cron_expr IS NULL work.
func TestUpsertSchedulerTask_EmptyCronStoredAsNull(t *testing.T) {
	store := newSchedulerTestStore(t)

	task := newTestTask("unit_nullcron")
	task.CronExpr = ""
	if err := store.UpsertSchedulerTask(task); err != nil {
		t.Fatalf("UpsertSchedulerTask: %v", err)
	}

	// Query raw so we can distinguish SQL NULL from empty string.
	var cronExpr *string
	err := store.db.QueryRow(`SELECT cron_expr FROM scheduler_tasks WHERE kind = ?`, "unit_nullcron").Scan(&cronExpr)
	if err != nil {
		t.Fatalf("raw query: %v", err)
	}
	if cronExpr != nil {
		t.Errorf("cron_expr in DB = %v, want NULL", *cronExpr)
	}

	// Round-trip through Go: empty string on the way in, empty string on the way out.
	got, _ := store.SchedulerTaskByKind("unit_nullcron")
	if got.CronExpr != "" {
		t.Errorf("round-tripped CronExpr = %q, want empty", got.CronExpr)
	}
}

func TestDueSchedulerTasks_ReturnsOnlyDue(t *testing.T) {
	store := newSchedulerTestStore(t)
	now := time.Now()

	overdue := newTestTask("unit_overdue")
	overdue.NextFire = now.Add(-10 * time.Minute)
	future := newTestTask("unit_future")
	future.NextFire = now.Add(10 * time.Minute)

	if err := store.UpsertSchedulerTask(overdue); err != nil {
		t.Fatalf("upsert overdue: %v", err)
	}
	if err := store.UpsertSchedulerTask(future); err != nil {
		t.Fatalf("upsert future: %v", err)
	}

	due, err := store.DueSchedulerTasks(now)
	if err != nil {
		t.Fatalf("DueSchedulerTasks: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("due count = %d, want 1 (only overdue)", len(due))
	}
	if due[0].Kind != "unit_overdue" {
		t.Errorf("due[0].Kind = %q, want unit_overdue", due[0].Kind)
	}
}

func TestDueSchedulerTasks_IncludesExactlyNow(t *testing.T) {
	store := newSchedulerTestStore(t)
	now := time.Now().Truncate(time.Second)

	task := newTestTask("unit_exactly_now")
	task.NextFire = now
	if err := store.UpsertSchedulerTask(task); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	due, err := store.DueSchedulerTasks(now)
	if err != nil {
		t.Fatalf("DueSchedulerTasks: %v", err)
	}
	if len(due) != 1 {
		t.Errorf("next_fire == now should be due; got %d rows", len(due))
	}
}

func TestSchedulerTaskByKind_UnknownReturnsNil(t *testing.T) {
	store := newSchedulerTestStore(t)

	got, err := store.SchedulerTaskByKind("does_not_exist")
	if err != nil {
		t.Fatalf("SchedulerTaskByKind: %v", err)
	}
	if got != nil {
		t.Errorf("SchedulerTaskByKind(unknown) = %v, want nil", got)
	}
}

func TestMarkSchedulerSuccess_ClearsErrorAndAttempts(t *testing.T) {
	store := newSchedulerTestStore(t)

	task := newTestTask("unit_success")
	if err := store.UpsertSchedulerTask(task); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	row, _ := store.SchedulerTaskByKind("unit_success")

	// Seed a prior failure so we can verify success clears it.
	if err := store.MarkSchedulerFailure(row.ID, time.Now().Add(1*time.Hour), "transient glitch", 2); err != nil {
		t.Fatalf("MarkSchedulerFailure: %v", err)
	}

	next := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	if err := store.MarkSchedulerSuccess(row.ID, next); err != nil {
		t.Fatalf("MarkSchedulerSuccess: %v", err)
	}

	got, _ := store.SchedulerTaskByKind("unit_success")
	if got.LastError != "" {
		t.Errorf("LastError = %q, want empty after success", got.LastError)
	}
	if got.AttemptCount != 0 {
		t.Errorf("AttemptCount = %d, want 0 after success", got.AttemptCount)
	}
	// SQLite stores DATETIME with second precision; compare truncated.
	if !got.NextFire.Equal(next.UTC()) {
		t.Errorf("NextFire = %v, want %v", got.NextFire, next.UTC())
	}
	if got.LastRunAt == nil {
		t.Error("LastRunAt = nil, want set by MarkSchedulerSuccess")
	}
}

func TestMarkSchedulerFailure_PersistsErrorAndAttempts(t *testing.T) {
	store := newSchedulerTestStore(t)

	task := newTestTask("unit_failure")
	if err := store.UpsertSchedulerTask(task); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	row, _ := store.SchedulerTaskByKind("unit_failure")

	next := time.Now().Add(5 * time.Minute).Truncate(time.Second)
	errMsg := "handler returned: connection refused"
	if err := store.MarkSchedulerFailure(row.ID, next, errMsg, 3); err != nil {
		t.Fatalf("MarkSchedulerFailure: %v", err)
	}

	got, _ := store.SchedulerTaskByKind("unit_failure")
	if got.LastError != errMsg {
		t.Errorf("LastError = %q, want %q", got.LastError, errMsg)
	}
	if got.AttemptCount != 3 {
		t.Errorf("AttemptCount = %d, want 3", got.AttemptCount)
	}
	if !got.NextFire.Equal(next.UTC()) {
		t.Errorf("NextFire = %v, want %v", got.NextFire, next.UTC())
	}
}

func TestDeleteSchedulerTask(t *testing.T) {
	store := newSchedulerTestStore(t)

	task := newTestTask("unit_delete")
	if err := store.UpsertSchedulerTask(task); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	row, _ := store.SchedulerTaskByKind("unit_delete")

	if err := store.DeleteSchedulerTask(row.ID); err != nil {
		t.Fatalf("DeleteSchedulerTask: %v", err)
	}

	got, _ := store.SchedulerTaskByKind("unit_delete")
	if got != nil {
		t.Errorf("SchedulerTaskByKind after delete = %v, want nil", got)
	}
}

// TestUpsertSchedulerTask_EnforcesKindUnique double-checks the UNIQUE
// index on kind is what lets the ON CONFLICT clause work. A direct
// INSERT (bypassing the upsert) should fail on the second write.
func TestUpsertSchedulerTask_EnforcesKindUnique(t *testing.T) {
	store := newSchedulerTestStore(t)

	task := newTestTask("unit_unique")
	if err := store.UpsertSchedulerTask(task); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Raw INSERT bypasses the ON CONFLICT path — should bounce off the
	// UNIQUE index, proving the index is doing its job.
	_, err := store.db.Exec(
		`INSERT INTO scheduler_tasks (kind, next_fire, payload_json, retry_backoff) VALUES (?, ?, '{}', 'none')`,
		"unit_unique", time.Now(),
	)
	if err == nil {
		t.Fatal("raw INSERT with duplicate kind succeeded; UNIQUE index missing")
	}
	if !strings.Contains(err.Error(), "UNIQUE") {
		t.Errorf("error = %v, want UNIQUE constraint violation", err)
	}
}

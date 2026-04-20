package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"her/logger"
	"her/memory"

	"github.com/robfig/cron/v3"
)

// scriptedHandler is a Handler whose Execute behavior is controlled by
// a callable script. Tests inject handlers that fail, panic, count
// calls, or record the payload they saw.
type scriptedHandler struct {
	kind       string
	callCount  int
	mu         sync.Mutex
	// fn is invoked on every Execute call. Return the error you want
	// the runner to see; return nil for success.
	fn func(call int, payload json.RawMessage) error
}

func (h *scriptedHandler) Kind() string       { return h.kind }
func (h *scriptedHandler) ConfigPath() string { return "" }
func (h *scriptedHandler) Execute(_ context.Context, payload json.RawMessage, _ *Deps) error {
	h.mu.Lock()
	h.callCount++
	call := h.callCount
	h.mu.Unlock()
	if h.fn == nil {
		return nil
	}
	return h.fn(call, payload)
}

// newRunnerTestScheduler returns a Scheduler wired to a fresh temp DB
// with no tasks registered yet. Caller registers handlers and seeds
// tasks via store.UpsertSchedulerTask directly.
func newRunnerTestScheduler(t *testing.T) (*Scheduler, *memory.Store) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "runner.db")
	store, err := memory.NewStore(dbPath, 0)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	s := &Scheduler{
		store:   store,
		deps:    &Deps{},
		rootDir: t.TempDir(),
		log:     logger.WithPrefix("scheduler-test"),
		parser: cron.NewParser(
			cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow,
		),
	}
	return s, store
}

// seedTask inserts a ready-to-fire task into the store and returns the
// stored row (with its ID populated).
func seedTask(t *testing.T, store *memory.Store, task *memory.SchedulerTask) *memory.SchedulerTask {
	t.Helper()
	if err := store.UpsertSchedulerTask(task); err != nil {
		t.Fatalf("UpsertSchedulerTask: %v", err)
	}
	got, err := store.SchedulerTaskByKind(task.Kind)
	if err != nil {
		t.Fatalf("SchedulerTaskByKind: %v", err)
	}
	if got == nil {
		t.Fatalf("seed: task %q not found after upsert", task.Kind)
	}
	return got
}

func TestDispatch_SuccessAdvancesNextFire(t *testing.T) {
	withCleanRegistry(t)
	s, store := newRunnerTestScheduler(t)

	h := &scriptedHandler{kind: "runner_success"}
	Register(h)

	task := seedTask(t, store, &memory.SchedulerTask{
		Kind:         "runner_success",
		CronExpr:     "0 * * * *", // every hour on the hour
		NextFire:     time.Now().Add(-1 * time.Minute),
		Payload:      json.RawMessage(`{}`),
		RetryBackoff: "none",
	})

	s.dispatch(context.Background(), task)

	if h.callCount != 1 {
		t.Errorf("callCount = %d, want 1", h.callCount)
	}

	got, _ := store.SchedulerTaskByKind("runner_success")
	if got.LastError != "" {
		t.Errorf("LastError = %q, want empty after success", got.LastError)
	}
	if got.AttemptCount != 0 {
		t.Errorf("AttemptCount = %d, want 0 after success", got.AttemptCount)
	}
	// next_fire should have jumped forward to the next top-of-hour.
	if !got.NextFire.After(time.Now()) {
		t.Errorf("NextFire = %v, should be in the future", got.NextFire)
	}
}

func TestDispatch_FailureWithinRetryIncrementsAttempts(t *testing.T) {
	withCleanRegistry(t)
	s, store := newRunnerTestScheduler(t)

	h := &scriptedHandler{
		kind: "runner_fail",
		fn: func(int, json.RawMessage) error {
			return errors.New("handler went boom")
		},
	}
	Register(h)

	task := seedTask(t, store, &memory.SchedulerTask{
		Kind:             "runner_fail",
		CronExpr:         "0 * * * *",
		NextFire:         time.Now().Add(-1 * time.Minute),
		Payload:          json.RawMessage(`{}`),
		RetryMaxAttempts: 3,
		RetryBackoff:     "linear",
		RetryInitialWait: 60 * time.Second,
	})

	s.dispatch(context.Background(), task)

	got, _ := store.SchedulerTaskByKind("runner_fail")
	if !strings.Contains(got.LastError, "boom") {
		t.Errorf("LastError = %q, want substring 'boom'", got.LastError)
	}
	if got.AttemptCount != 1 {
		t.Errorf("AttemptCount = %d, want 1 after first failure", got.AttemptCount)
	}
	// Linear backoff with InitialWait=60s and attempt=1 → 60s from now.
	waitUntil := time.Until(got.NextFire)
	if waitUntil < 50*time.Second || waitUntil > 70*time.Second {
		t.Errorf("NextFire delta = %v, want ~60s (linear backoff)", waitUntil)
	}
}

// TestDispatch_ExhaustedRetriesSkipsToNextCron verifies the "give up
// and wait for next scheduled fire" branch. Without this, a perma-
// failing handler would spin tightly against the backoff clock forever.
func TestDispatch_ExhaustedRetriesSkipsToNextCron(t *testing.T) {
	withCleanRegistry(t)
	s, store := newRunnerTestScheduler(t)

	h := &scriptedHandler{
		kind: "runner_exhaust",
		fn: func(int, json.RawMessage) error {
			return errors.New("perma failure")
		},
	}
	Register(h)

	// AttemptCount=2, MaxAttempts=3 — this failure pushes us to 3, hitting
	// the exhausted branch.
	task := seedTask(t, store, &memory.SchedulerTask{
		Kind:             "runner_exhaust",
		CronExpr:         "0 * * * *",
		NextFire:         time.Now().Add(-1 * time.Minute),
		Payload:          json.RawMessage(`{}`),
		RetryMaxAttempts: 3,
		RetryBackoff:     "linear",
		RetryInitialWait: 60 * time.Second,
		AttemptCount:     2,
	})

	// Bump attempt count to 2 in the DB (the upsert path preserves
	// history, but we're going through a fresh upsert here so we need
	// to set it explicitly via a raw UPDATE).
	if _, err := store.DueSchedulerTasks(time.Now()); err != nil {
		t.Fatalf("DueSchedulerTasks: %v", err)
	}
	// Use MarkSchedulerFailure to seed AttemptCount=2 while keeping
	// next_fire in the past.
	if err := store.MarkSchedulerFailure(task.ID, time.Now().Add(-1*time.Minute), "prior", 2); err != nil {
		t.Fatalf("seed failure count: %v", err)
	}
	task, _ = store.SchedulerTaskByKind("runner_exhaust")

	s.dispatch(context.Background(), task)

	got, _ := store.SchedulerTaskByKind("runner_exhaust")
	if got.AttemptCount != 0 {
		t.Errorf("AttemptCount = %d, want 0 after exhaustion (should reset)", got.AttemptCount)
	}
	// next_fire should jump to the next cron hour, not a retry backoff.
	// Verify by checking the minute is 0 (matches "0 * * * *") and the
	// time is in the future. We don't assert a minimum delta because the
	// next cron could be <5min away when the test runs near :56+.
	if got.NextFire.Minute() != 0 {
		t.Errorf("NextFire minute = %d, want 0 (should land on a cron boundary)", got.NextFire.Minute())
	}
	if !got.NextFire.After(time.Now()) {
		t.Errorf("NextFire = %v, want future (should skip to next cron)", got.NextFire)
	}
}

// TestDispatch_UnknownKindDefers exercises the safety valve for when a
// handler has been renamed/deleted in code but the DB row survived.
func TestDispatch_UnknownKindDefers(t *testing.T) {
	withCleanRegistry(t)
	// No handlers registered.
	s, store := newRunnerTestScheduler(t)

	task := seedTask(t, store, &memory.SchedulerTask{
		Kind:         "ghost_kind",
		CronExpr:     "0 * * * *",
		NextFire:     time.Now().Add(-1 * time.Minute),
		Payload:      json.RawMessage(`{}`),
		RetryBackoff: "none",
	})

	s.dispatch(context.Background(), task)

	got, _ := store.SchedulerTaskByKind("ghost_kind")
	if got == nil {
		t.Fatal("row deleted for unknown kind; should survive for human review")
	}
	if got.LastError == "" {
		t.Error("LastError empty; should note the missing handler")
	}
	// Deferred 24h out (runner.dispatch sets this).
	waitUntil := time.Until(got.NextFire)
	if waitUntil < 23*time.Hour {
		t.Errorf("NextFire delta = %v, want ~24h (deferred)", waitUntil)
	}
}

// TestDispatch_PanicIsRecovered — a buggy extension panicking inside
// Execute must NOT crash the runner. Verifies runHandler catches it.
func TestDispatch_PanicIsRecovered(t *testing.T) {
	withCleanRegistry(t)
	s, store := newRunnerTestScheduler(t)

	h := &scriptedHandler{
		kind: "runner_panic",
		fn: func(int, json.RawMessage) error {
			panic("nil pointer dereference or whatever")
		},
	}
	Register(h)

	task := seedTask(t, store, &memory.SchedulerTask{
		Kind:             "runner_panic",
		CronExpr:         "0 * * * *",
		NextFire:         time.Now().Add(-1 * time.Minute),
		Payload:          json.RawMessage(`{}`),
		RetryMaxAttempts: 2,
		RetryBackoff:     "none",
	})

	// The call should return cleanly (not panic up to us). Any panic
	// here would fail the test via the testing framework's default handling.
	s.dispatch(context.Background(), task)

	got, _ := store.SchedulerTaskByKind("runner_panic")
	if !strings.Contains(got.LastError, "panic") {
		t.Errorf("LastError = %q, want substring 'panic'", got.LastError)
	}
	if got.AttemptCount != 1 {
		t.Errorf("AttemptCount = %d, want 1 (panic = failure)", got.AttemptCount)
	}
}

func TestDispatch_PayloadReachesHandler(t *testing.T) {
	withCleanRegistry(t)
	s, store := newRunnerTestScheduler(t)

	var seen json.RawMessage
	h := &scriptedHandler{
		kind: "runner_payload",
		fn: func(_ int, p json.RawMessage) error {
			seen = p
			return nil
		},
	}
	Register(h)

	want := json.RawMessage(`{"widget":"42","path":["deep","nested"]}`)
	task := seedTask(t, store, &memory.SchedulerTask{
		Kind:         "runner_payload",
		CronExpr:     "0 * * * *",
		NextFire:     time.Now().Add(-1 * time.Minute),
		Payload:      want,
		RetryBackoff: "none",
	})

	s.dispatch(context.Background(), task)

	if string(seen) != string(want) {
		t.Errorf("handler received payload %q, want %q", seen, want)
	}
}

// TestTick_DispatchesEveryDueTask covers the higher-level tick()
// behavior: one call should drain every overdue row.
func TestTick_DispatchesEveryDueTask(t *testing.T) {
	withCleanRegistry(t)
	s, store := newRunnerTestScheduler(t)

	var hits int
	var mu sync.Mutex
	countingFn := func(int, json.RawMessage) error {
		mu.Lock()
		hits++
		mu.Unlock()
		return nil
	}

	Register(&scriptedHandler{kind: "due_a", fn: countingFn})
	Register(&scriptedHandler{kind: "due_b", fn: countingFn})
	Register(&scriptedHandler{kind: "due_c", fn: countingFn})

	for _, k := range []string{"due_a", "due_b", "due_c"} {
		if err := store.UpsertSchedulerTask(&memory.SchedulerTask{
			Kind:         k,
			CronExpr:     "0 * * * *",
			NextFire:     time.Now().Add(-1 * time.Minute),
			Payload:      json.RawMessage(`{}`),
			RetryBackoff: "none",
		}); err != nil {
			t.Fatalf("upsert %s: %v", k, err)
		}
	}

	s.tick(context.Background())

	if hits != 3 {
		t.Errorf("tick hit %d handlers, want 3", hits)
	}
}

func TestTick_RespectsCancellation(t *testing.T) {
	withCleanRegistry(t)
	s, store := newRunnerTestScheduler(t)

	Register(&scriptedHandler{
		kind: "never_runs",
		fn: func(int, json.RawMessage) error {
			t.Error("handler ran despite cancelled context")
			return nil
		},
	})
	_ = seedTask(t, store, &memory.SchedulerTask{
		Kind:         "never_runs",
		CronExpr:     "0 * * * *",
		NextFire:     time.Now().Add(-1 * time.Minute),
		Payload:      json.RawMessage(`{}`),
		RetryBackoff: "none",
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.tick(ctx)
}

// TestRunHandler_WrapsPanicAsError is the unit-level test for the
// recovery in runHandler. We exercise it via dispatch() above too, but
// this asserts the error shape more directly.
func TestRunHandler_WrapsPanicAsError(t *testing.T) {
	h := &scriptedHandler{
		kind: "x",
		fn: func(int, json.RawMessage) error {
			panic("zap")
		},
	}
	err := runHandler(context.Background(), h, nil, &Deps{})
	if err == nil {
		t.Fatal("runHandler returned nil after panic")
	}
	if !strings.Contains(err.Error(), "panic") {
		t.Errorf("err = %v, want substring 'panic'", err)
	}
	if !strings.Contains(err.Error(), "zap") {
		t.Errorf("err = %v, want substring 'zap'", err)
	}
}

// Smoke check: Run() honors ctx.Done() and exits cleanly. We use a
// tiny timeout; if Run blocks, the test blows past this and fails.
func TestRun_StopsOnContextCancel(t *testing.T) {
	withCleanRegistry(t)
	s, _ := newRunnerTestScheduler(t)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v, want nil on graceful shutdown", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

// Silence unused imports when none of the fmt-based helpers are needed.
var _ = fmt.Sprintf

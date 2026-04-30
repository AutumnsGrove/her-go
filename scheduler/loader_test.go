package scheduler

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"her/memory"

	"github.com/robfig/cron/v3"
)

// writeYAML is a test helper that writes YAML content to a temp file
// inside rootDir at the given relative path. Returns the relative path
// so it can be used directly as ConfigPath.
func writeYAML(t *testing.T, rootDir, relPath, body string) string {
	t.Helper()
	fullPath := filepath.Join(rootDir, relPath)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(fullPath, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return relPath
}

// newLoaderTestStore is a store helper with the scheduler_tasks table.
func newLoaderTestStore(t *testing.T) memory.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "loader.db")
	store, err := memory.NewStore(dbPath, 0)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestReadTaskConfig_HappyPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task.yaml")
	body := `kind: widget_ticker
cron: "*/5 * * * *"
payload:
  threshold: 3
retry:
  max_attempts: 2
  backoff: exponential
  initial_wait: 30s
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := readTaskConfig(path)
	if err != nil {
		t.Fatalf("readTaskConfig: %v", err)
	}

	if cfg.Kind != "widget_ticker" {
		t.Errorf("Kind = %q, want %q", cfg.Kind, "widget_ticker")
	}
	if cfg.Cron != "*/5 * * * *" {
		t.Errorf("Cron = %q, want %q", cfg.Cron, "*/5 * * * *")
	}
	if cfg.Retry.MaxAttempts != 2 {
		t.Errorf("Retry.MaxAttempts = %d, want 2", cfg.Retry.MaxAttempts)
	}
	if cfg.Retry.Backoff != "exponential" {
		t.Errorf("Retry.Backoff = %q, want exponential", cfg.Retry.Backoff)
	}
	if cfg.Retry.InitialWait != 30*time.Second {
		t.Errorf("Retry.InitialWait = %v, want 30s", cfg.Retry.InitialWait)
	}

	// Payload is pass-through so the handler controls its shape; YAML
	// maps become map[string]any which we re-marshal to JSON at load
	// time (see loader.go).
	m, ok := cfg.Payload.(map[string]any)
	if !ok {
		t.Fatalf("Payload type = %T, want map[string]any", cfg.Payload)
	}
	if _, ok := m["threshold"]; !ok {
		t.Errorf("Payload map missing 'threshold' key: %v", m)
	}
}

func TestReadTaskConfig_MissingFile(t *testing.T) {
	_, err := readTaskConfig(filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil {
		t.Fatal("readTaskConfig(missing file) returned nil error")
	}
}

func TestReadTaskConfig_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	body := "kind: widget_ticker\n  cron: this is not: valid yaml ::: at all\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := readTaskConfig(path); err == nil {
		t.Fatal("readTaskConfig(bad yaml) returned nil error")
	}
}

func TestComputeNextFire_Daily21(t *testing.T) {
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

	// Reference time: Jan 15 2026 10:00 — next fire should be same day 21:00.
	ref := time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC)
	got, err := computeNextFire(parser, "0 21 * * *", ref)
	if err != nil {
		t.Fatalf("computeNextFire: %v", err)
	}
	want := time.Date(2026, 1, 15, 21, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("nextFire = %v, want %v", got, want)
	}
}

func TestComputeNextFire_Daily21_AfterFireTime(t *testing.T) {
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

	// Reference 22:00 means today's fire is past — want tomorrow 21:00.
	ref := time.Date(2026, 1, 15, 22, 0, 0, 0, time.UTC)
	got, err := computeNextFire(parser, "0 21 * * *", ref)
	if err != nil {
		t.Fatalf("computeNextFire: %v", err)
	}
	want := time.Date(2026, 1, 16, 21, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("nextFire = %v, want %v", got, want)
	}
}

func TestComputeNextFire_EmptyCronErrors(t *testing.T) {
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	if _, err := computeNextFire(parser, "", time.Now()); err == nil {
		t.Error("computeNextFire(empty) returned nil error")
	}
}

func TestComputeNextFire_InvalidCronErrors(t *testing.T) {
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	if _, err := computeNextFire(parser, "not a cron expression", time.Now()); err == nil {
		t.Error("computeNextFire(bad) returned nil error")
	}
}

// testHandler is a full Handler implementation for loader integration
// tests — the handler Execute body is never called by the loader, but
// we need a real registration for loadAndUpsertAll to pick it up.
type testHandler struct {
	kind       string
	configPath string
}

func (h *testHandler) Kind() string       { return h.kind }
func (h *testHandler) ConfigPath() string { return h.configPath }
func (h *testHandler) Execute(_ context.Context, _ json.RawMessage, _ *Deps) error {
	return nil
}

func TestNew_UpsertsRegisteredHandlers(t *testing.T) {
	withCleanRegistry(t)

	root := t.TempDir()
	writeYAML(t, root, "mood/task.yaml", `kind: mood_daily_rollup
cron: "0 21 * * *"
payload: {}
retry:
  max_attempts: 2
  backoff: exponential
  initial_wait: 60s
`)
	Register(&testHandler{kind: "mood_daily_rollup", configPath: "mood/task.yaml"})

	store := newLoaderTestStore(t)
	_, err := New(store, &Deps{}, root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	task, err := store.SchedulerTaskByKind("mood_daily_rollup")
	if err != nil {
		t.Fatalf("SchedulerTaskByKind: %v", err)
	}
	if task == nil {
		t.Fatal("task not upserted by New")
	}
	if task.CronExpr != "0 21 * * *" {
		t.Errorf("CronExpr = %q, want %q", task.CronExpr, "0 21 * * *")
	}
	if task.RetryMaxAttempts != 2 {
		t.Errorf("RetryMaxAttempts = %d, want 2", task.RetryMaxAttempts)
	}
}

func TestNew_KindMismatchErrors(t *testing.T) {
	withCleanRegistry(t)

	root := t.TempDir()
	// YAML declares one kind; handler declares another. Should error —
	// this catches typos where task.yaml gets renamed without updating code.
	writeYAML(t, root, "things/task.yaml", `kind: wrong_kind
cron: "0 * * * *"
`)
	Register(&testHandler{kind: "right_kind", configPath: "things/task.yaml"})

	store := newLoaderTestStore(t)
	_, err := New(store, &Deps{}, root)
	if err == nil {
		t.Fatal("New with kind mismatch returned nil error")
	}
	if !strings.Contains(err.Error(), "kind") {
		t.Errorf("error = %v, want substring %q", err, "kind")
	}
}

func TestNew_InvalidBackoffErrors(t *testing.T) {
	withCleanRegistry(t)

	root := t.TempDir()
	writeYAML(t, root, "a/task.yaml", `kind: k
cron: "0 * * * *"
retry:
  backoff: fibonacci
`)
	Register(&testHandler{kind: "k", configPath: "a/task.yaml"})

	store := newLoaderTestStore(t)
	_, err := New(store, &Deps{}, root)
	if err == nil {
		t.Fatal("New with bad backoff returned nil error")
	}
	if !strings.Contains(err.Error(), "backoff") {
		t.Errorf("error = %v, want substring %q", err, "backoff")
	}
}

// TestNew_PreservesExistingNextFireWhenCronUnchanged is the test that
// catches restart-storm regressions: if the loader overwrites next_fire
// on every startup, then restarting the bot twice inside an hour would
// push the rollup an extra hour each time, eventually missing the 21:00
// window entirely.
func TestNew_PreservesExistingNextFireWhenCronUnchanged(t *testing.T) {
	withCleanRegistry(t)

	root := t.TempDir()
	writeYAML(t, root, "x/task.yaml", `kind: preserve_me
cron: "0 21 * * *"
retry:
  backoff: none
`)
	Register(&testHandler{kind: "preserve_me", configPath: "x/task.yaml"})

	store := newLoaderTestStore(t)

	// First load — computes next_fire from cron.
	if _, err := New(store, &Deps{}, root); err != nil {
		t.Fatalf("first New: %v", err)
	}
	first, _ := store.SchedulerTaskByKind("preserve_me")
	firstNext := first.NextFire

	// Pretend the runner hasn't fired yet (and won't for hours). Second
	// call to New should keep the same next_fire.
	if _, err := New(store, &Deps{}, root); err != nil {
		t.Fatalf("second New: %v", err)
	}
	second, _ := store.SchedulerTaskByKind("preserve_me")

	if !second.NextFire.Equal(firstNext) {
		t.Errorf("NextFire after restart = %v, want preserved %v", second.NextFire, firstNext)
	}
}

// TestNew_RecomputesNextFireWhenCronChanges is the flip side: if the
// user edits the cron expression in task.yaml, we DO want the next fire
// re-derived — otherwise the schedule change would silently do nothing
// until the old fire time hit.
func TestNew_RecomputesNextFireWhenCronChanges(t *testing.T) {
	withCleanRegistry(t)

	root := t.TempDir()
	writeYAML(t, root, "y/task.yaml", `kind: recompute_me
cron: "0 21 * * *"
retry:
  backoff: none
`)
	Register(&testHandler{kind: "recompute_me", configPath: "y/task.yaml"})

	store := newLoaderTestStore(t)
	if _, err := New(store, &Deps{}, root); err != nil {
		t.Fatalf("first New: %v", err)
	}
	first, _ := store.SchedulerTaskByKind("recompute_me")

	// Rewrite the YAML with a different cron.
	writeYAML(t, root, "y/task.yaml", `kind: recompute_me
cron: "0 9 * * *"
retry:
  backoff: none
`)
	if _, err := New(store, &Deps{}, root); err != nil {
		t.Fatalf("second New: %v", err)
	}
	second, _ := store.SchedulerTaskByKind("recompute_me")

	if second.NextFire.Equal(first.NextFire) {
		t.Errorf("NextFire unchanged after cron edit; should have been recomputed")
	}
	if second.CronExpr != "0 9 * * *" {
		t.Errorf("CronExpr = %q, want updated %q", second.CronExpr, "0 9 * * *")
	}
}

func TestNew_HandlerWithoutConfigPathIsSkipped(t *testing.T) {
	withCleanRegistry(t)

	// No YAML; handler declares empty ConfigPath. Loader should skip it
	// without error — useful for tests and for handlers registered in
	// code without a static config.
	Register(&testHandler{kind: "code_only", configPath: ""})

	store := newLoaderTestStore(t)
	if _, err := New(store, &Deps{}, t.TempDir()); err != nil {
		t.Fatalf("New: %v", err)
	}

	got, _ := store.SchedulerTaskByKind("code_only")
	if got != nil {
		t.Errorf("code-only handler got a DB row: %v", got)
	}
}

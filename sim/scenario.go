package sim

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"her/memory"
	"her/scheduler"
)

// TestingT is the slice of *testing.T (and testing.TB) the Harness
// needs. Defined locally so non-test callers (like the simtest CLI)
// can supply their own stub — you can't implement testing.TB from
// outside the testing package because it has unexported methods.
//
// Any *testing.T satisfies this implicitly.
type TestingT interface {
	Helper()
	Fatalf(format string, args ...any)
	Cleanup(fn func())
	TempDir() string
}

// Harness is the bundle of dependencies a scenario operates against.
// It stands in for the entire bot stack but with every external edge
// (Telegram, OpenRouter, wall clock) swapped for a fake.
//
// Scenarios mutate the Harness through Step functions and then assert
// on its state — usually `h.Transport.Events()` for "what the user
// saw" or direct DB queries for "what the bot remembered."
type Harness struct {
	T         TestingT
	Ctx       context.Context
	Clock     *FakeClock
	Transport *FakeTransport
	LLM       *FakeLLM
	Store     *memory.Store
	Scheduler *scheduler.Scheduler

	// ChatID is the single-user chat every scenario runs against. It's
	// fixed per-harness so scenarios don't have to thread it through
	// every Send/Edit call.
	ChatID int64

	// RootDir is the project root passed to scheduler.New (for
	// resolving Handler.ConfigPath()). Defaults to a fresh temp dir so
	// scenarios can write task.yaml fixtures without touching the real
	// repo.
	RootDir string

	// Out is where scenario narration goes when running via the CLI
	// (io.Discard during normal `go test`).
	Out io.Writer
}

// HarnessOptions tweaks harness construction. Zero-values are fine.
type HarnessOptions struct {
	// StartAt pins the FakeClock. Defaults to a stable base
	// (2026-01-15 10:00 UTC) so scenarios are reproducible.
	StartAt time.Time

	// ChatID overrides the default chat ID (42).
	ChatID int64

	// RootDir overrides the scheduler root dir. Default: t.TempDir().
	RootDir string

	// Out lets scenario runners (the CLI) attach a log writer. Default:
	// io.Discard.
	Out io.Writer
}

// NewHarness constructs a Harness with fresh fakes and a temp SQLite.
// Register scheduler extensions BEFORE calling NewHarness so the
// loader picks them up.
func NewHarness(t TestingT, opts HarnessOptions) *Harness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	start := opts.StartAt
	if start.IsZero() {
		start = time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC)
	}
	clock := NewFakeClock(start)

	dbPath := filepath.Join(t.TempDir(), "sim.db")
	store, err := memory.NewStore(dbPath, 0)
	if err != nil {
		t.Fatalf("sim: NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	transport := NewFakeTransport(clock)
	llm := NewFakeLLM(t)

	chatID := opts.ChatID
	if chatID == 0 {
		chatID = 42
	}

	rootDir := opts.RootDir
	if rootDir == "" {
		rootDir = t.TempDir()
	}

	deps := &scheduler.Deps{
		Store:   store,
		Send:    transport.SendFunc(),
		SendPNG: transport.SendPNGFunc(),
		ChatID:  chatID,
	}
	sched, err := scheduler.New(store, deps, rootDir)
	if err != nil {
		t.Fatalf("sim: scheduler.New: %v", err)
	}

	out := opts.Out
	if out == nil {
		out = io.Discard
	}

	return &Harness{
		T:         t,
		Ctx:       ctx,
		Clock:     clock,
		Transport: transport,
		LLM:       llm,
		Store:     store,
		Scheduler: sched,
		ChatID:    chatID,
		RootDir:   rootDir,
		Out:       out,
	}
}

// Step is a single narrated action in a scenario. Steps run in order
// and operate on the shared Harness. A step returning an error halts
// the scenario immediately; its Name is printed either way so CLI
// output reads like a pytest progress list.
type Step struct {
	Name string
	Do   func(h *Harness) error
}

// Assertion is evaluated at the end of a scenario's steps. Assertions
// get their own slice so they're visually distinct from steps in the
// scenario text — "here's what the scenario does, here's what must
// hold at the end."
type Assertion struct {
	Name  string
	Check func(h *Harness) error
}

// Scenario bundles steps, assertions, and metadata. Scenarios register
// themselves via RegisterScenario() at init time so both the test
// suite and the CLI pick them up without further wiring.
type Scenario struct {
	Name        string
	Description string
	Setup       func(h *Harness) error
	Steps       []Step
	Assertions  []Assertion
}

var (
	registryMu sync.Mutex
	registry   = map[string]Scenario{}
)

// RegisterScenario adds a scenario to the global registry. Usually
// called from init() in each *_scenario.go file.
func RegisterScenario(s Scenario) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if s.Name == "" {
		panic("sim.RegisterScenario: empty name")
	}
	if _, exists := registry[s.Name]; exists {
		panic("sim.RegisterScenario: duplicate name " + s.Name)
	}
	registry[s.Name] = s
}

// LookupScenario returns a scenario by name. Used by the CLI when the
// user runs `her sim -scenario foo`.
func LookupScenario(name string) (Scenario, bool) {
	registryMu.Lock()
	defer registryMu.Unlock()
	s, ok := registry[name]
	return s, ok
}

// AllScenarios returns every registered scenario, sorted by name.
func AllScenarios() []Scenario {
	registryMu.Lock()
	defer registryMu.Unlock()
	out := make([]Scenario, 0, len(registry))
	for _, s := range registry {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ResetScenariosForTest clears the registry. Tests that register a
// scenario ad-hoc use it to avoid cross-test pollution.
func ResetScenariosForTest() {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry = map[string]Scenario{}
}

// Run executes a scenario from start to finish, respecting any early
// error. Each step and assertion is announced via h.Out so the CLI
// gets a readable transcript.
func (s Scenario) Run(h *Harness) error {
	h.T.Helper()

	fmt.Fprintf(h.Out, "── scenario: %s ──\n", s.Name)
	if s.Description != "" {
		fmt.Fprintf(h.Out, "  %s\n\n", s.Description)
	}

	if s.Setup != nil {
		fmt.Fprintln(h.Out, "  setup")
		if err := s.Setup(h); err != nil {
			return fmt.Errorf("setup: %w", err)
		}
	}

	for i, step := range s.Steps {
		fmt.Fprintf(h.Out, "  [%d] %s\n", i+1, step.Name)
		if err := step.Do(h); err != nil {
			return fmt.Errorf("step %q: %w", step.Name, err)
		}
	}

	if len(s.Assertions) == 0 {
		fmt.Fprintln(h.Out, "  (no assertions — smoke test)")
		return nil
	}

	fmt.Fprintln(h.Out, "  assertions")
	var firstErr error
	for _, a := range s.Assertions {
		err := a.Check(h)
		mark := "✓"
		if err != nil {
			mark = "✗"
			if firstErr == nil {
				firstErr = err
			}
		}
		fmt.Fprintf(h.Out, "    %s %s", mark, a.Name)
		if err != nil {
			fmt.Fprintf(h.Out, " — %v", err)
		}
		fmt.Fprintln(h.Out)
	}
	return firstErr
}

// --- Convenience helpers for writing scenario steps -----------------

// AdvanceAndTick jumps the clock forward and runs the scheduler once.
// This is the common "simulate time passing" primitive — scenarios use
// it to let daily rollups come due.
func (h *Harness) AdvanceAndTick(d time.Duration) {
	h.Clock.Advance(d)
	h.Scheduler.TickOnce(h.Ctx)
}

// SeedOneShot schedules a synthetic task for the given kind to fire at
// the current sim time (so the next TickOnce will dispatch it). Useful
// when a scenario wants to exercise a handler without waiting for cron.
func (h *Harness) SeedOneShot(kind string, payload json.RawMessage) error {
	t := &memory.SchedulerTask{
		Kind:         kind,
		CronExpr:     "0 * * * *", // ignored for one-shot-style seeding
		NextFire:     h.Clock.Now(),
		Payload:      payload,
		RetryBackoff: "none",
	}
	return h.Store.UpsertSchedulerTask(t)
}

// WriteTaskYAML writes a task.yaml at relPath inside h.RootDir. Use
// this when a scenario needs to exercise the loader path end-to-end.
func (h *Harness) WriteTaskYAML(relPath, body string) error {
	full := filepath.Join(h.RootDir, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	return os.WriteFile(full, []byte(body), 0o644)
}

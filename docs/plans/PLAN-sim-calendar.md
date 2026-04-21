# Plan: Sim Harness Extensions for Calendar Testing

Extend the sim harness to support calendar and shift testing: a bridge interface with a fake implementation for sims, seed fields for shifts and calendar events, sim.db schema additions for results tracking, and a `calendar-a-thon.yaml` sim spec.

**Status:** Phase 1 complete, ready for Phase 2 - sim YAML fields
**Completed:** 
- Calendar bridge with multi-calendar support, list_calendars tool, and all 4 CRUD operations (commit 2ba807f)
- Bridge interface extraction: `Bridge` interface + `CLIBridge` (prod) + `FakeBridge` (sim/test) (2026-04-21)
- All tool handlers updated to use `NewCLIBridge()`
- Comprehensive unit tests for FakeBridge (all 5 commands tested)
**Tracking:** GH issue #64

---

## Current State (2026-04-21)

✅ **Complete:**
- Swift EventKit bridge with multi-calendar support (comma-separated names, wildcard search)
- Go tools: `list_calendars`, `calendar_list`, `calendar_create`, `calendar_update`, `calendar_delete`
- Wire protocol: `Request{Command, Calendar, Args}` and `Response{OK, Result, Error, Message}`
- All tools tested end-to-end with real Apple Calendar

**Swift bridge commands:**
- `list_calendars` (no args) → returns array of calendar names
- `list` (calendar: "Cal1,Cal2" or "*", start, end) → returns events with calendar field
- `create` (calendar: default, events: [{title, start, end, calendar?, ...}]) → returns event IDs
- `update` (calendar: "*", id, event: {title?, start?, ...}) → returns updated ID
- `delete` (calendar: "*", id) → returns deleted: true

🎯 **Next:** Extract `Bridge` interface so `CLIBridge` (prod) and `FakeBridge` (sims) share the same contract.

---

## Goals

- Run calendar and shift sims without requiring EventKit permission or the Swift binary.
- Seed pre-existing shifts and calendar events via YAML fields in sim specs.
- Capture shift and scheduler job state in sim.db for post-run analysis.
- Provide a reference sim (`calendar-a-thon.yaml`) covering the core shift workflows.

---

## Part 1 -- Bridge interface extraction

**Current code:** `calendar/bridge.go` has a concrete `Bridge` struct with `Call(ctx, Request) (Response, error)`. Request/Response are already well-defined. Need to:
1. Extract `Bridge` as an interface
2. Rename current implementation to `CLIBridge`
3. Update `NewBridge()` → `NewCLIBridge()`
4. Build `FakeBridge` for sims

```go
// calendar/bridge.go (updated)

// Bridge is the interface for calendar operations. Prod implementation
// shells out to the Swift CLI; test/sim implementation is in-memory.
type Bridge interface {
    Call(ctx context.Context, req Request) (Response, error)
}

// CLIBridge is the production implementation (existing Bridge struct renamed).
type CLIBridge struct {
    binaryPath string
    cfg        *config.Config
    logger     *log.Logger
}

func NewCLIBridge(cfg *config.Config, logger *log.Logger) *CLIBridge { ... }

// calendar/bridge_fake.go (NEW)
type FakeBridge struct {
    events    map[string]*FakeEvent  // keyed by event ID
    calendars []string                // available calendar names
    counter   int                     // for generating FAKE-001, FAKE-002...
    mu        sync.Mutex
}

type FakeEvent struct {
    ID       string
    Title    string
    Start    time.Time
    End      time.Time
    Location string
    Notes    string
    Calendar string  // which calendar this event belongs to
}

func NewFakeBridge(calendars []string) *FakeBridge { ... }
```

**FakeBridge capabilities:**
- `list_calendars`: returns the configured calendar list
- `list`: filters by calendar name(s) and time range, returns matching events
- `create`: generates deterministic IDs (`FAKE-001`, `FAKE-002`...), respects per-event calendar field
- `update`: modifies in-memory event by ID (wildcard `"*"` searches all calendars)
- `delete`: removes in-memory event by ID (wildcard supported)

**Why a fake and not a mock per test:** sims drive the real agent end-to-end through real tools, and each tool internally calls the bridge. A single fake (shared process-wide for the sim run) means tools work unchanged and we see genuine tool-call sequences in the report.

---

## Part 2 -- Sim YAML seed fields

New fields on the sim spec struct, processed after memory seeding and before the message loop.

**Design decision:** Shift metadata (job, role, scheduled_hours, actual_hours, status) will be stored in the calendar event's `notes` field as structured text. No new EventKit fields needed - just parse/format the notes on read/write. Example:
```
job=Panera
role=Bake
scheduled_hours=8.0
actual_hours=9.0
status=worked
stayed late to close
```

### `seed_shifts`

Inserted directly into `work_shifts` with pre-assigned `calendar_event_id`s (no bridge call). Corresponding fake events are also created in the `FakeBridge` so `calendar_list` returns them (with shift metadata in notes field).

```yaml
seed_shifts:
  - job: "Panera"
    scheduled_start: "2026-04-13T05:00:00-04:00"
    scheduled_end:   "2026-04-13T13:00:00-04:00"
    actual_start:    "2026-04-13T05:00:00-04:00"
    actual_end:      "2026-04-13T14:00:00-04:00"
    status: "worked"
    notes: "stayed late to close"
  - job: "Panera"
    scheduled_start: "2026-04-14T05:00:00-04:00"
    scheduled_end:   "2026-04-14T13:00:00-04:00"
    status: "scheduled"
    reminder: false   # don't auto-enqueue a reminder for this seed
```

### `seed_calendar_events`

Populates the `FakeBridge`'s in-memory store only (not the DB -- pure calendar events have no DB representation).

```yaml
seed_calendar_events:
  - id: "FAKE-DENTIST-1"
    title: "Dentist"
    start: "2026-04-22T14:00:00-04:00"
    end:   "2026-04-22T15:00:00-04:00"
```

### Go-side changes

Add to `cmd/sim.go`:

```go
type SeedShift struct {
    Job            string `yaml:"job"`
    ScheduledStart string `yaml:"scheduled_start"`
    ScheduledEnd   string `yaml:"scheduled_end"`
    ActualStart    string `yaml:"actual_start,omitempty"`
    ActualEnd      string `yaml:"actual_end,omitempty"`
    Status         string `yaml:"status"`
    Notes          string `yaml:"notes,omitempty"`
    Reminder       *bool  `yaml:"reminder,omitempty"`  // nil = default (true)
}

type SeedCalendarEvent struct {
    ID       string `yaml:"id"`
    Title    string `yaml:"title"`
    Start    string `yaml:"start"`
    End      string `yaml:"end"`
    Location string `yaml:"location,omitempty"`
    Notes    string `yaml:"notes,omitempty"`
}
```

Seed loop: after memory seeding, iterate `seed_shifts` to insert DB rows + create matching fake bridge events. Then iterate `seed_calendar_events` to populate the fake bridge only.

Sim runner picks the `FakeBridge` when `HER_SIM_MODE=1` (or a config flag).

---

## Part 3 -- sim.db schema additions

The sim results database currently snapshots memories, mood entries, metrics, and agent turns per run. Add parallel tables for shifts and scheduler jobs.

```sql
CREATE TABLE IF NOT EXISTS sim_shifts (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id              INTEGER NOT NULL REFERENCES sim_runs(id),
    captured_at         DATETIME DEFAULT CURRENT_TIMESTAMP,
    shift_id            INTEGER,
    job                 TEXT,
    role                TEXT,
    scheduled_start     TEXT,
    scheduled_end       TEXT,
    actual_start        TEXT,
    actual_end          TEXT,
    scheduled_hours     REAL,
    actual_hours        REAL,
    status              TEXT,
    calendar_event_id   TEXT,
    active              INTEGER,
    superseded_by       INTEGER,
    supersede_reason    TEXT,
    notes               TEXT
);

CREATE TABLE IF NOT EXISTS sim_scheduler_jobs (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id      INTEGER NOT NULL REFERENCES sim_runs(id),
    captured_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    job_id      INTEGER,
    kind        TEXT,
    fire_at     TEXT,
    payload     TEXT,
    status      TEXT,
    attempts    INTEGER,
    last_error  TEXT,
    fired_at    TEXT
);
```

**Snapshot point:** same place the runner currently snapshots memories (end of run). Fresh scan of `work_shifts` and `scheduler_jobs` from her.db, insert into sim.db keyed by `run_id`. Preserves audit history (superseded rows included).

---

## Part 4 -- `calendar-a-thon.yaml` sim

**Style reference:** model on `sims/inbox-cleanup.yaml` (added in `9a8ebee`), not the older fact-a-thon. Rich `description`, `tags`, `seed_*` block, multi-step flow assertions.

### Scenarios

1. **Schedule drop** -- user pastes a 5-shift batch. Agent parses, calls `shift_schedule`, reports totals.
2. **Clock out** -- user says "just got off work." Agent calls `shift_log_time` with implicit on-time start.
3. **Shift moved** -- "Wed actually moved to Thu." Agent calls `shift_update`, supersede chain created.
4. **Hours query** -- "How many hours last week?" Agent calls `shift_list`, reports totals from response.

### Assertions

- Tool-call sequences match expected patterns per scenario.
- Reply mentions computed hours (not hallucinated).
- Superseded rows visible in sim.db post-run.
- No bridge errors (fake bridge should handle everything cleanly).

---

## Decisions

| Decision | Choice | Why |
|---|---|---|
| Fake vs mock | Single process-wide fake bridge | Sims drive the real agent through real tools. A shared fake means tools work unchanged; we see genuine tool-call sequences. |
| Seed shifts create fake events | Yes, both DB + fake bridge | So `calendar_list` returns seeded shifts if the agent queries the calendar directly. Consistency. |
| Sim.db snapshots include superseded rows | Yes | Lets you inspect how a shift evolved across turns in post-run analysis. |

## Known Limitations (v1)

- **FakeBridge doesn't simulate errors.** No permission denied, no EventKit failures. Error-path testing requires unit tests with explicit mocks, not sims.
- **No reminder firing in sims.** The scheduler tick loop may not run during sims (depends on timing). Reminder enqueue is verifiable via `sim_scheduler_jobs` table, but actual delivery is not tested in sims.

---

## Phases

### Phase 1 -- Bridge interface extraction (prod vs fake)

**Files to modify:**
- `calendar/bridge.go` — Extract `Bridge` interface, rename existing `Bridge` struct → `CLIBridge`, rename `NewBridge()` → `NewCLIBridge()`
- All tool handlers (`tools/*/handler.go`) — Change `calendar.NewBridge()` → `calendar.NewCLIBridge()` OR inject bridge via context

**Files to create:**
- `calendar/bridge_fake.go` — Implement `FakeBridge` with in-memory event store
- `calendar/bridge_fake_test.go` — Unit tests for FakeBridge (list, create, update, delete, list_calendars)

**Sim runner integration:**
- `cmd/sim.go` — Detect sim mode (env var or flag), use `NewFakeBridge(cfg.Calendar.Calendars)` instead of CLIBridge
- Pass bridge instance through agent.RunParams (new field) so tools can access it

**Done when:** 
- All existing calendar tools work with both `CLIBridge` (prod) and `FakeBridge` (sim/test)
- `FakeBridge` unit tests pass for all 5 commands (list_calendars, list, create, update, delete)
- A simple sim with `seed_calendar_events` can run without Swift binary or EventKit permissions

### Phase 2 -- Sim YAML fields + seed logic

- Add `SeedShift` and `SeedCalendarEvent` structs to sim spec
- Implement seed loop in `cmd/sim.go` (after memory seeding, before message loop)
- Seed shifts create both DB rows and fake bridge events
- Seed calendar events populate fake bridge only

**Done when:** a sim spec with `seed_shifts` and `seed_calendar_events` populates state correctly before the message loop starts.

### Phase 3 -- sim.db schema additions

- Add `sim_shifts` and `sim_scheduler_jobs` tables to sim.db migrations
- Add snapshot logic at end-of-run to capture shift and scheduler job state

**Done when:** post-run sim.db contains accurate snapshots of all shift and scheduler job rows from the run.

### Phase 4 -- `calendar-a-thon.yaml` sim

- Write the sim spec covering all 4 scenarios
- Run it, verify tool-call sequences and reply quality
- Iterate on seed data and assertions until the sim is a reliable regression test

**Done when:** sim runs green with correct tool sequences, computed hours in replies, and supersede chains visible in sim.db.

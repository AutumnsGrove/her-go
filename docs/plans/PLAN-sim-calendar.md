---
title: "Sim Harness Calendar Extensions"
status: complete
created: 2026-04-20
updated: 2026-04-21
category: infrastructure
completed: 2026-04-21
---

# Plan: Sim Harness Extensions for Calendar Testing

Extend the sim harness to support calendar and shift testing: a bridge interface with a fake implementation for sims, seed fields for shifts and calendar events, sim.db schema additions for results tracking, and a `calendar-a-thon.yaml` sim spec.

**Completed:** 
- Calendar bridge with multi-calendar support, list_calendars tool, and all 4 CRUD operations (commit 2ba807f)
- Bridge interface extraction: `Bridge` interface + `CLIBridge` (prod) + `FakeBridge` (sim/test)
- All tool handlers updated to use injected bridge (via `tools.Context.CalendarBridge`)
- Comprehensive unit tests for FakeBridge (all 5 commands tested)
- `seed_calendar_events` YAML field + seeding logic (inserts into DB + FakeBridge)
- Shifts simplified: just calendar events with `job` field (aligned with PLAN-shifts.md)
- `SeedCalendarEvent.Job` ready for PLAN-shifts.md Phase 1 schema migration
- `sim_calendar_events` table added to sim.db schema with `job` column
- Calendar event snapshots captured at end-of-run via `copyCalendarEvents`
- Calendar tool handlers registered in agent/agent.go (blank imports)
- `calendar-a-thon.yaml` reference sim with 6 test scenarios
- Calendar Events section added to sim reports (markdown table with job field)

**Dependencies:**
- Full shift support requires PLAN-shifts.md Phase 1 (job column + InsertCalendarEvent signature update)

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

New field on the sim spec struct: `seed_calendar_events`. Processed after memory seeding and before the message loop.

**Design decision:** Shifts ARE calendar events. No separate `work_shifts` seeding — just seed calendar events with shift metadata in the `notes` field. The calendar tools handle everything (create, list, update, delete). Shift-specific behavior is inferred from metadata like `type: shift` in the notes.

### `seed_calendar_events`

Populates both SQLite (source of truth) and FakeBridge (EventKit simulation). Can represent any calendar event: meetings, shifts, appointments, etc.

```yaml
seed_calendar_events:
  - id: "SEED-001"
    title: "Panera shift"
    start: "2026-04-13T05:00:00-04:00"
    end:   "2026-04-13T13:00:00-04:00"
    job: "Panera"  # Marks this as a shift event (requires PLAN-shifts.md Phase 1 migration)
    notes: |
      position: Bake
      trainer: Mike
  - id: "SEED-002"
    title: "Dentist"
    start: "2026-04-22T14:00:00-04:00"
    end:   "2026-04-22T15:00:00-04:00"
    location: "Main St Dental"
```

### Go-side changes

Already done in Phase 2:
- `SeedCalendarEvent` struct in `cmd/sim.go` with `job` field for shift events
- Seeding loop inserts into DB via `store.InsertCalendarEvent()` (job param requires PLAN-shifts.md Phase 1 migration)
- Also seeds FakeBridge so `calendar_list` returns them
- FakeBridge passed to agent via `CalendarBridge` field
- Job field logged when seeding shift events

---

## Part 3 -- sim.db schema additions

The sim results database currently snapshots memories, mood entries, metrics, and agent turns per run. Add a table for calendar events.

```sql
CREATE TABLE IF NOT EXISTS sim_calendar_events (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id      INTEGER NOT NULL REFERENCES sim_runs(id),
    captured_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    event_id    TEXT,      -- EventKit identifier
    title       TEXT,
    start       TEXT,      -- ISO8601
    end         TEXT,      -- ISO8601
    location    TEXT,
    notes       TEXT,      -- May contain shift metadata like "position: Bake\ntrainer: Mike"
    calendar    TEXT,
    job         TEXT       -- Shift job name (e.g., "Panera") — NULL for regular events
);
```

**Snapshot point:** same place the runner currently snapshots memories (end of run). Fresh scan of `calendar_events` from her.db, insert into sim.db keyed by `run_id`. Shows the state of the calendar after the sim completes.

---

## Part 4 -- `calendar-a-thon.yaml` sim

**Style reference:** model on `sims/inbox-cleanup.yaml` (added in `9a8ebee`), not the older fact-a-thon. Rich `description`, `tags`, `seed_*` block, multi-step flow assertions.

### Scenarios

1. **Create events** -- user says "Add a dentist appointment tomorrow at 2pm." Agent calls `calendar_create`, confirms creation.
2. **List events** -- "What's on my calendar this week?" Agent calls `calendar_list`, reports upcoming events.
3. **Update event** -- "Actually move the dentist to Thursday." Agent calls `calendar_update`, confirms change.
4. **Delete event** -- "Cancel that appointment." Agent calls `calendar_delete`, confirms deletion.

Optional shift scenarios (if you want to test shift metadata parsing):
5. **Create shift** -- "I work at Panera Monday 5am-1pm." Agent creates calendar event with `type: shift` metadata.
6. **List shifts** -- "Show my shifts this week." Agent calls `calendar_list`, filters for `type: shift` in notes.

### Assertions

- Tool-call sequences match expected patterns per scenario.
- Events appear in `sim_calendar_events` table after the run.
- Updated/deleted events reflect correct final state.
- No bridge errors (FakeBridge handles all operations cleanly).

---

## Decisions

| Decision | Choice | Why |
|---|---|---|
| Fake vs mock | Single process-wide fake bridge | Sims drive the real agent through real tools. A shared fake means tools work unchanged; we see genuine tool-call sequences. |
| Shifts are calendar events | Yes, no separate work_shifts table or seeding | Simplifies the model. A "shift" is just a calendar event with metadata like `type: shift`. Same CRUD tools for everything. |
| Seed to both DB + FakeBridge | Yes | DB is source of truth (calendar_list reads from it). FakeBridge simulates EventKit for testing bridge operations. |

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

- Add `SeedCalendarEvent` struct to sim spec
- Implement seed loop in `cmd/sim.go` (after memory seeding, before message loop)
- Seed calendar events insert into DB (SQLite) + populate FakeBridge
- Calendar events can represent anything: shifts, meetings, appointments

**Done when:** a sim spec with `seed_calendar_events` populates both DB and FakeBridge correctly before the message loop starts.

### Phase 3 -- sim.db schema additions

- Add `sim_calendar_events` table to sim.db schema (includes `job` column for shifts)
- Add `copyCalendarEvents` function to snapshot calendar state at end-of-run
- Call `copyCalendarEvents` in the copy sequence after mood entries

**Done when:** post-run sim.db contains accurate snapshots of all calendar events (both regular events and shifts) from the run.

### Phase 4 -- `calendar-a-thon.yaml` sim

- Write the sim spec covering all 6 scenarios (4 CRUD + 2 shift-specific)
- Register calendar tool handlers in agent/agent.go (blank imports)
- Add calendar events section to sim reports
- Run and verify tool-call sequences work end-to-end

**Done when:** sim runs green with correct tool sequences, events appear in sim.db, and Calendar Events section shows final state in reports.

**Completed:** 2026-04-21 (commit 29a270a)

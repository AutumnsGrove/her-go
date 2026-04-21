# Plan: Sim Harness Extensions for Calendar Testing

Extend the sim harness to support calendar and shift testing: a bridge interface with a fake implementation for sims, seed fields for shifts and calendar events, sim.db schema additions for results tracking, and a `calendar-a-thon.yaml` sim spec.

**Status:** ready for implementation
**Depends on:** PLAN-calendar-bridge (bridge interface definition). Can start once the `Bridge` interface shape is defined, before all tools are complete.
**Tracking:**

---

## Goals

- Run calendar and shift sims without requiring EventKit permission or the Swift binary.
- Seed pre-existing shifts and calendar events via YAML fields in sim specs.
- Capture shift and scheduler job state in sim.db for post-run analysis.
- Provide a reference sim (`calendar-a-thon.yaml`) covering the core shift workflows.

---

## Part 1 -- Bridge interface extraction

The prod bridge shells out to the Swift CLI. Sims need a fake that keeps events in memory. Extract a shared interface so tools don't care which implementation they're using.

```go
// calendar/bridge.go

// Bridge is the interface for calendar operations. Prod implementation
// shells out to the Swift CLI; test/sim implementation is in-memory.
type Bridge interface {
    Call(ctx context.Context, req Request) (Response, error)
}

// CLIBridge is the production implementation.
type CLIBridge struct { ... }

// FakeBridge is the in-memory implementation for sims and tests.
// calendar/bridge_fake.go
type FakeBridge struct {
    events map[string]Event  // keyed by event ID
    mu     sync.Mutex
}
```

The fake supports all 4 commands (list, create, update, delete). Create generates deterministic IDs (`FAKE-<counter>`). List filters by time range. Update and delete operate on the in-memory map.

**Why a fake and not a mock per test:** sims drive the real agent end-to-end through real tools, and each tool internally calls the bridge. A single fake (shared process-wide for the sim run) means tools work unchanged and we see genuine tool-call sequences in the report.

---

## Part 2 -- Sim YAML seed fields

New fields on the sim spec struct, processed after memory seeding and before the message loop.

### `seed_shifts`

Inserted directly into `work_shifts` with pre-assigned `calendar_event_id`s (no bridge call). Corresponding fake events are also created in the `FakeBridge` so `calendar_list` returns them.

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

- Extract `Bridge` interface in `calendar/bridge.go`
- Move existing (or planned) CLI implementation behind it
- Implement `FakeBridge` in `calendar/bridge_fake.go`
- Wire sim runner to use `FakeBridge` when in sim mode

**Done when:** existing calendar tools work with both implementations; `FakeBridge` passes the same logical tests as the CLI bridge.

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

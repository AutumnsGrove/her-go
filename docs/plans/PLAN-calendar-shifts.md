# Plan: Calendar Integration + Shift Tracking + One-Off Reminders

Wire her into Apple Calendar via a small Swift EventKit bridge, give her a job-aware shift tracker (scheduled vs. actual hours, audit history), and extend the existing scheduler to fire one-off jobs so each shift gets a "leave for work" reminder.

**Status:** design locked via interview. Ready for implementation.

**Branch:** `claude/schedule-calendar-integration-cASf7`

**Related:**
- `docs/plans/PLAN-mood-tracking-redesign.md` — the scheduler this plan extends
- `REFACTOR.md` — confirms `get_current_time` was moved to a layer (no time tool needed)

---

## Goals

- **Read & write Apple Calendar** through a tiny Swift CLI bridge (`her-calendar`) using EventKit. JSON over stdin/stdout. No HTTP, no daemon.
- **Track work shifts**, not just calendar events. One row per shift with both scheduled and actual times, linked to the calendar event by id. Hours computed in Go, not by the LLM.
- **Audit-friendly.** Edits don't overwrite — they supersede, mirroring the memory pattern (`memory/store_facts.go:434`). Cancellations don't delete. Full history queryable.
- **Generic, not job-named.** Tools are `shift_schedule` / `calendar_create` etc., never `add_panera_shift`. Jobs (Panera, Cava, anything else) are config rows.
- **Proactive reminders** — "🍞 Panera in 45 min" on Telegram. Per-job timing in config, agent can override per-shift.
- **Extend the scheduler, don't replace it.** The current scheduler handles one row per recurring kind; we add a sibling table for one-off jobs so the same Handler interface serves both.
- **No new time tool.** `layers/agent_time.go` already injects current time + tz into the agent prompt every turn — sufficient for grounding "Tuesday at 5am" → a date.

## Architecture overview

```
                     ┌──────────────────────────────────────┐
                     │              main agent              │
                     │   (Qwen3 — orchestrates the turn)    │
                     └──────────────┬───────────────────────┘
                                    │ tool calls
                  ┌─────────────────┼─────────────────┐
                  ▼                 ▼                 ▼
          ┌──────────────┐  ┌──────────────┐  ┌───────────────┐
          │ shift_*      │  │ calendar_*   │  │ scheduler     │
          │ (combo)      │  │ (calendar    │  │ enqueues one- │
          │              │  │ only)        │  │ off jobs      │
          └──────┬───────┘  └──────┬───────┘  └───────┬───────┘
                 │                 │                  │
                 ▼                 ▼                  ▼
          ┌──────────────┐  ┌──────────────────┐  ┌──────────────────┐
          │ work_shifts  │  │ her-calendar     │  │ scheduler_jobs   │
          │ (SQLite)     │  │ (Swift / EventKit│  │ (SQLite, new)    │
          └──────────────┘  │  CLI bridge)     │  └─────────┬────────┘
                            └──────────────────┘            │
                                                            ▼
                                                  ┌──────────────────┐
                                                  │ shift_reminder   │
                                                  │ Handler → TG     │
                                                  └──────────────────┘
```

**Three layers, three responsibilities:**

1. **Swift bridge (`calendar/bridge/her-calendar`).** Single binary. Reads a JSON command from stdin, performs an EventKit operation, writes a JSON response to stdout. Knows nothing about jobs or shifts — just events.
2. **Go tools (`tools/`).** Combo tools (`shift_*`) handle calendar + DB atomically with retry/backoff. Pure calendar tools (`calendar_*`) exist for ad-hoc events. All routes shell out to the Swift bridge.
3. **Scheduler (`scheduler/`).** Existing recurring task system gets a sibling table (`scheduler_jobs`) for one-offs. Same `Handler` interface. Per-shift reminders are enqueued at schedule time, cancelled on edit/cancel.

---

## Part 2 — Config additions

All shape lives in `config.yaml`. Code reads it via `cfg.Calendar.*` — no model, calendar, or path strings ever appear inline in `.go` (per the project's primary design principle).

```yaml
calendar:
  # Path to the compiled Swift bridge binary. Relative paths are resolved
  # from the project root. Bot logs a warning at startup if missing and
  # all calendar tools become no-ops with a clear error message.
  bridge_path: "calendar/bridge/.build/release/her-calendar"

  # EventKit calendar to read/write. Must already exist in Apple Calendar —
  # her does not auto-create. Errors loudly at first calendar_* call if missing.
  calendar_name: "Work"

  # Used when the agent passes a "naive" timestamp (no offset). The agent
  # is instructed to always include the offset, but this is a safety net.
  default_timezone: "America/New_York"

  # Default minutes-before-start for reminders when no per-job override
  # exists and the agent doesn't specify one.
  default_reminder_minutes: 30

  # Generic job list. Add or remove freely — code never references these
  # by name. Match is case-insensitive against name + aliases.
  jobs:
    - name: "Panera"
      address: "3625 Spring Hill Pkwy SE, Smyrna, GA 30080"
      default_role: ""              # blank = read role from schedule text
      reminder_minutes: 45          # overrides default_reminder_minutes
      aliases: ["panera bread"]

    - name: "Cava"
      address: "855 Peachtree St NE, Atlanta, GA 30308"
      default_role: "Grill Cook"
      reminder_minutes: 60
      aliases: []
```

**Config struct (Go side):**

```go
// config/config.go additions

type CalendarConfig struct {
    BridgePath             string       `yaml:"bridge_path"`
    CalendarName           string       `yaml:"calendar_name"`
    DefaultTimezone        string       `yaml:"default_timezone"`
    DefaultReminderMinutes int          `yaml:"default_reminder_minutes"`
    Jobs                   []JobConfig  `yaml:"jobs"`
}

type JobConfig struct {
    Name             string   `yaml:"name"`
    Address          string   `yaml:"address"`
    DefaultRole      string   `yaml:"default_role"`
    ReminderMinutes  int      `yaml:"reminder_minutes"`  // 0 = use default
    Aliases          []string `yaml:"aliases"`
}

// MatchJob returns the job whose name or alias matches (case-insensitive),
// or nil if no match. Used by shift_schedule to validate the job param.
func (c *CalendarConfig) MatchJob(name string) *JobConfig { ... }

// ReminderMinutesFor returns the per-job override or the default.
func (c *CalendarConfig) ReminderMinutesFor(job string) int { ... }
```

**`config.yaml.example`** gets the same block with comments explaining each field.

---

## Part 3 — SQLite schema

Two new tables, both following existing project conventions (`memory/store.go` style — `IF NOT EXISTS` migrations, ISO 8601 timestamps as TEXT).

### `work_shifts`

```sql
CREATE TABLE IF NOT EXISTS work_shifts (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    job                 TEXT NOT NULL,             -- matches config jobs[].name
    role                TEXT,                      -- e.g. "Grill Cook", nullable
    scheduled_start     TEXT NOT NULL,             -- ISO 8601 with offset
    scheduled_end       TEXT NOT NULL,             -- ISO 8601 with offset
    actual_start        TEXT,                      -- nullable until clocked
    actual_end          TEXT,                      -- nullable until clocked
    calendar_event_id   TEXT,                      -- EventKit event identifier
    status              TEXT NOT NULL DEFAULT 'scheduled',
                        -- scheduled | worked | no_show | cancelled
    notes               TEXT,
    active              INTEGER NOT NULL DEFAULT 1,
    superseded_by       INTEGER REFERENCES work_shifts(id),
    supersede_reason    TEXT,
    created_at          TEXT NOT NULL,
    updated_at          TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_shifts_active_start
    ON work_shifts(active, scheduled_start);
CREATE INDEX IF NOT EXISTS idx_shifts_job_active
    ON work_shifts(job, active);
CREATE INDEX IF NOT EXISTS idx_shifts_event
    ON work_shifts(calendar_event_id);
```

**Two orthogonal axes — by design:**

| Axis | Field(s) | Meaning |
|---|---|---|
| Lifecycle | `status` | What happened to the shift-as-event: scheduled, worked, no-show, cancelled. |
| Version history | `active` + `superseded_by` + `supersede_reason` | Tracks edits to the shift's *definition* (time moved, hours changed). |

**Examples:**
- Cancelled shift: one row, `status='cancelled'`, `active=1`. Calendar event renamed `[CANCELLED] …`.
- No-show: one row, `status='no_show'`, `actual_start == actual_end`, hours=0. Calendar event untouched.
- Time moved Wed → Thu: two rows. Old row `active=0`, `superseded_by=<new_id>`, `supersede_reason='moved Wed to Thu'`. New row inherits `calendar_event_id` (same event, updated in place).

**Helpers (`memory/store_shifts.go`, mirroring `memory/store_facts.go`):**

```go
func (s *Store) InsertShift(sh Shift) (int64, error)
func (s *Store) GetShift(id int64) (Shift, error)
func (s *Store) ListShifts(filter ShiftFilter) ([]Shift, ShiftTotals, error)
func (s *Store) UpdateShiftActuals(id int64, actualStart, actualEnd *time.Time, notes string) error
func (s *Store) UpdateShiftStatus(id int64, status string) error
func (s *Store) SupersedeShift(oldID, newID int64, reason string) error
func (s *Store) ShiftHistory(currentID int64) ([]Shift, error)  // walk supersede chain backwards
```

`ListShifts` returns per-row `scheduled_hours` / `actual_hours` plus a `ShiftTotals{ScheduledHours, ActualHours, Count}` summary so the agent never has to do time math.

### `scheduler_jobs`

Sibling to existing `scheduler_tasks`. One-offs only — recurring stays in `scheduler_tasks`.

```sql
CREATE TABLE IF NOT EXISTS scheduler_jobs (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    kind          TEXT NOT NULL,                   -- handler key, e.g. "shift_reminder"
    fire_at       TEXT NOT NULL,                   -- ISO 8601
    payload       TEXT,                            -- JSON, opaque to scheduler
    status        TEXT NOT NULL DEFAULT 'pending', -- pending | done | failed | cancelled
    attempts      INTEGER NOT NULL DEFAULT 0,
    last_error    TEXT,
    fired_at      TEXT,                            -- when actually executed
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_jobs_due
    ON scheduler_jobs(status, fire_at);
```

**No-delete rule applies here too.** Done jobs stay in the table. A maintenance task can prune `status IN ('done','cancelled') AND fired_at < now() - 90 days` later if the table grows; not in v1.

---

## Part 4 — Swift bridge (`her-calendar`)

A single Swift CLI binary that speaks EventKit. Go never touches EventKit APIs directly — it shells out to `her-calendar` and pipes JSON in and out.

### Why a CLI, not a daemon

- **Simpler.** No HTTP server, no ports, no keepalive. One invocation per call.
- **Sandbox-friendly.** The macOS permission prompt has to come from a GUI-attached process on first use. A Terminal-launched binary counts; a launchd daemon may not.
- **Reproducible install.** `swift build -c release` produces one executable. No app bundle, no signing ceremony.

### Layout

```
calendar/
  bridge/
    Package.swift
    Sources/
      her-calendar/
        main.swift           # stdin/stdout dispatcher
        Commands.swift       # list / create / update / delete
        JSON.swift           # Codable models
    README.md                # build + permission-grant steps
    .gitignore               # .build/
```

### Wire protocol

Single-shot: one JSON command on stdin, one JSON response on stdout, process exits. All commands share an envelope.

**Request:**
```json
{
  "command": "list" | "create" | "update" | "delete",
  "calendar": "Work",
  "args": { ... command-specific ... }
}
```

**Response (success):**
```json
{ "ok": true, "result": { ... command-specific ... } }
```

**Response (error):**
```json
{ "ok": false, "error": "permission_denied" | "calendar_not_found" | "event_not_found" | "...", "message": "Human-readable detail." }
```

Exit codes: 0 = success, 1 = bridge error (bad JSON, no permission, etc.), 2 = calendar-side error (event not found, etc.). Go distinguishes them for retry decisions.

### Commands

**`list`** — events in a window:
```json
// args
{ "start": "2026-04-20T00:00:00-04:00", "end": "2026-04-27T00:00:00-04:00" }
// result
{ "events": [
    { "id": "ABC123", "title": "Panera 5a-1p", "start": "...", "end": "...",
      "location": "3625 Spring Hill...", "notes": "..." }
]}
```

**`create`** — one or many events atomic-per-call:
```json
// args
{ "events": [
    { "title": "Panera 5a-1p", "start": "...", "end": "...",
      "location": "...", "notes": "..." }
]}
// result
{ "events": [ { "id": "ABC123" } ] }
```

On EventKit failure mid-batch, the bridge attempts to delete anything it successfully created in this call, then returns the error. That gives the Go side a clean "nothing persisted" signal for retry decisions.

**`update`** — in-place edit by id. Omitted fields are left unchanged:
```json
// args
{ "id": "ABC123", "title": "...", "start": "...", "end": "...",
  "location": "...", "notes": "..." }
// result
{ "id": "ABC123" }
```

**`delete`** — by id:
```json
// args
{ "id": "ABC123" }
// result
{ "deleted": true }
```

### Install + permissions (Autumn's setup, one-time)

Per-decision: manual one-time setup. README in `calendar/bridge/` walks through:

1. `cd calendar/bridge && swift build -c release`
2. Binary appears at `.build/release/her-calendar`.
3. Run it once from Terminal: `echo '{"command":"list","calendar":"Work","args":{"start":"2026-04-20T00:00:00-04:00","end":"2026-04-21T00:00:00-04:00"}}' | .build/release/her-calendar`
4. macOS shows the EventKit permission prompt. Click Allow.
5. Subsequent invocations (including from her, running headless) use the granted permission.

**If permission was denied**, System Settings → Privacy & Security → Calendars → enable `her-calendar`.

### Bridge is optional at startup

Her boots even if the bridge is missing or unbuildable. On startup:

- `agent/agent.go` (or wherever tool init runs) checks `cfg.Calendar.BridgePath` exists and is executable.
- If missing, log a single warning: `calendar bridge not found at <path>; calendar/shift tools will return errors if called`.
- Tool handlers return a clear error message to the agent (`"calendar bridge not installed — see calendar/bridge/README.md"`) so it can tell the user.

No panics, no startup failures. Consistent with how `get_weather` handles a missing API key.

### Retry + backoff (Go side)

Every Swift-bridge invocation from a tool goes through a shared helper:

```go
// calendar/bridge.go
func (b *Bridge) Call(ctx context.Context, req Request) (Response, error) {
    // 3 attempts: 0ms, 500ms, 1s, 2s backoff.
    // Retry only on exit code 1 (bridge error) — calendar-side errors
    // (event not found, calendar missing) fail fast.
}
```

Retry budget per tool call: 3 attempts. Total worst-case latency: ~3.5s. Logged at each retry so flaky permissions/EventKit-locked-by-Calendar.app scenarios are visible in `logger`.

---

## Part 5 — Tool catalog

Two namespaces. **`shift_*`** are combos that touch both calendar and DB atomically. **`calendar_*`** are pure calendar (for ad-hoc events that aren't shifts: dentist, birthday, etc.).

All tools follow the existing pattern: a `tool.yaml` manifest in `tools/<name>/` plus a `handler.go` that registers via `tools.Register`. None are hot — all loaded via `use_tools(["calendar"])`.

### Shift tools (combo: calendar + DB)

#### `shift_schedule`

Create one or more shifts for a job. Calendar events created first (with retry); on success, DB rows inserted; on partial failure, persists what succeeded and reports the rest.

```yaml
# tools/shift_schedule/tool.yaml
name: shift_schedule
description: >-
  Schedule one or more work shifts for a known job. Creates calendar
  events in {{her}}'s configured Work calendar AND writes shift rows
  to the local DB so they can be tracked, edited, and reminded about.
  Use this when the user pastes a work schedule or asks to add shifts.
  Always include the timezone offset in start/end (use the Current Time
  block's timezone if not specified). Returns shift_ids and event_ids
  per shift, plus any failures with reasons.
hot: false
category: calendar
parameters:
  type: object
  properties:
    job:
      type: string
      description: "Job name from config — e.g. 'Panera'. Aliases match case-insensitively."
    role:
      type: string
      description: "Optional role override (e.g. 'Grill Cook'). Defaults to job's default_role from config."
    shifts:
      type: array
      description: "One or more shifts to schedule. All atomic-per-call on the calendar side."
      items:
        type: object
        properties:
          start:
            type: string
            description: "ISO 8601 with offset, e.g. 2026-04-21T05:00:00-04:00"
          end:
            type: string
            description: "ISO 8601 with offset"
          notes:
            type: string
            description: "Optional per-shift notes"
          reminder_minutes:
            type: integer
            description: "Optional override of the per-job reminder timing"
        required: [start, end]
  required: [job, shifts]
```

**Return shape:**
```json
{
  "scheduled": [
    { "shift_id": 17, "event_id": "ABC", "start": "...", "end": "...",
      "scheduled_hours": 8.0, "reminder_at": "2026-04-21T04:15:00-04:00",
      "warnings": ["overlaps with shift_id 14 (Cava 4-9p)"] }
  ],
  "failed": [
    { "index": 2, "start": "...", "error": "calendar bridge timeout after 3 attempts" }
  ],
  "totals": { "shifts_scheduled": 4, "shifts_failed": 1, "scheduled_hours": 32.0 }
}
```

**Behavior notes:**
- Per-decision: **partial success.** Successful shifts persist; failed ones returned in `failed[]`. Tool doesn't return an error unless ALL shifts failed.
- **Overlap detection** (per-decision: warn but allow): for each new shift, query active `work_shifts` where ranges intersect. Append a warning string to that shift's return entry. Don't block.
- **Reminders enqueued atomically** with the shift insert — same DB transaction. If the scheduler insert fails, the shift insert rolls back too (the calendar event lingers; cleanup is the agent's job via `shift_cancel`).

#### `shift_update`

Edit an existing shift's scheduled time. Creates a new shift row, supersedes the old one, updates the EventKit event in place (keeps the same `calendar_event_id`), cancels the old reminder, enqueues a new one.

```yaml
name: shift_update
description: >-
  Move or resize an existing scheduled shift. Use when {{user}} says
  things like "actually that's Thursday not Wednesday" or "they pushed
  me to start at 6 instead of 5". Looks up the shift via shift_list
  first to get the shift_id. Old row preserved as audit history
  (active=0, superseded_by=new_id). Calendar event updated in place.
hot: false
category: calendar
parameters:
  type: object
  properties:
    shift_id:
      type: integer
      description: "ID of the active shift row (from shift_list)"
    start:
      type: string
      description: "New start, ISO 8601 with offset. Omit to leave unchanged."
    end:
      type: string
      description: "New end, ISO 8601 with offset. Omit to leave unchanged."
    role:
      type: string
      description: "Updated role. Omit to leave unchanged."
    reason:
      type: string
      description: "Why the change happened — stored in supersede_reason for audit (e.g. 'moved Wed to Thu')"
  required: [shift_id, reason]
```

**Return:**
```json
{
  "old_shift_id": 17,
  "new_shift_id": 18,
  "event_id": "ABC",          // same event, updated in place
  "scheduled_hours": 8.0,
  "warnings": ["overlaps with shift_id 19 (Panera 4-9p)"]
}
```

#### `shift_cancel`

Mark a shift cancelled (boss removed it). Row preserved. Calendar event renamed `[CANCELLED] <original title>` to keep history visible. Pending reminder cancelled.

```yaml
name: shift_cancel
description: >-
  Mark a scheduled shift as cancelled (e.g., boss took {{user}} off the
  schedule). Does NOT delete the row — cancellations are part of work
  history. Calendar event is renamed with [CANCELLED] prefix so it
  stays visible. Pending reminder is cancelled. Use shift_log_time
  with zero hours instead if {{user}} simply didn't show up.
hot: false
category: calendar
parameters:
  type: object
  properties:
    shift_id:
      type: integer
    reason:
      type: string
      description: "Optional reason, stored in notes (e.g. 'boss removed', 'store closed for snow')"
  required: [shift_id]
```

#### `shift_log_time`

Record actual clock-in/out for a shift. If `actual_start` omitted, defaults to `scheduled_start` (the common "showed up on time" case).

```yaml
name: shift_log_time
description: >-
  Record actual hours worked for a shift {{user}} has finished. If
  actual_start is omitted, defaults to the shift's scheduled_start
  (common case — they clocked in on time). Pass actual_start ==
  actual_end to log a no-show (zero hours). Use this — not shift_cancel
  — when {{user}} simply didn't go in.
hot: false
category: calendar
parameters:
  type: object
  properties:
    shift_id:
      type: integer
    actual_start:
      type: string
      description: "ISO 8601 with offset. Defaults to scheduled_start if omitted."
    actual_end:
      type: string
      description: "ISO 8601 with offset. Required."
    notes:
      type: string
      description: "Optional — 'stayed late to close', 'covered for Sam', etc."
  required: [shift_id, actual_end]
```

**Behavior:** updates `actual_start`, `actual_end`, `notes` on the active row. Sets `status='worked'` (or `status='no_show'` if `actual_start == actual_end`). Returns the row with computed `actual_hours`.

#### `shift_list`

Query shifts. The agent's primary way to get `shift_id`s for editing or to answer "how many hours did I work last week."

```yaml
name: shift_list
description: >-
  List shifts in a date range with computed hours and totals. Use this
  to answer "how much did I work" questions or to look up shift_ids
  before calling shift_update / shift_cancel / shift_log_time. Returns
  active rows by default; pass include_history=true to walk supersede
  chains for audit.
hot: false
category: calendar
parameters:
  type: object
  properties:
    start:
      type: string
      description: "ISO 8601 with offset. Defaults to 30 days ago."
    end:
      type: string
      description: "ISO 8601 with offset. Defaults to 7 days from now."
    job:
      type: string
      description: "Filter to a specific job (case-insensitive, matches aliases)."
    status:
      type: string
      description: "Filter: scheduled | worked | no_show | cancelled. Omit for all."
    include_history:
      type: boolean
      description: "Include superseded (active=0) rows. Default false."
  required: []
```

**Return:**
```json
{
  "shifts": [
    { "shift_id": 17, "job": "Panera", "role": "...",
      "scheduled_start": "...", "scheduled_end": "...",
      "actual_start": "...", "actual_end": "...",
      "scheduled_hours": 8.0, "actual_hours": 9.0,
      "status": "worked", "calendar_event_id": "ABC", "notes": "..." }
  ],
  "totals": {
    "scheduled_hours": 40.0, "actual_hours": 41.5,
    "by_status": { "scheduled": 0, "worked": 5, "no_show": 0, "cancelled": 0 }
  }
}
```

### Calendar tools (pure calendar, no DB)

For events that aren't work shifts. Same Swift bridge underneath.

#### `calendar_list`

```yaml
name: calendar_list
description: >-
  Read events from {{her}}'s configured calendar in a date range.
  Use when the user asks "what's on my calendar" or for context like
  "am I free Friday afternoon".
parameters:
  start: ISO 8601 with offset (required)
  end:   ISO 8601 with offset (required)
  calendar: optional, defaults to config.calendar.calendar_name
```

#### `calendar_create`

```yaml
name: calendar_create
description: >-
  Create one or more calendar events that are NOT work shifts (use
  shift_schedule for those). Examples: dentist appointment, birthday,
  meeting. Atomic-per-call.
parameters:
  events: array of { title, start, end, location?, notes? }
  calendar: optional override
```

#### `calendar_update`

```yaml
name: calendar_update
description: >-
  Edit a calendar event in place by id. Get the id from calendar_list.
  Pass only the fields you want to change.
parameters:
  event_id: required
  title|start|end|location|notes: all optional
```

#### `calendar_delete`

```yaml
name: calendar_delete
description: >-
  Delete a calendar event by id. Use sparingly — for shifts, prefer
  shift_cancel which preserves history.
parameters:
  event_id: required
```

### Tool count summary

| Tool | Calendar | DB | Reminder |
|---|---|---|---|
| `shift_schedule` | create (batch) | insert | enqueue |
| `shift_update` | update | supersede + insert | cancel + enqueue |
| `shift_cancel` | rename `[CANCELLED]` | status update | cancel |
| `shift_log_time` | — | update actuals | — |
| `shift_list` | — | read | — |
| `calendar_list` | read | — | — |
| `calendar_create` | create (batch) | — | — |
| `calendar_update` | update | — | — |
| `calendar_delete` | delete | — | — |

9 new tools, 1 category (`calendar`).

---

## Part 6 — Scheduler enhancement (one-off jobs)

The current scheduler (`scheduler/scheduler.go`) handles **recurring** tasks: one row per `Kind`, fires on a cron schedule. We extend it to also handle **one-offs**: many rows per `Kind`, each with a specific `fire_at` and payload.

### Design

**Same `Handler` interface, two source tables.**

```go
// scheduler/types.go — Handler stays unchanged
type Handler interface {
    Kind() string
    ConfigPath() string                                       // empty string is fine for one-off-only handlers
    Execute(ctx context.Context, payload json.RawMessage, deps *Deps) error
}
```

A handler can register for one or both modes. `shift_reminder` is one-off-only (has no recurring schedule). The mood rollup is recurring-only. Nothing forbids a future handler from being both.

**Tick loop changes (`scheduler.go:tick`):**

```
1. (existing) Process scheduler_tasks where next_fire <= now.
2. (NEW) Process scheduler_jobs where status='pending' AND fire_at <= now,
         ORDER BY fire_at ASC LIMIT N.
   For each:
     a. handler := lookup(row.kind)
     b. if handler == nil: mark status='failed', last_error='unknown kind'
     c. else: handler.Execute(ctx, row.payload, deps)
     d. on success: status='done', fired_at=now, attempts++
     e. on error: attempts++; if attempts < retry.MaxAttempts and retry policy
        allows, update fire_at to now + RetryConfig.NextWait(attempts);
        else status='failed'.
```

### New scheduler API

```go
// scheduler/jobs.go (new file)

// EnqueueJob schedules a one-off task to fire at a specific time.
// Returns the job id so callers can cancel it later.
func (s *Scheduler) EnqueueJob(kind string, fireAt time.Time, payload any) (int64, error)

// CancelJob marks a pending job as cancelled. Idempotent — already-fired
// or already-cancelled jobs return nil without error.
func (s *Scheduler) CancelJob(id int64) error

// ListPendingJobs returns all pending jobs of a given kind, ordered by
// fire_at. Used by maintenance/debug paths.
func (s *Scheduler) ListPendingJobs(kind string) ([]Job, error)
```

The `Scheduler` struct itself doesn't change shape — these are methods on the existing receiver. The DB store gets matching `InsertJob`, `CancelJob`, `DueJobs(now)` methods on `memory.Store`.

### Why this and not Path A or B from interview

The interview offered three paths (generic queue / specialized handler / extend infra). Per Autumn's call: extend the infrastructure rather than work around it. This buys:

- **One-offs available to any future feature** (dentist reminders, persona reflection trigger, weekly digests) without more plumbing.
- **No semantic change** to existing recurring tasks. The mood rollup keeps working untouched.
- **Same retry policy** (`RetryConfig` from `scheduler/types.go`) applies to both modes.

### Concurrency note

The existing tick loop is single-threaded (one goroutine, one `time.Ticker`). We keep that. If a tick processes both recurring and one-off rows, total tick time grows linearly with backlog size. With a 30s tick interval and per-call work measured in milliseconds, we have plenty of headroom — backlog of hundreds of due jobs would still complete inside one tick. Revisit if/when we have a feature firing thousands of one-offs.

---

## Part 7 — Reminders (`shift_reminder` handler)

A single Handler that fires per-shift one-off jobs, sends a Telegram message, and marks the job done. Templated message text (per design principle: data in YAML, not Go).

### Layout

```
scheduler/
  shift_reminder/
    handler.go               # implements scheduler.Handler
    task.yaml                # message template + retry policy
```

### `task.yaml`

This file is the source of truth for reminder phrasing. Hot-reloadable on next tick (the loader reads YAML on each fire — same pattern as the mood rollup).

```yaml
# scheduler/shift_reminder/task.yaml
kind: shift_reminder

# This handler is one-off only; cron is empty.
cron: ""

# Default message template. {{.Job}}, {{.MinutesAway}}, {{.StartTime}},
# {{.Address}}, {{.Role}} are available. Plain Go text/template.
message_template: |
  🍞 {{.Job}} in {{.MinutesAway}} min — clock in at {{.StartTime}}{{if .Address}} ({{.Address}}){{end}}.

# Retry on Telegram delivery failure (e.g., transient network).
retry:
  max_attempts: 3
  backoff: exponential
  initial_wait: 30s
```

A future enhancement: per-job message overrides (e.g., a different emoji per job). Out of scope for v1 — one template covers the case.

### Handler

```go
// scheduler/shift_reminder/handler.go

package shift_reminder

import (
    "context"
    "encoding/json"
    "her/memory"
    "her/scheduler"
    "text/template"
)

type Payload struct {
    ShiftID int64 `json:"shift_id"`
}

type handler struct {
    tmpl *template.Template
    cfg  Config  // loaded from task.yaml
}

func init() {
    scheduler.Register(&handler{ /* lazy-load tmpl on first Execute */ })
}

func (h *handler) Kind() string         { return "shift_reminder" }
func (h *handler) ConfigPath() string   { return "scheduler/shift_reminder/task.yaml" }

func (h *handler) Execute(ctx context.Context, raw json.RawMessage, deps *scheduler.Deps) error {
    var p Payload
    if err := json.Unmarshal(raw, &p); err != nil { return err }

    store := deps.Store.(*memory.Store)
    sh, err := store.GetShift(p.ShiftID)
    if err != nil { return err }

    // Skip if cancelled or already worked — don't ping for stale reminders.
    if sh.Status != "scheduled" || sh.Active == 0 {
        return nil  // success, no-op
    }

    msg := h.render(sh)
    _, err = deps.Send(deps.ChatID, msg)
    return err
}
```

### Lifecycle wiring

| Event | Reminder action |
|---|---|
| `shift_schedule` creates shift #17 | `EnqueueJob("shift_reminder", scheduled_start - reminder_minutes, {shift_id: 17})` → store returned `job_id` on shift row (new column? or query by payload? — see design call below) |
| `shift_update` supersedes #17 → #18 | `CancelJob(old_job_id)`, `EnqueueJob` for the new row |
| `shift_cancel` on #17 | `CancelJob(old_job_id)` |
| Reminder fires | Handler re-checks shift status; skips if no longer scheduled |

**Design call for the linkage:** how does `shift_update` find the pending reminder job to cancel? Two options:

- **A — store `reminder_job_id` on `work_shifts`** (new column, FK to `scheduler_jobs.id`). One DB lookup. Simple.
- **B — query `scheduler_jobs WHERE kind='shift_reminder' AND payload LIKE '%"shift_id":17%' AND status='pending'`.** No schema change but a JSON-substring scan — gross and brittle.

Going with **A**. Add `reminder_job_id INTEGER` to `work_shifts` (also nullable so non-shift-using schedules still work).

### Reminder cancellation safety

`shift_reminder.handler` re-checks shift state on fire (status, active). So even if `CancelJob` races with the tick, we won't ping for a cancelled shift — the handler treats it as a no-op. Belt and suspenders.

### Update to `work_shifts` schema

Add one column:
```sql
ALTER TABLE work_shifts ADD COLUMN reminder_job_id INTEGER REFERENCES scheduler_jobs(id);
```
(Or include in the initial CREATE TABLE if landing both at once.)

---

## Part 8 — Wiring (categories, agent prompt, cleanup)

### `tools/categories.yaml`

Add one entry:

```yaml
calendar:
  hint: "User mentions a work shift, schedule, calendar event, or asks about hours worked"
```

That's it — the existing `RenderCategoryTable()` (`tools/loader.go:355`) picks it up automatically and the agent sees it in the deferred-tools table on next boot. No `main_agent_prompt.md` edit required (the table between markers is regenerated at load time).

### Agent prompt

The static text in `main_agent_prompt.md` doesn't strictly need new flows — the tool descriptions are detailed enough. But two small additions help the agent learn the pattern faster:

1. Add an example flow under "Typical Flows":
   ```
   7. User pastes a work schedule:
      think("schedule drop, parse into shifts") →
      use_tools(["calendar"]) →
      shift_schedule({job:"...", shifts:[...]}) →
      reply("scheduled N shifts, total X hrs") → done
   ```
2. Add a one-line note under "Order of Operations" that the Current Time block is the source of truth for grounding "Tuesday at 5am" → an actual date.

### Stale-code cleanup (from audit)

Bundled with this work, since we're touching adjacent surface area:

| File | Fix |
|---|---|
| `tools/loader.go:354` | Doc comment example references `get_current_time` (nonexistent). Replace with `get_weather, set_location` (the actual `context` category). |
| `compact/agent_summary_prompt.md:4` | Tool names are stale: `save_fact, update_fact, remove_fact, create_reminder` → `save_memory, update_memory, remove_memory`. Drop `create_reminder` (still doesn't exist as a tool — and won't, post-this-plan, since reminders are scheduler-driven, not agent-driven). |
| `sims/tool-a-thon.yaml:21` | References nonexistent `get_current_time`. Either remove the turn or rewrite to use `set_location` only. |
| `docs/skills-architecture.md` lines 185, 921-934 | References nonexistent `log_mood` and `get_current_time`. Out of scope for this PR — flag for separate follow-up. |

### `_junkdrawer/`

Untouched. Old `get_current_time`, `set_location`, `weather.go`, etc. are dormant by directory name and don't affect live code.

---

## Part 9 — Design decisions log

Pulled from the planning conversation, recorded for future-Autumn (and future-Claude) so the rationale doesn't get lost.

| Decision | Choice | Why |
|---|---|---|
| Bridge transport | Single Swift CLI, JSON over stdin/stdout | Simplest. No daemon, no ports, no signing ceremony. macOS permission prompt fires on first manual Terminal run. |
| Bridge install | Manual one-time setup | Triggering the EventKit permission prompt requires a GUI-attached process at least once; documented as a setup step. |
| Batch atomicity | Partial success + report | Don't lose 4 good shifts because shift #5 had a bad timestamp. Failed ones returned in `failed[]`. |
| Overlap policy | Warn but allow | Real life has overlapping commitments (a meeting during a shift). Surface the warning, let the user decide. |
| Backfill of existing calendar events | Fresh start, no import | Lowest risk. Pre-existing calendar events stay untouched, just aren't tracked. Backfill becomes a future CLI command if needed. |
| Time storage | ISO 8601 with offset (TEXT) | Readable in DB, round-trips losslessly, matches existing project conventions. Default tz lives in `config.calendar.default_timezone`. |
| Shift edits | Full EventKit update via stored `calendar_event_id` | Cleaner than delete + recreate; preserves any reminders set in Apple Calendar itself. |
| Audit history | Memory-style supersession (`active`, `superseded_by`, `supersede_reason`) | Mirrors `memory/store_facts.go:434`. Proven pattern in this codebase. |
| Cancellation | Status flag + `[CANCELLED]` calendar prefix | Honors the no-delete principle; keeps history visible on the calendar itself. |
| No-show | `shift_log_time` with zero hours, `status='no_show'` | Reuses the actuals path; doesn't conflate "I didn't show up" with "shift was cancelled." |
| Per-category guide injection | Skipped | `LookupToolDefs` already passes full schemas (descriptions + per-param descriptions) to the LLM when a category loads. Tool descriptions alone are sufficient. |
| Time tool | None — already a prompt layer | `layers/agent_time.go:23-24` injects current time + tz every turn. No tool needed. |
| Tool naming | Generic (`shift_schedule`, not `schedule_panera`) | Per "code translates data, never defines it." Job names live in config. |
| Drop `duration_between` | Yes | `shift_list` returns computed hours per row + totals. Tickets show totals on paper. No need. |
| Reminder design | Specialized `shift_reminder` Handler + `scheduler_jobs` one-off table | Extends the scheduler rather than working around it. Generic infra usable by future features. |
| Reminder timing | Per-job in config + agent override per-shift | Panera 5 min away vs Cava 35 min away; one knob doesn't fit. |
| Reminder text | Templated in `task.yaml` | Same principle: prose lives in YAML, not Go. |
| Reminder ↔ shift link | New `reminder_job_id` column on `work_shifts` | One DB lookup beats a JSON-substring scan over `scheduler_jobs.payload`. |

---

## Part 10 — Known limitations (v1)

Things this plan deliberately does not solve. Documented so they don't surprise anyone.

- **No two-way sync from Apple Calendar.** If Autumn edits a shift event directly in Apple Calendar (drags it to a new time, deletes it), her doesn't know. The DB row stays with stale times. Mitigation: agent always reads `shift_list` (DB) for shift state, not the calendar, so the source of truth stays consistent within her's domain. Future work: a periodic poll that diffs DB vs calendar and flags drift.
- **No travel-time / "leave now" reminders.** Apple-Maps-style "time to leave based on traffic" requires routing. Per-job `reminder_minutes` is a static heuristic for now.
- **macOS-only.** EventKit is Apple-only. Linux/Windows hosts can't use the calendar tools — they'll fail with the bridge-not-found error. Future work: a Google Calendar HTTP bridge for cross-platform.
- **Single calendar.** All shifts go to the calendar named in `config.calendar.calendar_name`. Multi-calendar support (e.g., a separate calendar per job, color-coded) is straightforward to add later but not in v1.
- **Reminder once.** One reminder per shift, fired N minutes before. No "snooze" or "second reminder if you don't respond" behavior. Tractable as a future scheduler feature.
- **No conflict resolution UI.** Overlap warnings are surfaced in `shift_schedule` returns but no inline confirmation flow ("two shifts overlap, want to keep both?"). Agent decides whether to relay to user.
- **Bridge invocation cost.** Each calendar call spawns a Swift process (~50-100ms cold start on M-series). For batch operations the bridge handles N events in one invocation, so this only stings for one-off calls. Future work: a long-lived bridge subprocess if it becomes a bottleneck.

---

## Part 11 — Implementation phases

Suggested order. Each phase is independently testable and shippable.

### Phase 1 — Swift bridge

- `calendar/bridge/Package.swift`, `Sources/her-calendar/{main,Commands,JSON}.swift`
- `calendar/bridge/README.md` with build + permission steps
- Manual smoke test: `echo '{...}' | her-calendar` from Terminal for each command (list, create, update, delete)
- No Go changes yet

**Done when:** Swift binary builds, runs against a test calendar in Apple Calendar, all 4 commands round-trip successfully.

### Phase 2 — Schema + config

- `config/config.go`: `CalendarConfig` + `JobConfig` structs, helpers
- `config.yaml.example`: documented `calendar:` block
- `memory/store.go`: migrations for `work_shifts`, `scheduler_jobs`, `reminder_job_id` column
- `memory/store_shifts.go`: CRUD + supersession + history helpers (mirror `store_facts.go` style)
- Tests: `memory/store_shifts_test.go` covering insert, list with totals, supersede chain walk, status transitions

**Done when:** unit tests pass, config loads cleanly, migrations apply on a fresh DB without error.

### Phase 3 — Calendar bridge wrapper (Go)

- `calendar/bridge.go`: `Bridge` type with `Call()`, retry policy, fail-soft when binary missing
- `calendar/bridge_test.go`: tests with a fake binary (shell script that echoes canned JSON)
- Hook bridge initialization into `agent/agent.go` startup; log warning + continue if missing

**Done when:** Go can drive the bridge end-to-end, retries on flaky exit codes, returns useful errors.

### Phase 4 — Pure calendar tools

- `tools/calendar_list/`, `tools/calendar_create/`, `tools/calendar_update/`, `tools/calendar_delete/`
- Each: `tool.yaml` + `handler.go` + minimal handler test
- Add `calendar` to `tools/categories.yaml`
- Smoke test: agent invokes `calendar_create` for a fake event, sees it appear in Apple Calendar

**Done when:** all 4 calendar tools work end-to-end with a real calendar.

### Phase 5 — Scheduler one-offs

- `memory/store.go`: `scheduler_jobs` table CRUD on `*memory.Store` (`InsertJob`, `DueJobs`, `MarkJobDone`, `MarkJobFailed`, `CancelJob`)
- `scheduler/jobs.go`: `EnqueueJob`, `CancelJob`, `ListPendingJobs` on `*Scheduler`
- `scheduler/scheduler.go::tick`: extend to also process due jobs from `scheduler_jobs`
- Tests: `scheduler/jobs_test.go` covering enqueue, fire, cancel, retry, unknown-kind handling

**Done when:** unit tests pass, a registered fake handler fires correctly via the new path without disturbing the recurring path.

### Phase 6 — Shift tools

- `tools/shift_schedule/`, `tools/shift_update/`, `tools/shift_cancel/`, `tools/shift_log_time/`, `tools/shift_list/`
- Each combo tool wraps bridge call + DB write + (where applicable) scheduler enqueue/cancel in a single transaction-shaped flow
- Overlap detection in `shift_schedule` and `shift_update`
- Tests: per-tool handler tests with mocked bridge + real in-memory SQLite

**Done when:** flows from Part X (sim) work end-to-end manually.

### Phase 7 — Reminder handler

- `scheduler/shift_reminder/handler.go` + `task.yaml`
- Register in scheduler init
- Tests: handler reads payload, fetches shift, skips cancelled/superseded, sends Telegram
- End-to-end manual test: schedule a shift 2 minutes out, confirm reminder fires

**Done when:** real shift triggers a real Telegram reminder at the right time, after edit/cancel re-routes correctly.

### Phase 8 — Stale-code cleanup + agent prompt polish

- Fix the four stale references from Part 8
- Add the example flow to `main_agent_prompt.md`
- Run `sims/tool-a-thon.yaml` (after fixing) to verify no regressions

**Done when:** sims green, lint clean, no stale references in code or docs.

---

## Part 12 — Testing strategy

Mirrors the existing project conventions (`*_test.go` files, table-driven tests where it fits, real SQLite for store tests).

**Unit:**
- `memory/store_shifts_test.go` — schema, CRUD, supersede chains, totals math
- `scheduler/jobs_test.go` — one-off enqueue/fire/cancel/retry, isolation from recurring path
- `scheduler/shift_reminder/handler_test.go` — payload parsing, status check (skip cancelled), template render
- `calendar/bridge_test.go` — fake-binary harness, retry behavior, error code routing
- `tools/shift_*/handler_test.go` — each combo tool with mocked bridge + in-memory store; cover happy path, partial failure, overlap warning

**Integration / sim:**
- New `sims/calendar-a-thon.yaml` walking the four scenarios from the interview:
  1. Schedule drop (5-shift batch, totals reported)
  2. Clock out with implicit on-time start
  3. "Wed actually moved to Thu" → supersede + new reminder
  4. "How many hours last week" → totals from `shift_list`
- **Style reference:** model on `sims/inbox-cleanup.yaml` (added in `9a8ebee`), not the older `sims/fact-a-thon.yaml`. The inbox sim demonstrates the current best-practice shape — rich `description`, `tags`, `seed_*` block, and multi-step flow assertions. Our calendar sim mirrors that structure closely (schedule drop → parse → batch tool call → reply, same general pipeline shape as inbox's recall → send_task → memory agent → notify flow).
- Use the existing sim harness (extended per below); assert tool-call sequences and final reply shape.

### Sim harness extensions (required)

The sim runner already supports `seed_memories` (`cmd/sim.go:150,765`) which embeds and inserts memories before the message loop starts. We need parallel seeding for calendar state, plus a fake bridge so sims don't require EventKit permission or the Swift binary.

**New YAML fields on the sim spec:**

```yaml
# sims/calendar-a-thon.yaml (sketch)
seed_shifts:
  # Inserted directly into work_shifts with pre-assigned calendar_event_ids
  # (no bridge call). Each seed gets a reminder_job_id auto-enqueued unless
  # reminder: false is set.
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
    reminder: false

seed_calendar_events:
  # For sims exercising calendar_list / calendar_update on non-shift events.
  # Lives only in the fake bridge's in-memory store — not in the DB.
  - id: "FAKE-DENTIST-1"
    title: "Dentist"
    start: "2026-04-22T14:00:00-04:00"
    end:   "2026-04-22T15:00:00-04:00"
```

**Go-side changes to support this:**

1. **`cmd/sim.go`**: add `SeedShifts []SeedShift` and `SeedCalendarEvents []SeedEvent` fields on the sim spec struct. Seed loop runs after memory seeding, before the message loop.
2. **`calendar/bridge.go`**: introduce a `Bridge` interface; prod impl shells out to the Swift CLI, test impl is an in-memory fake. Sim runner picks the fake when `HER_SIM_MODE=1` (or a config flag).
3. **Fake bridge (`calendar/bridge_fake.go`)**: a minimal Go type that honors the same `Call()` signature and keeps events in a `map[string]Event`. Seeded events populate it pre-run. Supports all 4 commands.

**Why a fake and not a mock per test:** sims drive the real agent end-to-end through real tools, and each shift_* tool internally calls the bridge. A single fake (shared process-wide for the sim run) means tools work unchanged and we see genuine tool-call sequences in the report.

### `sim.db` schema additions (for results tracking)

The sim results database (`sim.db`, schema at `cmd/sim.go:209`) currently snapshots `sim_memories`, `sim_mood_entries`, `sim_metrics`, `sim_agent_turns` per run. To make calendar sims reviewable in the same way (across runs, via the report + the `./sims query` subcommands if they exist), add parallel tables:

```sql
CREATE TABLE IF NOT EXISTS sim_shifts (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id              INTEGER NOT NULL REFERENCES sim_runs(id),
    captured_at         DATETIME DEFAULT CURRENT_TIMESTAMP,
    shift_id            INTEGER,                -- original id in her.db for this run
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
    job_id      INTEGER,                        -- original id in her.db for this run
    kind        TEXT,
    fire_at     TEXT,
    payload     TEXT,
    status      TEXT,
    attempts    INTEGER,
    last_error  TEXT,
    fired_at    TEXT
);
```

**Snapshot point:** same place the runner currently snapshots `sim_memories` (end of run). Fresh scan of `work_shifts` and `scheduler_jobs` from her.db → insert into sim.db keyed by `run_id`. Preserves audit history (superseded rows included) so you can inspect how a shift evolved across turns in post-run analysis.

**Views (optional, mirror existing pattern):** `latest_sim_shifts` that filters to the most recent run, mirroring how `sim_memories` probably has one.

Phase 6 of the implementation plan extends Phase 2's unit tests with sim-integration coverage once these harness changes land.

**Manual smoke checklist:**
- Build + grant permission flow on a fresh machine, following only the README
- Schedule a shift starting in 2 minutes, confirm Telegram reminder fires
- Edit that shift to start 5 minutes later, confirm old reminder cancelled and new one fires
- Cancel the shift, confirm calendar event renamed and no reminder
- Pre-existing calendar events untouched throughout

---

## Part 13 — Open questions / future

Not blocking implementation. Captured for the next planning round.

- **Two-way sync.** Periodic diff job that detects calendar drift and either (a) tells Autumn or (b) updates the DB. Needs UX thought — silent updates feel surprising.
- **Multi-calendar.** A `calendar` field per-job in config so Panera shifts go to a "Panera" calendar (color-coded), Cava shifts to "Cava." Trivial schema change, more interesting UX (filtering in `calendar_list`).
- **Travel-time reminders.** Wire up Apple Maps via MapKit in the Swift bridge to compute "leave now" time. Replaces static `reminder_minutes` with a dynamic per-day estimate.
- **Snooze / second reminder.** Telegram inline button on the reminder message that re-enqueues a 5-min follow-up.
- **Shift summary digest.** Weekly Sunday-evening recap: hours worked, overtime, no-shows. Recurring scheduler task; cheap to add once Phase 5 is in.
- **Generic event reminders.** `calendar_create` doesn't enqueue reminders today — only `shift_schedule` does. Easy to add a `reminder_minutes` param to `calendar_create` once one-offs are proven via shifts.
- **Promote scheduler from "single SQLite" to "any-store" interface.** Currently `Deps.Store any` with cast-on-use (`scheduler/types.go:55`). Fine for now; a real interface would help if we ever split storage.

---

*End of plan.*

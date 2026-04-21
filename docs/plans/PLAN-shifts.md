# Plan: Shift Tracking Tools

Five tools for tracking work shifts (`shift_schedule`, `shift_update`, `shift_cancel`, `shift_log_time`, `shift_list`), backed by a `work_shifts` SQLite table with audit history. Shifts are linked to calendar events via the bridge from PLAN-calendar-bridge. Job definitions live in config -- tools are generic, never job-named.

**Status:** ready for implementation
**Depends on:** PLAN-calendar-bridge (bridge wrapper, config struct, calendar category)
**Tracking:**

---

## Goals

- Track work shifts, not just calendar events. One row per shift with both scheduled and actual times, linked to the calendar event by id.
- Audit-friendly. Edits don't overwrite -- they supersede, mirroring the memory pattern (`memory/store_facts.go`). Cancellations don't delete. Full history queryable.
- Generic, not job-named. Tools are `shift_schedule` / `shift_list` etc., never `add_panera_shift`. Jobs (Panera, Cava, anything else) are config rows.
- Hours computed in Go, not by the LLM.

---

## Part 1 -- Config additions

Extends the `CalendarConfig` struct from PLAN-calendar-bridge with shift-specific fields.

```yaml
calendar:
  # ... bridge_path, calendar_name, default_timezone from PLAN-calendar-bridge ...

  # Default minutes-before-start for reminders when no per-job override
  # exists and the agent doesn't specify one.
  default_reminder_minutes: 30

  # Generic job list. Add or remove freely -- code never references these
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

**Config struct additions:**

```go
// Added to CalendarConfig from PLAN-calendar-bridge
type CalendarConfig struct {
    // ... existing fields from PLAN-calendar-bridge ...
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

---

## Part 2 -- SQLite schema: `work_shifts`

Follows existing project conventions (`memory/store.go` style -- `IF NOT EXISTS` migrations, ISO 8601 timestamps as TEXT).

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

Note: the `reminder_job_id` column is NOT included here -- it's added in PLAN-scheduler-oneoffs when the `scheduler_jobs` table exists to reference.

**Two orthogonal axes -- by design:**

| Axis | Field(s) | Meaning |
|---|---|---|
| Lifecycle | `status` | What happened to the shift-as-event: scheduled, worked, no-show, cancelled. |
| Version history | `active` + `superseded_by` + `supersede_reason` | Tracks edits to the shift's *definition* (time moved, hours changed). |

**Examples:**
- Cancelled shift: one row, `status='cancelled'`, `active=1`. Calendar event renamed `[CANCELLED] ...`.
- No-show: one row, `status='no_show'`, `actual_start == actual_end`, hours=0.
- Time moved Wed to Thu: two rows. Old row `active=0`, `superseded_by=<new_id>`, `supersede_reason='moved Wed to Thu'`. New row inherits `calendar_event_id` (same event, updated in place).

**Store helpers (`memory/store_shifts.go`, mirroring `memory/store_facts.go`):**

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

---

## Part 3 -- Shift tools

Five tools registered in a `shifts` category (separate from `calendar`). All follow the existing pattern: `tool.yaml` + `handler.go` in `tools/<name>/`.

Add to `tools/categories.yaml`:

```yaml
shifts:
  hint: "User mentions a work shift, schedule, hours worked, clocking in/out, or a specific job like Panera/Cava"
```

### `shift_schedule`

Create one or more shifts for a job. Calendar events created first (via bridge, with retry); on success, DB rows inserted; on partial failure, persists what succeeded and reports the rest.

```yaml
name: shift_schedule
description: >-
  Schedule one or more work shifts for a known job. Creates calendar
  events AND writes shift rows to the local DB so they can be tracked,
  edited, and reminded about. Use when the user pastes a work schedule
  or asks to add shifts. Always include the timezone offset in start/end
  (use get_time if unsure). Returns shift_ids and event_ids per shift,
  plus any failures with reasons.
hot: false
category: shifts
parameters:
  type: object
  properties:
    job:
      type: string
      description: "Job name from config (e.g. 'Panera'). Aliases match case-insensitively."
    role:
      type: string
      description: "Optional role override. Defaults to job's default_role from config."
    shifts:
      type: array
      description: "One or more shifts to schedule."
      items:
        type: object
        properties:
          start: { type: string, description: "ISO 8601 with offset" }
          end: { type: string, description: "ISO 8601 with offset" }
          notes: { type: string, description: "Optional per-shift notes" }
          reminder_minutes: { type: integer, description: "Optional override of per-job reminder timing" }
        required: [start, end]
  required: [job, shifts]
```

**Return shape:**
```json
{
  "scheduled": [
    { "shift_id": 17, "event_id": "ABC", "start": "...", "end": "...",
      "scheduled_hours": 8.0,
      "warnings": ["overlaps with shift_id 14 (Cava 4-9p)"] }
  ],
  "failed": [
    { "index": 2, "start": "...", "error": "calendar bridge timeout after 3 attempts" }
  ],
  "totals": { "shifts_scheduled": 4, "shifts_failed": 1, "scheduled_hours": 32.0 }
}
```

**Behavior:**
- Partial success: successful shifts persist; failed ones returned in `failed[]`. Tool doesn't error unless ALL failed.
- Overlap detection: warn but allow. For each new shift, query active `work_shifts` where ranges intersect. Append warning. Don't block.
- Note: reminder enqueue is NOT part of this plan. When PLAN-scheduler-oneoffs lands, `shift_schedule` will be updated to enqueue reminders atomically with the shift insert.

### `shift_update`

Edit an existing shift's scheduled time. Creates a new row, supersedes the old one, updates the EventKit event in place (same `calendar_event_id`).

```yaml
name: shift_update
description: >-
  Move or resize an existing scheduled shift. Use when the user says
  "actually that's Thursday not Wednesday" or "they pushed me to 6
  instead of 5". Old row preserved as audit history (active=0,
  superseded_by=new_id). Calendar event updated in place.
hot: false
category: shifts
parameters:
  type: object
  properties:
    shift_id: { type: integer, description: "ID of the active shift row (from shift_list)" }
    start: { type: string, description: "New start, ISO 8601. Omit to leave unchanged." }
    end: { type: string, description: "New end, ISO 8601. Omit to leave unchanged." }
    role: { type: string, description: "Updated role. Omit to leave unchanged." }
    reason: { type: string, description: "Why the change happened (stored in supersede_reason)" }
  required: [shift_id, reason]
```

### `shift_cancel`

Mark a shift cancelled. Row preserved. Calendar event renamed `[CANCELLED] <original title>`.

```yaml
name: shift_cancel
description: >-
  Mark a scheduled shift as cancelled (e.g., boss took the user off the
  schedule). Does NOT delete the row -- cancellations are part of work
  history. Calendar event renamed with [CANCELLED] prefix. Use
  shift_log_time with zero hours if the user simply didn't show up.
hot: false
category: shifts
parameters:
  type: object
  properties:
    shift_id: { type: integer }
    reason: { type: string, description: "Optional, stored in notes" }
  required: [shift_id]
```

### `shift_log_time`

Record actual clock-in/out for a shift.

```yaml
name: shift_log_time
description: >-
  Record actual hours worked for a completed shift. If actual_start is
  omitted, defaults to scheduled_start (common case -- clocked in on
  time). Pass actual_start == actual_end to log a no-show (zero hours).
hot: false
category: shifts
parameters:
  type: object
  properties:
    shift_id: { type: integer }
    actual_start: { type: string, description: "ISO 8601. Defaults to scheduled_start." }
    actual_end: { type: string, description: "ISO 8601. Required." }
    notes: { type: string, description: "Optional -- 'stayed late to close', etc." }
  required: [shift_id, actual_end]
```

### `shift_list`

Query shifts. Primary way to get `shift_id`s for editing or to answer "how many hours did I work."

```yaml
name: shift_list
description: >-
  List shifts in a date range with computed hours and totals. Use to
  answer "how much did I work" or look up shift_ids before calling
  shift_update / shift_cancel / shift_log_time.
hot: false
category: shifts
parameters:
  type: object
  properties:
    start: { type: string, description: "ISO 8601. Defaults to 30 days ago." }
    end: { type: string, description: "ISO 8601. Defaults to 7 days from now." }
    job: { type: string, description: "Filter to a specific job (case-insensitive)." }
    status: { type: string, description: "Filter: scheduled | worked | no_show | cancelled" }
    include_history: { type: boolean, description: "Include superseded rows. Default false." }
  required: []
```

---

## Decisions

| Decision | Choice | Why |
|---|---|---|
| Overlap policy | Warn but allow | Real life has overlapping commitments. Surface the warning, let the user decide. |
| Shift edits | Full EventKit update via stored `calendar_event_id` | Cleaner than delete + recreate; preserves any reminders set in Apple Calendar itself. |
| Audit history | Memory-style supersession (`active`, `superseded_by`, `supersede_reason`) | Mirrors `memory/store_facts.go`. Proven pattern in this codebase. |
| Cancellation | Status flag + `[CANCELLED]` calendar prefix | Honors the no-delete principle; keeps history visible on the calendar itself. |
| No-show | `shift_log_time` with zero hours, `status='no_show'` | Reuses the actuals path; doesn't conflate "I didn't show up" with "shift was cancelled." |
| Tool naming | Generic (`shift_schedule`, not `schedule_panera`) | Per "code translates data, never defines it." Job names live in config. |
| Drop `duration_between` | Yes | `shift_list` returns computed hours per row + totals. No need for a separate duration tool. |
| Separate category | `shifts` not merged with `calendar` | Different use cases. Calendar tools are for ad-hoc events; shift tools are for work tracking. Separate deferred loading keeps the tool set lean per-turn. |

## Known Limitations (v1)

- **No two-way sync from Apple Calendar.** If Autumn edits a shift event directly in Apple Calendar, her doesn't know. The DB row stays with stale times. Agent always reads `shift_list` (DB) for shift state, not the calendar.
- **No travel-time / "leave now" reminders.** Per-job `reminder_minutes` is a static heuristic. Routing-based estimates are future work.
- **No conflict resolution UI.** Overlap warnings are surfaced in returns but no inline confirmation flow. Agent decides whether to relay to user.

---

## Phases

### Phase 1 -- Config additions

- Add `DefaultReminderMinutes`, `Jobs`, `JobConfig` to `CalendarConfig`
- Add `MatchJob()` and `ReminderMinutesFor()` helpers
- Update `config.yaml.example` with documented `jobs:` block
- Tests for `MatchJob` (case-insensitive, alias matching) and `ReminderMinutesFor` (per-job override, default fallback)

**Done when:** config loads jobs cleanly, helpers pass tests.

### Phase 2 -- `work_shifts` schema + store helpers

- `memory/store.go`: migration for `work_shifts` table
- `memory/store_shifts.go`: full CRUD + supersession + history helpers
- `memory/store_shifts_test.go`: insert, list with totals, supersede chain walk, status transitions

**Done when:** unit tests pass, migrations apply on fresh DB.

### Phase 3 -- 5 shift tools + `shifts` category

- `tools/shift_schedule/`, `tools/shift_update/`, `tools/shift_cancel/`, `tools/shift_log_time/`, `tools/shift_list/`
- Each: `tool.yaml` + `handler.go` + handler test with mocked bridge + real in-memory SQLite
- Add `shifts` to `tools/categories.yaml`
- Overlap detection in `shift_schedule` and `shift_update`

**Done when:** all 5 tools pass handler tests. Agent can load `shifts` category and execute shift workflows end-to-end.

---
title: "Shift Tracking"
status: ready
created: 2026-04-21
updated: 2026-04-21
category: features
priority: medium
related:
  - PLAN-calendar-bridge.md
---

# Plan: Shift Tracking (Calendar Extension)

Shift tracking built on top of the existing calendar tools and `calendar_events` SQLite table. No separate tools or tables -- shifts are calendar events with optional shift metadata. One new tool (`shift_hours`) for computing totals.

**Depends on:** Calendar DB mirror (commit 05a4346), config `CalendarConfig`
**Supersedes:** Original 5-tool shift plan (dropped in favor of extending calendar tools)

---

## Goals

- Track work shifts using the same calendar infrastructure. One event = one potential shift. No parallel data system.
- Shift metadata lives in the event's `notes` field as human-readable key: value pairs -- visible in Apple Calendar.
- A nullable `job` column on `calendar_events` enables fast, indexed queries for shift-specific filtering without bloating regular events.
- Hours computed in Go, never by the LLM. One dedicated `shift_hours` tool for totals.
- Job definitions live in config -- tools are generic, never job-named.

---

## Part 1 -- Config Additions

Extends `CalendarConfig` with a jobs list. Jobs define defaults the handler auto-fills when the agent passes a `job` param.

```yaml
calendar:
  # ... existing: bridge_path, calendars, default_calendar, default_timezone ...

  jobs:
    - name: "Panera"
      address: "3625 Spring Hill Pkwy SE, Smyrna, GA 30080"
      default_role: ""              # blank = read from schedule/photo
      aliases: ["panera bread"]

    - name: "Cava"
      address: "855 Peachtree St NE, Atlanta, GA 30308"
      default_role: "Grill Cook"
      aliases: []
```

**Config struct additions:**

```go
// Added to CalendarConfig
type CalendarConfig struct {
    // ... existing fields ...
    Jobs []JobConfig `yaml:"jobs"`
}

type JobConfig struct {
    Name        string   `yaml:"name"`
    Address     string   `yaml:"address"`
    DefaultRole string   `yaml:"default_role"`
    Aliases     []string `yaml:"aliases"`
}

// MatchJob returns the job whose name or alias matches (case-insensitive),
// or nil if no match. Used by calendar_create to validate and auto-fill.
func (c *CalendarConfig) MatchJob(name string) *JobConfig { ... }
```

---

## Part 2 -- Schema: Add `job` Column

One nullable column on the existing `calendar_events` table.

```sql
ALTER TABLE calendar_events ADD COLUMN job TEXT;
CREATE INDEX IF NOT EXISTS idx_calendar_events_job ON calendar_events(job);
```

Added to `memory/store.go` migrations (same pattern as existing `ALTER TABLE ... ADD COLUMN` migrations -- silently ignored if column already exists).

**CalendarEvent struct update:**

```go
type CalendarEvent struct {
    // ... existing fields ...
    Job string // nullable -- empty string for regular events
}
```

**Store helper updates:**
- `InsertCalendarEvent`: add `job` parameter (empty string → NULL)
- `UpdateCalendarEvent`: support `"job"` key in the updates map
- `ListCalendarEvents`: add optional `job` filter parameter
- `GetCalendarEventByEventID`: include `job` in SELECT

---

## Part 3 -- Notes Format

Shift metadata stored as key: value pairs in the event's `notes` field. Human-readable in Apple Calendar, parseable in Go.

**Example notes for a shift event:**

```
position: Grill Cook
trainer: Mike
time chit: 6h 15m
stayed late to close, covered for Sarah
```

**Keys (all optional):**

| Key | Description | Example |
|---|---|---|
| `position` | Role/position worked | `Grill Cook` |
| `trainer` | Training supervisor (temporary) | `Mike` |
| `time chit` | Actual hours worked (from receipt photo) | `6h 15m`, `8h 0m` |

Any text not in `key: value` format is treated as freeform notes -- sits at the bottom, no prefix needed.

**Parsing rules:**
- Lines matching `^(\w[\w ]*\w): (.+)$` are key: value pairs
- Everything else is freeform notes
- Parsing is only needed for `shift_hours` (extracting `time chit`) and `calendar_list` (enriching shift responses)

**`time chit` format:** `Xh Ym` (e.g., `6h 15m`, `8h 0m`). Standardized for reliable Go parsing. The VLLM extracts this from time chit receipt photos; the agent prompt instructs it to output this format.

---

## Part 4 -- Extend Existing Calendar Tools

### `calendar_create` -- Add Optional Shift Params

New optional parameters in `tool.yaml`:

```yaml
# Added to each event in the events array:
job:
  type: string
  description: "Job name from config (e.g. 'Panera'). Triggers shift behavior. Optional."
position:
  type: string
  description: "Role/position. Defaults to job's default_role if omitted."
trainer:
  type: string
  description: "Training supervisor name. Optional."
```

**Handler changes:**
- When `job` is present, run `MatchJob()` to validate + auto-fill:
  - `location` ← job's `address` (if not explicitly provided)
  - `position` ← job's `default_role` (if not explicitly provided)
- Write `job` to the DB column
- Serialize `position`, `trainer` as key: value lines in `notes`
- Overlap detection: warn if new shift overlaps an existing shift for any job (query `calendar_events WHERE job IS NOT NULL AND active = 1` with overlapping time range). Warn but don't block.

**Updated description:** Remove the "NOT work shifts" language -- calendar_create now handles both.

### `calendar_update` -- Add Shift Params

New optional parameters:

```yaml
job:
  type: string
  description: "Set or change the job for this event. Optional."
position:
  type: string
  description: "Update position. Optional."
trainer:
  type: string
  description: "Update trainer. Optional."
time_chit:
  type: string
  description: "Actual hours worked, e.g. '6h 15m'. From time chit receipt. Optional."
shift_notes:
  type: string
  description: "Freeform shift notes (e.g. 'stayed late to close'). Optional."
```

**Handler changes:**
- When shift params are present, merge them into the event's `notes` field:
  1. Parse existing notes into key: value pairs + freeform text
  2. Update/add the provided keys
  3. Reserialize back to the `key: value` format + freeform text at the bottom
- Update `job` column in DB if provided

### `calendar_list` -- Add Shift Filtering

New optional parameters:

```yaml
job:
  type: string
  description: "Filter to events for this job only. Optional."
shifts_only:
  type: boolean
  description: "Only return events that have a job set. Default false."
```

**Handler changes:**
- Pass `job` filter to `ListCalendarEvents` store method
- When `shifts_only` is true, add `WHERE job IS NOT NULL`
- For shift events in the response, parse `notes` and include structured shift fields alongside the raw notes
- Include `scheduled_hours` (computed from start/end) per shift event

### `calendar_delete` -- No Changes

Deleting a shift event is the same as deleting any event. Soft-delete via `active = 0` already preserves history.

**Updated description:** Remove the "prefer shift_cancel" language -- it no longer exists.

---

## Part 5 -- New Tool: `shift_hours`

One new a la carte tool for computing hour totals. This is a DB aggregation, not a calendar operation.

```yaml
name: shift_hours
agent: main
description: >-
  Compute total hours worked over a time period. Parses time chit values
  from shift events and returns per-job and overall totals. Use when the
  user asks "how many hours did I work this week/month."
hot: false
category: calendar
parameters:
  type: object
  properties:
    period:
      type: string
      description: "'week', 'month', 'year', or 'custom'. Defaults to 'month'."
    start:
      type: string
      description: "ISO 8601. Required if period is 'custom'."
    end:
      type: string
      description: "ISO 8601. Required if period is 'custom'."
    job:
      type: string
      description: "Filter to one job. Omit for all jobs."
  required: []
trace:
  emoji: "🕐"
  format: "{{.period}} hours{{if .job}} for {{.job}}{{end}}"
```

**Return shape:**

```json
{
  "period": "Apr 1 – Apr 21, 2026",
  "by_job": [
    { "job": "Panera", "shifts": 8, "hours": "47h 30m" },
    { "job": "Cava", "shifts": 5, "hours": "31h 45m" }
  ],
  "total": { "shifts": 13, "hours": "79h 15m" }
}
```

**Implementation:**
1. Resolve `period` to start/end timestamps (week = current Mon–Sun, month = 1st–last, year = Jan 1–Dec 31). Use `config.Calendar.DefaultTimezone`.
2. Query `calendar_events WHERE job IS NOT NULL AND active = 1` in the date range, optionally filtered by `job`.
3. For each event, parse `time chit` from notes. If no time chit, fall back to `scheduled_hours` (end - start).
4. Sum per job, sum overall. Format as `Xh Ym`.
5. All computation in Go -- the agent just relays the formatted result.

**Store helper:**

```go
// ListShiftEvents returns active calendar events that have a job set,
// filtered by date range and optionally by job name. Used by shift_hours.
func (s *Store) ListShiftEvents(start, end, job string) ([]CalendarEvent, error)
```

---

## Part 6 -- Update `categories.yaml`

No new category needed. Shifts live in the `calendar` category. The hint already covers it:

```yaml
calendar:
  hint: "User mentions a work shift, schedule, calendar event, or asks about hours worked"
```

---

## Decisions

| Decision | Choice | Why |
|---|---|---|
| No separate shift tools | Extend calendar tools | Shifts ARE calendar events. One interface, not two. |
| `job` as a DB column | Nullable TEXT on `calendar_events` | Fast indexed queries. NULL for regular events -- no bloat. |
| Shift metadata in `notes` | Key: value pairs | Human-readable in Apple Calendar. Parseable in Go. |
| `time chit` format | `Xh Ym` | Dead simple to parse, VLLM can output it, human-readable. |
| One new tool | `shift_hours` only | Hour totals need DB aggregation. Everything else rides on existing tools. |
| No audit/supersession | Dropped | Soft-delete (`active = 0`) already preserves deleted events. Edit history not needed for v1. |
| Overlap policy | Warn but allow | Real life has overlapping commitments. Surface the warning, let the user decide. |

## Comparison: Original Plan vs. Revised

| Aspect | Original (5-tool plan) | Revised (calendar extension) |
|---|---|---|
| New tools | 5 (`shift_schedule`, `shift_update`, `shift_cancel`, `shift_log_time`, `shift_list`) | 1 (`shift_hours`) |
| New tables | `work_shifts` (14 columns, 3 indexes) | 1 column + 1 index on existing table |
| Store helpers | 7 new functions + `ShiftFilter` + `ShiftTotals` types | 1 new function (`ListShiftEvents`) + updates to existing helpers |
| Complexity | Parallel data system with supersession chains | Flat extension of existing infrastructure |

---

## Phases

### Phase 1 -- Config + Schema

- Add `Jobs []JobConfig` to `CalendarConfig`, add `JobConfig` struct
- Add `MatchJob()` helper with case-insensitive + alias matching
- Add `job TEXT` column migration to `memory/store.go`
- Update `CalendarEvent` struct, store helpers
- Update `config.yaml.example` with `jobs:` block
- Tests: `MatchJob` matching, store helpers with `job` column

**Done when:** config loads jobs, `MatchJob` passes tests, migration applies cleanly.

### Phase 2 -- Extend Calendar Tools

- Update `calendar_create`: `job`, `position`, `trainer` params, auto-fill from config, notes serialization, overlap warnings
- Update `calendar_update`: `time_chit`, `position`, `trainer`, `shift_notes` params, notes merge logic
- Update `calendar_list`: `job` filter, `shifts_only` flag, structured shift fields in response
- Update `calendar_delete`: description cleanup only
- Notes parser: extract key: value pairs from notes text, reserialize after edits
- Clean up tool.yaml descriptions (remove old shift_schedule/shift_cancel references)

**Done when:** handler tests pass. Agent can create a shift, update it with time chit, list shifts by job, delete a shift.

### Phase 3 -- `shift_hours` Tool

- `tools/shift_hours/tool.yaml` + `handler.go`
- `ListShiftEvents` store helper
- Period resolution (week/month/year/custom)
- Time chit parser (`Xh Ym` → minutes)
- Summation + formatting logic
- Handler tests with various period/job combinations

**Done when:** `shift_hours` returns correct totals across jobs and periods. Agent can answer "how many hours did I work this month."

## Workflow Example

Autumn sends a photo of her Panera schedule:

1. Agent calls `view_image` → VLLM extracts: "Panera shifts: Mon 9a-3p, Wed 11a-5p, Fri 7a-2p"
2. Agent calls `calendar_create` with:
   ```json
   {
     "events": [
       {"title": "Panera", "start": "2026-04-27T09:00:00-04:00", "end": "2026-04-27T15:00:00-04:00", "job": "Panera"},
       {"title": "Panera", "start": "2026-04-29T11:00:00-04:00", "end": "2026-04-29T17:00:00-04:00", "job": "Panera"},
       {"title": "Panera", "start": "2026-05-01T07:00:00-04:00", "end": "2026-05-01T14:00:00-04:00", "job": "Panera"}
     ]
   }
   ```
   Handler auto-fills `location` from config. Events land in DB + Apple Calendar.

3. After Monday's shift, Autumn sends a photo of her time chit receipt.
4. Agent calls `view_image` → VLLM extracts: "clocked in 8:58a, clocked out 3:12p, 6h 14m"
5. Agent calls `calendar_update` with:
   ```json
   {"event_id": "ABC-123", "time_chit": "6h 14m", "shift_notes": "stayed a few extra mins"}
   ```
   Handler merges `time chit: 6h 14m` into the event's notes.

6. End of month, Autumn asks "how many hours did I work?"
7. Agent calls `shift_hours` with `{"period": "month"}` → returns totals per job.

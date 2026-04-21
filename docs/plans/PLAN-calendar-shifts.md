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

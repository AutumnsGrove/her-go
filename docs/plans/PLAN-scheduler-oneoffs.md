---
title: "Scheduler One-Off Jobs"
status: ready
created: 2026-04-20
updated: 2026-04-20
category: features
priority: medium
related:
  - PLAN-shifts.md
---

# Plan: Scheduler One-Off Jobs + Shift Reminders

Extend the existing scheduler to handle one-off jobs (fire-once-at-a-specific-time) alongside recurring tasks. Use this to power per-shift "leave for work" reminders delivered via Telegram. Same `Handler` interface, new sibling table.

**Depends on:** PLAN-shifts (work_shifts schema, shift store helpers, shift tools)
**Tracking:**

---

## Goals

- **Extend the scheduler, don't replace it.** The current scheduler handles one row per recurring kind; we add a sibling table (`scheduler_jobs`) for one-offs so the same Handler interface serves both.
- **Proactive reminders** -- "Panera in 45 min" on Telegram. Per-job timing in config, agent can override per-shift.
- **One-offs available to any future feature** (dentist reminders, persona reflection trigger, weekly digests) without more plumbing.

---

## Part 1 -- `scheduler_jobs` schema

Sibling to existing `scheduler_tasks`. One-offs only -- recurring stays in `scheduler_tasks`.

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

**No-delete rule applies.** Done jobs stay in the table. A maintenance task can prune `status IN ('done','cancelled') AND fired_at < now() - 90 days` later if needed; not in v1.

**Store methods on `memory.Store`:**

```go
func (s *Store) InsertJob(kind string, fireAt time.Time, payload json.RawMessage) (int64, error)
func (s *Store) DueJobs(now time.Time, limit int) ([]Job, error)
func (s *Store) MarkJobDone(id int64) error
func (s *Store) MarkJobFailed(id int64, errMsg string) error
func (s *Store) CancelJob(id int64) error
```

---

## Part 2 -- Tick loop extension

**Same `Handler` interface, two source tables.**

```go
// scheduler/types.go -- Handler stays unchanged
type Handler interface {
    Kind() string
    ConfigPath() string  // empty string is fine for one-off-only handlers
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

**New scheduler API (methods on existing `*Scheduler` receiver):**

```go
// scheduler/jobs.go (new file)

// EnqueueJob schedules a one-off task to fire at a specific time.
// Returns the job id so callers can cancel it later.
func (s *Scheduler) EnqueueJob(kind string, fireAt time.Time, payload any) (int64, error)

// CancelJob marks a pending job as cancelled. Idempotent -- already-fired
// or already-cancelled jobs return nil without error.
func (s *Scheduler) CancelJob(id int64) error

// ListPendingJobs returns all pending jobs of a given kind, ordered by
// fire_at. Used by maintenance/debug paths.
func (s *Scheduler) ListPendingJobs(kind string) ([]Job, error)
```

### Concurrency note

The existing tick loop is single-threaded (one goroutine, one `time.Ticker`). We keep that. With a 30s tick interval and per-call work measured in milliseconds, we have plenty of headroom. Revisit if/when we have a feature firing thousands of one-offs.

---

## Part 3 -- `shift_reminder` handler

A single Handler that fires per-shift one-off jobs, sends a Telegram message, and marks the job done. Templated message text (data in YAML, not Go).

### Layout

```
scheduler/
  shift_reminder/
    handler.go               # implements scheduler.Handler
    task.yaml                # message template + retry policy
```

### `task.yaml`

Source of truth for reminder phrasing. Hot-reloadable on next tick.

```yaml
kind: shift_reminder
cron: ""   # one-off only

message_template: |
  {{.Job}} in {{.MinutesAway}} min -- clock in at {{.StartTime}}{{if .Address}} ({{.Address}}){{end}}.

retry:
  max_attempts: 3
  backoff: exponential
  initial_wait: 30s
```

### Handler

```go
type Payload struct {
    ShiftID int64 `json:"shift_id"`
}

func (h *handler) Execute(ctx context.Context, raw json.RawMessage, deps *scheduler.Deps) error {
    // 1. Unmarshal payload
    // 2. Fetch shift from store
    // 3. Skip if cancelled or already worked (no-op, return nil)
    // 4. Render template with shift data
    // 5. Send via Telegram
}
```

---

## Part 4 -- `reminder_job_id` linkage

How does `shift_update` find the pending reminder to cancel? Store the job ID directly on the shift row.

```sql
ALTER TABLE work_shifts ADD COLUMN reminder_job_id INTEGER REFERENCES scheduler_jobs(id);
```

(Or include in the initial `work_shifts` CREATE TABLE if landing both plans together.)

### Lifecycle wiring

| Event | Reminder action |
|---|---|
| `shift_schedule` creates shift #17 | `EnqueueJob("shift_reminder", scheduled_start - reminder_minutes, {shift_id: 17})` -> store returned `job_id` on shift row |
| `shift_update` supersedes #17 -> #18 | `CancelJob(old_job_id)`, `EnqueueJob` for the new row |
| `shift_cancel` on #17 | `CancelJob(old_job_id)` |
| Reminder fires | Handler re-checks shift status; skips if no longer scheduled |

### Cancellation safety

`shift_reminder` handler re-checks shift state on fire (status, active). Even if `CancelJob` races with the tick, we won't ping for a cancelled shift -- the handler treats it as a no-op. Belt and suspenders.

### Updates to shift tools

`shift_schedule`, `shift_update`, and `shift_cancel` from PLAN-shifts get updated to include reminder lifecycle:
- `shift_schedule`: after inserting shift row, `EnqueueJob` and store `reminder_job_id`
- `shift_update`: `CancelJob(old)`, `EnqueueJob(new)`, store new `reminder_job_id`
- `shift_cancel`: `CancelJob(reminder_job_id)`

---

## Decisions

| Decision | Choice | Why |
|---|---|---|
| Reminder design | Specialized `shift_reminder` Handler + `scheduler_jobs` one-off table | Extends the scheduler rather than working around it. Generic infra usable by future features. |
| Reminder timing | Per-job in config + agent override per-shift | Panera 5 min away vs Cava 35 min away; one knob doesn't fit. |
| Reminder text | Templated in `task.yaml` | Same principle: prose lives in YAML, not Go. |
| Reminder-shift link | `reminder_job_id` column on `work_shifts` | One DB lookup beats a JSON-substring scan over `scheduler_jobs.payload`. |
| Reminder once | One reminder per shift, no snooze | Tractable as a future scheduler feature. |

## Known Limitations (v1)

- **Reminder once.** One reminder per shift, fired N minutes before. No "snooze" or "second reminder if you don't respond" behavior.
- **No generic event reminders.** Only `shift_schedule` enqueues reminders, not `calendar_create`. Easy to add once one-offs are proven via shifts.

---

## Phases

### Phase 1 -- `scheduler_jobs` schema + store CRUD

- `memory/store.go`: migration for `scheduler_jobs` table
- Store methods: `InsertJob`, `DueJobs`, `MarkJobDone`, `MarkJobFailed`, `CancelJob`
- Tests: enqueue, query due, cancel idempotency

**Done when:** unit tests pass, migrations apply on fresh DB.

### Phase 2 -- Tick loop extension

- `scheduler/jobs.go`: `EnqueueJob`, `CancelJob`, `ListPendingJobs` on `*Scheduler`
- `scheduler/scheduler.go::tick`: extend to also process due jobs from `scheduler_jobs`
- Tests: registered fake handler fires correctly via the new path without disturbing the recurring path, retry on failure, unknown-kind handling

**Done when:** unit tests pass, recurring tasks unaffected.

### Phase 3 -- `shift_reminder` handler + `task.yaml`

- `scheduler/shift_reminder/handler.go` + `task.yaml`
- Register in scheduler init
- Tests: handler reads payload, fetches shift, skips cancelled/superseded, sends Telegram

**Done when:** handler passes unit tests with mocked store and Telegram sender.

### Phase 4 -- Wire reminder lifecycle into shift tools

- Add `reminder_job_id` column to `work_shifts` (migration)
- Update `shift_schedule` to enqueue reminder + store job ID
- Update `shift_update` to cancel old + enqueue new reminder
- Update `shift_cancel` to cancel reminder
- End-to-end manual test: schedule shift 2 min out, confirm reminder fires; edit shift, confirm old cancelled + new fires; cancel shift, confirm no reminder

**Done when:** real shift triggers a real Telegram reminder at the right time; edit/cancel re-routes correctly.

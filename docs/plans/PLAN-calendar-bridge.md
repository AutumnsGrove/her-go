---
title: "Calendar Bridge + Calendar Tools"
status: ready
created: 2026-04-20
updated: 2026-04-20
category: features
priority: medium
related:
  - PLAN-shifts.md
  - PLAN-scheduler-oneoffs.md
---

# Plan: Calendar Bridge + Calendar Tools + get_time

Build the Swift EventKit bridge, wrap it in Go with retry, ship the 4 pure calendar tools (`calendar_list`, `calendar_create`, `calendar_update`, `calendar_delete`), add a `get_time` hot tool so the agent can re-check the clock mid-turn, and register the `calendar` category. This is the foundation layer -- shifts, reminders, and sim extensions all depend on this being solid.

**Depends on:** none
**Tracking:**

---

## Goals

- Read and write Apple Calendar through a tiny Swift CLI bridge (`her-calendar`) using EventKit. JSON over stdin/stdout. No HTTP, no daemon.
- Give the agent 4 calendar tools for ad-hoc events (dentist, birthday, meetings -- anything that is not a work shift).
- Give the agent a `get_time` hot tool so it can reliably check the current time mid-turn without scrolling back hundreds of lines to the prompt header.
- Register a `calendar` tool category so the agent loads calendar tools on demand via `use_tools(["calendar"])`.

---

## Part 1 -- Config additions

Only the fields needed for the bridge and calendar tools. The `jobs` list, `default_reminder_minutes`, and shift-related config come later in PLAN-shifts.

```yaml
calendar:
  # Path to the compiled Swift bridge binary. Relative paths resolved
  # from the project root. Bot logs a warning at startup if missing;
  # all calendar tools become no-ops with a clear error message.
  bridge_path: "calendar/bridge/.build/release/her-calendar"

  # EventKit calendar to read/write. Must already exist in Apple Calendar --
  # her does not auto-create. Errors loudly at first calendar_* call if missing.
  calendar_name: "Work"

  # Used when the agent passes a "naive" timestamp (no offset). The agent
  # is instructed to always include the offset, but this is a safety net.
  # Also used by get_time to format the current time.
  default_timezone: "America/New_York"
```

**Config struct (Go side):**

```go
// config/config.go additions

type CalendarConfig struct {
    BridgePath      string `yaml:"bridge_path"`
    CalendarName    string `yaml:"calendar_name"`
    DefaultTimezone string `yaml:"default_timezone"`
    // Jobs, DefaultReminderMinutes added in PLAN-shifts
}
```

`config.yaml.example` gets the same block with comments.

---

## Part 2 -- Swift bridge (`her-calendar`)

A single Swift CLI binary that speaks EventKit. Go never touches EventKit APIs directly -- it shells out to `her-calendar` and pipes JSON in and out.

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

Single-shot: one JSON command on stdin, one JSON response on stdout, process exits.

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

**`list`** -- events in a window:
```json
// args
{ "start": "2026-04-20T00:00:00-04:00", "end": "2026-04-27T00:00:00-04:00" }
// result
{ "events": [
    { "id": "ABC123", "title": "Panera 5a-1p", "start": "...", "end": "...",
      "location": "3625 Spring Hill...", "notes": "..." }
]}
```

**`create`** -- one or many events atomic-per-call:
```json
// args
{ "events": [
    { "title": "Panera 5a-1p", "start": "...", "end": "...",
      "location": "...", "notes": "..." }
]}
// result
{ "events": [ { "id": "ABC123" } ] }
```

On EventKit failure mid-batch, the bridge attempts to delete anything it successfully created in this call, then returns the error. Clean "nothing persisted" signal for retry decisions.

**`update`** -- in-place edit by id. Omitted fields left unchanged:
```json
// args
{ "id": "ABC123", "title": "...", "start": "...", "end": "...",
  "location": "...", "notes": "..." }
// result
{ "id": "ABC123" }
```

**`delete`** -- by id:
```json
// args
{ "id": "ABC123" }
// result
{ "deleted": true }
```

### Install + permissions (one-time)

README in `calendar/bridge/` walks through:

1. `cd calendar/bridge && swift build -c release`
2. Binary appears at `.build/release/her-calendar`.
3. Run it once from Terminal: `echo '{"command":"list","calendar":"Work","args":{"start":"2026-04-20T00:00:00-04:00","end":"2026-04-21T00:00:00-04:00"}}' | .build/release/her-calendar`
4. macOS shows the EventKit permission prompt. Click Allow.
5. Subsequent invocations (including from her, running headless) use the granted permission.

**If permission was denied**, System Settings > Privacy & Security > Calendars > enable `her-calendar`.

### Bridge is optional at startup

Her boots even if the bridge is missing or unbuildable. On startup:

- Tool init checks `cfg.Calendar.BridgePath` exists and is executable.
- If missing, log a single warning: `calendar bridge not found at <path>; calendar tools will return errors if called`.
- Tool handlers return a clear error message to the agent (`"calendar bridge not installed -- see calendar/bridge/README.md"`) so it can tell the user.

No panics, no startup failures. Consistent with how `get_weather` handles a missing API key.

---

## Part 3 -- Go bridge wrapper

Every Swift-bridge invocation from a tool goes through a shared helper:

```go
// calendar/bridge.go
func (b *Bridge) Call(ctx context.Context, req Request) (Response, error) {
    // 3 attempts: 0ms, 500ms, 1s, 2s backoff.
    // Retry only on exit code 1 (bridge error) -- calendar-side errors
    // (event not found, calendar missing) fail fast.
}
```

Retry budget per tool call: 3 attempts. Total worst-case latency: ~3.5s. Logged at each retry so flaky permissions/EventKit-locked-by-Calendar.app scenarios are visible in `logger`.

---

## Part 4 -- `get_time` hot tool

### Rationale

The time layer (`layers/agent_time.go`) injects current time and timezone at the start of every agent prompt. But as context grows -- tools loaded, memories recalled, conversation history -- that header ends up hundreds of lines back. For calendar operations that need precise timestamps (scheduling events, checking "is this before or after now?"), the agent needs a reliable way to re-check the clock mid-turn without guessing.

### Design

- **Hot tool** (always available, like `think`, `reply`, `done`).
- **No parameters.** Just returns the current time.
- **Returns:** ISO 8601 with offset + human-readable format + day of week.
- **Reads timezone from** `config.Calendar.DefaultTimezone`. Falls back to `time.Local` if not configured.

### Tool YAML

```yaml
# tools/get_time/tool.yaml
name: get_time
agent: main
hint: "check the current date and time"
description: >-
  Return the current date and time in the configured timezone. Use this
  when you need to check what time it is mid-turn -- for example, to
  calculate how far away a scheduled event is, or to confirm the current
  day of the week before scheduling something. The time layer at the top
  of the prompt also shows this, but it may be hundreds of lines back.
hot: true
parameters:
  type: object
  properties: {}
  required: []
trace:
  emoji: "clock"
  format: "checked the time"
```

### Handler

```go
// tools/get_time/handler.go

// Returns:
// {
//   "iso": "2026-04-20T14:32:07-04:00",
//   "human": "Sunday, April 20, 2026 2:32 PM EDT",
//   "day_of_week": "Sunday",
//   "timezone": "America/New_York"
// }
```

The handler loads the timezone from `cfg.Calendar.DefaultTimezone`, calls `time.Now().In(loc)`, and formats both representations. Minimal code, no external calls.

---

## Part 5 -- Calendar tools

Four tools, all following the existing pattern: `tool.yaml` manifest in `tools/<name>/` plus a `handler.go` that registers via `tools.Register`. None are hot -- loaded via `use_tools(["calendar"])`.

### `calendar_list`

```yaml
name: calendar_list
description: >-
  Read events from the configured calendar in a date range.
  Use when the user asks "what's on my calendar" or for context like
  "am I free Friday afternoon".
hot: false
category: calendar
parameters:
  type: object
  properties:
    start:
      type: string
      description: "ISO 8601 with offset (required)"
    end:
      type: string
      description: "ISO 8601 with offset (required)"
    calendar:
      type: string
      description: "Optional override, defaults to config calendar_name"
  required: [start, end]
```

### `calendar_create`

```yaml
name: calendar_create
description: >-
  Create one or more calendar events that are NOT work shifts (use
  shift_schedule for those). Examples: dentist appointment, birthday,
  meeting. Atomic-per-call.
hot: false
category: calendar
parameters:
  type: object
  properties:
    events:
      type: array
      description: "Events to create"
      items:
        type: object
        properties:
          title: { type: string }
          start: { type: string, description: "ISO 8601 with offset" }
          end: { type: string, description: "ISO 8601 with offset" }
          location: { type: string, description: "Optional" }
          notes: { type: string, description: "Optional" }
        required: [title, start, end]
    calendar:
      type: string
      description: "Optional override"
  required: [events]
```

### `calendar_update`

```yaml
name: calendar_update
description: >-
  Edit a calendar event in place by id. Get the id from calendar_list.
  Pass only the fields you want to change.
hot: false
category: calendar
parameters:
  type: object
  properties:
    event_id: { type: string, description: "Required, from calendar_list" }
    title: { type: string }
    start: { type: string, description: "ISO 8601 with offset" }
    end: { type: string, description: "ISO 8601 with offset" }
    location: { type: string }
    notes: { type: string }
  required: [event_id]
```

### `calendar_delete`

```yaml
name: calendar_delete
description: >-
  Delete a calendar event by id. Use sparingly -- for shifts, prefer
  shift_cancel which preserves history.
hot: false
category: calendar
parameters:
  type: object
  properties:
    event_id: { type: string, description: "Required" }
  required: [event_id]
```

---

## Part 6 -- Category registration + agent prompt

### `tools/categories.yaml`

Add one entry:

```yaml
calendar:
  hint: "User mentions a work shift, schedule, calendar event, or asks about hours worked"
```

The existing `RenderCategoryTable()` picks it up automatically.

### Agent prompt

Two small additions to `main_agent_prompt.md`:

1. Add an example flow under "Typical Flows":
   ```
   7. User pastes a work schedule:
      think("schedule drop, parse into shifts") ->
      use_tools(["calendar"]) ->
      shift_schedule({job:"...", shifts:[...]}) ->
      reply("scheduled N shifts, total X hrs") -> done
   ```
2. One-line note under "Order of Operations" that the Current Time block is the source of truth for grounding "Tuesday at 5am" to a date, and `get_time` can re-check it mid-turn.

### Stale-code cleanup

The four stale references identified in the original plan's Part 8 have already been fixed in commit `52428d3`. No cleanup needed in this plan.

---

## Decisions

| Decision | Choice | Why |
|---|---|---|
| Bridge transport | Single Swift CLI, JSON over stdin/stdout | Simplest. No daemon, no ports, no signing ceremony. macOS permission prompt fires on first manual Terminal run. |
| Bridge install | Manual one-time setup | Triggering the EventKit permission prompt requires a GUI-attached process at least once; documented as a setup step. |
| Batch atomicity | Partial success + report | Don't lose 4 good events because #5 had a bad timestamp. Failed ones returned in `failed[]`. |
| Backfill of existing calendar events | Fresh start, no import | Lowest risk. Pre-existing calendar events stay untouched, just aren't tracked. Backfill becomes a future CLI command if needed. |
| Time storage | ISO 8601 with offset (TEXT) | Readable in DB, round-trips losslessly, matches existing project conventions. |
| Per-category guide injection | Skipped | `LookupToolDefs` already passes full schemas to the LLM when a category loads. Tool descriptions alone are sufficient. |
| Time tool | `get_time` hot tool | Original plan said no time tool because `layers/agent_time.go` injects time at prompt start. But as context grows, that header is hundreds of lines back. A zero-parameter hot tool lets the agent re-check the clock mid-turn cheaply. |

## Known Limitations (v1)

- **macOS-only.** EventKit is Apple-only. Linux/Windows hosts can't use the calendar tools -- they'll fail with the bridge-not-found error. Future: Google Calendar HTTP bridge.
- **Single calendar.** All events go to the calendar named in `config.calendar.calendar_name`. Multi-calendar support is straightforward to add later.
- **No two-way sync from Apple Calendar.** If events are edited directly in Apple Calendar, her doesn't know. Future: periodic poll that diffs and flags drift.
- **Bridge invocation cost.** Each call spawns a Swift process (~50-100ms cold start on M-series). Batch operations help. Future: long-lived bridge subprocess if it becomes a bottleneck.

---

## Phases

### Phase 1 -- Swift bridge (standalone)

- `calendar/bridge/Package.swift`, `Sources/her-calendar/{main,Commands,JSON}.swift`
- `calendar/bridge/README.md` with build + permission steps
- Manual smoke test: `echo '{...}' | her-calendar` from Terminal for each command
- No Go changes yet

**Done when:** Swift binary builds, runs against a test calendar in Apple Calendar, all 4 commands round-trip successfully.

### Phase 2 -- Config additions

- `config/config.go`: `CalendarConfig` struct (bridge_path, calendar_name, default_timezone only)
- `config.yaml.example`: documented `calendar:` block (bridge fields only)

**Done when:** config loads cleanly, struct is accessible via `cfg.Calendar.*`.

### Phase 3 -- Go bridge wrapper with retry

- `calendar/bridge.go`: `Bridge` type with `Call()`, retry policy, fail-soft when binary missing
- `calendar/bridge_test.go`: tests with a fake binary (shell script that echoes canned JSON)
- Hook bridge initialization into startup; log warning + continue if missing

**Done when:** Go can drive the bridge end-to-end, retries on flaky exit codes, returns useful errors.

### Phase 4 -- `get_time` hot tool

- `tools/get_time/tool.yaml` + `tools/get_time/handler.go`
- Reads timezone from `cfg.Calendar.DefaultTimezone`, falls back to `time.Local`
- Returns ISO 8601, human-readable, day of week, timezone name

**Done when:** agent can call `get_time` and receive accurate current time in configured timezone.

### Phase 5 -- 4 calendar tools + category + agent prompt

- `tools/calendar_list/`, `tools/calendar_create/`, `tools/calendar_update/`, `tools/calendar_delete/`
- Each: `tool.yaml` + `handler.go` + minimal handler test
- Add `calendar` to `tools/categories.yaml`
- Agent prompt additions (example flow + time note)
- Smoke test: agent invokes `calendar_create` for a fake event, sees it in Apple Calendar

**Done when:** all 4 calendar tools work end-to-end with a real calendar. `get_time` is available. Category loads correctly.

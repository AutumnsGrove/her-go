# Plan: Apple Reminders Bridge + Reminder Tools

Build a Swift EventKit bridge for Apple Reminders, wrap it in Go with the same CLIBridge pattern as the calendar bridge, ship 4 consolidated agent tools (`reminder_lists`, `reminder_query`, `reminder_create`, `reminder_manage`), and mirror bot-created reminders in SQLite. The architecture mirrors the calendar bridge exactly -- JSON over stdin/stdout, SQLite as source of truth, EventKit as secondary sync target.

**Status:** ready for implementation
**Depends on:** calendar bridge (for shared Swift library extraction)
**Tracking:**

---

## Goals

- Read and write Apple Reminders through a Swift CLI bridge (`her-reminders`) using EventKit. JSON over stdin/stdout.
- Extract shared EventKit helpers into a Swift library (`her-eventkit-common`) so both bridges avoid code duplication.
- Give the agent 4 consolidated tools: list management, querying, creating, and managing (update/complete/uncomplete/delete).
- Support rich reminder properties: priority, location triggers (arrive/depart), recurrence, and configurable time-of-day defaults.
- Full lifecycle: create, update, complete, uncomplete, delete.
- Read ALL reminders (including manually-created), but only modify bot-created ones (tracked by SQLite ownership).
- Full mirror of bot-created reminders in SQLite for fast queries and audit trail.

---

## Part 1 -- Config additions

```yaml
reminders:
  # Path to the compiled Swift bridge binary. Relative paths resolved
  # from the project root. Bot logs a warning at startup if missing;
  # all reminder tools become no-ops with a clear error message.
  bridge_path: "reminders/bridge/.build/release/her-reminders"

  # Default reminder list for new reminders. Must exist in Apple Reminders.
  default_list: "Reminders"

  # Optional time-of-day defaults. When the user says "remind me tomorrow
  # morning" and doesn't give a specific time, the agent uses these.
  # If omitted entirely, the agent picks a reasonable time from context.
  time_defaults:
    morning: "09:00"
    afternoon: "14:00"
    evening: "19:00"

  # Named places for location-based reminders. Each value is a full
  # address string -- the Swift bridge geocodes it via CLGeocoder.
  # The agent can say "when I get to work" and the bridge resolves
  # the address to coordinates for the geofence trigger.
  known_places:
    home: "123 Main St, Anytown, NY 10001, US"
    work: "456 Office Blvd, Suite 200, Cityville, NY 10002, US"
```

**Config struct (Go side):**

```go
// config/config.go additions

type RemindersConfig struct {
    BridgePath   string            `yaml:"bridge_path"`
    DefaultList  string            `yaml:"default_list"`
    TimeDefaults *TimeDefaults     `yaml:"time_defaults,omitempty"` // nil = agent decides
    KnownPlaces  map[string]string `yaml:"known_places,omitempty"` // name -> full address
}

// TimeDefaults maps time-of-day labels to HH:MM strings.
// Optional -- if nil, the agent uses its own judgment for vague times.
type TimeDefaults struct {
    Morning   string `yaml:"morning"`   // e.g. "09:00"
    Afternoon string `yaml:"afternoon"` // e.g. "14:00"
    Evening   string `yaml:"evening"`   // e.g. "19:00"
}
```

Add `Reminders RemindersConfig \`yaml:"reminders"\`` to the top-level `Config` struct.

`config.yaml.example` gets the same block with comments.

---

## Part 2 -- Shared Swift library extraction

Before building the reminders bridge, extract common code from `her-calendar` into a shared Swift package. Both bridges will depend on it.

### What moves to `her-eventkit-common`

- JSON wire protocol base types: `Response`, error formatting, exit code constants
- Date parsing helpers (ISO 8601 with offset)
- stdin/stdout JSON I/O dispatcher pattern
- EventKit permission request helpers

### What stays in each binary

- `her-calendar`: `EKEvent`-specific commands, calendar lookup, event input/output structs
- `her-reminders`: `EKReminder`-specific commands, list lookup, reminder input/output structs, geocoding

### Layout after extraction

```
swift-common/
  Package.swift                    # library target: HerEventKit
  Sources/
    HerEventKit/
      IO.swift                     # stdin/stdout JSON dispatch
      Protocol.swift               # Response, error codes, exit helpers
      DateParsing.swift            # ISO 8601 helpers

calendar/
  bridge/
    Package.swift                  # depends on swift-common via local path
    Sources/her-calendar/
      main.swift
      Commands.swift
      JSON.swift                   # calendar-specific Codable types

reminders/
  bridge/
    Package.swift                  # depends on swift-common via local path
    Sources/her-reminders/
      main.swift
      Commands.swift
      JSON.swift                   # reminder-specific Codable types
      Geocoder.swift               # CLGeocoder wrapper for location triggers
```

Both `Package.swift` files reference the shared library:
```swift
.package(path: "../../swift-common")
```

---

## Part 3 -- Swift reminders bridge (`her-reminders`)

### Wire protocol

Same pattern as `her-calendar`: one JSON command on stdin, one JSON response on stdout, process exits.

**Request:**
```json
{
  "command": "list_lists" | "create_list" | "list_reminders" | "create_reminder" | "update_reminder" | "complete_reminder" | "uncomplete_reminder" | "delete_reminder",
  "args": { ... command-specific ... }
}
```

**Response (success):**
```json
{ "ok": true, "result": { ... command-specific ... } }
```

**Response (error):**
```json
{ "ok": false, "error": "permission_denied" | "list_not_found" | "reminder_not_found" | "geocoding_failed" | "...", "message": "Human-readable detail." }
```

Exit codes: 0 = success, 1 = bridge error (retryable), 2 = reminders-side error (fail fast).

### Commands

**`list_lists`** -- all reminder lists, flat:
```json
// args: (none)
// result
{ "lists": [
    { "id": "ABC123", "title": "Reminders", "count": 12 },
    { "id": "DEF456", "title": "Groceries", "count": 3 },
    { "id": "GHI789", "title": "Work", "count": 7 }
]}
```

**`create_list`** -- create a new reminder list:
```json
// args
{ "title": "Trip Packing" }
// result
{ "id": "JKL012", "title": "Trip Packing" }
```

**`list_reminders`** -- query reminders with filters:
```json
// args
{
  "list": "Groceries",           // optional: filter to one list (omit = all lists)
  "completed": false,            // optional: true = only completed, false = only incomplete, omit = both
  "start": "2026-04-20T00:00:00-04:00",  // optional: due date range start
  "end": "2026-04-27T00:00:00-04:00"     // optional: due date range end
}
// result
{ "reminders": [
    {
      "id": "REM001",
      "title": "Buy oat milk",
      "notes": "The Oatly barista one",
      "list": "Groceries",
      "due_date": "2026-04-21T09:00:00-04:00",
      "priority": "none",
      "completed": false,
      "completed_date": null,
      "location": null,
      "recurrence": null
    }
]}
```

**`create_reminder`** -- create one reminder with full properties:
```json
// args
{
  "title": "Buy groceries",
  "notes": "Oat milk, bread, eggs",
  "list": "Reminders",
  "due_date": "2026-04-22T09:00:00-04:00",
  "priority": "medium",
  "location": {
    "address": "123 Main St, Anytown, NY 10001, US",
    "proximity": "arrive",       // "arrive" or "depart"
    "radius": 100                // meters, optional (default 100)
  },
  "recurrence": {
    "frequency": "weekly",       // "daily" | "weekly" | "biweekly" | "monthly" | "yearly" | "custom"
    "days_of_week": [1, 3]       // 0=Sun, 1=Mon, ... 6=Sat. Only used with "custom" frequency.
  }
}
// result
{ "id": "REM002" }
```

**`update_reminder`** -- sparse update by id. Omitted fields left unchanged:
```json
// args
{ "id": "REM002", "title": "Buy groceries + snacks", "priority": "high" }
// result
{ "id": "REM002" }
```

**`complete_reminder`** -- mark as done:
```json
// args
{ "id": "REM002" }
// result
{ "id": "REM002", "completed": true, "completed_date": "2026-04-21T14:32:00-04:00" }
```

**`uncomplete_reminder`** -- reopen:
```json
// args
{ "id": "REM002" }
// result
{ "id": "REM002", "completed": false }
```

**`delete_reminder`** -- permanent delete:
```json
// args
{ "id": "REM002" }
// result
{ "deleted": true }
```

### Geocoding flow (location triggers)

When `location` is present in a create/update:

1. `CLGeocoder().geocodeAddressString(address)` → `CLPlacemark`
2. Extract `CLLocationCoordinate2D` from placemark
3. Create `EKStructuredLocation(title: address)` with coordinate
4. Set `EKAlarm` with `.proximity = .enter` (arrive) or `.leave` (depart)
5. Set alarm's `structuredLocation` and `radius` (default 100m if not specified)

If geocoding fails, the bridge returns exit code 2 with `"error": "geocoding_failed"` and a message explaining what happened. The Go handler can surface this to the agent, which can ask the user for a more specific address.

### Recurrence mapping

| Config value | EKRecurrenceRule |
|---|---|
| `daily` | `EKRecurrenceRule(recurrenceWith: .daily, interval: 1, end: nil)` |
| `weekly` | `.weekly, interval: 1` |
| `biweekly` | `.weekly, interval: 2` |
| `monthly` | `.monthly, interval: 1` |
| `yearly` | `.yearly, interval: 1` |
| `custom` + `days_of_week` | `.weekly, interval: 1, daysOfTheWeek: [EKRecurrenceDayOfWeek(day)]` |

### Install + permissions (one-time)

Same pattern as `her-calendar`:

1. `cd reminders/bridge && swift build -c release`
2. Binary at `.build/release/her-reminders`
3. Run once from Terminal to trigger macOS permission prompt for Reminders access
4. Grant `.fullAccess` to reminders (macOS 14+ API)

---

## Part 4 -- Go bridge wrapper

Mirrors `calendar/bridge.go` exactly:

```go
// reminders/bridge.go

// Bridge is the interface for talking to Apple Reminders.
// CLIBridge shells out to the her-reminders Swift binary.
// FakeBridge provides an in-memory implementation for tests/sims.
type Bridge interface {
    Call(ctx context.Context, req Request) (Response, error)
}

type CLIBridge struct {
    binaryPath string
}

// Call sends a JSON request to her-reminders via stdin, reads JSON response
// from stdout. Retry policy: 3 attempts, exponential backoff (0/500ms/1s/2s).
// Exit 1 = retry, exit 2 = fail fast.
```

Plus `reminders/bridge_fake.go` with an in-memory store for tests and sims.

---

## Part 5 -- SQLite schema (`store_reminders.go`)

Full mirror of bot-created reminders. Manually-created reminders are read-only through the bridge and NOT stored in SQLite.

```sql
CREATE TABLE IF NOT EXISTS reminders (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    reminder_id TEXT,                -- EventKit identifier (null until synced)
    title       TEXT NOT NULL,
    notes       TEXT,
    list        TEXT NOT NULL,       -- reminder list name
    due_date    TEXT,                -- ISO 8601 with offset (null = no due date)
    priority    TEXT DEFAULT 'none', -- none, low, medium, high
    completed   INTEGER DEFAULT 0,  -- 0 = incomplete, 1 = complete
    completed_date TEXT,             -- ISO 8601 when marked complete
    location_address TEXT,           -- full address string (null = no location trigger)
    location_proximity TEXT,         -- "arrive" or "depart" (null = no trigger)
    location_radius INTEGER,         -- geofence radius in meters
    recurrence_frequency TEXT,       -- daily, weekly, biweekly, monthly, yearly, custom
    recurrence_days TEXT,            -- JSON array of day numbers [0-6] for custom
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    active      INTEGER DEFAULT 1   -- soft delete (0 = deleted)
);

CREATE INDEX IF NOT EXISTS idx_reminders_due ON reminders(due_date) WHERE active = 1;
CREATE INDEX IF NOT EXISTS idx_reminders_list ON reminders(list) WHERE active = 1;
CREATE INDEX IF NOT EXISTS idx_reminders_completed ON reminders(completed) WHERE active = 1;
```

### Store methods

```go
// memory/store_reminders.go

InsertReminder(r *Reminder) (int64, error)
UpdateReminder(id int64, fields map[string]any) error
UpdateReminderID(localID int64, eventKitID string) error  // fill in reminder_id after sync
CompleteReminder(id int64) error                          // set completed=1, completed_date=now
UncompleteReminder(id int64) error                        // set completed=0, completed_date=null
DeleteReminder(id int64) error                            // soft delete (active=0)
ListReminders(opts ReminderQuery) ([]Reminder, error)     // filter by list, date range, completed status
GetReminderByReminderID(eventKitID string) (*Reminder, error)
IsOwnedReminder(eventKitID string) (bool, error)          // check if bot created this reminder
```

### Ownership enforcement

The `IsOwnedReminder` method checks whether a given EventKit ID exists in the `reminders` table. Tool handlers call this before any write operation. If the reminder wasn't created by the bot, the handler returns a clear error: `"this reminder was not created by the bot and cannot be modified"`.

---

## Part 6 -- Agent tools (4 consolidated)

All tools are cold, category `reminders`, loaded via `use_tools(["reminders"])`.

### `reminder_lists` -- list + create lists

```yaml
name: reminder_lists
agent: main
description: >-
  Manage Apple Reminder lists. Use "list" action to discover available
  lists, or "create" to make a new one. Returns all lists flat in one go.
hot: false
category: reminders
parameters:
  type: object
  properties:
    action:
      type: string
      enum: [list, create]
      description: "'list' to see all lists, 'create' to make a new one"
    title:
      type: string
      description: "Required for 'create' action: name for the new list"
  required: [action]
trace:
  emoji: "clipboard"
  format: "{{action}} reminder lists"
```

### `reminder_query` -- search/filter reminders

```yaml
name: reminder_query
agent: main
description: >-
  Query reminders across all lists or filtered to one. Can filter by
  date range and completion status. Returns both bot-created and
  manually-created reminders (read access to all).
hot: false
category: reminders
parameters:
  type: object
  properties:
    list:
      type: string
      description: "Filter to this list (omit for all lists)"
    start:
      type: string
      description: "ISO 8601 with offset -- due date range start"
    end:
      type: string
      description: "ISO 8601 with offset -- due date range end"
    completed:
      type: boolean
      description: "true = only completed, false = only incomplete, omit = both"
  required: []
trace:
  emoji: "magnifying_glass"
  format: "searched reminders{{#list}} in {{list}}{{/list}}"
```

### `reminder_create` -- create a reminder

```yaml
name: reminder_create
agent: main
description: >-
  Create a new reminder in Apple Reminders. Supports due date/time,
  priority, location-based triggers (arrive/depart), and recurrence.
  Location addresses are geocoded automatically -- use full addresses
  or configured place names (home, work, etc.).
hot: false
category: reminders
parameters:
  type: object
  properties:
    title:
      type: string
      description: "What to be reminded about"
    notes:
      type: string
      description: "Additional details (optional)"
    list:
      type: string
      description: "Which reminder list (default: config default_list)"
    due_date:
      type: string
      description: "ISO 8601 with offset. For vague times, use config time_defaults if available."
    priority:
      type: string
      enum: [none, low, medium, high]
      description: "Reminder priority (default: none)"
    location:
      type: object
      description: "Location-based trigger (optional). Address is geocoded automatically."
      properties:
        address:
          type: string
          description: "Full address OR a known_places name from config (e.g. 'home', 'work')"
        proximity:
          type: string
          enum: [arrive, depart]
          description: "Trigger when arriving at or departing from location"
        radius:
          type: integer
          description: "Geofence radius in meters (default: 100)"
      required: [address, proximity]
    recurrence:
      type: object
      description: "Repeating schedule (optional)"
      properties:
        frequency:
          type: string
          enum: [daily, weekly, biweekly, monthly, yearly, custom]
          description: "How often. Use 'custom' with days_of_week for specific days."
        days_of_week:
          type: array
          items:
            type: integer
          description: "Days of week (0=Sun..6=Sat). Only used with 'custom' frequency."
      required: [frequency]
  required: [title]
trace:
  emoji: "bell"
  format: "created reminder: {{title}}"
```

### `reminder_manage` -- update, complete, uncomplete, delete

```yaml
name: reminder_manage
agent: main
description: >-
  Manage an existing bot-created reminder. Actions: update (change fields),
  complete (mark done), uncomplete (reopen), delete (permanent removal).
  Can only modify reminders the bot created -- manually-created reminders
  are read-only.
hot: false
category: reminders
parameters:
  type: object
  properties:
    action:
      type: string
      enum: [update, complete, uncomplete, delete]
      description: "What to do with the reminder"
    reminder_id:
      type: string
      description: "EventKit reminder ID (from reminder_query results)"
    title:
      type: string
      description: "New title (update only)"
    notes:
      type: string
      description: "New notes (update only)"
    due_date:
      type: string
      description: "New due date, ISO 8601 (update only)"
    priority:
      type: string
      enum: [none, low, medium, high]
      description: "New priority (update only)"
    location:
      type: object
      description: "New location trigger (update only, null to remove)"
      properties:
        address:
          type: string
        proximity:
          type: string
          enum: [arrive, depart]
        radius:
          type: integer
      required: [address, proximity]
    recurrence:
      type: object
      description: "New recurrence (update only, null to remove)"
      properties:
        frequency:
          type: string
          enum: [daily, weekly, biweekly, monthly, yearly, custom]
        days_of_week:
          type: array
          items:
            type: integer
      required: [frequency]
  required: [action, reminder_id]
trace:
  emoji: "wrench"
  format: "{{action}} reminder {{reminder_id}}"
```

---

## Part 7 -- Known places resolution (Go side)

The `reminder_create` handler resolves known place names before sending to the bridge:

```go
// In the handler, before calling bridge:
if loc := args.Location; loc != nil {
    // Check if address matches a known_places key
    if addr, ok := cfg.Reminders.KnownPlaces[strings.ToLower(loc.Address)]; ok {
        loc.Address = addr  // replace "home" with "123 Main St, Anytown, NY 10001, US"
    }
}
```

This keeps the Swift bridge simple -- it always receives a full address string. The Go handler does the config lookup.

---

## Part 8 -- Category registration + agent prompt

### `tools/categories.yaml`

Add one entry:

```yaml
reminders:
  hint: "User mentions a reminder, to-do, task, grocery list, or wants to be reminded about something"
```

### Agent prompt

Add to `main_agent_prompt.md`:

1. Example flow under "Typical Flows":
   ```
   N. User asks to be reminded about something:
      think("reminder request, need reminder tools") ->
      use_tools(["reminders"]) ->
      reminder_create({title:"...", due_date:"...", ...}) ->
      reply("done, I set a reminder for ...") -> done
   ```

2. Note under tool guidance:
   ```
   Reminders: You can read all reminders but only modify ones you created.
   For location triggers, use known place names from config or full addresses.
   Time defaults (morning/afternoon/evening) are available in config if set.
   ```

---

## Decisions

| Decision | Choice | Why |
|---|---|---|
| Binary architecture | Shared Swift library + 2 separate binaries | DRY common code (JSON I/O, date parsing, permissions) while keeping each bridge focused on its EventKit type |
| Tool consolidation | 4 tools instead of 7-8 | Fewer tools = less for the agent to reason about. `action` param keeps intent clear. Mirrors how modern APIs bundle CRUD. |
| Ownership boundary | Read all, write own | User sees their full reminder state through the bot, but can't accidentally have the bot modify their hand-created reminders. |
| Location resolution | Go resolves known places, Swift geocodes addresses | Separation of concerns: config lookup is Go's job, Apple APIs (CLGeocoder) are Swift's job |
| Geocoding | CLGeocoder in Swift bridge | Apple's own geocoder works best with Apple Reminders. Full address strings from config (no lat/lon). User never looks up coordinates. |
| Time defaults | Optional config, agent judgment as fallback | Flexible: power users define presets, everyone else gets smart defaults from the agent |
| Recurrence | Presets + custom days of week | Covers 95% of real-world use cases without the complexity of arbitrary EKRecurrenceRule configurations |
| SQLite scope | Full mirror of bot-created reminders | Fast local queries, audit trail, graceful degradation if bridge is down. Consistent with calendar pattern. |
| Completion lifecycle | Full (complete, uncomplete, delete) | Maximum flexibility. Users change their minds. |
| List management | Discover + create | Agent can see what exists and create new lists on the fly. No config-only restriction. |

## Known Limitations (v1)

- **macOS-only.** EventKit is Apple-only. Linux/Windows hosts can't use reminder tools -- they'll fail with bridge-not-found error.
- **No two-way sync from Apple Reminders.** If reminders are completed/modified in the Reminders app, SQLite doesn't know. The bridge reads live state on query, but the local mirror may drift for bot-created items.
- **Geocoding requires network.** CLGeocoder needs an internet connection. Offline location triggers fail with a clear error.
- **Bridge invocation cost.** Each call spawns a Swift process (~50-100ms cold start on M-series). Acceptable for reminder operations which are lower-frequency than calendar queries.
- **No attachments/images.** EKReminder supports attachments but we skip them in v1. Can be added later.

---

## Phases

### Phase 1 -- Shared Swift library extraction

- Create `swift-common/` with shared IO, protocol, and date parsing helpers
- Refactor `her-calendar` to depend on the shared library
- Verify calendar bridge still works after extraction

**Done when:** `her-calendar` builds against the shared library and all calendar operations pass.

### Phase 2 -- Swift reminders bridge (standalone)

- `reminders/bridge/Package.swift`, `Sources/her-reminders/{main,Commands,JSON,Geocoder}.swift`
- Depends on `swift-common` via local path
- All 8 commands: list_lists, create_list, list_reminders, create_reminder, update_reminder, complete_reminder, uncomplete_reminder, delete_reminder
- Manual smoke test from Terminal for each command
- Test geocoding with real addresses

**Done when:** Swift binary builds, runs against Apple Reminders, all commands round-trip successfully, location triggers create valid geofences.

### Phase 3 -- Config additions

- `config/config.go`: `RemindersConfig` struct with all fields
- `config.yaml.example`: documented `reminders:` block
- Add `Reminders` field to top-level `Config` struct

**Done when:** config loads cleanly, struct is accessible via `cfg.Reminders.*`.

### Phase 4 -- Go bridge wrapper + SQLite schema

- `reminders/bridge.go`: CLIBridge with same retry policy as calendar
- `reminders/bridge_fake.go`: in-memory fake for tests/sims
- `memory/store_reminders.go`: full schema + all store methods
- Ownership check method (`IsOwnedReminder`)
- Hook bridge initialization into startup (fail-soft)

**Done when:** Go can drive the reminders bridge end-to-end, SQLite schema migrates, fake bridge passes tests.

### Phase 5 -- 4 reminder tools + category + agent prompt

- `tools/reminder_lists/`, `tools/reminder_query/`, `tools/reminder_create/`, `tools/reminder_manage/`
- Each: `tool.yaml` + `handler.go`
- Known places resolution in `reminder_create` handler
- Ownership enforcement in `reminder_manage` handler
- Add `reminders` to `tools/categories.yaml`
- Agent prompt additions
- End-to-end smoke test: agent creates a reminder, queries it, completes it, sees it in Apple Reminders app

**Done when:** all 4 tools work end-to-end. Bot can create location-triggered recurring reminders. Ownership boundary enforced. Category loads correctly.

### Phase 6 -- Testing + polish

- FakeBridge tests for all operations
- Handler tests with FakeBridge
- Test ownership enforcement (reject modify on non-owned reminder)
- Test known places resolution
- Test graceful degradation (bridge missing, geocoding fails, permission denied)
- README in `reminders/bridge/` with build + permission steps

**Done when:** test coverage on all major paths, README complete, ready for daily use.

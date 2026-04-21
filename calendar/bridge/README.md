# her-calendar

Swift CLI bridge to Apple's EventKit framework. Provides JSON-based stdin/stdout access to calendar operations for the Go-based her chatbot.

## Why Swift?

EventKit is Apple's native calendar framework (Objective-C/Swift only). Rather than dealing with Objective-C bindings in Go, we built a tiny Swift CLI that acts as a clean JSON bridge. Go shells out to this binary when calendar tools are invoked.

## Build

Requires Xcode Command Line Tools or Xcode installed.

```bash
cd calendar/bridge
swift build -c release
```

The compiled binary appears at `.build/release/her-calendar`.

## Permission Setup (One-Time)

EventKit requires explicit user permission to access calendars. The first time you run the binary, macOS will show a permission prompt.

### Grant Permission

Run the binary once from Terminal (before the bot tries to use it):

```bash
cd calendar/bridge
echo '{"command":"list","calendar":"Work","args":{"start":"2026-04-20T00:00:00-04:00","end":"2026-04-21T00:00:00-04:00"}}' | .build/release/her-calendar
```

macOS will show a permission dialog. **Click "Allow"**.

If you accidentally clicked "Deny", go to **System Settings → Privacy & Security → Calendars** and enable the checkbox for `her-calendar`.

## Wire Protocol

Single-shot JSON stdin/stdout. One command in, one response out, process exits.

### Request Format

```json
{
  "command": "list" | "create" | "update" | "delete",
  "calendar": "Work",
  "args": { ...command-specific... }
}
```

### Response Format (Success)

```json
{
  "ok": true,
  "result": { ...command-specific... }
}
```

### Response Format (Error)

```json
{
  "ok": false,
  "error": "calendar_not_found" | "event_not_found" | "permission_denied" | ...,
  "message": "Human-readable error detail"
}
```

### Exit Codes

- **0** — Success
- **1** — Bridge error (bad JSON, permission denied, etc.) → Go retries
- **2** — Calendar-side error (event not found, calendar missing, etc.) → Go fails fast

## Commands

### list

Get events in a date range.

**Args:**
```json
{
  "start": "2026-04-20T00:00:00-04:00",
  "end": "2026-04-27T00:00:00-04:00"
}
```

**Result:**
```json
{
  "events": [
    {
      "id": "ABC123",
      "title": "Panera 5a-1p",
      "start": "2026-04-20T05:00:00-04:00",
      "end": "2026-04-20T13:00:00-04:00",
      "location": "3625 Spring Hill Ave",
      "notes": "Bring apron"
    }
  ]
}
```

### create

Create one or more events atomically (all or nothing).

**Args:**
```json
{
  "events": [
    {
      "title": "Dentist",
      "start": "2026-04-22T14:00:00-04:00",
      "end": "2026-04-22T15:00:00-04:00",
      "location": "123 Main St",
      "notes": "Bring insurance card"
    }
  ]
}
```

**Result:**
```json
{
  "events": [
    { "id": "XYZ789" }
  ]
}
```

On failure mid-batch, the bridge attempts to delete all events created in this call before returning the error.

### update

Update an existing event by ID. Omitted fields are left unchanged.

**Args:**
```json
{
  "id": "ABC123",
  "event": {
    "title": "Panera 6a-2p",
    "start": "2026-04-20T06:00:00-04:00",
    "end": "2026-04-20T14:00:00-04:00"
  }
}
```

**Result:**
```json
{
  "id": "ABC123"
}
```

### delete

Delete an event by ID.

**Args:**
```json
{
  "id": "ABC123"
}
```

**Result:**
```json
{
  "deleted": true
}
```

## Testing

### Manual Test (List)

```bash
echo '{"command":"list","calendar":"Work","args":{"start":"2026-04-20T00:00:00-04:00","end":"2026-04-21T00:00:00-04:00"}}' | .build/release/her-calendar
```

### Manual Test (Create)

```bash
echo '{"command":"create","calendar":"Work","args":{"events":[{"title":"Test Event","start":"2026-04-22T10:00:00-04:00","end":"2026-04-22T11:00:00-04:00"}]}}' | .build/release/her-calendar
```

Check Apple Calendar for the event. Delete it manually when done testing.

## Limitations

- **macOS only** — EventKit is an Apple framework
- **Single calendar** — All operations target one calendar (specified in each request)
- **No daemon** — Each invocation is independent (50-100ms cold start overhead)
- **No recurring events** — Only one-time events supported (future: recurrence rules)

## Architecture Notes

- **Why no HTTP server?** Simpler. No ports, no daemon lifecycle, no authentication. stdin/stdout is sufficient for local IPC.
- **Why JSON instead of protobuf/msgpack?** Debuggability. You can test manually with `echo` and pipe. Trade 50-100 bytes per call for instant comprehension.
- **Why exit code distinction (1 vs 2)?** Go needs to know whether to retry. Exit 1 (permission flake, EventKit locked by Calendar.app) = retry. Exit 2 (event not found, calendar missing) = fail fast.

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

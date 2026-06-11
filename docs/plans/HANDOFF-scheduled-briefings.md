# Handoff: Conversational Scheduled Briefings

## Context

The worker agent system is fully shipped and working in production. Mira can:
- Accept research/briefing requests via `send_task(target="worker")`
- Run background web research with configurable model tiers (low/medium/high)
- Write markdown reports to `reports/`
- Publish to Telegraph for rich rendering
- Auto-inject Telegraph links after replies
- Narrate reports aloud via TTS

**What's missing:** Mira can't CREATE scheduled briefings from conversation. Right now all research/briefings are one-off — the user asks, Mira delegates, the worker runs once. There's no way to say "brief me every morning about Go news" and have it recur automatically.

## What to Build

A `create_briefing_schedule` tool (or extend the existing junkdrawered `create_schedule` tool) that lets the driver agent create recurring worker tasks from conversation.

### User Stories

1. "Mira, every morning at 8am, send me a briefing on Go programming and AI agents"
   → Creates a recurring scheduler row: `kind=worker_briefing`, `cron="0 8 * * *"`, `payload={topics: "Go programming, AI agents", depth: "brief"}`

2. "Give me a weekly deep dive on Rust ecosystem news every Friday"
   → Creates: `kind=worker_briefing`, `cron="0 9 * * 5"`, `payload={topics: "Rust ecosystem", depth: "deep"}`, `model_tier: "medium"`

3. "What briefings do I have scheduled?" → Lists active briefing schedules
4. "Cancel the morning tech briefing" → Disables/deletes the schedule
5. "Change my morning briefing to 7am instead of 8" → Updates the cron

### Architecture

The pieces that exist:
- **Scheduler system** (`scheduler/`) — fully functional cron-based task runner with retry, damping, quiet hours
- **`scheduler_tasks` table** — already stores kind, cron_expr, payload, enabled flag
- **Worker briefing handler** (`workeragent/briefing_handler.go`) — already reads `instruction` and `topics` from the payload
- **Junkdrawered schedule tools** — `_junkdrawer/tools/create_schedule/`, `_junkdrawer/tools/list_schedules/`, `_junkdrawer/tools/update_schedule/` — these were built for the old v0.2 scheduler but have the right shape

The pieces that need building:
1. **Revive `create_schedule` tool** — pull from junkdrawer, update to support `task_type: "worker_briefing"` with custom payloads
2. **Extend payload schema** — briefing payloads need `topics`, `depth` ("brief"/"deep"), and optionally `model_tier` override
3. **Briefing handler reads payload** — already partially done (`briefing_handler.go` reads `instruction` and `topics` from payload), but needs to support the `depth` → model tier mapping
4. **List/update/delete schedule tools** — revive from junkdrawer for managing existing schedules
5. **Driver prompt update** — teach the driver about scheduling briefings

### Key Decisions

- **One handler, variable payloads:** Don't create separate scheduler handlers for each briefing topic. Use one `worker_briefing` handler that reads its behavior from the payload. Multiple rows in `scheduler_tasks` with `kind=worker_briefing` but different payloads = different briefings.
- **The scheduler already supports this:** `scheduler_tasks` can have multiple rows with the same `kind` — each fires independently with its own cron and payload. The loader only upserts from `task.yaml`; user-created rows are untouched.
- **Model tier in payload:** Default to the `meta.yaml` tier (low for briefings), but allow the payload to override it for "deep dive" recurring schedules that want medium tier.

### Files to Reference

- `scheduler/` — the full scheduler system
- `scheduler/types.go` — Handler interface, Deps, TaskConfig
- `workeragent/briefing_handler.go` — existing handler that reads payload
- `_junkdrawer/tools/create_schedule/handler.go` — old create_schedule tool
- `_junkdrawer/tools/list_schedules/handler.go` — old list_schedules tool  
- `_junkdrawer/tools/update_schedule/handler.go` — old update_schedule tool
- `_junkdrawer/store_tasks.go` — old ScheduledTask CRUD methods
- `docs/spec/scheduler.md` — full scheduler spec with all task types documented
- `driver_agent_prompt.md` — needs scheduling flows added

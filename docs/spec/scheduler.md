# Scheduler (`scheduler/`)

**Mira's internal cron system.** A goroutine-based task runner that powers all of Mira's proactive behavior — reminders, morning briefings, mood check-ins, medication check-ins, proactive follow-ups, auto-journaling, and anything else that needs to happen on a schedule.

## Design Philosophy

The scheduler is a **dumb executor with a smart payload**. It doesn't know what a morning briefing is or how mood check-ins work. It knows how to:
1. Wake up every minute
2. Find tasks where `next_run <= now`
3. Execute them by type
4. Compute the next run time

All the intelligence lives in the task payloads and the agent pipeline. The most powerful task type is `run_prompt` — it sends a prompt through the full agent pipeline, which means any scheduled task can do anything the agent can do. Morning briefing? A scheduled `run_prompt` with "Generate a morning briefing with weather, tasks, and follow-ups." The scheduler doesn't need to understand briefings — the agent does.

## Three Types of Scheduled Work

**1. One-shot (`once`)** — fire at a specific time, then auto-disable.
```
"remind me to call the dentist at 3pm"
  → schedule_type: 'once'
  → trigger_at: '2026-03-22 15:00:00'
  → task_type: 'send_message'
  → payload: {"message": "Hey — you wanted to call the dentist!"}
  → max_runs: 1
```

**2. Recurring (`recurring`)** — fire on a cron schedule, indefinitely or N times.
```
"check in on my mood every evening at 9pm"
  → schedule_type: 'recurring'
  → cron_expr: '0 21 * * *'
  → task_type: 'mood_checkin'
  → payload: {"style": "gentle", "follow_up": true}
  → max_runs: NULL (forever)
```

**3. Conditional (`conditional`)** — fire on a cron schedule, but only execute if a condition is met. The condition is evaluated by the agent at runtime.
```
"follow up on important things from yesterday"
  → schedule_type: 'conditional'
  → cron_expr: '0 9 * * *'
  → task_type: 'run_prompt'
  → payload: {
      "prompt": "Scan facts from the last 48 hours with importance >= 7. If any warrant a follow-up, send a brief, warm check-in. If nothing stands out, do nothing.",
      "condition": "has_important_recent_facts"
    }
```

The difference between `recurring` and `conditional`: recurring always fires, conditional evaluates a check first and skips silently if the condition isn't met. This prevents Mira from sending empty "nothing to report" messages.

## Built-in Task Types

| Task Type | What It Does | Payload Fields |
|---|---|---|
| `send_message` | Send a plain text message to the user | `message` (string) |
| `run_prompt` | Run a prompt through the full agent pipeline — the agent can use all its tools (weather, Todoist, facts, search, etc.) and generates a natural response | `prompt` (string), `condition` (optional string) |
| `mood_checkin` | Send a mood check-in with Telegram inline keyboard | `style` ("gentle"/"direct"), `follow_up` (bool) |
| `medication_checkin` | Send a medication check-in message | `medications` (list), `time_of_day` ("morning"/"evening") |
| `run_extraction` | Trigger fact extraction on recent messages | `message_count` (int) |
| `run_journal` | Generate an auto-journal entry for the day | `style` ("narrative"/"bullet") |

**`run_prompt` is the escape hatch.** If a feature needs scheduled behavior that doesn't fit a built-in type, it can always be expressed as a `run_prompt`. The agent is the universal executor.

## The Runner

```go
// scheduler.go — simplified
func (s *Scheduler) Run(ctx context.Context) {
    ticker := time.NewTicker(1 * time.Minute)
    defer ticker.Stop()

    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            s.tick()
        }
    }
}

func (s *Scheduler) tick() {
    // 1. Query: SELECT * FROM scheduled_tasks WHERE enabled = 1 AND next_run <= NOW()
    // 2. For each task: execute by task_type
    // 3. Update: last_run = NOW(), run_count++, compute next_run
    // 4. If max_runs != NULL && run_count >= max_runs: set enabled = 0
}
```

Uses `github.com/robfig/cron/v3` for parsing cron expressions and computing `next_run`. The scheduler itself is just a `time.Ticker` — robfig/cron handles the expression parsing, not the scheduling loop. This keeps the runner dead simple and all state in SQLite (survives restarts).

**Timezone handling:** Cron expressions are evaluated in the user's local timezone (configured in `config.yaml`). robfig/cron supports `cron.WithLocation(loc)` for this. One-shot `trigger_at` timestamps are stored as UTC internally, displayed in local time.

**Startup recovery:** On boot, the scheduler scans for any tasks where `next_run` is in the past (missed while the process was down). One-shot tasks that were missed get executed immediately. Recurring tasks just compute their next future run — no backfill of missed executions.

## Agent Tools

The agent can create, list, and manage scheduled tasks through conversation. These are registered as tools in the agent's tool set.

**`create_reminder`** — Create a one-shot reminder.
```json
{
  "name": "create_reminder",
  "parameters": {
    "message": "Call the dentist",
    "trigger_at": "2026-03-22T15:00:00",
    "natural_time": "today at 3pm"
  }
}
```
The agent parses natural language times ("tomorrow morning", "in 2 hours", "next Tuesday at 3pm") and converts to an absolute timestamp. `natural_time` is stored for display purposes.

**`create_schedule`** — Create a recurring or conditional scheduled task.
```json
{
  "name": "create_schedule",
  "parameters": {
    "name": "morning briefing",
    "cron_expr": "0 8 * * *",
    "task_type": "run_prompt",
    "payload": {"prompt": "Generate a morning briefing..."},
    "description": "Every day at 8am"
  }
}
```

**`list_schedules`** — List active scheduled tasks.
```json
{
  "name": "list_schedules",
  "parameters": {
    "include_disabled": false
  }
}
```
Returns a formatted list: name, next run time, schedule description, run count.

**`update_schedule`** — Modify an existing scheduled task (change time, enable/disable, update payload).
```json
{
  "name": "update_schedule",
  "parameters": {
    "task_id": 3,
    "enabled": false
  }
}
```

**`delete_schedule`** — Remove a scheduled task entirely.
```json
{
  "name": "delete_schedule",
  "parameters": {
    "task_id": 3
  }
}
```

## User Commands

- `/remind <time> <message>` — Quick one-shot reminder. "Remind me at 3pm to call the dentist." Parsed by the agent, creates a `send_message` one-shot task.
- `/schedule` — List all active scheduled tasks with next run times.
- `/schedule pause <id>` — Disable a scheduled task without deleting it.
- `/schedule resume <id>` — Re-enable a paused task.
- `/schedule delete <id>` — Remove a scheduled task.

## System-Created Defaults

On first run (or when features are enabled in config), Mira creates default scheduled tasks:

| Task | Default Schedule | Task Type | Created When |
|---|---|---|---|
| Morning briefing | `0 8 * * *` (8am daily) | `run_prompt` | `scheduler.morning_briefing: true` |
| Mood check-in | `0 21 * * *` (9pm daily) | `mood_checkin` | `scheduler.mood_checkin: true` |
| Medication check-in | `0 21 * * *` (9pm daily) | `medication_checkin` | `scheduler.medication_checkin: true` |
| Proactive follow-ups | `0 9 * * *` (9am daily) | `run_prompt` (conditional) | `scheduler.proactive_followups: true` |
| Auto-journal | `0 22 * * *` (10pm daily) | `run_journal` | `scheduler.auto_journal: true` |
| Fact extraction | `@every 30m` | `run_extraction` | Always (core system) |

All defaults can be customized via `/schedule` commands or conversation ("change my morning briefing to 7am"). The user can also disable any default.

## Damping & Rate Limiting

To prevent Mira from being annoying:
- **Max proactive messages per day:** Configurable (default: 5). Scheduled tasks that would exceed this limit are silently skipped and rescheduled.
- **Quiet hours:** Configurable window (default: 11pm–7am) where no scheduled messages are sent. Tasks that fire during quiet hours are deferred to the end of the quiet period.
- **Conversation-aware:** If the user is actively chatting (message within the last 10 minutes), mood check-ins and other interruptive tasks are deferred by 30 minutes. Reminders always fire on time.
- **Backoff on no response:** If the user doesn't respond to 3 consecutive mood check-ins, Mira reduces frequency automatically and mentions it: "I noticed you've been skipping check-ins — I'll ease off. Just say 'resume check-ins' whenever."

## Milestone Phasing

The scheduler is built incrementally:

- **v0.2:** Basic one-shot reminders (`/remind`), `send_message` task type only. Simple ticker loop. The `scheduled_tasks` table is created with the full schema but only `once` + `send_message` is implemented.
- **v0.6:** Full cron system. Recurring jobs, conditional tasks, `run_prompt` task type, all agent tools, system defaults, damping/rate limiting, quiet hours. This is what powers morning briefings, mood check-ins, medication check-ins, proactive follow-ups.
- **v1.0:** Auto-journaling task type (`run_journal`). Job follow-up reminders created by the agent automatically.

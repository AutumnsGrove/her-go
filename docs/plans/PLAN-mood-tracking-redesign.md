# Plan: Mood Tracking Redesign + Scheduler Revival

Replaces the legacy 1–5 rating mood system (now in `_junkdrawer/`) with an Apple State-of-Mind-style tracker, and revives a lean, extension-based scheduler package to power it.

**Status:** design locked via interview. Ready for implementation.

**Branch:** `claude/mood-tracking-redesign-TJ9qV`

**Inspiration:**
- [Apple Support — Log your state of mind on iPhone](https://support.apple.com/guide/iphone/log-your-state-of-mind-iph6a6decb13/ios)
- [WWDC24 — Explore wellbeing APIs in HealthKit](https://developer.apple.com/videos/play/wwdc2024/10109/)
- [pi-mono coding-agent extensions](https://github.com/badlogic/pi-mono/blob/main/packages/coding-agent/docs/extensions.md) (extension pattern reference)

---

## Part 1 — Mood System

### Goals

- **Richer than a 1–5 rating.** Capture valence, labels (what it feels like), associations (what's driving it), and a free-text note.
- **Inferred + confirmed + manual**, with tiered confidence so the bot isn't aggressively guessing.
- **Agent-driven extraction** — a dedicated mood agent runs post-reply, in parallel with the memory agent.
- **Embedding-based dedup.** No more 30-minute hard gate. Moods are embedded and deduped via cosine similarity over a sliding window.
- **Telegram-first UX.** Inline keyboards for inference proposals and manual logging. PNG charts for review.

### Schema

**Table: `mood_entries`**

| Column | Type | Notes |
|---|---|---|
| `id` | INTEGER PRIMARY KEY | |
| `ts` | DATETIME | when the mood was logged |
| `kind` | TEXT | `momentary` \| `daily` |
| `valence` | INTEGER | 1–7 (very unpleasant → very pleasant) |
| `labels` | TEXT | JSON array of strings from vocab |
| `associations` | TEXT | JSON array of strings from vocab |
| `note` | TEXT | optional free-text context |
| `source` | TEXT | `inferred` \| `confirmed` \| `manual` |
| `confidence` | REAL | 0–1; only meaningful for inferred entries |
| `conversation_id` | INTEGER | FK when source=inferred |
| `embedding` | BLOB | cached note+labels embedding (float32) |
| `created_at` | DATETIME | |

**Virtual table: `vec_moods`** — mirrors `vec_facts`. Used for KNN semantic-similarity dedup on write.

**Table: `pending_mood_proposals`** — medium-confidence inference proposals the user hasn't tapped yet.

| Column | Type | Notes |
|---|---|---|
| `id` | INTEGER PRIMARY KEY | |
| `ts` | DATETIME | created |
| `telegram_message_id` | INTEGER | so we can edit in place on expiry |
| `proposal_json` | TEXT | the would-be `mood_entries` row |
| `status` | TEXT | `pending` \| `expired` \| `confirmed` \| `rejected` |
| `expires_at` | DATETIME | |

### Vocabulary — `mood/vocab.yaml`

Apple's full labels + associations lists. **Not hardcoded.** Loaded at startup, hot-reloadable.

```yaml
# mood/vocab.yaml
valence_buckets:
  1: { label: "Very Unpleasant", emoji: "😩", color: "#6B4AC4" }
  2: { label: "Unpleasant",      emoji: "😟", color: "#8A6CE8" }
  3: { label: "Slightly Unpleasant", emoji: "🙁", color: "#A98DF1" }
  4: { label: "Neutral",         emoji: "😐", color: "#B0B0B0" }
  5: { label: "Slightly Pleasant", emoji: "🙂", color: "#F5C26B" }
  6: { label: "Pleasant",        emoji: "😊", color: "#F6A945" }
  7: { label: "Very Pleasant",   emoji: "😄", color: "#F68B22" }

labels:
  unpleasant: [Angry, Anxious, Ashamed, Disappointed, Discouraged, Disgusted,
                Embarrassed, Frustrated, Guilty, Hopeless, Irritated, Jealous,
                Lonely, Overwhelmed, Sad, Scared, Stressed, Worried]
  neutral:    [Calm, Content, Indifferent, Relaxed, Satisfied]
  pleasant:   [Amazed, Amused, Brave, Confident, Excited, Grateful, Happy,
                Hopeful, Joyful, Passionate, Peaceful, Proud, Relieved, Surprised]

associations:
  - Health
  - Fitness
  - Self-care
  - Hobbies
  - Identity
  - Spirituality
  - Community
  - Family
  - Friends
  - Partner
  - Dating
  - Tasks
  - Work
  - Education
  - Travel
  - Weather
  - Current Events
  - Money
```

Loader in `mood/vocab.go` parses YAML → in-memory structs. Filesystem watcher re-reads on change. If YAML fails to parse at startup, bot refuses to boot (fail-loud). Hot-reload failures log and keep the last-good vocab.

### Mood Agent — `mood/agent.go`

Dedicated agent running in its own goroutine, triggered after each bot reply is delivered (parallel to the memory agent).

| Setting | Value |
|---|---|
| Model | `moonshotai/kimi-k2-0905` (same as memory agent) |
| Provider | Groq via OpenRouter |
| Prompt file | `mood_agent_prompt.md` (hot-reloadable) |
| Input context | last `mood.context_turns` turns of active conversation (default 5), **scrubbed** (PII tokenized) |
| Output | structured JSON: `{ valence, labels[], associations[], note, confidence, signals[] }` or `{ "skip": true, "reason": "..." }` |

**Hybrid confidence:**
```go
llmConfidence := output.Confidence
signalScore   := scoreExplicitSignals(turns)  // counts affect words, emoji, intensity language
finalConfidence := max(llmConfidence, signalScore)
```

**Classifier pass** — small focused LLM check (Haiku 4.5 via OpenRouter) before write. Asks one question: "Is this a real first-person mood observation by the user, or is it fictional / hypothetical / someone else's mood?" Fail-open (if classifier down, write proceeds). Mirrors the existing memory classifier pattern.

### Inference Tiers

| Confidence | Behavior |
|---|---|
| ≥ 0.75 (high) | Auto-log as `source=inferred`. Optionally mention in next reply. |
| 0.40 – 0.75 (medium) | Telegram message with inline keyboard: **✅ Log it** / **✏️ Edit** / **❌ No** |
| < 0.40 (low) | Drop silently |

Thresholds configurable via `config.yaml`.

### Dedup — embedding-based, no hard time gate

Mirrors the fact dedup pattern (`store_facts.go` + `vec_facts`).

1. On every write (inferred or manual), compute embedding of `note + " " + strings.Join(labels, " ")`.
2. Insert into `mood_entries` + `vec_moods` in a single transaction.
3. **Before** an inferred write, run KNN over `vec_moods` filtered to entries within the last `mood.dedup_window_minutes` (default 120). If top match has cosine ≥ `mood.dedup_similarity_threshold` (default 0.80), skip the write and optionally bump the existing entry's timestamp (decide during impl — probably skip-only is cleaner).
4. Manual entries (`/mood` command) **bypass dedup** entirely. User intent wins.

### Stale-proposal Handling

A medium-confidence proposal left untapped for `mood.proposal_expiry_minutes` (default 30):

1. Background sweeper (runs every 5 min, small goroutine in `mood/sweeper.go`) finds `pending_mood_proposals` rows where `expires_at < now AND status = 'pending'`.
2. Edits the original Telegram message in place (via stored `telegram_message_id`): buttons replaced with grey text "⏳ expired — tap /mood recent to revisit".
3. Marks row as `status=expired`. Row stays in DB.

`/mood recent` command lists the last 5 expired proposals, each with re-issue buttons (Log / Edit / Drop). Logging from here writes `source=confirmed` (you did confirm it, just late).

### Daily Rollup

Scheduled extension (see Part 2). Fires at `mood.daily_rollup_cron` (default `0 21 * * *` = 21:00 daily).

Algorithmic draft from the day's `momentary` entries:
- **Valence:** mean, rounded to nearest bucket (1–7)
- **Labels:** top 3 by frequency
- **Association:** single most-frequent
- **Note:** auto-generated sentence ("Mostly pleasant, with moments of frustration around work.") — simple template, not an LLM call

Bot sends Telegram message with the draft pre-filled as inline buttons (valence row highlighted, labels shown as pressable chips toggled on, association pressable). User taps to adjust. Final tap of **✅ Save** writes `kind=daily, source=confirmed`.

If unresponded by 08:00 next morning: auto-log the draft unchanged as `kind=daily, source=inferred`.

### Manual `/mood` Wizard

Multi-step Telegram flow, each step edits the same message (Telegram `editMessageText` API) for clean UX.

| Step | UI |
|---|---|
| 1. Valence | 7 colored buttons, one per bucket, two rows |
| 2. Labels | Chips filtered by valence bucket (Apple-style). Multi-select. "Done" button. |
| 3. Associations | Association chips (all visible). Multi-select. "Skip" / "Done". |
| 4. Note | "Add context?" — text reply parsed by bot, or tap "Skip" |

Final confirmation → write `source=manual, kind=momentary`.

State for the in-flight wizard kept in a small in-memory map keyed by `chat_id`, with a 10-minute timeout. On timeout, message is edited to "⏳ mood entry cancelled".

### Prompt Layer — `mood/layer_chat.go`

Revives and simplifies old `chat_mood.go`. Slot `500` (same as old).

- Injects only when ≥ 2 entries exist
- Shows last 5 entries with valence emoji + label summary
- Weighting in trend prose: manual/confirmed > inferred
- Max ~400 tokens

### Graphs — `/mood week|month|year`

Server-side PNG generation via `github.com/wcharczuk/go-chart/v2`. Chart types:

- **Valence over time:** line chart, y-axis 1–7, x-axis timestamps, color band per valence bucket
- **Label frequency:** horizontal bar chart, top 10 labels
- **Association heat strip:** bottom strip showing frequency of each association

All three on one PNG, vertically stacked. Send as Telegram `sendPhoto`.

### Config additions — `config.yaml`

```yaml
mood:
  context_turns: 5
  confidence:
    high: 0.75
    low: 0.40
  dedup_window_minutes: 120
  dedup_similarity_threshold: 0.80
  proposal_expiry_minutes: 30
  daily_rollup_cron: "0 21 * * *"
  prompt_layer_order: 500
  classifier_model: "anthropic/claude-haiku-4.5"
```

### Deferred (explicitly out of scope for this branch)

- TUI mood widget beyond a "last entry + mini sparkline" placeholder
- Weekly / monthly digest notifications
- Manual edit-past-entry flow (e.g. `/mood edit <id>`)
- Correlation features (mood × weather, mood × sleep, mood × people mentioned)

---

## Part 2 — Scheduler Revival

Old scheduler (`_junkdrawer/scheduler/`, ~964 LOC) is being replaced with a focused extension-based system. We revive only what mood needs, structured so adding extensions later is trivial.

### Design Principles

- **Dumb executor, smart extensions.** Scheduler knows how to fire tasks at the right time. It doesn't know what a "mood rollup" is.
- **Extensions are domain-owned.** Mood's scheduler bits live in `mood/`, not `scheduler/extensions/mood/`. A future reminder tool lives entirely in `reminder/`.
- **Self-register at init() time.** Same pattern as `tools/`.
- **Per-extension YAML config** (`task.yaml`) alongside handler code. Hot-reloadable.
- **Per-extension retry policy** — no built-in backoff at scheduler level, but declared by each extension and executed by the runner.

### Core Package — `scheduler/`

```
scheduler/
├── registry.go   # Register(Handler) + lookup
├── runner.go     # Ticker loop, dispatch, retry
├── store.go      # scheduled_tasks table CRUD
├── types.go      # Handler interface, Task struct, Deps
├── loader.go     # Walks extension task.yaml files at startup
└── scheduler.go  # package doc + constructor
```

**~250 LOC target.**

### Handler Interface

```go
// Handler is implemented by each scheduler extension. It declares a task
// kind and knows how to execute a payload for that kind.
type Handler interface {
    // Kind is the stable string identifier for this task type (e.g.
    // "mood_daily_rollup"). Used for registry lookup and in the
    // scheduled_tasks.kind column.
    Kind() string

    // Execute runs the scheduled task. Payload is the raw JSON from the
    // scheduled_tasks row — the handler unmarshals into its own struct.
    // Deps carries the shared dependencies (store, telegram sender, LLM
    // client) so handlers don't need their own singletons.
    Execute(ctx context.Context, payload json.RawMessage, deps *Deps) error
}
```

### Deps Bundle

```go
// Deps is passed to every Handler.Execute call. Add new fields as extensions
// need them; extensions only use the fields they care about.
type Deps struct {
    Store    *memory.Store
    Send     func(chatID int64, text string) (int, error) // Telegram text send
    SendPNG  func(chatID int64, png []byte, caption string) error
    LLM      *llm.Client
    Logger   *log.Logger
    ChatID   int64  // primary user chat id; extensions that need it read here
}
```

### Task Model

```go
type Task struct {
    ID           int64
    Kind         string
    CronExpr     string        // empty for one-shot
    NextFire     time.Time
    PayloadJSON  json.RawMessage
    RetryConfig  RetryConfig
    LastRunAt    *time.Time
    LastError    string
    AttemptCount int
    CreatedAt    time.Time
}

type RetryConfig struct {
    MaxAttempts int           // 0 = no retry
    Backoff     BackoffPolicy // "none" | "linear" | "exponential"
    InitialWait time.Duration
}
```

### Storage — `scheduler_tasks` table

Single polymorphic table. Scheduler doesn't care about the payload shape.

> **Naming note:** the new table is `scheduler_tasks`, not `scheduled_tasks`, because the old v0.2 skeleton (`memory/store_tasks.go` + the `/schedule` and `/remind` commands in `bot/`) still references the legacy `scheduled_tasks` table with a different schema. The runner that would have executed those tasks is in `_junkdrawer/`, so nothing actually fires — it's zombie code. Rather than migrate the zombie callers, we coexist with a new table and delete the old one (and its callers) when the reminder tool is rebuilt as a scheduler extension.

```sql
CREATE TABLE IF NOT EXISTS scheduler_tasks (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    kind             TEXT NOT NULL,
    cron_expr        TEXT,                 -- NULL for one-shot
    next_fire        DATETIME NOT NULL,
    payload_json     TEXT NOT NULL,
    retry_config_json TEXT NOT NULL,
    last_run_at      DATETIME,
    last_error       TEXT,
    attempt_count    INTEGER DEFAULT 0,
    created_at       DATETIME DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_scheduled_tasks_next_fire ON scheduled_tasks(next_fire);
```

### Runner

30-second ticker. On each tick:

1. Fetch tasks where `next_fire <= now()`.
2. For each task: look up handler by kind, call `handler.Execute(ctx, payload, deps)`.
3. On success: compute next fire (cron → next occurrence; one-shot → delete task) and update row.
4. On error: apply retry config. If attempts exhausted, log + either drop (one-shot) or skip to next cron tick (recurring).

Dependency: `github.com/robfig/cron/v3` for cron expression parsing.

### Extension Registration

Each domain package owns its extension and registers in `init()`:

```go
// mood/rollup_task.go
package mood

import (
    "context"
    "encoding/json"
    "her/scheduler"
)

type dailyRollupHandler struct{}

func (h *dailyRollupHandler) Kind() string { return "mood_daily_rollup" }

func (h *dailyRollupHandler) Execute(ctx context.Context, payload json.RawMessage, deps *scheduler.Deps) error {
    // build algorithmic draft, send Telegram message with inline buttons
    // ...
}

func init() {
    scheduler.Register(&dailyRollupHandler{})
}
```

### Extension Config — `task.yaml`

Alongside each extension's handler file. Loaded at startup by `scheduler/loader.go` (walks known extension dirs OR reads from a manifest — TBD at impl time, probably manifest for simplicity).

```yaml
# mood/task.yaml
kind: mood_daily_rollup
cron: "0 21 * * *"        # 9pm daily, user-local TZ
payload: {}               # static payload; the handler computes dynamic data at run time
retry:
  max_attempts: 2
  backoff: exponential
  initial_wait: 60s
```

Loader on startup:
1. Reads every `task.yaml` from registered extension dirs.
2. For each, upserts into `scheduled_tasks`:
   - If a row with this `kind` exists and `cron_expr` differs → update.
   - If no row exists → insert with `next_fire` = next cron occurrence.
3. Existing one-shot tasks with `kind` no longer registered → leave them (they'll fail gracefully and be dropped after retries).

### Scope for this branch

Scheduler core + `mood_daily_rollup` extension only.

**Not revived this pass:** reminder tool, `create_reminder` / `list_schedules` tools, `/remind` command, weekly/monthly digest, persona reflection cadence hook. All possible as future extensions using the same pattern.

---

## Implementation Order

1. **Scheduler core.** Package skeleton, `scheduled_tasks` migration, `Register()`, `Runner`, cron integration. Smoke-test with a dummy kind.
2. **Mood schema + vocab.** Migrations for `mood_entries`, `vec_moods`, `pending_mood_proposals`. `mood/vocab.yaml` + loader.
3. **Mood store methods.** `SaveMoodEntry`, `RecentMoods`, `SimilarMoods` (KNN), `pending_mood_proposals` CRUD.
4. **Mood agent.** Prompt file, call loop, hybrid confidence scorer, classifier pass.
5. **Wire into reply pipeline.** Launch mood agent in its own goroutine post-reply, alongside memory agent.
6. **Proposal inline keyboard + sweeper.** Telegram message with buttons, confirm / edit / reject handlers, expiry sweeper.
7. **Manual `/mood` wizard.** 4-step flow, state map, edit-in-place.
8. **Daily rollup extension.** `rollup_task.go`, `task.yaml`, algorithmic draft + Telegram flow.
9. **Prompt layer.** `mood/layer_chat.go`.
10. **Graphs.** `/mood week|month|year`, PNG generation, Telegram photo send.

Each step ends with: builds green, linter happy, manual Telegram smoke test if user-facing. Commit per step with a descriptive message.

---

## New Dependencies

- `github.com/robfig/cron/v3` — cron expression parsing
- `github.com/wcharczuk/go-chart/v2` — PNG chart generation (decision deferred; may use `gonum.org/v1/plot` if go-chart limits styling)

## Files Retired

- `_junkdrawer/scheduler/` → replaced by new `scheduler/`
- Any remaining references to the old mood rating (1–5) in active code → removed as we touch them

## Known tech debt (deferred cleanup)

- Legacy `scheduled_tasks` table + `memory/store_tasks.go` methods (`CreateScheduledTask`, `GetDueTasks`, `ListActiveTasks`, `MarkTaskRun`, etc.)
- Legacy `/schedule` command and `/remind` callback handlers in `bot/`
- All live, all call the old API, but no runner executes them. Clean up when the reminder tool is re-implemented as a scheduler extension.

## Notes for the Implementer

- Vocabulary is YAML-driven. **Never** hardcode a label list in Go.
- Dedup is embedding-based. **Never** add a time-window hard gate without user sign-off.
- Extensions are domain-owned. Putting mood task code under `scheduler/extensions/` is a code smell in this design.
- Scrubbed text only in the mood agent. PII firewall is non-negotiable.
- Prefer editing existing packages (migrations, config loader, prompt layer wiring) to creating parallel ones.

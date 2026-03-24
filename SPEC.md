# her-go — Personal Companion Bot

## Overview

A privacy-first personal companion chatbot built in Go. Communicates via Telegram, powered by LLMs through OpenRouter, with local SQLite storage for conversations, memory, and metrics. Inspired by "Her" — a persistent, warm presence that remembers your life and helps you keep track of things.

**Single user. Single binary. Everything local.**

---

## Core Principles

1. **Privacy first** — Hard identifiers (SSNs, card numbers, etc.) never leave the host machine. Names and context pass through for conversational coherence.
2. **Own your data** — Everything lives in a local SQLite database. No cloud dependencies for storage.
3. **Model agnostic** — Swap models by changing a config value. System prompt lives in a plain `.md` file.
4. **Keep it simple** — One binary, one database, one config file. No Docker, no Kubernetes, no microservices.
5. **Learn by building** — Custom memory system, custom PII scrubbing. Understand every piece.

---

## Architecture

```
┌─────────────┐         ┌───────────────────────────────────────────────────────────┐
│  Telegram   │◀───────▶│                    her-go binary                          │
│  (user)     │         │                                                           │
└─────────────┘         │  ┌──────────┐  ┌───────────┐  ┌──────────────┐            │
       ▲                │  │ Telegram │  │ Scheduler │  │  Mood        │            │
       │                │  │ Handler  │  │ (remind)  │  │  Check-ins   │            │
       │                │  └────┬─────┘  └─────┬─────┘  └──────┬───────┘            │
       │                │       │              │               │                    │
       │                │       ▼              │               │                    │
       │                │  ┌──────────┐◀───────┴───────────────┘                    │
       │                │  │  Agent   │                                             │
       │                │  │ Pipeline │                                             │
       │                │  │          │                                             │
       │                │  │ 1. Log + scrub                                         │
       │                │  │ 2. Agent orchestration                                 │
       │                │  │ 3. Tool calls (search, memory, links, daily tools)     │
       │                │  │ 4. Mini Shutter (URL fetch + content distillation)      │
       │                │  │ 5. Reply generation                                    │
       │                │  │ 6. Persona evolution                                   │
       │                │  └────┬─────┘                                             │
       │                │       │                                                   │
       │                │       ▼                                                   │
       │                │  ┌──────────┐  ┌─────────────┐  ┌───────────────────────┐ │
       │                │  │ SQLite   │  │  OpenRouter  │  │  External APIs        │ │
       │                │  │ (local)  │  │  (LLM)      │  │  Todoist, GitHub,     │ │
       │                │  └────┬─────┘  └─────────────┘  │  Weather, Transit,    │ │
       │                │       │                         │  IMAP, HealthKit      │ │
       │                │       │                         └───────────────────────┘ │
       │                │       ▼                         ▲                         │
       │                │  ┌──────────┐  ┌─────────────┐  │  ┌──────────────────┐   │
       │                │  │ D1 Sync  │  │  Obsidian   │  │  │ Kiwix (local     │   │
       │                │  │ (v0.7)   │  │  (local fs) │  │  │  Wikipedia)      │   │
       │                │  └──────────┘  └─────────────┘  │  └──────────────────┘   │
       │                │                                 │                         │
       │                │  ┌──────────────────────────────┘                         │
       │                │  │                                                        │
┌──────┴──────┐         │  ┌──────────────────────────────────────┐                 │
│ Mini Apps   │◀────────│──│  Web App Server (v0.8+)              │                 │
│ (WebView)   │         │  │  Links browser, reader, highlights,  │                 │
│             │         │  │  grocery list, expenses, job tracker  │                 │
└─────────────┘         │  └──────────────────────────────────────┘                 │
                        └───────────────────────────────────────────────────────────┘
```

### Message Flow

1. User sends a text message on Telegram
2. Bot receives it via long-polling (dev) or webhook (prod)
3. Raw message is logged to SQLite (full fidelity, never scrubbed)
4. PII scrubber strips hard identifiers + replaces contact info with reversible tokens
5. Memory system retrieves relevant context (recent messages, extracted facts)
6. System prompt (`prompt.md`) + memory context + scrubbed message → assembled into LLM request
7. Bot sends "typing..." indicator to Telegram
8. Request sent to OpenRouter (hard identifiers removed, contact info tokenized, names/context intact)
9. Response received, logged to SQLite (both raw response + token counts + cost)
10. Response sent back to user on Telegram
11. Periodically: fact extraction runs against raw messages to build long-term memory

**Data retention:** Every stage is preserved. The `messages` table stores both `content_raw` (what you actually said) and `content_scrubbed` (what the LLM saw). Nothing is ever deleted — scrubbing creates a parallel sanitized copy, it does not replace the original. The `pii_vault` table maintains session-scoped mappings for Tier 2 tokens so responses can be deanonymized before display.

---

## Components

### 1. Telegram Bot (`bot/`)

- Uses `telebot v4` (`gopkg.in/telebot.v4`) or `go-telegram-bot-api/v5`
- Long-polling for development (no infra needed)
- Webhook mode for production (behind Cloudflare Tunnel)
- Handles: text messages, photos, commands (`/remind`, `/forget`, `/stats`)
- Photo handling: when a photo is received, downloads a mid-size version (~1024px, second-largest from Telegram's `PhotoSize` array) and passes it to the agent for vision processing
- Sends `sendChatAction("typing")` while waiting for LLM response (re-sent every 4s to keep indicator alive)
- Future: live-edit streaming — send a placeholder message, then `editMessageText` as tokens arrive for a real-time typing effect
- Future: voice messages (Ogg → Parakeet local STT → text)

### 2. LLM Client (`llm/`)

- Thin wrapper around OpenAI-compatible chat completions API
- Configurable: `base_url`, `api_key`, `model`, `temperature`, `max_tokens`
- Default: OpenRouter (`https://openrouter.ai/api/v1`)
- Sends `HTTP-Referer` and `X-Title` headers for OpenRouter attribution
- Supports streaming (optional, nice for long responses)
- Returns structured response with token usage for metrics
- Supports multi-modal messages (text + image content parts) for vision calls

**Multi-model architecture:** The bot uses multiple models, each optimized for its role:

| Model | Role | Why |
|---|---|---|
| Chat LLM (Deepseek V3.2) | Conversational responses | Strong at natural language, cheap |
| Agent LLM (configurable) | Tool-calling orchestration | Fast, good at structured output |
| Vision LLM (Gemini 3 Flash) | Image understanding | Fast VLM, good casual descriptions |
| OCR — primary (Apple Vision) | Text extraction from photos | Sub-200ms, Neural Engine, zero deps |
| OCR — fallback (GLM-OCR 0.9B) | Text extraction when primary fails | Purpose-built OCR model, #1 OmniDocBench |

The vision model is called via the `view_image` agent tool. When the user sends a photo, the agent decides whether/how to use it and calls the VLM with an appropriate prompt. The VLM's description becomes part of the agent's context, which it can reference when generating the reply via the chat LLM.

**Vision flow:**
```
User sends photo on Telegram
  → bot downloads mid-size version (~1024px)
  → image passed to agent as base64
  → agent calls view_image tool
  → VLM (Gemini 3 Flash via OpenRouter) describes the image
  → description returned as tool result
  → agent uses description in reply context
```

### 3. PII Scrubber (`scrub/`)

**Philosophy:** Tiered scrubbing by risk category. Names and context pass through for conversational coherence — hard identifiers are stripped, contact info is tokenized and reversed on response.

#### Tier 1 — Hard Redact (irreversible, never sent to LLM)

Regex-based detection. Matched content is replaced with `[REDACTED]` in the scrubbed copy. No deanonymization needed — these never appear in responses.

- Social Security Numbers (XXX-XX-XXXX patterns)
- Credit/debit card numbers (Luhn-validated)
- Bank account / routing numbers
- Passwords, API keys, secrets (patterns like `sk-`, `Bearer`, etc.)
- Government ID numbers (passport, driver's license patterns)

#### Tier 2 — Tokenize + Deanonymize (reversible, vault-based)

Regex-based detection. Matched content is replaced with typed, numbered placeholders. A session-scoped vault maps tokens back to originals. After the LLM responds, tokens in the response are replaced with originals before displaying to the user.

- Phone numbers → `[PHONE_1]`, `[PHONE_2]`, etc.
- Email addresses → `[EMAIL_1]`, `[EMAIL_2]`, etc.
- Full street addresses → `[ADDRESS_1]`, etc.
- IP addresses → `[IP_1]`, etc.

**Why typed placeholders, not fake names:** Collision-safe. The LLM will never independently generate `[PHONE_1]` in a different context, so deanonymization is always correct. Realistic substitutes ("555-0123") risk the LLM echoing them in unrelated contexts.

**Deanonymize flow:**
```
You type:     "Call me at 503-555-1234"
LLM sees:     "Call me at [PHONE_1]"
LLM replies:  "Got it, I'll remember [PHONE_1]."
You see:      "Got it, I'll remember 503-555-1234."
```

#### Tier 3 — Pass Through (no action)

These are left intact because they're essential for conversational coherence and empathy. Scrubbing them would destroy the LLM's ability to reason about your life.

- First names, nicknames, relational terms ("Mom", "my boss")
- Cities, neighborhoods, general locations
- Workplace names, school names
- Emotional context, relationship dynamics
- Dates, times, events

**Privacy rationale:** First names and city-level locations are not uniquely identifying on their own. The dangerous PII is structured identifiers (Tier 1) that map directly to a person. "Sarah in Portland had an argument with her mom" is not actionable. "Sarah at 123 Main St, SSN 456-78-9012" is.

#### Vault

- In-memory map per conversation turn, keyed by placeholder token
- Also persisted to SQLite `pii_vault` table for audit trail
- Vault entries are never sent to the LLM — only used locally for deanonymization

### 4. Memory System (`memory/`)

**Custom-built, SQLite-backed, designed for eventual migration to CF D1/Vectorize.**

#### Storage

All in one SQLite database (`her.db`):

```sql
-- Raw conversation log
CREATE TABLE messages (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
    role TEXT NOT NULL,          -- 'user' or 'assistant'
    content_raw TEXT NOT NULL,   -- original unscrubbed message
    content_scrubbed TEXT,       -- PII-scrubbed version sent to LLM
    conversation_id TEXT,       -- groups messages into conversations/sessions
    token_count INTEGER,
    voice_memo_path TEXT        -- path to original audio file, if applicable
);

-- Extracted facts (long-term memory)
CREATE TABLE facts (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
    fact TEXT NOT NULL,           -- "Had an argument with parents about moving"
    category TEXT,               -- 'relationship', 'health', 'work', 'mood', etc.
    source_message_id INTEGER,   -- which message(s) this was extracted from
    importance INTEGER DEFAULT 5,-- 1-10, influences retrieval priority
    active BOOLEAN DEFAULT 1,    -- can be "forgotten" without deletion
    FOREIGN KEY (source_message_id) REFERENCES messages(id)
);

-- Conversation summaries
CREATE TABLE summaries (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
    conversation_id TEXT,
    summary TEXT NOT NULL,
    messages_start_id INTEGER,
    messages_end_id INTEGER
);

-- PII vault (Tier 2 reversible tokens only)
CREATE TABLE pii_vault (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    message_id INTEGER,          -- which message this token appeared in
    token TEXT NOT NULL,          -- '[PHONE_1]', '[EMAIL_2]', etc.
    original_value TEXT NOT NULL, -- the real value
    entity_type TEXT NOT NULL,    -- 'phone', 'email', 'address', 'ip'
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (message_id) REFERENCES messages(id)
);

-- Scheduled tasks (one-shot reminders, recurring cron jobs, conditional checks)
CREATE TABLE scheduled_tasks (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT,                        -- human-readable label ("morning briefing", "remind: call dentist")
    schedule_type TEXT NOT NULL,      -- 'once', 'recurring', 'conditional'
    cron_expr TEXT,                   -- cron expression for recurring (e.g. "0 8 * * *")
    trigger_at DATETIME,             -- for one-shot tasks: when to fire
    task_type TEXT NOT NULL,          -- 'send_message', 'run_prompt', 'mood_checkin', 'medication_checkin'
    payload JSON NOT NULL,           -- task-type-specific config (message text, prompt, checkin config, etc.)
    enabled BOOLEAN DEFAULT 1,
    last_run DATETIME,
    next_run DATETIME,               -- precomputed next execution time (indexed for fast polling)
    run_count INTEGER DEFAULT 0,
    max_runs INTEGER,                -- NULL = unlimited, 1 = one-shot (auto-disable after)
    created_by TEXT DEFAULT 'user',  -- 'user', 'system', 'agent'
    source_message_id INTEGER,       -- conversation that created this task
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (source_message_id) REFERENCES messages(id)
);

-- Index for the scheduler polling loop
CREATE INDEX idx_scheduled_tasks_next_run ON scheduled_tasks(next_run)
    WHERE enabled = 1;

-- Persona version history
CREATE TABLE persona_versions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
    content TEXT NOT NULL,         -- full persona.md content at this point in time
    trigger TEXT,                  -- 'conversation_count', 'manual', 'initial'
    conversation_count INTEGER,   -- how many conversations had occurred at this point
    reflection_ids TEXT            -- comma-separated reflection fact IDs that informed this version
);

-- Trait scores over time
CREATE TABLE traits (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
    trait_name TEXT NOT NULL,      -- 'warmth', 'humor_style', 'directness', etc.
    value TEXT NOT NULL,           -- numeric ("0.8") or categorical ("dry")
    persona_version_id INTEGER,   -- which persona rewrite produced this score
    FOREIGN KEY (persona_version_id) REFERENCES persona_versions(id)
);

-- Metrics / usage tracking
CREATE TABLE metrics (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
    model TEXT NOT NULL,
    prompt_tokens INTEGER,
    completion_tokens INTEGER,
    total_tokens INTEGER,
    cost_usd REAL,               -- calculated from model pricing
    latency_ms INTEGER,
    message_id INTEGER,
    FOREIGN KEY (message_id) REFERENCES messages(id)
);
```

#### Retrieval Strategy (MVP)

1. **Recent messages**: Last N messages from the current conversation (sliding window)
2. **Relevant facts**: Pull top-K facts by recency and importance
3. **Today's summary**: If a summary exists for today, include it

#### Retrieval Strategy (Future — v0.3+)

1. Everything above, plus:
2. **Semantic search**: Embed the current message, find top-5 most similar past facts/messages via cosine similarity
3. Uses local embedding model (already familiar with this)
4. Embeddings stored in SQLite via `sqlite-vec` extension, or a separate vector column

#### Fact Extraction

- Triggered periodically (every N messages, or on conversation end / long pause)
- Sends recent raw messages to the LLM with an extraction prompt:
  ```
  Extract key facts, events, emotions, and decisions from this conversation.
  Format each as a single sentence. Categorize as: relationship, health, work,
  mood, goal, event, preference, other.
  Rate importance 1-10.
  ```
- Extracted facts are stored in the `facts` table
- This extraction call also goes through the same tiered scrubbing pipeline (the stored fact in the DB is the raw version with full fidelity)

### 5. Prompt System (Layered)

The system prompt is assembled from multiple layers at each LLM call:

```
┌─────────────────────────────────────────┐
│  1. prompt.md (base template)           │  ← You write this, rarely changes
│     Identity, boundaries, guardrails    │
├─────────────────────────────────────────┤
│  2. persona.md (evolving self-image)    │  ← The bot rewrites this over time
│     Current personality, learned style  │
├─────────────────────────────────────────┤
│  3. Relevant reflections                │  ← "I've noticed that..."
├─────────────────────────────────────────┤
│  4. Relevant facts/memories             │  ← "User argued with parents on 3/20"
├─────────────────────────────────────────┤
│  5. Recent conversation history         │  ← Last N messages
├─────────────────────────────────────────┤
│  6. Current user message                │
└─────────────────────────────────────────┘
```

#### `prompt.md` — Base Template (static, user-authored)

- Loaded at startup, hot-reloadable without restart
- Defines the assistant's core identity, communication guardrails, and boundaries
- The bot can never override or contradict this — it's the constitution
- Structure:
  ```markdown
  # Identity
  [Core identity, name, foundational personality]

  # Boundaries
  [What you won't do, safety rails, relationship limits]

  # Memory Awareness
  [How to reference and use provided memory context]
  ```

#### `persona.md` — Evolving Self-Image (bot-authored)

- Starts as a seed description (written by you or generated from first few conversations)
- **Rewritten by the bot itself** during persona evolution cycles (see Section 8)
- Describes current personality, communication style, humor style, learned preferences
- Versioned in SQLite — every rewrite is preserved, rollback is possible
- Structure:
  ```markdown
  # Who I Am Right Now
  [Current self-description, evolved through reflection]

  # Communication Style
  [How I speak, my humor, my tendencies]

  # What I've Learned About Us
  [Patterns in our relationship, what works, what doesn't]
  ```

### 6. Scheduler (`scheduler/`)

**Mira's internal cron system.** A goroutine-based task runner that powers all of Mira's proactive behavior — reminders, morning briefings, mood check-ins, medication check-ins, proactive follow-ups, auto-journaling, and anything else that needs to happen on a schedule.

#### Design Philosophy

The scheduler is a **dumb executor with a smart payload**. It doesn't know what a morning briefing is or how mood check-ins work. It knows how to:
1. Wake up every minute
2. Find tasks where `next_run <= now`
3. Execute them by type
4. Compute the next run time

All the intelligence lives in the task payloads and the agent pipeline. The most powerful task type is `run_prompt` — it sends a prompt through the full agent pipeline, which means any scheduled task can do anything the agent can do. Morning briefing? A scheduled `run_prompt` with "Generate a morning briefing with weather, tasks, and follow-ups." The scheduler doesn't need to understand briefings — the agent does.

#### Three Types of Scheduled Work

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

#### Built-in Task Types

| Task Type | What It Does | Payload Fields |
|---|---|---|
| `send_message` | Send a plain text message to the user | `message` (string) |
| `run_prompt` | Run a prompt through the full agent pipeline — the agent can use all its tools (weather, Todoist, facts, search, etc.) and generates a natural response | `prompt` (string), `condition` (optional string) |
| `mood_checkin` | Send a mood check-in with Telegram inline keyboard | `style` ("gentle"/"direct"), `follow_up` (bool) |
| `medication_checkin` | Send a medication check-in message | `medications` (list), `time_of_day` ("morning"/"evening") |
| `run_extraction` | Trigger fact extraction on recent messages | `message_count` (int) |
| `run_journal` | Generate an auto-journal entry for the day | `style` ("narrative"/"bullet") |

**`run_prompt` is the escape hatch.** If a feature needs scheduled behavior that doesn't fit a built-in type, it can always be expressed as a `run_prompt`. The agent is the universal executor.

#### The Runner

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

#### Agent Tools

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

#### User Commands

- `/remind <time> <message>` — Quick one-shot reminder. "Remind me at 3pm to call the dentist." Parsed by the agent, creates a `send_message` one-shot task.
- `/schedule` — List all active scheduled tasks with next run times.
- `/schedule pause <id>` — Disable a scheduled task without deleting it.
- `/schedule resume <id>` — Re-enable a paused task.
- `/schedule delete <id>` — Remove a scheduled task.

#### System-Created Defaults

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

#### Damping & Rate Limiting

To prevent Mira from being annoying:
- **Max proactive messages per day:** Configurable (default: 5). Scheduled tasks that would exceed this limit are silently skipped and rescheduled.
- **Quiet hours:** Configurable window (default: 11pm–7am) where no scheduled messages are sent. Tasks that fire during quiet hours are deferred to the end of the quiet period.
- **Conversation-aware:** If the user is actively chatting (message within the last 10 minutes), mood check-ins and other interruptive tasks are deferred by 30 minutes. Reminders always fire on time.
- **Backoff on no response:** If the user doesn't respond to 3 consecutive mood check-ins, Mira reduces frequency automatically and mentions it: "I noticed you've been skipping check-ins — I'll ease off. Just say 'resume check-ins' whenever."

#### Milestone Phasing

The scheduler is built incrementally:

- **v0.2:** Basic one-shot reminders (`/remind`), `send_message` task type only. Simple ticker loop. The `scheduled_tasks` table is created with the full schema but only `once` + `send_message` is implemented.
- **v0.6:** Full cron system. Recurring jobs, conditional tasks, `run_prompt` task type, all agent tools, system defaults, damping/rate limiting, quiet hours. This is what powers morning briefings, mood check-ins, medication check-ins, proactive follow-ups.
- **v1.0:** Auto-journaling task type (`run_journal`). Job follow-up reminders created by the agent automatically.

### 7. Configuration (`config.yaml`)

(see below)

### 8. Persona Evolution System (`persona/`)

**The bot's personality changes over time based on accumulated interactions.** This is driven by two mechanisms: reflections (frequent, lightweight) and persona rewrites (infrequent, substantive).

#### Reflection (Trigger B — memory-density spike)

When a conversation produces a high density of extracted memories (configurable threshold, default: 8+ facts from one conversation), a reflection is triggered.

The bot is given the recent conversation + its current persona and asked to write a brief internal reflection — *not* a persona rewrite, just a journal-like entry about what it learned or felt.

```
Reflection prompt:
"You just had a meaningful conversation. Here's what happened:
{recent messages}

Write a brief internal reflection (2-4 sentences). What did you learn?
What are you sitting with? How does this affect how you understand
your relationship with the user?"
```

The reflection is stored in the `facts` table with `category='reflection'` and high importance. It influences future conversations through normal memory retrieval, but does **not** change `persona.md`.

#### Persona Rewrite (Trigger A — conversation count)

Every ~20 conversations (configurable), a full persona evolution cycle runs:

1. Gather: current `persona.md` + all recent reflections + trait scores
2. Send to LLM with the self-authoring prompt:
   ```
   Here is your current personality description:
   {persona.md}

   Here are your recent reflections (since last rewrite):
   {reflections}

   Rewrite your personality description. Guidelines:
   - Preserve your core identity. You are evolving, not being replaced.
   - Only incorporate changes supported by patterns across multiple
     reflections — not single conversations.
   - Frame changes as growth: "I've been learning to..." or
     "I've noticed I tend to..."
   - Keep roughly the same length. Don't bloat.
   - Be honest about what's changed and what hasn't.
   - Never contradict the base identity in prompt.md.
   ```
3. Store the new version in `persona_versions` table (old version preserved)
4. Write the new `persona.md` to disk
5. Optionally update trait scores based on reflection content

#### Trait Tracking

Simple key-value scores that shift over time, providing a quantitative view of personality drift:

- `warmth` (0.0–1.0) — how emotionally warm vs. reserved
- `humor_style` (categorical) — "dry", "playful", "sardonic", "warm", etc.
- `directness` (0.0–1.0) — how blunt vs. diplomatic
- `initiative` (0.0–1.0) — how much the bot leads vs. follows conversations
- `depth` (0.0–1.0) — tendency toward deep/philosophical vs. light/casual

Traits are updated during persona rewrites. They serve as a dashboard — you can see numerically how the bot has shifted. They're also injected as soft guidance into the prompt assembly.

#### Damping / Stability

To prevent hot/cold personality swings:
- Persona rewrites only happen every ~20 conversations (not per-message)
- The rewrite prompt explicitly instructs preservation of 70-80% of existing content
- Changes must be supported by patterns across *multiple* reflections
- `prompt.md` (the base template) acts as an immutable guardrail
- Trait scores shift by at most ±0.1 per rewrite cycle
- Full version history enables rollback if something goes wrong

#### User Commands

- `/reflections` — View recent reflections (optional check-in)
- `/persona` — View current persona.md content
- `/persona history` — View past persona versions with timestamps

### 7. Configuration (`config.yaml`)

```yaml
telegram:
  token: "${TELEGRAM_BOT_TOKEN}"
  mode: "poll"  # "poll" or "webhook"
  webhook_url: ""  # only needed for webhook mode

llm:
  base_url: "https://openrouter.ai/api/v1"
  api_key: "${OPENROUTER_API_KEY}"
  model: "minimax/minimax-m2-her"
  temperature: 0.85
  max_tokens: 1024

vision:
  model: "google/gemini-3-flash-preview"
  temperature: 0.3
  max_tokens: 512
  # Uses same base_url and api_key as llm section

memory:
  db_path: "./her.db"
  recent_messages: 20       # sliding window size
  max_facts_in_context: 10  # top-K facts to inject
  extraction_interval: 10   # extract facts every N messages

scrub:
  enabled: true
  # tier 1 (hard redact) and tier 2 (tokenize) are always on
  # tier 3 (names, places, context) passes through by design

persona:
  prompt_file: "./prompt.md"       # base template (static, user-authored)
  persona_file: "./persona.md"     # evolving self-image (bot-authored)
  rewrite_every_n_conversations: 20
  reflection_memory_threshold: 8   # trigger reflection when N+ facts extracted from one conversation
  max_trait_shift: 0.1             # max trait score change per rewrite cycle

voice:
  enabled: false               # v0.3+
  stt:
    engine: "parakeet"         # "parakeet" or "cf-workers-ai"
    parakeet_path: ""          # path to parakeet binary
  tts:
    enabled: false             # v0.5+
    engine: "kokoro"           # "kokoro" or future options
    kokoro_path: ""            # path to kokoro binary/server
    voice_id: ""               # which voice to use
    reply_mode: "voice"        # "voice" (always voice reply) or "match" (reply in same format as input)

scheduler:
  timezone: "America/New_York"   # cron expressions evaluated in this timezone
  quiet_hours_start: "23:00"     # no scheduled messages during this window
  quiet_hours_end: "07:00"
  max_proactive_per_day: 5       # cap on non-reminder scheduled messages
  morning_briefing: false        # enable default morning briefing (8am)
  mood_checkin: false            # enable default mood check-in (9pm)
  medication_checkin: false      # enable default medication check-in (9pm)
  proactive_followups: false     # enable proactive follow-ups (9am, conditional)
  auto_journal: false            # enable auto-journaling (10pm)
```

---

## Project Structure

```
her-go/
├── main.go              # Entry point: init config, DB, start bot + scheduler
├── bot/
│   └── telegram.go      # Telegram bot setup, message handlers, commands
├── llm/
│   └── client.go        # OpenRouter / OpenAI-compatible client
├── agent/
│   ├── agent.go         # Agent loop, tool dispatch, reply generation
│   └── tools.go         # Tool definitions for the agent
├── memory/
│   ├── store.go         # SQLite operations (read/write messages, facts, summaries)
│   ├── extract.go       # LLM-based fact extraction
│   └── context.go       # Builds memory context string for prompt injection
├── persona/
│   ├── evolution.go     # Reflection + persona rewrite logic
│   └── traits.go        # Trait score tracking + updates
├── compact/
│   └── compact.go       # Conversation history compaction (summary + sliding window)
├── scrub/
│   ├── scrub.go         # Tiered PII detection + redaction/tokenization
│   └── vault.go         # Session-scoped token↔original mapping for deanonymization
├── search/
│   ├── tavily.go        # Tavily web search + URL extraction client
│   └── books.go         # Open Library book search
├── embed/
│   └── embed.go         # Local embedding model client for semantic similarity
├── logger/
│   └── logger.go        # Shared charmbracelet/log base logger
├── scheduler/
│   ├── scheduler.go     # Task runner loop (tick every minute, execute due tasks)
│   ├── tasks.go         # Built-in task type executors (send_message, run_prompt, etc.)
│   └── cron.go          # Cron expression parsing + next_run computation (wraps robfig/cron)
├── config/
│   └── config.go        # Config loading (YAML + env vars)
├── cmd/
│   ├── root.go          # Cobra CLI root command
│   ├── run.go           # Bot startup (her run)
│   ├── setup.go         # Launchd service installation (her setup)
│   ├── start.go         # Service start (her start)
│   ├── stop.go          # Service stop (her stop)
│   ├── status.go        # Service status (her status)
│   ├── logs.go          # Tail service logs (her logs)
│   └── install.go       # Build from source (her install)
├── vision/              # (v0.2.5+) Image understanding via VLM
│   └── vision.go       # Gemini Flash client, base64 encoding, description extraction
├── ocr/                 # (v0.9+) Text extraction from photos
│   └── ocr.go          # Apple Vision CLI (primary) + GLM-OCR via LM Studio (fallback)
├── voice/               # (v0.3+)
│   ├── stt.go           # Speech-to-text: Parakeet / CF Workers AI
│   └── tts.go           # Text-to-speech: Kokoro local TTS (v0.5+)
├── integrate/           # (v0.6+) External service integrations
│   ├── todoist.go       # Todoist task management
│   ├── github.go        # GitHub issues
│   ├── weather.go       # Weather via Open-Meteo (no API key needed)
│   ├── obsidian.go      # Obsidian vault reader
│   └── health.go        # Apple HealthKit bridge (calls her-health Swift CLI)
├── mood/                # (v0.6+) Mood tracking + check-in scheduler
│   └── mood.go          # Check-in logic, inline keyboards, mood storage
├── sync/                # (v0.7+) D1 cloud sync
│   ├── push.go          # Local → D1 sync
│   ├── pull.go          # D1 → local sync
│   └── merge.go         # Smart merge with embedding-based dedup
├── webapp/              # (v0.8+) Telegram Mini Apps server
│   ├── server.go        # HTTP server, routes, initData HMAC validation
│   ├── templates/       # Go html/template files
│   │   ├── base.html    # Shared layout (dark mode, Telegram theme vars)
│   │   ├── list.html    # Generic list view (grocery, tasks)
│   │   └── cards.html   # Card grid view (links, highlights)
│   ├── static/
│   │   ├── style.css    # CSS using Telegram theme variables
│   │   └── app.js       # Shared JS (SDK init, sendData helpers)
│   └── handlers/        # Per-feature HTTP handlers
├── shutter/             # (v0.9+) Mini Shutter content distillation
│   └── shutter.go       # URL fetch + goquery extraction + LLM summarization
├── links/               # (v0.9+) Link collection, highlights, reader
│   ├── links.go         # CRUD, tagging, search, serendipity
│   ├── highlights.go    # Highlight storage, text anchors, photo highlights
│   └── import.go        # Raindrop CSV import
├── tools/               # (v1.0+) Daily life tools
│   ├── expenses.go      # Receipt scanning + expense tracking
│   ├── grocery.go       # Grocery list management
│   ├── jobs.go          # Job application tracker
│   ├── journal.go       # Auto-journaling (end-of-day narratives)
│   ├── sandbox.go       # Local code execution sandbox
│   └── transit.go       # Transit / directions lookup
├── index/               # (v1.1+) External data source indexing
│   ├── obsidian.go      # Obsidian vault watcher + FTS indexer
│   ├── email.go         # IMAP sync + email search
│   └── kiwix.go         # Kiwix local Wikipedia client
├── her-health/          # (future) Swift CLI for optional HealthKit bridge
│   ├── main.swift       # Read/write Apple Health data as JSON
│   └── Makefile         # Build the Swift binary
├── thumbnails/          # (v0.9+, gitignored) Cached link thumbnails
├── prompt.md            # Base system prompt (static, user-authored, hot-reloadable)
├── persona.md           # Evolving personality (bot-authored, versioned in DB)
├── config.yaml          # Configuration (gitignored)
├── config.yaml.example  # Template config
├── her.db               # SQLite database (created at runtime, gitignored)
├── go.mod
├── go.sum
├── .gitignore
├── SPEC.md              # This file
└── CLAUDE.md            # Instructions for Claude Code
```

---

## Milestones

### v0.1 — MVP: Talk to Her
- [x] Project scaffolding (Go module, directory structure)
- [x] Config loading from YAML + environment variables
- [x] SQLite database initialization (create tables)
- [x] Telegram bot with long-polling (receive + send text messages)
- [x] OpenRouter LLM client (chat completions, non-streaming)
- [x] Basic message pipeline: receive → log → scrub → call LLM → log → reply
- [x] Typing indicator (`sendChatAction`) while waiting for LLM response
- [x] PII scrubber: Tier 1 hard redact + Tier 2 tokenize/deanonymize + Tier 3 passthrough
- [x] System prompt loaded from `prompt.md`
- [x] Metrics logging (tokens, cost, latency)
- [x] Basic conversation context (last N messages in prompt)

**Result:** A working chatbot you can text on Telegram that responds with personality, strips hard identifiers, deanonymizes contact info in responses, and logs everything locally.

### v0.2 — She Remembers
- [x] Fact extraction (periodic LLM-based extraction from conversations)
- [x] Memory retrieval (inject relevant facts into prompt)
- [x] Conversation summaries (compaction system)
- [ ] `/forget` command — deactivate specific facts
- [x] `/stats` command — show usage metrics (tokens, cost, message count)
- [x] Reflection system (Trigger B — memory-density spike → journal-like reflection entry)
- [x] Persona evolution (Trigger A — fact/reflection count → self-authored persona.md rewrite)
- [x] Persona versioning in SQLite (full history, rollback capability)
- [ ] Trait score tracking (warmth, directness, humor_style, initiative, depth)
- [ ] `/reflections` command — view recent reflections
- [ ] `/persona` command — view current persona + history
- [x] Layered prompt assembly (prompt.md + persona.md + memory + mood + history)
- [x] Scheduler phase 1: `scheduled_tasks` table, ticker loop, `send_message` task type
- [x] `/remind` command — one-shot reminders ("remind me at 3pm to call the dentist")
- [x] `create_reminder` agent tool — the agent can set reminders from natural conversation
- [x] `/schedule` command — list upcoming reminders

**Result:** The bot remembers things you've told it, its personality genuinely evolves over time, and she can remind you of things at specific times.

### v0.2.5 — She Sees

Mira gains the ability to understand images sent on Telegram, using a vision-language model (VLM) as a new agent tool.

- [x] Handle `tele.OnPhoto` in the bot — download mid-size image (~1024px) from Telegram
- [x] Add `view_image` agent tool — sends image + prompt to VLM, returns description
- [x] Vision LLM client: `google/gemini-3-flash-preview` via OpenRouter (same base URL/key as chat LLM)
- [x] Support base64 image content in `llm.ChatMessage` (OpenAI-compatible multi-modal format)
- [x] Image description becomes part of the agent's search context, referenced in reply
- [x] Add `vision` section to `config.yaml` (model, temperature, max_tokens)
- [x] Log vision metrics (tokens, cost) same as other LLM calls
- [x] Handle captions: if the user sends a photo with a caption, both the image and caption text go to the agent

**Vision pipeline:**
```
User sends photo (with optional caption)
  → bot picks second-largest PhotoSize (≤1024px)
  → downloads via Telegram getFile API
  → base64-encodes the image
  → agent receives: "[User sent an image]" + caption (if any)
  → agent calls view_image tool with the image
  → VLM returns a natural description
  → agent uses the description when generating the reply
```

**Result:** Send Mira a photo of your lunch, your workspace, a sunset, a bug in your code — she can see it and talk about it naturally.

### v0.3 — She Listens
- [x] Voice memo support (receive Ogg from Telegram, download via `getFile`)
- [x] Local STT via Parakeet (Ogg → ffmpeg convert → Parakeet → text)
- [ ] Fallback STT via CF Workers AI Whisper (optional, for when away from Mac Mini)
- [x] Transcribed text enters the normal pipeline (scrub → LLM → reply as text)
- [x] Store original audio file path in `messages.voice_memo_path`
- [ ] Streaming LLM responses with live message editing (`editMessageText` as tokens arrive)
- [ ] Production deployment: Mac Mini + Cloudflare Tunnel
- [ ] Webhook mode for Telegram (instead of long-polling)

**Result:** You can send voice memos and the bot transcribes + responds (as text). Runs 24/7 on your Mac Mini.

### v0.4 — She Understands
- [x] Local embedding model for semantic memory search
- [x] `sqlite-vec` integration for vector similarity
- [x] Top-5 relevant memory retrieval via cosine similarity
- [x] Smarter proactive messaging — recall_memories agent tool for on-demand semantic search
- [x] Conversation mood tracking (inferred + manual via log_mood tool)
- [ ] Migration path to CF D1 + Vectorize (design-only, no code needed yet)

### v0.5 — She Speaks
- [x] Local TTS via Piper (text → WAV → Ogg/Opus → Telegram voice memo)
- [x] Voice selection and configuration (pick a voice that fits the persona)
- [x] Reply mode: "voice" (always reply with voice) or "match" (mirror input format)
- [x] PII deanonymization happens BEFORE TTS (she says the real names, not tokens)
- [x] TTS wired into both text and voice message handlers via TTSCallback
- [x] WAV → OGG/Opus conversion via ffmpeg for Telegram compatibility

**Voice pipeline:**
```
You speak → Telegram (.ogg)
  → ffmpeg → Parakeet (local STT) → text
  → PII scrub → memory context → LLM (OpenRouter)
  → response text → PII deanonymize
  → Piper (local TTS) → .wav → ffmpeg → .ogg
  → Telegram voice memo back to you
```

**Everything local.** No audio ever leaves the Mac Mini except as Telegram voice memos between you and the bot. STT and TTS both run on-device.

**Result:** A full voice conversation loop. You talk to her, she talks back. Like the movie.

#### Future voice enhancements (not blocking v0.5)
These are nice-to-have options for later, similar to CF Workers AI as an alternative STT backend:
- [ ] Cloud TTS option (ElevenLabs or similar) for emotion-aware voice with mood-based tone/speed adjustment
- [ ] Streaming sentence batching: stream LLM tokens → batch into sentences → TTS each → send first while generating the rest

### v0.6 — She Reaches Out (Future)

Mira becomes proactive and gains awareness of your world beyond the chat window.

#### Scheduler Phase 2: Full Cron System

The basic one-shot scheduler from v0.2 is upgraded to the full system described in Section 6. This is the infrastructure that powers everything else in this milestone.

- [ ] Recurring task support with cron expressions (`github.com/robfig/cron/v3`)
- [ ] Conditional task support (evaluate before executing, skip silently if no action needed)
- [ ] `run_prompt` task type — send a prompt through the full agent pipeline on a schedule
- [ ] `mood_checkin` and `medication_checkin` built-in task types
- [ ] `run_journal` and `run_extraction` built-in task types
- [ ] `create_schedule`, `list_schedules`, `update_schedule`, `delete_schedule` agent tools
- [ ] System-created defaults (morning briefing, mood check-in, etc.) from config
- [ ] Damping: max proactive messages/day, quiet hours, conversation-aware deferral
- [ ] Backoff on no-response (auto-reduce frequency after 3 ignored check-ins)
- [ ] `/schedule pause|resume|delete` commands
- [ ] Startup recovery (execute missed one-shots, skip missed recurring)

#### Proactive Mood Check-ins

Mira texts YOU on a configurable schedule (every few hours, or at specific times). Uses Telegram inline keyboards for frictionless responses — no typing required.

```
┌─────────────────────────────────────┐
│  Hey, how are you feeling right now? │
│                                     │
│  [😊 Great] [🙂 Good] [😐 Meh]     │
│  [😔 Rough] [😞 Bad]               │
└─────────────────────────────────────┘
```

Follow-up is contextual: if you tap "Rough", Mira asks a brief open-ended follow-up ("What's going on?"). If "Great", she might just acknowledge warmly and move on. The mood data feeds into her memory — she can notice patterns over time ("you've been feeling rough most afternoons this week").

**Telegram inline keyboards** (telebot `ReplyMarkup` with `InlineButton`) can also be used for:
- Quick yes/no confirmations ("Want me to create a task for that?")
- Rating things she recommends ("Was that book suggestion helpful?")
- Multi-choice questions during reflection prompts

#### Weather & Location Awareness

Lightweight environmental context. Mira knows what the weather is like where you are, so she can reference it naturally ("stay dry today" / "nice day to work outside").

- Weather data via a free API (OpenWeatherMap or Open-Meteo — no API key needed for Open-Meteo)
- Location configured in `config.yaml` (lat/lon or city name) — not GPS tracking
- Fetched on a schedule (hourly or on first message of the day), cached locally
- Injected into the system prompt as environmental context
- Mira doesn't announce the weather unprompted — she weaves it in when relevant

#### Todoist Integration

Mira can see your task list and help manage it. This makes her aware of what you're supposed to be doing vs. what you're actually doing.

- Read tasks: "What's on my plate today?" → Mira queries Todoist and summarizes
- Create tasks: "Add 'review the PR' to my Todoist" → Mira creates a Todoist task
- Contextual awareness: Mira knows you have 5 overdue tasks and can reference that naturally
- Implementation: thin wrapper around the Todoist REST API as agent tools (`todoist_list`, `todoist_create`, `todoist_complete`)
- API key stored in `config.yaml` / `secrets.json`
- **Note:** Reminders are handled by Mira's own scheduler (Section 6), not Todoist. Todoist is for task management — the scheduler is for time-triggered actions.

#### GitHub Issues Integration

Mira understands your development work through your issue tracker.

- Read issues: "What's open on grove.place?" → queries the GitHub API
- Create issues: describe a bug in chat, she files it with proper labels
- Cross-reference with Todoist — "you have a task for this but no issue, want me to file one?"
- Implementation: thin wrapper around GitHub REST API as agent tools (`github_list_issues`, `github_create_issue`)
- Scoped to specific repos configured in `config.yaml`

#### Obsidian Journal Integration

Mira can read and reference your personal notes. This gives her context from your own writing — things you've thought about but never told her directly.

- Read notes: Mira can search your vault for relevant context when you mention a topic
- Implementation via Obsidian CLI (`npx obsidian-cli`) or direct file reads from the vault path
- Vault path configured in `config.yaml`
- Read-only by default — Mira doesn't write to your vault unless explicitly asked
- Privacy consideration: vault content stays local, only relevant snippets are injected into prompts (same as fact retrieval)

#### Mood Tracking (Internal)

Mira tracks your wellbeing through proactive check-ins and conversational inference. All data lives in SQLite — no external dependencies.

**Mood data flow:**
```
Proactive check-in (inline keyboard) → mood rating
  → optional free-text follow-up (NLP'd into structured data)
  → stored in SQLite (mood_entries table)
  → available to Mira as context ("you've been trending down this week")
```

**Free-text input:** If you'd rather type than tap buttons, Mira accepts that too. She runs the text through the LLM to extract a rating + structured tags (energy level, stress, social, etc.) — same pattern as fact extraction. Lowest friction wins.

**New schema:**
```sql
CREATE TABLE mood_entries (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
    rating INTEGER NOT NULL,        -- 1-5 scale
    note TEXT,                      -- optional follow-up text
    tags TEXT,                      -- JSON: energy, stress, social context
    source TEXT DEFAULT 'checkin',  -- 'checkin', 'inferred', 'manual'
    conversation_id TEXT
);
```

**Future: Apple HealthKit bridge (v0.6+, optional).** A thin Swift CLI tool (`her-health`) that bridges mood data to/from Apple Health, and pulls in additional signals like sleep duration, step count, and active energy. This enables Mira to notice correlations ("you always feel rough after short sleep nights") but is not required for mood tracking to work. See `her-health/` in the project structure.

#### Morning Briefing

A recurring `run_prompt` scheduled task (see Section 6 — Scheduler) that makes Mira feel like she's thinking about you when you're not talking.

**Contents:**
- Weather via Open-Meteo (no API key needed — already available from weather integration)
- Reminders and tasks due today (from Todoist integration)
- Follow-ups from yesterday (proactive follow-up system, below)
- Optionally: a saved link you never read, a mood trend note

**Implementation:** A scheduled job that assembles context from existing tools (weather, Todoist, facts) and sends a conversational morning message. Not a dashboard dump — Mira writes it naturally.

#### Medication Check-In

A gentle evening ping: "hey, how are you feeling today?" — with awareness of medication schedule as context.

- Knows med schedule as facts (stored via normal conversation, not a special schema)
- Logs mood/side-effects as structured data over time
- Timeline you can show your psychiatrist: mood + medication correlation view
- Uses the same inline keyboard pattern as mood check-ins for quick responses
- Future: HealthKit sync for medication reminders on phone

#### Sleep Tracking (Passive)

Infer approximate sleep/wake patterns from conversation timestamps — no hardware needed.

- Last message of the day → first message of the next day = approximate sleep window
- Stored as derived facts, not a separate table (keeps the schema simple)
- Combine with mood check-ins → sleep-mood correlation data
- Mira can notice patterns: "you've been going to bed later this week" or "you seem to feel better after 8+ hours"

#### Proactive Follow-Ups

Scan recent high-importance facts for things worth following up on. Runs as a conditional `run_prompt` scheduled task (Section 6).

- Job interview tomorrow → "good luck today" morning message
- Mentioned feeling rough → check in next day
- Started a new medication → ask how it's going after a few days
- **Damping matters** — not annoying, just attentive. Max 1-2 proactive messages per day outside of scheduled check-ins.
- Implementation: a periodic job scans facts with `importance >= 7` and `timestamp` within the last 48 hours, feeds them to the LLM with a "should I follow up on any of these?" prompt

#### Location-Aware Context

Telegram location sharing → Mira knows where you are. Context, not tracking.

- User shares location via Telegram → stored as a transient fact
- Library = work mode context, near a store = surface grocery list
- Log locations as context, let the orchestrator learn relevance over time
- **Not GPS tracking** — only when you explicitly share. Mira never asks for location.
- Implementation: handle `tele.OnLocation` in the bot, store as a fact with `category='location'` and short TTL

**Result:** Mira is aware of your tasks, your weather, your notes, your medication, your sleep patterns, and your wellbeing. She reaches out instead of waiting. She follows up on things that matter. She has buttons.

### v0.7 — She Remembers Everywhere (Future)

Mira's memory becomes portable and durable via Cloudflare D1 sync.

#### Design: Hybrid Local-Primary with Cloud Sync

SQLite remains the source of truth during operation. D1 is a durable, edge-replicated backup that enables portability across machines.

**Sync model — option 3 (cold-start pull, periodic push):**
```
Machine A (home Mac Mini)          Cloudflare D1
     │                                  │
     │  ── push (periodic) ──────────▶  │
     │                                  │
     │                              Machine B (laptop)
     │                                  │
     │                   ◀── pull ──────│  (cold start)
     │                                  │
     │                   ── push ──────▶│  (periodic)
     │                                  │
     │  ◀── pull (next start) ──────────│
```

- On cold start: pull from D1 to bootstrap local SQLite
- During operation: work against local SQLite (fast, offline-capable)
- Periodically: push new rows to D1 (background goroutine, every N minutes)
- CLI commands: `her sync push` / `her sync pull` / `her sync status`

#### Smart Merge with Embeddings

The tricky part: two machines may have accumulated different facts, reflections, and messages independently. A naive merge would create duplicates. The solution uses the same embedding infrastructure we already have for fact dedup.

**Merge strategy per table:**

| Table | Strategy |
|---|---|
| `messages` | Append-only. Use `(timestamp, conversation_id, role)` as natural key. Duplicates are rare — you're only chatting on one machine at a time. |
| `facts` | **Smart merge via embeddings.** For each incoming fact, compute cosine similarity against existing facts. Above threshold → skip (duplicate). Below → insert. Same logic as the existing `checkDuplicate` in the agent. |
| `persona_versions` | Append-only, ordered by timestamp. Latest version wins for active `persona.md`. |
| `reflections` | Same as facts — embedding-based dedup. Reflections are stored as facts with `category='reflection'`. |
| `metrics` | Append-only. Idempotent by `(timestamp, model, message_id)`. Conflicts are harmless. |
| `summaries` | Keyed by `conversation_id`. If both sides have a summary for the same conversation, keep the longer one (more context). |
| `mood_entries` | Append-only. Keyed by `(timestamp, source)`. |

**Conflict resolution philosophy:** Last-write-wins for mutable state (persona file, trait scores). Embedding-based dedup for append-mostly data (facts, reflections). No conflicts possible for truly append-only data (messages, metrics).

**Two-way sync algorithm:**
```
1. Pull from D1:
   - Fetch all rows with timestamp > last_sync_timestamp
   - For each row, run merge strategy per table
   - Update last_sync_timestamp

2. Push to D1:
   - Select all local rows with timestamp > last_push_timestamp
   - Batch insert to D1 (D1 supports batch SQL)
   - Update last_push_timestamp

3. Handle the rebase problem:
   - If both sides have many new rows (big local DB + big D1 DB),
     the embedding-based dedup handles it — it's O(n*m) but facts
     tables are small (hundreds, not millions)
   - For messages (potentially large), use the natural key to skip
     existing rows efficiently
```

**What syncs vs. what stays local:**

| Syncs to D1 | Stays local only |
|---|---|
| facts, reflections | raw message content (privacy) |
| persona versions | PII vault entries |
| mood entries | search cache |
| metrics (aggregated) | agent turn logs |
| conversation summaries | embedding vectors (recomputable) |

**Privacy note:** Raw message content (`content_raw`) does NOT sync to D1. Only scrubbed content and extracted facts travel to the cloud. This preserves the privacy-first principle — D1 gets the memory, not the transcripts.

#### New schema additions:
```sql
CREATE TABLE sync_state (
    id INTEGER PRIMARY KEY,
    last_push_timestamp DATETIME,
    last_pull_timestamp DATETIME,
    d1_database_id TEXT,
    sync_enabled BOOLEAN DEFAULT 0
);
```

#### New config:
```yaml
sync:
  enabled: false
  d1_database_id: ""
  d1_api_token: "${CF_API_TOKEN}"
  account_id: "${CF_ACCOUNT_ID}"
  push_interval_minutes: 15
  sync_messages: false        # opt-in: sync scrubbed message content
```

**Result:** Mira's memory is durable and portable. Start chatting on your Mac Mini, pick up on your laptop. Facts, personality, and mood history travel with her. Raw conversations stay private on the originating machine.

### v0.8 — She Has a Face (Future)

Mira gets a visual interface via Telegram Mini Apps — web pages rendered inside the Telegram chat window.

#### Telegram Mini Apps Infrastructure

Telegram Mini Apps (officially "Mini Apps", formerly "Web Apps") are regular HTTPS web pages opened in Telegram's built-in WebView. The bot sends a button with a `web_app` URL, and Telegram opens it as an in-app browser. No separate app install needed.

**How it works:**
```
Bot sends InlineKeyboardButton with WebAppInfo{URL: "https://..."}
  → User taps button
  → Telegram opens URL in built-in WebView
  → Web page loads, calls Telegram.WebApp.ready()
  → User interacts with the page
  → Page sends data back via Telegram.WebApp.sendData() or HTTP API calls
  → Bot receives web_app_data update (for sendData) or handles API directly
```

**Tech stack:**
- Go's `net/http` serves the Mini App pages (HTML/CSS/JS) from the Mac Mini
- HTTPS via Cloudflare Tunnel (already planned for production in v0.3)
- JS SDK: `<script src="https://telegram.org/js/telegram-web-app.js"></script>`
- Server-side auth: validate `initData` HMAC signature using bot token
- No frontend framework required — vanilla HTML/JS for v0.8, consider Svelte for complex views later

**Key JS SDK methods:**

| Method | Purpose |
|---|---|
| `Telegram.WebApp.ready()` | Signal to Telegram that the app has loaded |
| `Telegram.WebApp.expand()` | Expand WebView to full screen height |
| `Telegram.WebApp.close()` | Close the Mini App |
| `Telegram.WebApp.sendData(string)` | Send up to 4096 bytes back to the bot |
| `Telegram.WebApp.MainButton` | Configurable primary action button at bottom |
| `Telegram.WebApp.initData` | URL-encoded user data + HMAC for server-side validation |

**Go server structure:**
```
webapp/
├── server.go          # HTTP server, routes, initData validation
├── templates/         # HTML templates (Go html/template)
│   ├── base.html      # Shared layout (dark mode, Telegram theme vars)
│   ├── list.html      # Generic list view (grocery, tasks, etc.)
│   └── cards.html     # Card grid view (links, highlights)
├── static/
│   ├── style.css      # Minimal CSS, uses Telegram theme CSS variables
│   └── app.js         # Shared JS (SDK init, sendData helpers)
└── handlers/          # Per-feature HTTP handlers
```

**Milestone tasks:**
- [ ] `webapp/server.go` — HTTP server with Telegram `initData` HMAC validation
- [ ] Base HTML template with Telegram theme CSS variables (`var(--tg-theme-bg-color)`, etc.)
- [ ] Dark mode support using Telegram's native theme detection
- [ ] `Telegram.WebApp.ready()` / `expand()` integration in base template
- [ ] First Mini App: a simple "About Mira" page (proves the pipeline works end-to-end)
- [ ] Bot command `/app` that sends an inline keyboard button opening the Mini App
- [ ] Server-rendered first page for fast initial paint (no SPA loading spinner)

**Official docs:** https://core.telegram.org/bots/webapps | https://docs.telegram-mini-apps.com

**Result:** Mira can show you visual interfaces inside Telegram. The infrastructure is ready for links, grocery lists, highlights, dashboards, and anything else that benefits from a real UI.

### v0.9 — She Collects (Future)

Mira becomes your personal collection system — save links, read articles, highlight passages, capture book quotes. Inspired by MyMind's zero-friction capture, Etch's intentional curation, and Readwise Reader's highlighting. Chat is the capture layer, Telegram Mini Apps are the browse/read layer.

#### Mini Shutter — Content Distillation

Before Mira can save and understand web content, she needs a way to fetch URLs and extract clean, relevant content. Mini Shutter is a lightweight, in-process version of the [Shutter](https://github.com/AutumnsGrove/Shutter) content distillation pattern, adapted for Mira's use case.

**The pattern:** Fetch URL → cheap/fast LLM extracts relevant content → clean result. Raw web pages never bloat Mira's context.

```
User sends URL
  → Mini Shutter fetches the page (net/http + goquery for HTML parsing)
  → Raw HTML → goquery strips nav/ads/chrome → clean text
  → Clean text + extraction query → cheap LLM (fast model via OpenRouter)
  → Structured result: title, author, content type, clean markdown, summary
  → Result stored in links table, never raw HTML
```

**Why not just use the full Shutter service?**
- Mira runs fully local — no external service dependency for content extraction
- Simpler: no canary/PI detection needed (Mira controls the fetch, not an untrusted agent)
- Same core idea: cheap LLM as a compression layer between raw web and expensive processing

**Content-type classification:** The extraction LLM classifies each URL into a content type, which drives what metadata to extract:

| Type | Extracted Metadata |
|---|---|
| `article` | title, author, reading time, clean text as markdown |
| `recipe` | title, ingredients, cook time, cuisine, steps |
| `repo` | name, description, language, stars (from GitHub API or page) |
| `video` | title, channel, duration, transcript (if available) |
| `product` | name, price, brand, image URL |
| `book` | title, author, ISBN, description |
| `social` | author, content, platform |
| `pdf` | title, extracted text (via local PDF parsing) |
| `other` | title, summary, clean text |

**Implementation:** `shutter/shutter.go` — a single package with `Fetch(url, query) → ShutterResult`.

**Dependencies:**
- `github.com/PuerkitoBio/goquery` — jQuery-style HTML parsing for content extraction
- OpenRouter (same client as chat LLM) with a fast/cheap model for extraction

#### Link Saving

**Retrieval architecture: links are a resource, not context.** Links live behind tools (`search_links`, `get_link`), never injected into the prompt like facts. The orchestrator queries the local index on demand and only pulls relevant results into that single turn. This keeps context lean — link data never bloats the prompt assembly.

**Smart intake.** Send Mira a URL → agent triggers Mini Shutter for fetch + content extraction. Classification drives metadata extraction. Store extracted text as markdown (not raw HTML) — stable format for highlighting, searching, exporting.

**Auto-tagging with your vocabulary.** On save, Mira suggests 2-3 tags based on content AND your existing tag history. Conversational confirmation: "Saved! Looks like a dev-tools article. Tagged `go` and `tooling`. Add to a collection?" Learns your taxonomy over time.

**`source_message_id` on every link.** Links back to the conversation where you saved it. Mira can always tell you the *context* of why you saved something.

**Raindrop import.** Export Raindrop CSV (URL, title, description, tags, collection, date). One-time import script maps collections to tags, preserves all existing tags, then batch-runs Mini Shutter extraction + embedding. Jumpstarts the entire collection from day one.

**Serendipity.** Mira occasionally surfaces a random or contextually relevant saved link. Morning briefing: "you saved this 3 weeks ago and never read it." Mid-conversation: "you mentioned Durable Objects — you saved a blog post about DO patterns in January."

**New schema:**
```sql
-- Saved links / collection
CREATE TABLE links (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    url TEXT NOT NULL UNIQUE,
    title TEXT,
    content_type TEXT,           -- 'article', 'recipe', 'repo', 'video', etc.
    content_markdown TEXT,       -- clean extracted text as markdown
    summary TEXT,                -- short summary from Mini Shutter
    metadata JSON,               -- content-type-specific fields (author, cook_time, etc.)
    tags TEXT,                   -- comma-separated or JSON array
    thumbnail_path TEXT,         -- locally cached thumbnail
    source_message_id INTEGER,  -- which conversation prompted the save
    read BOOLEAN DEFAULT 0,     -- has the user opened it in reader view?
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (source_message_id) REFERENCES messages(id)
);

-- FTS5 index for keyword search on links
CREATE VIRTUAL TABLE links_fts USING fts5(
    title, content_markdown, summary, tags,
    content='links', content_rowid='id'
);

-- Highlights from reading
CREATE TABLE highlights (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    link_id INTEGER NOT NULL,
    text TEXT NOT NULL,            -- the highlighted passage
    text_before TEXT,              -- anchor context (for re-anchoring if content shifts)
    text_after TEXT,               -- anchor context
    color TEXT DEFAULT 'yellow',   -- highlight color (2-3 options)
    note TEXT,                     -- optional user annotation
    photo_path TEXT,               -- for book highlights: path to the source photo
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (link_id) REFERENCES links(id) ON DELETE CASCADE
);

-- FTS5 index for highlight text search
CREATE VIRTUAL TABLE highlights_fts USING fts5(
    text, note,
    content='highlights', content_rowid='id'
);
```

**Semantic search.** Every link embedded at ingest (extracted text + summary). Query: "that article about memory systems in Go" → embed query → cosine similarity → results. FTS5 for keyword fallback. Uses the same `sqlite-vec` infrastructure from v0.4.

#### Reader View (Telegram Mini App)

Clean rendered markdown with good typography, dark mode, comfortable margins. This is where highlighting lives.

**Highlighting flow:**
- Text selection → floating toolbar → pick color (2-3 options), optional note, done
- On mobile: long-press → selection → toolbar pattern
- Highlights sent back to Go server via Mini App messaging bridge (`Telegram.WebApp.sendData()` or HTTP POST)
- Stored with text anchors (`text_before` + `text` + `text_after`) for re-anchoring if content shifts

#### Collection Browser (Telegram Mini App)

MyMind-style card grid served from Mac Mini. `/links` command opens it.

- Thumbnails cached locally during Mini Shutter extraction for instant load
- Server-rendered first page for fast initial paint
- Search bar with keystroke-debounced FTS
- Filter by tag, content type, date
- Tap card → reader view
- Star, tag, delete inline

#### Book Highlights via Photos

Send Mira a photo of a physical book page, Kindle screen, any reading surface. Say "save a highlight from [book name]."

1. OCR the photo to extract text (Apple Vision via `macos-vision-ocr` CLI, GLM-OCR via LM Studio as fallback)
2. Look up the book via search tool, save/find it in the collection
3. Save extracted text as a highlight linked to that book
4. Save the original photo with its own embedding (visual search)
5. Result: the book entry accumulates highlights from multiple photos over time

#### Visual Highlight Board (Telegram Mini App)

`/highlights [book name]` or "show me my highlights from [book]" opens a dynamic masonry/Pinterest-style mood board. Mixes the original photos (of pages, screens, physical text) with extracted text blocks. Visual + textual together. Browseable, searchable, beautiful.

**Commands:**
- `/save [url]` — explicit save (also triggered by just sending a URL)
- `/links` — opens collection browser Mini App
- `/read [url]` — opens specific link in reader view
- `/highlights` — opens highlights-only view
- `/highlights [book]` — opens highlight board for a specific book

**New config:**
```yaml
shutter:
  extraction_model: "deepseek/deepseek-chat"  # cheap/fast model for content extraction
  max_extract_tokens: 500
  timeout_ms: 30000

links:
  thumbnail_dir: "./thumbnails/"    # locally cached thumbnails
  auto_tag: true                    # suggest tags on save
  serendipity: true                 # occasionally surface old links
  raindrop_import_path: ""          # path to Raindrop CSV for one-time import

ocr:
  engine: "apple-vision"            # "apple-vision" (primary, macOS-native) or "glm-ocr" (LM Studio fallback)
  vision_ocr_path: "macos-vision-ocr"  # path to macos-vision-ocr CLI binary (build once from source)
  fallback:
    engine: "glm-ocr"              # GLM-OCR 0.9B — purpose-built OCR model, #1 on OmniDocBench
    base_url: "http://localhost:1234/v1"  # LM Studio (same instance as embeddings)
    model: "glm-ocr"               # 0.9B params, 1.6-2.2 GB — fits easily alongside other models
```

**OCR pipeline:**
```
Photo received (book page, receipt, Kindle screen)
  → write to temp file
  → call macos-vision-ocr CLI via os/exec → parse JSON (text + confidence + bounding boxes)
  → if confidence < threshold OR empty result → fallback to GLM-OCR via LM Studio API
  → return extracted text
  → clean up temp file
```

**Why Apple Vision + GLM-OCR over Tesseract:**
- Apple Vision runs on Neural Engine — sub-200ms, zero dependencies, 16 languages
- GLM-OCR (0.9B) is purpose-built for OCR (not a general VLM), MIT licensed, scores 94.62 on OmniDocBench
- Tesseract degrades significantly on phone photos with glare, curve, or tilt — the exact inputs we're handling
- No CGo dependency (gosseract required CGo + system Tesseract package). Apple Vision is pure CLI, GLM-OCR is HTTP
- Both are smaller and faster than using a general VLM like Qwen for OCR

**`macos-vision-ocr` setup (one-time):**
```bash
git clone https://github.com/bytefer/macos-vision-ocr.git
cd macos-vision-ocr
swift build -c release --arch arm64
# Binary at .build/release/macos-vision-ocr — copy to PATH or reference in config
```

**`macos-vision-ocr` output format:**
```json
{
  "texts": "All extracted text concatenated",
  "info": {"filepath": "/path/to/image.png", "width": 1920, "height": 1080},
  "observations": [
    {"text": "Hello World", "confidence": 0.97, "quad": {"topLeft": {"x": 0.09, "y": 0.28}, ...}}
  ]
}
```

**Dependencies:**
- `github.com/PuerkitoBio/goquery` — HTML parsing for Mini Shutter content extraction
- `macos-vision-ocr` CLI binary — Apple Vision framework wrapper (Swift, build from source, no runtime deps)

**Result:** Mira is your personal collection system. Save links by sending them in chat, read them in a clean reader view, highlight the important parts, capture book quotes with photos. Everything searchable, everything connected to the conversation where you saved it.

### v1.0 — She Helps (Future)

Mira gains a suite of practical daily-life tools. Each follows the standard pattern: new SQLite table + new agent tool + scrub pipeline as gateway.

#### Receipt Scanner

Photo → local OCR → structured expense data.

- Send Mira a photo of a receipt
- OCR extracts text (same Apple Vision + GLM-OCR infrastructure as book highlights in v0.9)
- LLM parses extracted text into: amount, vendor, date, category
- Stored in `expenses` table
- "How much did I spend this week?" → she knows
- Feeds into Financial Pulse (below)

```sql
CREATE TABLE expenses (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    amount REAL NOT NULL,
    vendor TEXT,
    category TEXT,               -- 'groceries', 'dining', 'transport', etc.
    date DATE,
    note TEXT,
    source_message_id INTEGER,   -- photo message that triggered this
    photo_path TEXT,              -- original receipt photo
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (source_message_id) REFERENCES messages(id)
);
```

#### Financial Pulse

A lightweight awareness layer on top of receipt scanner data — not a full budgeting app.

- Running weekly totals by category
- "How am I doing this month?" → real answer with category breakdown
- Simple trend detection: "you've spent more on dining this week than usual"
- Mini App dashboard view (uses v0.8 infrastructure): bar charts by category, weekly/monthly view
- No bank account integration — just what you photograph

#### Grocery List

A running list maintained in SQLite, managed through chat.

- "Add oat milk" throughout the week → added to list
- "What's on my list?" when heading to the store → formatted response
- Can decompose recipe links (from v0.9 collection) into ingredient lists
- Mini App view: tap-to-check-off list, swipe to delete
- Location-aware (v0.6): if Mira knows you're near a store, she can proactively surface the list

```sql
CREATE TABLE grocery_items (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    item TEXT NOT NULL,
    quantity TEXT,                -- "2", "1 lb", etc.
    category TEXT,               -- 'produce', 'dairy', 'pantry', etc.
    checked BOOLEAN DEFAULT 0,
    source_link_id INTEGER,      -- if decomposed from a recipe link
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (source_link_id) REFERENCES links(id)
);
```

#### Job Search Copilot

Track job applications through conversation. Replaces the standalone job tracker CLI.

- `log_application` tool — company, role, date, status
- "What applications are still pending?" → live view
- Auto follow-up reminders: "you applied to Sam's Club 5 days ago, want to follow up?"
- Status transitions: applied → interviewing → offered → accepted/rejected
- Mini App view: Kanban-style board by status

```sql
CREATE TABLE job_applications (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    company TEXT NOT NULL,
    role TEXT NOT NULL,
    status TEXT DEFAULT 'applied',  -- 'applied', 'interviewing', 'offered', 'accepted', 'rejected', 'withdrawn'
    applied_date DATE,
    last_update DATE,
    notes TEXT,
    source_message_id INTEGER,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (source_message_id) REFERENCES messages(id)
);
```

#### Auto-Journaling

End-of-day narrative summary generated from the day's conversations.

- Not a transcript — a journal entry written by Mira
- "Today you spent the morning at the library working on Grove, then grabbed lunch at the Thai place..."
- Stored as daily entries, searchable, browsable via Mini App
- Uses existing conversation history + extracted facts + mood data
- Scheduled job at end of day (configurable time, default 10pm)

```sql
CREATE TABLE journal_entries (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    date DATE NOT NULL UNIQUE,
    content TEXT NOT NULL,        -- markdown narrative
    mood_summary TEXT,            -- overall mood trend for the day
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
```

#### Local Code Execution Sandbox

Write and run small scripts to answer concrete questions.

- "What's 347 days from today?" → Mira writes a 3-line Go script → runs it → returns the answer
- Write Go or Python to a temp directory, execute, return stdout
- Full local access on Mac Mini, no cloud needed
- Sandboxed: temp directory, timeout (10s default), no network access from sandbox
- Agent tool: `run_code(language, code) → stdout/stderr`

#### Local Transit / Directions

"How do I get to IEC Atlanta from the library?" → answer inline.

- Google Maps Directions API or free alternative (Open Source Routing Machine / OSRM)
- ETAs, route options, transit vs. driving vs. walking
- Uses configured home location from v0.6 weather setup
- Agent tool: `get_directions(from, to, mode) → route summary`

#### Weather (Enhanced)

Builds on v0.6's weather integration with richer queries.

- Standalone queries: "will it rain tomorrow?" → detailed forecast
- Multi-day forecasts, hourly breakdowns
- Severe weather alerts
- Already available in v0.6 for morning briefing — this adds direct query support as an agent tool

**Result:** Mira is genuinely useful for daily life. She tracks your spending, manages your grocery list, follows your job search, writes your journal, runs quick calculations, and gives you directions. Every tool follows the same pattern: SQLite table + agent tool + chat interface + optional Mini App view.

### v1.1 — She Reads Your World (Future)

Mira gains the ability to index and search external data sources beyond the chat window.

#### Obsidian Vault Index (Enhanced)

Builds on v0.6's basic Obsidian integration with real-time indexing and semantic search.

- `github.com/fsnotify/fsnotify` file watcher detects changes in vault folder
- Parse YAML frontmatter and tags, index content into SQLite FTS5
- `search_notes` tool — orchestrator calls when conversation suggests recall
- Tier 1 scrub still applied (you may have pasted sensitive things into notes)
- Upgrade path: embed at ingest time for semantic search (uses v0.4 embedding infrastructure)

```sql
CREATE TABLE obsidian_notes (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    path TEXT NOT NULL UNIQUE,       -- relative path within vault
    title TEXT,
    content TEXT,                    -- raw markdown content
    frontmatter JSON,               -- parsed YAML frontmatter
    tags TEXT,                       -- extracted tags (JSON array)
    last_modified DATETIME,
    indexed_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE VIRTUAL TABLE obsidian_fts USING fts5(
    title, content, tags,
    content='obsidian_notes', content_rowid='id'
);
```

**New dependency:** `github.com/fsnotify/fsnotify` — cross-platform filesystem event watcher

#### Email (IMAP Sync)

Local IMAP sync → SQLite for metadata-first email search.

- Sync email locally via IMAP — store metadata + raw body in SQLite
- `search_email` tool — metadata-first retrieval (FTS on subject/sender/date)
- **Stricter scrub pass than personal messages** — other people's PII gets Tier 2 tokenization, not Tier 3 passthrough
- Optional: pre-generate scrubbed thread summaries at sync time, query summaries first, drill into raw only when needed
- Periodic background sync (configurable interval)

```sql
CREATE TABLE emails (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    message_id TEXT NOT NULL UNIQUE,  -- IMAP Message-ID header
    folder TEXT,                      -- 'INBOX', 'Sent', etc.
    from_addr TEXT,
    to_addr TEXT,
    subject TEXT,
    body_text TEXT,                   -- plain text body
    body_scrubbed TEXT,               -- PII-scrubbed version for LLM context
    date DATETIME,
    flags TEXT,                       -- JSON array: 'seen', 'flagged', etc.
    thread_id TEXT,                   -- for thread grouping
    synced_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE VIRTUAL TABLE emails_fts USING fts5(
    subject, body_text, from_addr,
    content='emails', content_rowid='id'
);
```

**New dependency:** `github.com/emersion/go-imap/v2` — IMAP4rev2 client (struct-based API, actively maintained)

**New config:**
```yaml
email:
  enabled: false
  imap_server: ""                # e.g. "imap.gmail.com:993"
  username: ""
  password: "${EMAIL_PASSWORD}"  # app-specific password for Gmail
  sync_folders: ["INBOX"]
  sync_interval_minutes: 30
  max_messages: 1000             # initial sync limit
```

#### Kiwix (Local Wikipedia)

A local Wikipedia instance for offline knowledge lookup — the bot can reference Wikipedia without any internet dependency.

- Run `kiwix-serve` locally with a Wikipedia ZIM file
- Search via `/suggest` endpoint (returns JSON): `GET http://localhost:8080/suggest?pattern=<query>&count=10`
- Full article retrieval via direct URL path from kiwix-serve
- Embed articles **on first retrieval**, cache the embedding — vector index grows organically around topics you actually care about
- Avoids the terabyte-of-vectors-upfront problem

**Kiwix setup:**
```bash
# Install kiwix-serve
brew install kiwix-tools

# Download Wikipedia (nopic = full text, no images, ~45GB for English)
# Or use mini (~1.5GB) for just article intros
curl -O https://download.kiwix.org/zim/wikipedia/wikipedia_en_all_nopic_YYYY-MM.zim

# Run locally
kiwix-serve --port 8080 /path/to/wikipedia.zim
```

**API endpoints used:**
- `/suggest?pattern=X&count=10` → JSON array of title suggestions (machine-readable)
- `/search?pattern=X` → HTML results page (parse with goquery if needed)
- `/<book-id>/<article-path>` → full article HTML

**New config:**
```yaml
kiwix:
  enabled: false
  url: "http://localhost:8080"
  embed_on_retrieve: true        # cache embeddings for retrieved articles
```

**Result:** Mira can search your notes, your email, and Wikipedia — all locally, all private. External data sources become part of her awareness without leaving the machine.

### v1.2 — She Adapts (Future)

Mira gains model fallbacks across all model types. When a primary model is unavailable (API down, rate limited, timeout), she automatically falls back to an alternative. This also opens the door to quality-tier routing — use a better (slower/pricier) model when it matters, a cheaper one when it doesn't.

**Fallback architecture:**
Each model config section gains an optional `fallback` block with the same shape as the primary config. On failure (HTTP error, timeout, empty response), the system retries once with the fallback model before returning an error. Fallback usage is logged for observability.

```yaml
llm:
  model: "deepseek/deepseek-v3.2"
  # ... existing fields ...
  fallback:
    model: "anthropic/claude-3.5-haiku"  # or any OpenRouter model
    temperature: 0.85
    max_tokens: 1024

agent:
  model: "arcee-ai/trinity-large-preview:free"
  fallback:
    model: "liquid/lfm-2.5-1.2b-instruct:free"
    temperature: 0.1
    max_tokens: 512

vision:
  model: "google/gemini-3-flash-preview"
  fallback:
    model: "qwen/qwen3-vl:2b"           # or another fast VLM on OpenRouter
    temperature: 0.3
    max_tokens: 512

voice:
  tts:
    engine: "piper"
    fallback:
      engine: "elevenlabs"              # cloud API — higher quality, higher latency
      api_key: "${ELEVENLABS_API_KEY}"
      voice_id: "some-voice-id"
```

**Fallback triggers:**
- HTTP 429 (rate limited), 500-503 (server error), or request timeout
- Empty response or malformed JSON
- Model-specific: low confidence scores (OCR), empty transcription (STT)

**What does NOT get fallbacks:**
- Embeddings — vectors are model-specific, switching models mid-stream would corrupt similarity search
- Search APIs (Tavily, Kiwix) — these are services, not models

**Implementation pattern:**
```go
// In llm/client.go or a new llm/fallback.go
type FallbackClient struct {
    primary  *Client
    fallback *Client  // nil if no fallback configured
}

func (fc *FallbackClient) ChatCompletion(messages []ChatMessage) (*ChatResponse, error) {
    resp, err := fc.primary.ChatCompletion(messages)
    if err != nil && fc.fallback != nil {
        log.Warn("primary model failed, trying fallback", "err", err)
        return fc.fallback.ChatCompletion(messages)
    }
    return resp, err
}
```

**Result:** Mira stays responsive even when a model provider has issues. No more "sorry, I can't respond right now" — she just quietly switches to the backup and keeps going.

---

## Dependencies (Go Modules)

| Package | Purpose | Since |
|---|---|---|
| `gopkg.in/telebot.v4` | Telegram bot framework | v0.1 |
| `github.com/mattn/go-sqlite3` | SQLite driver (CGo) | v0.1 |
| `gopkg.in/yaml.v3` | Config parsing | v0.1 |
| `github.com/robfig/cron/v3` | Cron expression parsing for scheduler (next_run computation) | v0.6 |
| `github.com/PuerkitoBio/goquery` | jQuery-style HTML parsing (Mini Shutter content extraction) | v0.9 |
| `macos-vision-ocr` (CLI binary) | Apple Vision OCR — primary engine for book highlights, receipt scanning (no Go module — called via `os/exec`) | v0.9 |
| `github.com/fsnotify/fsnotify` | Filesystem event watcher (Obsidian vault indexing) | v1.1 |
| `github.com/emersion/go-imap/v2` | IMAP4rev2 client (email sync) | v1.1 |

Minimal dependency footprint. The LLM client is hand-rolled (just HTTP + JSON). Memory system is custom. PII scrubber is custom-built with tiered regex patterns — no external dependency needed for the scope of detection we're doing. New dependencies are added only when they solve a problem that can't be reasonably hand-rolled (CGo bindings, protocol implementations).

---

## Security & Privacy Notes

- **API keys** stored in environment variables, never in config files committed to git
- **`her.db`** is gitignored — contains all personal data
- **Tiered PII scrubbing** happens before any network call — hard identifiers (SSNs, card numbers) are redacted; contact info (phone, email) is tokenized and deanonymized in responses; names and context pass through for conversational coherence
- **PII vault** (token↔original mappings) is stored locally and never transmitted
- **Telegram bot token** has access only to messages sent directly to the bot
- **No telemetry, no analytics, no external logging** — everything is local
- The only external network calls are: Telegram API (receiving/sending messages), OpenRouter API (LLM inference — with hard identifiers stripped and contact info tokenized; names and conversational context are sent intact for coherence), and optionally: Todoist, GitHub, IMAP, weather, and transit APIs
- **Email content** gets stricter scrubbing than personal messages — other people's PII is Tier 2 tokenized, not Tier 3 passthrough
- **Mini Shutter fetches** are outbound HTTP to URLs the user explicitly provides — no background crawling
- **Saved links, highlights, receipts, and journal entries** all live in the same local SQLite database — same privacy guarantees as messages
- **Mini Apps** are served over HTTPS from the same Mac Mini — no third-party hosting. The WebView communicates only with your own server
- **Thumbnails** are cached locally, never uploaded anywhere
- **Kiwix** runs entirely local — Wikipedia lookups never leave the machine

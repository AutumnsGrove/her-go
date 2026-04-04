# Shared Infrastructure

Core systems that every message touches — the pipeline backbone.

## Telegram Bot (`bot/`)

- Uses `telebot v4` (`gopkg.in/telebot.v4`) or `go-telegram-bot-api/v5`
- Long-polling for development (no infra needed)
- Webhook mode for production (behind Cloudflare Tunnel)
- Handles: text messages, photos, commands (`/remind`, `/forget`, `/stats`, `/traces`)
- Photo handling: when a photo is received, downloads a mid-size version (~1024px, second-largest from Telegram's `PhotoSize` array) and passes it to the agent for vision processing
- Sends `sendChatAction("typing")` while waiting for LLM response (re-sent every 4s to keep indicator alive)
- Future: live-edit streaming — send a placeholder message, then `editMessageText` as tokens arrive for a real-time typing effect
- Future: voice messages (Ogg → Parakeet local STT → text)

## LLM Client (`llm/`)

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
| Agent LLM (Trinity) | Tool-calling orchestration | Fast, good at structured output |
| Vision LLM (Gemini 3 Flash) | Image understanding | Fast VLM, good casual descriptions |
| OCR — primary (Apple Vision) | Text extraction from photos | Sub-200ms, Neural Engine, zero deps |
| OCR — fallback (GLM-OCR 0.9B) | Text extraction when primary fails | Purpose-built OCR model, #1 OmniDocBench |

**LLM client internals:** `ChatResponse` exposes `FinishReason` (why the model stopped — `"stop"`, `"tool_calls"`, `"length"`, etc.), which the agent loop uses to determine whether another iteration is needed. `ToolChoice` infrastructure is available for directing the model to call a specific tool or forcing tool use on a given turn.

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

## PII Scrubber (`scrub/`)

**Philosophy:** Tiered scrubbing by risk category. Names and context pass through for conversational coherence — hard identifiers are stripped, contact info is tokenized and reversed on response.

### Tier 1 — Hard Redact (irreversible, never sent to LLM)

Regex-based detection. Matched content is replaced with `[REDACTED]` in the scrubbed copy. No deanonymization needed — these never appear in responses.

- Social Security Numbers (XXX-XX-XXXX patterns)
- Credit/debit card numbers (Luhn-validated)
- Bank account / routing numbers
- Passwords, API keys, secrets (patterns like `sk-`, `Bearer`, etc.)
- Government ID numbers (passport, driver's license patterns)

### Tier 2 — Tokenize + Deanonymize (reversible, vault-based)

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

### Tier 3 — Pass Through (no action)

These are left intact because they're essential for conversational coherence and empathy. Scrubbing them would destroy the LLM's ability to reason about your life.

- First names, nicknames, relational terms ("Mom", "my boss")
- Cities, neighborhoods, general locations
- Workplace names, school names
- Emotional context, relationship dynamics
- Dates, times, events

**Privacy rationale:** First names and city-level locations are not uniquely identifying on their own. The dangerous PII is structured identifiers (Tier 1) that map directly to a person. "Sarah in Portland had an argument with her mom" is not actionable. "Sarah at 123 Main St, SSN 456-78-9012" is.

### Vault

- In-memory map per conversation turn, keyed by placeholder token
- Also persisted to SQLite `pii_vault` table for audit trail
- Vault entries are never sent to the LLM — only used locally for deanonymization

## Memory System (`memory/`)

**Custom-built, SQLite-backed, designed for eventual migration to CF D1/Vectorize.**

### Storage

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

### Retrieval Strategy (MVP)

1. **Recent messages**: Last N messages from the current conversation (sliding window)
2. **Relevant facts**: Pull top-K facts by recency and importance
3. **Today's summary**: If a summary exists for today, include it

### Retrieval Strategy (Future — v0.3+)

1. Everything above, plus:
2. **Semantic search**: Embed the current message, find top-5 most similar past facts/messages via cosine similarity
3. Uses local embedding model (already familiar with this)
4. Embeddings stored in SQLite via `sqlite-vec` extension, or a separate vector column

### Fact Extraction

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
- **Max fact length:** 200 characters. Facts exceeding this are rejected at insertion — they indicate the model returned a paragraph rather than a single-sentence fact.
- **Style gates:** A blocklist of AI writing tics ("it's worth noting", "certainly", "I should mention", etc.) rejects facts that read like LLM hedging rather than real information. Facts must describe the user's life, not the model's reasoning.
- **Classifier gate:** A small LLM (Haiku-class, temperature 0) validates every memory write (save_fact, update_fact, log_mood, scan_receipt) before the DB write. Returns one of: SAVE (proceed), FICTIONAL (in-game/book/show content), LOW_VALUE (too vague), MOOD_NOT_FACT (transient mood that should use log_mood skill), INFERRED (agent editorializing beyond what user stated), EXTERNAL (mood about a fictional character). Fail-open on errors. Rejection messages are actionable — e.g., MOOD_NOT_FACT tells the agent to use log_mood instead. See `agent/classifier.go`.
- **"context" category:** Ephemeral day-to-day facts (current mood, what the user is working on today, recent events) are stored with `category='context'`. These are auto-injected with a timestamp (`[as of 2026-03-24]`) so the LLM knows how fresh they are, and are prioritized for replacement when the context window is tight.

## Prompt System (Layered)

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

### `prompt.md` — Base Template (static, user-authored)

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

### `persona.md` — Evolving Self-Image (bot-authored)

- Starts as a seed description (written by you or generated from first few conversations)
- **Rewritten by the bot itself** during persona evolution cycles (see [Persona Evolution](persona.md))
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

## Configuration (`config.yaml`)

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

agent:
  model: "arcee-ai/trinity-large-preview:free"
  temperature: 0.1
  max_tokens: 1024
  trace: false  # show agent thinking traces in chat (/traces to toggle)

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
    engine: "piper"            # "piper" or future options
    piper_path: ""             # path to piper binary
    voice_model: ""            # path to .onnx voice model file
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

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
┌─────────────┐         ┌──────────────────────────────────┐
│  Telegram   │◀───────▶│         her-go binary            │
│  (user)     │         │                                  │
└─────────────┘         │  ┌──────────┐   ┌─────────────┐  │
                        │  │ Telegram │   │  Scheduler  │  │
                        │  │ Handler  │   │  (reminders)│  │
                        │  └────┬─────┘   └──────┬──────┘  │
                        │       │                │         │
                        │       ▼                │         │
                        │  ┌──────────┐          │         │
                        │  │ Pipeline │◀─────────┘         │
                        │  │          │                    │
                        │  │ 1. Log raw message            │
                        │  │ 2. PII scrub                  │
                        │  │ 3. Retrieve memories          │
                        │  │ 4. Build prompt               │
                        │  │ 5. Call LLM                   │
                        │  │ 6. Log response + metrics     │
                        │  │ 7. Send reply                 │
                        │  └────┬─────┘                    │
                        │       │                          │
                        │       ▼                          │
                        │  ┌──────────┐   ┌─────────────┐  │
                        │  │ SQLite   │   │  OpenRouter │  │
                        │  │ (local)  │   │  (external) │  │
                        │  └──────────┘   └─────────────┘  │
                        └──────────────────────────────────┘
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
- Handles: text messages, commands (`/remind`, `/forget`, `/stats`)
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

-- Reminders / scheduled messages
CREATE TABLE reminders (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    trigger_at DATETIME NOT NULL,
    message TEXT NOT NULL,
    delivered BOOLEAN DEFAULT 0
);

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

- In-process goroutine that checks for pending reminders every minute
- When a reminder triggers, sends a Telegram message to the user
- Future: could support more complex scheduling (daily check-ins, weekly summaries)

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

### 6. Scheduler (`scheduler/`)

- In-process goroutine that checks for pending reminders every minute
- When a reminder triggers, sends a Telegram message to the user
- Future: could support more complex scheduling (daily check-ins, weekly summaries)

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
├── memory/
│   ├── store.go         # SQLite operations (read/write messages, facts, summaries)
│   ├── extract.go       # LLM-based fact extraction
│   └── context.go       # Builds memory context string for prompt injection
├── persona/
│   ├── evolution.go     # Reflection + persona rewrite logic
│   └── traits.go        # Trait score tracking + updates
├── voice/
│   ├── stt.go           # Speech-to-text: Parakeet / CF Workers AI integration
│   └── tts.go           # Text-to-speech: Kokoro local TTS (v0.5+)
├── scrub/
│   ├── scrub.go         # Tiered PII detection + redaction/tokenization
│   └── vault.go         # Session-scoped token↔original mapping for deanonymization
├── scheduler/
│   └── scheduler.go     # Reminder checker + delivery
├── config/
│   └── config.go        # Config loading (YAML + env vars)
├── prompt.md            # Base system prompt (static, user-authored, hot-reloadable)
├── persona.md           # Evolving personality (bot-authored, versioned in DB)
├── config.yaml          # Configuration
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
- [ ] Config loading from YAML + environment variables
- [ ] SQLite database initialization (create tables)
- [ ] Telegram bot with long-polling (receive + send text messages)
- [ ] OpenRouter LLM client (chat completions, non-streaming)
- [ ] Basic message pipeline: receive → log → scrub → call LLM → log → reply
- [ ] Typing indicator (`sendChatAction`) while waiting for LLM response
- [ ] PII scrubber: Tier 1 hard redact + Tier 2 tokenize/deanonymize + Tier 3 passthrough
- [ ] System prompt loaded from `prompt.md`
- [ ] Metrics logging (tokens, cost, latency)
- [ ] Basic conversation context (last N messages in prompt)

**Result:** A working chatbot you can text on Telegram that responds with personality, strips hard identifiers, deanonymizes contact info in responses, and logs everything locally.

### v0.2 — She Remembers
- [ ] Fact extraction (periodic LLM-based extraction from conversations)
- [ ] Memory retrieval (inject relevant facts into prompt)
- [ ] Conversation summaries (end-of-day or end-of-session)
- [ ] `/remind` command — set reminders that fire at a specific time
- [ ] Reminder scheduler (goroutine, checks every minute)
- [ ] `/forget` command — deactivate specific facts
- [ ] `/stats` command — show usage metrics (tokens, cost, message count)
- [ ] Reflection system (Trigger B — memory-density spike → journal-like reflection entry)
- [ ] Persona evolution (Trigger A — every ~20 conversations → self-authored persona.md rewrite)
- [ ] Persona versioning in SQLite (full history, rollback capability)
- [ ] Trait score tracking (warmth, directness, humor_style, initiative, depth)
- [ ] `/reflections` command — view recent reflections
- [ ] `/persona` command — view current persona + history
- [ ] Layered prompt assembly (prompt.md + persona.md + reflections + facts + history)

**Result:** The bot remembers things you've told it, can remind you, and its personality genuinely evolves over time based on your interactions.

### v0.3 — She Listens
- [ ] Voice memo support (receive Ogg from Telegram, download via `getFile`)
- [ ] Local STT via Parakeet (Ogg → ffmpeg convert → Parakeet → text)
- [ ] Fallback STT via CF Workers AI Whisper (optional, for when away from Mac Mini)
- [ ] Transcribed text enters the normal pipeline (scrub → LLM → reply as text)
- [ ] Store original audio file path in `messages.voice_memo_path`
- [ ] Streaming LLM responses with live message editing (`editMessageText` as tokens arrive)
- [ ] Production deployment: Mac Mini + Cloudflare Tunnel
- [ ] Webhook mode for Telegram (instead of long-polling)

**Result:** You can send voice memos and the bot transcribes + responds (as text). Runs 24/7 on your Mac Mini.

### v0.4 — She Understands (Future)
- [ ] Local embedding model for semantic memory search
- [ ] `sqlite-vec` integration for vector similarity
- [ ] Top-5 relevant memory retrieval via cosine similarity
- [ ] Smarter proactive messaging (not just reminders)
- [ ] Conversation mood tracking
- [ ] Migration path to CF D1 + Vectorize

### v0.5 — She Speaks (Future)

Full end-to-end voice: you speak, she speaks back.

- [ ] Local TTS via Kokoro (text → WAV → Ogg/Opus → Telegram voice memo)
- [ ] Voice selection and configuration (pick a voice that fits the persona)
- [ ] Reply mode: "voice" (always reply with voice) or "match" (mirror input format)
- [ ] PII deanonymization happens BEFORE TTS (she says the real names, not tokens)
- [ ] Audio caching for repeated phrases (greetings, acknowledgments)
- [ ] Latency optimization: stream LLM tokens → batch into sentences → TTS each sentence → send first sentence as voice memo while generating the rest
- [ ] Emotion-aware TTS: adjust speed/tone based on conversation mood (if Kokoro supports it)

**Voice pipeline:**
```
You speak → Telegram (.ogg)
  → ffmpeg → Parakeet (local STT) → text
  → PII scrub → memory context → LLM (OpenRouter)
  → response text → PII deanonymize
  → Kokoro (local TTS) → .ogg
  → Telegram voice memo back to you
```

**Everything local.** No audio ever leaves the Mac Mini except as Telegram voice memos between you and the bot. STT and TTS both run on-device.

**Result:** A full voice conversation loop. You talk to her, she talks back. Like the movie.

---

## Dependencies (Go Modules)

| Package | Purpose |
|---|---|
| `gopkg.in/telebot.v4` | Telegram bot framework |
| `github.com/mattn/go-sqlite3` | SQLite driver (CGo) |
| `gopkg.in/yaml.v3` | Config parsing |
Minimal dependency footprint. The LLM client is hand-rolled (just HTTP + JSON). Memory system is custom. PII scrubber is custom-built with tiered regex patterns — no external dependency needed for the scope of detection we're doing.

---

## Security & Privacy Notes

- **API keys** stored in environment variables, never in config files committed to git
- **`her.db`** is gitignored — contains all personal data
- **Tiered PII scrubbing** happens before any network call — hard identifiers (SSNs, card numbers) are redacted; contact info (phone, email) is tokenized and deanonymized in responses; names and context pass through for conversational coherence
- **PII vault** (token↔original mappings) is stored locally and never transmitted
- **Telegram bot token** has access only to messages sent directly to the bot
- **No telemetry, no analytics, no external logging** — everything is local
- The only external network calls are: Telegram API (receiving/sending messages) and OpenRouter API (LLM inference — with hard identifiers stripped and contact info tokenized; names and conversational context are sent intact for coherence)

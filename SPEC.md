# her-go — Personal Companion Bot

## Overview

A privacy-first personal companion chatbot built in Go. Communicates via Telegram, powered by LLMs through OpenRouter, with local SQLite storage for conversations, memory, and metrics. Inspired by "Her" — a persistent, warm presence that remembers your life and helps you keep track of things.

**Single user. Single binary. Everything local.**

---

## Core Principles

1. **Privacy first** — Hard identifiers (SSNs, card numbers, etc.) never leave the host machine. Names and context pass through for conversational coherence. Local inference wherever possible.
2. **Own your data** — Everything lives in a local SQLite database. D1 is a sync mirror, not a replacement. No cloud dependencies for storage.
3. **Model agnostic** — Swap models by changing a config value. System prompt lives in a plain `.md` file.
4. **Defense in depth** — Trust tiers, proxy layers, PII scrubbing all carry forward.
5. **Single user** — This is a personal tool for Autumn, not a platform.
6. **The agent cannot read her own source** — This boundary is non-negotiable.
7. **Learn by building** — Custom memory system, custom PII scrubbing. Understand every piece.

### v2 Principles

8. **Always-alive heartbeat** — Mira should be reachable 24/7, even if the full brain is sleeping.
9. **Delegated execution** — Mira describes intent, a sandboxed agent executes. She never touches a shell directly.
10. **Composability through delegation** — Text processing, file creation, HTTP requests all available through the sandbox, not through individual tools.
11. **One leader at a time** — Only one instance owns the Telegram bot token. Automatic handoff on startup.

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
       │                │  │ 4. Classifier gate (validates memory writes)           │
       │                │  │ 5. Mini Shutter (URL fetch + content distillation)      │
       │                │  │ 6. Reply generation                                    │
       │                │  │ 7. Persona evolution                                   │
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

### v2 Architecture (Target)

```
┌─────────────────────────────────────────────────────────────────┐
│                    Cloudflare Edge (always alive, $0)            │
│                                                                 │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────────────┐  │
│  │ Webhook recv │  │ Telegram     │  │ Workers AI           │  │
│  │ (GitHub,     │  │ webhook      │  │ (optional, free tier │  │
│  │  Todoist,    │  │ (bot API)    │  │  offline ack only)   │  │
│  │  email)      │  │              │  │                      │  │
│  └──────┬───────┘  └──────┬───────┘  └──────────────────────┘  │
│         │                 │                                     │
│         ▼                 ▼                                     │
│  ┌─────────────────────────────────────────────────────────┐    │
│  │              MiraDO (Durable Object, SQLite-backed)     │    │
│  │                                                         │    │
│  │  Schedule table ──→ storage.setAlarm() at exact times   │    │
│  │  Task queue ──────→ holds events when brain is offline  │    │
│  │  Leader lock ─────→ which machine owns the bot          │    │
│  │  Brain status ────→ online/offline heartbeat tracking   │    │
│  │  Trigger rules ───→ "when X happens, do Y" definitions  │    │
│  │                                                         │    │
│  │  Sleeping DO costs $0. Wakes only on alarm or request.  │    │
│  └─────────────────────────────────────────────────────────┘    │
│                                                                 │
│  ┌──────────────┐                                               │
│  │ D1           │  (sync mirror for facts, messages, mood)      │
│  │              │                                               │
│  └──────────────┘                                               │
└─────────────────────────────────┬───────────────────────────────┘
                                  │
                        Tailscale tunnel (or CF Tunnel)
                                  │
                                  ▼
┌─────────────────────────────────────────────────────────────────┐
│                Mac Mini — Full Brain (primary runtime)           │
│                                                                 │
│  ┌───────────────────────────────────────────────────────────┐  │
│  │                    her-go binary (Go)                      │  │
│  │                                                           │  │
│  │  Agent loop (Kimi K2.5)  ←→  Skills harness (4-tier)     │  │
│  │  Chat model (DSv3.2)     ←→  Memory (SQLite + KNN)       │  │
│  │  Vision (Gemini Flash)   ←→  PII scrubber (3-tier)       │  │
│  │  Classifier (Haiku)      ←→  Scheduler (local cron)      │  │
│  │  STT (Parakeet, local)   ←→  Persona evolution            │  │
│  │  TTS (Piper, local)      ←→  Telegram WebApp server       │  │
│  │  Embeddings (nomic, local)                                │  │
│  │  OCR (Vision OCR, local)                                  │  │
│  └────────────────────────────────┬──────────────────────────┘  │
│                                   │                             │
│                                   ▼                             │
│  ┌───────────────────────────────────────────────────────────┐  │
│  │            Docker Container — "The Workshop"              │  │
│  │                                                           │  │
│  │  Persistent workspace (survives restarts)                 │  │
│  │  Coding agent (pi-agent / Claude Code / similar)          │  │
│  │  Real bash, real filesystem — but fully isolated           │  │
│  │  No host filesystem access                                │  │
│  │  Network: proxied through Mira's existing proxy layer     │  │
│  │                                                           │  │
│  │  Mira delegates → agent executes → results return         │  │
│  │  She can observe the workspace state at any time          │  │
│  └───────────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────────┘
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
9. Memory writes (save_fact, log_mood, scan_receipt) pass through classifier gate — a small LLM (Haiku) that rejects fictional, low-value, inferred, or misrouted content
10. Response received, logged to SQLite (both raw response + token counts + cost)
11. Response sent back to user on Telegram
12. Periodically: fact extraction runs against raw messages to build long-term memory

**Data retention:** Every stage is preserved. The `messages` table stores both `content_raw` (what you actually said) and `content_scrubbed` (what the LLM saw). Nothing is ever deleted — scrubbing creates a parallel sanitized copy, it does not replace the original. The `pii_vault` table maintains session-scoped mappings for Tier 2 tokens so responses can be deanonymized before display.

---

## Detailed Specs

### Components

- **[Shared Infrastructure](docs/spec/shared-infra.md)** — LLM client, PII scrubber, memory system, prompt assembly, configuration
- **[Scheduler](docs/spec/scheduler.md)** — Cron system, reminders, task types, damping
- **[Persona Evolution](docs/spec/persona.md)** — Reflections, persona rewrites, trait tracking

### Milestones — Completed

- **[v0.1 — MVP: Talk to Her](docs/spec/v0.1.md)** — Basic pipeline, PII scrubbing, Telegram bot
- **[v0.2 — She Remembers](docs/spec/v0.2.md)** — Memory, fact extraction, persona evolution, reminders
- **[v0.2.5 — She Sees](docs/spec/v0.2.5.md)** — Vision model, image understanding
- **[v0.3 — She Listens](docs/spec/v0.3.md)** — Voice memos, local STT
- **[v0.4 — She Understands](docs/spec/v0.4.md)** — Semantic search, embeddings, mood tracking
- **[v0.5 — She Speaks](docs/spec/v0.5.md)** — Local TTS, full voice loop, thinking traces

### Milestones — In Progress / Future

- **[v0.6 — She Reaches Out](docs/spec/v0.6.md)** — Scheduler phase 2 (mostly done), weather (done), mood tracking (done), Todoist (done), GitHub (done), morning briefing (done). Remaining: proactive follow-ups, nearby search
- **[v0.7 — She Adapts](docs/spec/v0.7.md)** — Model fallbacks (done), cloud sync via D1
- **[v0.8 — She Has a Face](docs/spec/v0.8.md)** — Telegram Mini Apps infrastructure
- **[v0.9 — She Collects](docs/spec/v0.9.md)** — Link saving, Mini Shutter, reader view, book highlights. OCR already implemented
- **[v1.0 — She Helps](docs/spec/v1.0.md)** — Receipt scanner (done), grocery list, job tracker, auto-journaling, code sandbox
- **[v1.1 — She Reads Your World](docs/spec/v1.1.md)** — Obsidian vault integration, IMAP email, local Wikipedia

### Architecture Vision

- **[v2 — Architecture Vision](docs/spec/v2.md)** — Heartbeat (MiraDO), leader election, D1 sync, Workshop (Docker sandbox), `/update`, reactive triggers

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
│   ├── static/          # CSS + JS
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

### v2 Security Properties

| Property | Status in v2 |
|---|---|
| Agent cannot read own source | Unchanged |
| 4-tier trust model | Extended to workshop (treated as 4th-party) |
| SSRF-safe network proxy | Extended to workshop container |
| PII scrubber (3-tier) | Unchanged |
| Credentials invisible to agent | Unchanged. Workshop gets no host env vars. |
| Hash-based trust verification | Unchanged for skills |
| Manual promotion only | Unchanged |
| Local embedding / inference | Unchanged (Mac Mini runs all local models) |

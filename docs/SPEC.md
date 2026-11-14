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
│  │  Driver (DeepSeek V4.1 Flash) ←→ Memory (SQLite+vec)      │  │
│  │  Chat model (MiMo v2.6 Pro)   ←→ PII scrubber (3-tier)    │  │
│  │  Classifier (Gemini Lite)     ←→ Scheduler (local cron)   │  │
│  │  Vision (DeepSeek V4.1 Flash) ←→ Persona evolution        │  │
│  │  STT (Parakeet, local)        ←→ TTS (Piper / Nova)       │  │
│  │  Embeddings (pplx-embed)      ←→ Dream cycle + traces     │  │
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

> **Status note (2026-11):** the roadmap below is aspirational. Several items were closed as not-pursued during an issue sweep (#16 Mini Apps, #25 code sandbox, #30 Kiwix, #31 HealthKit, #65 Apple Reminders), and the Todoist/GitHub integrations were removed rather than shipped. Treat "done" claims in the v0.6+ files with suspicion; the linked issue tracker is the real source of truth.

- **[v0.6 — She Reaches Out](docs/spec/v0.6.md)** — Scheduler phase 2 ✅, weather ✅, mood tracking ✅ (redesigned, see `plans/PLAN-mood-tracking-redesign.md`), morning briefing → **superseded by scheduled worker briefings**. Todoist / GitHub issues: **removed**, not shipped
- **[v0.7 — She Adapts](docs/spec/v0.7.md)** — Model fallbacks ✅, cloud sync via D1 ✅
- **[v0.8 — She Has a Face](docs/spec/v0.8.md)** — Telegram Mini Apps: **not pursued**. The browser UI shipped as `gateway/chat.html` instead
- **[v0.9 — She Collects](docs/spec/v0.9.md)** — Link saving, Mini Shutter, reader view, highlights: **not pursued**
- **[v1.0 — She Helps](docs/spec/v1.0.md)** — Receipt scanning / expenses ✅. Grocery list ✅. Auto-journaling and code sandbox: **not pursued**
- **[v1.1 — She Reads Your World](docs/spec/v1.1.md)** — Obsidian vault, IMAP email (Gmail bridge shipped instead), local Wikipedia: **not pursued**

### Architecture Vision

- **[v2 — Architecture Vision](docs/spec/v2.md)** — Heartbeat (MiraDO), leader election, D1 sync, Workshop (Docker sandbox), `/update`, reactive triggers

---

## Project Structure

```
her-go/
├── main.go              # Entry point → cobra root
├── cmd/                 # CLI: run, setup, start/stop/status, sim, sync, reembed, logs, tunnel, …
├── gateway/             # Pipeline + Telegram/Gradio adapters (SSE web UI, sim adapter)
├── bot/                 # Telegram handler, frontends, per-turn orchestration (run_agent.go)
├── agent/               # Driver, memory, mood, introspection, persona, dream agents
├── agent_engine/        # Bridge onto Nova's agent loop (her-go keeps the contracts)
├── llm/                 # OpenRouter client on Nova's provider layer (fallback, cost)
├── layers/              # Prompt layer registry (facts, persona, time, weather, …)
├── memory/              # SQLite store, SyncedStore (D1), cards, context assembly, vec index
├── mood/                # Mood agent, vocab loader, daily rollup, graphs
├── persona/             # Reflections, persona evolution, traits, dream cycle
├── classifier/          # Memory + reply classifiers (classifiers.yaml)
├── compact/             # Conversation + agent-history compaction
├── scrub/               # Tiered PII detection + deanonymization
├── embed/               # Embedding client (OpenRouter pplx-embed)
├── search/              # Web search (Tavily / SearXNG) + Open Library
├── integrate/           # Geocoding (Nominatim), Foursquare places, timezone
├── calendar/            # Apple EventKit bridge (Swift CLI + Bridge/CLIBridge/FakeBridge)
├── gmail/               # Gmail bridge (API client + FakeBridge) and MIME parsing
├── weather/             # Current conditions via Open-Meteo (no API key)
├── vision/              # Image understanding via VLM
├── voice/               # STT (Parakeet) + TTS (Piper / ElevenLabs / OpenRouter via Nova)
├── scheduler/           # Extension-based cron system (registry, runner, retry policy)
├── tools/               # Tool YAML manifests + handlers (per-tool dirs, agent routing)
├── trace/               # Trace streams + board (agents share one Telegram message)
├── turn/                # Turn phase tracking + per-turn cost/metrics
├── tui/                 # Event bus + terminal UI
├── workeragent/         # Background research/report worker (Telegraph publishing)
├── telegraph/           # Telegraph API client
├── d1/                  # Cloudflare D1 HTTP client (outbox + sync)
├── procmgr/             # Process supervision (start/stop/status)
├── calendar/bridge/     # Swift package: her-calendar EventKit CLI
├── sims/                # Simulation suites (YAML scenarios)
├── scripts/             # Piper TTS server and helpers
├── web/                 # Svelte chat scaffold — not wired into the Go server
├── docs/                # Architecture, specs, plans
├── prompt.md            # Base system prompt (static, user-authored, hot-reloadable)
├── driver_agent_prompt.md
├── memory_agent_prompt.md
├── mood_agent_prompt.md
├── introspection_agent_prompt.md
├── persona.md           # Evolving personality (bot-authored, versioned in DB)
├── config.yaml.example  # Template config
├── her.db               # SQLite database (created at runtime, gitignored)
├── config.yaml          # Configuration (gitignored)
├── go.mod
└── CLAUDE.md            # Instructions for the agent harness
```

---

## Dependencies (Go Modules)

| Package | Purpose | Since |
|---|---|---|
| `gopkg.in/telebot.v4` | Telegram bot framework | v0.1 |
| `github.com/mattn/go-sqlite3` | SQLite driver (CGo) | v0.1 |
| `github.com/asg017/sqlite-vec-go-bindings/cgo` | Vector search (KNN) for memory + mood recall | v0.4 |
| `gopkg.in/yaml.v3` | Config parsing | v0.1 |
| `github.com/robfig/cron/v3` | Cron expression parsing for scheduler (next_run computation) | v0.6 |
| `github.com/AutumnsGrove/nova` | Agent loop and OpenRouter transport (agent, provider, voice) | v1.0 |
| `charm.land/bubbletea/v2` | Terminal UI | v0.5 |
| `github.com/spf13/cobra` | CLI commands | v0.1 |
| `github.com/golang-migrate/migrate/v4` | Forward-only embedded migrations | v0.3 |
| `golang.org/x/oauth2`, `google.golang.org/api` | Gmail bridge credentials | v1.0 |

Minimal dependency footprint for the scope. The memory system, PII scrubber and every tool handler are custom. New dependencies are added only when they solve a problem that can't be reasonably hand-rolled (CGo bindings, protocol implementations). Earlier drafts of this table listed `goquery`, `fsnotify`, `go-imap` and the `macos-vision-ocr` binary for planned features that were never built — dropped during the 2026-11 sweep.

---

## Security & Privacy Notes

- **API keys** stored in environment variables, never in config files committed to git
- **`her.db`** is gitignored — contains all personal data
- **Tiered PII scrubbing** happens before any network call — hard identifiers (SSNs, card numbers) are redacted; contact info (phone, email) is tokenized and deanonymized in responses; names and context pass through for conversational coherence
- **PII vault** (token↔original mappings) is stored locally and never transmitted
- **Telegram bot token** has access only to messages sent directly to the bot
- **No telemetry, no analytics, no external logging** — everything is local
- The only external network calls are: Telegram API (receiving/sending messages), OpenRouter API (LLM inference — with hard identifiers stripped and contact info tokenized; names and conversational context are sent intact for coherence), and optionally: weather (Open-Meteo), geocoding (Nominatim), places (Foursquare), web search (Tavily / SearXNG), Gmail, Apple Calendar via the local XPC bridge, and Telegraph for publishing reports
- **Email content** gets stricter scrubbing than personal messages — other people's PII is Tier 2 tokenized, not Tier 3 passthrough
- **Node modules / build output** are gitignored; no personal data is written outside `her.db`, the vault, and `reports/`

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

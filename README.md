# her

A privacy-first personal companion chatbot, inspired by the movie *Her*. Built in Go.

Mira is an AI companion you chat with on Telegram. She remembers your conversations, evolves her personality over time, tracks your mood in the background, searches the web, understands images and voice, and keeps your private data local. Three specialized agents share the work — orchestration, memory, mood — and report back through a shared trace inbox you can watch as it happens.

## Quick Start

```bash
# Clone and enter
git clone <your-repo-url>
cd her-go

# Copy config and fill in your API keys
cp config.yaml.example config.yaml
# Edit config.yaml: add TELEGRAM_BOT_TOKEN, OPENROUTER_API_KEY, TAVILY_API_KEY

# Run directly
go run main.go run

# Or build and run
go build -o her && ./her run
```

## Architecture

```
You (Telegram) → her binary → driver agent (Qwen3 235B, orchestrates the turn)
                                ├── think (reason about what to do)
                                ├── recall_memories (semantic fact search)
                                ├── use_tools → search  (web_search, web_read, search_books)
                                │             / context (get_weather, set_location, nearby_search)
                                │             / calendar (calendar_list, calendar_create, shift_hours)
                                ├── reply (calls Kimi K2, sends response)
                                └── done (signals turn complete)
                              ↓ (after reply sent — these run in parallel)
                              memory agent (Kimi K2)        mood agent (Kimi K2)
                                ├── save_memory               ├── infer valence + labels
                                ├── update_memory             ├── classifier check
                                ├── remove_memory             ├── KNN dedup
                                └── done                      └── auto-log / propose / drop
                                  ↑ (each write gated by)         ↑
                                  classifier (Gemini 3.1 Flash Lite) — fail-open one-shot verdicts
                              ↓
                              scheduler (cron-driven extensions)
                                └── mood_daily_rollup at 21:00 local
```

Every message goes through the driver agent first — it decides whether to think, search, recall memories, or reply. The reply model generates the natural-language response when the agent calls `reply`. After the reply is sent, the memory and mood agents run **in parallel** in background goroutines, both writing into a shared **trace inbox** if `/traces` is on. The user never waits for either.

For a deep dive into model calls, data flow, and the dual compaction system, see [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## The Gang (agents + models)

Three agents do the LLM-driven work — they have their own loops, run autonomously, and produce traces. Three more LLM roles (reply, classifier, vision) are one-shot calls invoked by the agents — same kind of model, simpler shape: one input, one verdict, no loop. Local models handle embeddings, speech, and audio.

| Role | Type | Model | Via | When it runs |
|---|---|---|---|---|
| **driver** | agent | Qwen3 235B (`qwen/qwen3-235b-a22b-2507`) | OpenRouter | every turn (foreground) |
| **memory** | agent | Kimi K2 (`moonshotai/kimi-k2-0905`) | OpenRouter → Groq | after each reply (background) |
| **mood** | agent | Kimi K2 (`moonshotai/kimi-k2-0905`) | OpenRouter → Groq | after each reply (background, parallel to memory) |
| reply | tool LLM | Kimi K2 (`moonshotai/kimi-k2-0905`) | OpenRouter → Groq | called by driver via the `reply` tool |
| classifier | tool LLM | Gemini 3.1 Flash Lite (`google/gemini-3.1-flash-lite-preview`) | OpenRouter | called by memory + mood to gate writes (fail-open) |
| vision | tool LLM | Gemini 3 Flash (`google/gemini-3-flash-preview`) | OpenRouter | called by driver via `view_image` |
| embeddings | local | Nomic Embed Text | Local (Ollama) | semantic dedup for memories + moods |
| STT | local | Parakeet | Local | voice memo → text |
| TTS | local | Piper | Local | text → voice reply |

Each agent registers itself with the **trace inbox** (`trace/`) at init time — adding a fourth agent later is one `trace.Register(...)` call from its package.

## Features

- **Multi-agent pipeline** — main orchestrates, memory remembers, mood tracks state of mind. Each runs only when needed; the user waits on none of them. Reply, classifier, and vision are one-shot LLM calls the agents make.
- **Memory system** — Facts extracted by a background agent, stored in SQLite, semantically deduplicated via local embeddings, gated by a classifier LLM call before they land.
- **Mood tracking (Apple State of Mind style)** — A dedicated agent infers valence (1–7), labels, and life-area associations from the turn. High confidence auto-logs; medium sends a Telegram proposal with inline buttons; low drops silently. Embedding-based dedup over a sliding window. Charts via `/mood week|month|year` (PNG, color-coded by valence). Manual `/mood` opens a 4-step wizard. See [docs/plans/PLAN-mood-tracking-redesign.md](docs/plans/PLAN-mood-tracking-redesign.md).
- **Trace inbox** — `/traces` lights up a single shared Telegram message with one slot per agent (main → memory → mood). Slots render in registry-defined order regardless of which agent finishes first. Adding a new agent is one `trace.Register` call.
- **Scheduler** — Extension-based cron system (`scheduler/`). Domain packages register `task.yaml` files at init time; the runner dispatches due tasks every 30s with per-task retry policy. Currently powers the **daily mood rollup** at 21:00 local; designed to host more (reminders, weekly digests) without scheduler edits.
- **Persona evolution** — Mira reflects on conversations and rewrites her own personality description over time, with trait tracking and damped updates.
- **Web search** — Tavily integration for real-time information, loaded on demand via `use_tools`.
- **Book search** — Open Library lookup for titles, authors, topics, or ISBNs. No API key, loaded on demand alongside web search.
- **Weather + location** — On-demand current conditions via Open-Meteo. `set_location` geocodes a city, address, landmark, or raw coords (via Nominatim) and persists to both `config.yaml` and the `location_history` table so Mira remembers where "home" is across restarts.
- **Vision** — Image understanding via Gemini Flash, loaded on demand via `use_tools`.
- **Voice** — Local speech-to-text (Parakeet) and text-to-speech (Piper).
- **PII scrubbing** — Tiered: hard identifiers redacted, contact info tokenized + deanonymized, names pass through. The mood agent only ever sees scrubbed text.
- **Dual compaction** — Separate compaction streams for chat (conversation flow) and agent (tool call history), each with independent budgets and summaries.
- **Sim harness** — Scripted message suites for regression testing, model comparison, and threshold tuning. `run_dream: true` forces a nightly reflection at the end of a sim; `run_rollup: true` does the same for the daily mood rollup so you can verify both without waiting on cron.
- **Full observability** — Agent turns, tool calls, classifier verdicts, mood inferences, scheduled tasks, metrics — all stored in SQLite.

## Commands (Telegram)

| Command | Description |
|---|---|
| `/status` | Uptime, models, services, session stats |
| `/stats` | Detailed usage: messages, tokens, cost |
| `/facts` | List all active facts with IDs |
| `/forget <id>` | Soft-delete a fact |
| `/reflect` | Trigger Mira to write a reflection |
| `/persona` | View current personality description |
| `/persona history` | View past persona versions |
| `/compact` | Force conversation compaction |
| `/traces` | Toggle trace inbox visibility (main/memory/mood in one shared message) |
| `/mood` | Manual mood entry — multi-step wizard (valence → labels → associations → optional note) |
| `/mood week` | PNG chart of the last 7 days of mood entries |
| `/mood month` | PNG chart of the last 30 days |
| `/mood year` | PNG chart of the last 365 days |

## CLI Commands

```bash
her run      # Start the bot (foreground, with TUI)
her sim      # Run scripted simulation suites (supports run_dream + run_rollup flags)
her shape    # Show what fills each model's context window (per-layer token breakdown)
her logs     # Tail logs (--stderr, --lines N)
```

## Configuration

Copy `config.yaml.example` to `config.yaml` and fill in:

- `telegram.token` — from @BotFather on Telegram
- `llm.api_key` — from OpenRouter
- `chat.model` — reply model (default: `moonshotai/kimi-k2-0905` via Groq)
- `driver.model` — orchestration model (default: `qwen/qwen3-235b-a22b-2507`)
- `memory_agent.model` — memory extraction model (default: `moonshotai/kimi-k2-0905`)
- `mood_agent.model` — mood inference model (same default as memory; empty disables the entire mood pipeline)
- `classifier.model` — memory/mood write gate (default: `google/gemini-3.1-flash-lite-preview`)
- `mood.*` — confidence thresholds, dedup window, daily rollup cron
- `search.tavily_api_key` — from Tavily (free tier: 1000 searches/month)
- `embed.base_url` — local embedding server (Ollama recommended)
- `embed.model` — embedding model name (default: `nomic-embed-text`)
- `voice` — Parakeet STT and Piper TTS server URLs
- `location` — home coordinates + unit prefs. Populated automatically by the `set_location` tool.
- `calendar` — Apple Calendar integration via EventKit bridge.

## Editable Files (no recompilation needed)

| File | Purpose | Who edits it |
|---|---|---|
| `prompt.md` | Mira's personality, tone, boundaries | You |
| `driver_agent_prompt.md` | Driver agent orchestration rules, tool usage patterns | You |
| `memory_agent_prompt.md` | Memory agent instructions, fact quality rules | You |
| `mood_agent_prompt.md` | Mood agent inference rules | You |
| `mood/vocab.yaml` | Apple-style mood vocab (valence buckets, labels, associations) | You |
| `mood/task.yaml` | Daily rollup cron + retry config | You |
| `persona.md` | Mira's evolving self-image | Mira (automatically) |

All prompts are hot-reloaded from disk on every message.

## Project Structure

```
her-go/
├── cmd/              # CLI commands (Cobra): run, sim, shape, logs, tunnel, sync
├── agent/            # Driver agent + memory agent + classifier + tool dispatch
├── bot/              # Telegram bot, message pipeline, mood wizard, trace wiring
├── calendar/         # Apple Calendar EventKit bridge (Swift CLI + Go wrapper)
├── classifier/       # Memory + mood write classifiers (classifiers.yaml)
├── compact/          # Dual compaction (chat conversations + agent action history)
├── config/           # YAML config loading + env var substitution
├── d1/               # Cloudflare D1 HTTP client for cross-machine sync
├── embed/            # Local embedding client for semantic similarity
├── integrate/        # External integrations (Nominatim geocoding, Foursquare places)
├── layers/           # Prompt layer registry (one file per layer for agent + chat)
├── llm/              # OpenAI-compatible LLM client (fallback, cost tracking)
├── logger/           # Structured logging (charmbracelet/log)
├── memory/           # SQLite store + SyncedStore decorator for D1 mirroring
├── mood/             # Mood agent, vocab loader, daily rollup, PNG graphs
├── persona/          # Reflection, persona evolution, trait tracking, dreaming
├── retry/            # Unified retry package with configurable backoff
├── scheduler/        # Extension-based cron system (registry, runner, retry policy)
├── scrub/            # Tiered PII detection + deanonymization
├── search/           # Tavily web search + Open Library book search
├── tools/            # Tool YAML manifests + handlers (init-registered, per-tool directories)
├── trace/            # Trace inbox (Stream registry + Board); driver/memory/mood share one message
├── tui/              # Terminal UI events and rendering
├── turn/             # Turn phase tracking (driver → memory → mood)
├── vision/           # Image understanding via Gemini Flash
├── voice/            # Parakeet STT + Piper TTS clients
├── weather/          # Current conditions via Open-Meteo (no API key)
├── worker/           # Cloudflare Worker for webhook routing (Wrangler project)
├── sims/             # Simulation suites + results
├── docs/             # Architecture docs
├── _junkdrawer/      # Archived code (old skills system, deprecated tools)
├── prompt.md         # Mira's personality
├── driver_agent_prompt.md   # Driver agent behavior rules
├── memory_agent_prompt.md   # Memory agent instructions
├── mood_agent_prompt.md     # Mood agent instructions
├── persona.md        # Mira's evolving self-image (bot-authored)
└── config.yaml       # Your configuration (gitignored)
```

## Privacy

- All conversation data, memories, and mood entries stay in local SQLite (`her.db`, gitignored)
- Hard identifiers (SSN, credit cards) are redacted before reaching any LLM
- Contact info (phone, email) is tokenized and deanonymized in responses
- Names and context pass through for conversational coherence
- The mood agent **only ever sees PII-scrubbed text** — same firewall as the chat model
- Voice processing runs entirely locally via Parakeet and Piper
- External services used only when the matching tool is invoked: Tavily (web search), Open Library (book search), Open-Meteo (weather), Nominatim (geocoding). All four are free; only Tavily needs a key. Everything else stays on your machine.

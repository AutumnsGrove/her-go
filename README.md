# her

A privacy-first personal companion chatbot, inspired by the movie *Her*. Built in Go.

Mira is an AI companion you chat with on Telegram. She remembers your conversations, evolves her personality over time, tracks your mood, reflects on herself, and consolidates memories while you sleep. She can search the web, understand images and voice, and keeps all your private data local on your machine.

Behind the scenes, five specialized agents work in concert: one orchestrates each conversation turn, another extracts and organizes memories, a third tracks emotional patterns, a fourth generates self-reflections about your relationship, and a fifth runs nightly to consolidate everything she's learned. All of this happens transparently — if you turn on traces, you can watch the whole pipeline unfold in real-time.

## Quick Start

Getting started takes about 5 minutes:

```bash
# Clone the repo
git clone https://github.com/AutumnsGrove/her-go.git
cd her-go

# Copy the example config and add your API keys
cp config.yaml.example config.yaml
# Edit config.yaml and add:
#   - TELEGRAM_BOT_TOKEN (from @BotFather on Telegram)
#   - OPENROUTER_API_KEY (from openrouter.ai)
#   - TAVILY_API_KEY (optional, for web search - free tier available)

# Install dependencies and download voice models (optional)
go run main.go setup

# Start chatting
go run main.go run
```

The setup command installs local dependencies (UV for Python tools, Parakeet for STT on Apple Silicon) and downloads the default Piper TTS voice model (`en_GB-southern_english_female-low`). If you just want to get started with text chat, you can skip setup and run immediately — voice features are optional.

See [Configuration](#configuration) below for config details, [Voice Setup](#voice-setup) for enabling voice messages, [Resource Requirements](#resource-requirements) for minimal setups, or [docs/SETUP.md](docs/SETUP.md) for detailed guides on calendar integration, cross-machine sync, and deployment.

## Architecture

```
You (Telegram) → her binary → driver agent (Qwen3 235B, orchestrates the turn)
                                ├── think (reason about what to do)
                                ├── recall_memories (semantic fact search)
                                ├── use_tools → search   (web_search, web_read, search_books)
                                │             / context  (get_weather, set_location, nearby_search)
                                │             / calendar (calendar_list, calendar_create, shift_hours)
                                ├── reply (calls Kimi K2, sends response)
                                └── done (signals turn complete)
                              ↓ (after reply sent — background goroutine chain)
                              memory agent (Qwen3 235B)
                                ├── save_memory / update_memory / remove_memory
                                ├── create_card / read_card / merge_memories
                                └── done  ← each write gated by classifier (Gemini Flash Lite)
                              ↓
                              mood agent (Qwen3 235B)
                                ├── infer valence + labels + confidence
                                ├── classifier check → KNN dedup
                                └── auto-log / propose / drop
                              ↓
                              introspection agent (Qwen3 235B)
                                ├── save_self_memory (bot's own reflections)
                                └── done  ← pre-filter skips informational turns
                              ↓ (nightly dream cycle — 04:00 local)
                              dream agent (Qwen3 235B)
                                ├── merge_memories / expire_memory / update_card
                                └── consolidate clusters, maintain card summaries
                              persona agent (Qwen3 235B)
                                └── rewrite persona.md from accumulated reflections
                              ↓
                              scheduler (cron-driven extensions)
                                └── mood_daily_rollup at 21:00 local
```

Here's how a typical conversation flows: when you send a message, the **driver agent** wakes up and decides what to do. It might think about your message, search for current information, recall relevant memories, or just reply directly. When it's ready to respond, it calls the reply model to generate natural language, and that's what you see in Telegram.

But the interesting part happens *after* you get your reply. Three background agents kick off in sequence — you never wait for them, but they're doing important work. The **memory agent** reviews the conversation and extracts facts worth remembering. The **mood agent** infers your emotional state and logs it if the confidence is high enough. The **introspection agent** generates self-memories: Mira's own reflections about the conversation, the relationship, and her behavior. If you have `/traces` enabled, all of this appears in a single shared message so you can watch the pipeline unfold.

Then, while you sleep, two more agents run at 4 AM: the **dream agent** consolidates memories (merging duplicates, expiring old facts, organizing clusters), and the **persona agent** rewrites Mira's personality description based on accumulated reflections. It's a form of offline processing — she literally sleeps on it.

For a deep dive into model calls, data flow, and the dual compaction system, see [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## The Gang (agents + models)

Mira uses a mix of agents (autonomous LLM loops with their own decision-making) and tool LLMs (one-shot calls for specific tasks). The five agents — driver, memory, mood, introspection, and dream — each have their own prompt, tools, and trace stream. The tool LLMs (reply, classifier, vision) are simpler: they get called by agents, return one answer, and that's it. Local models handle embeddings (for semantic search), speech-to-text, and text-to-speech, so your voice data never leaves your machine.

| Role | Type | Model | Via | When it runs |
|---|---|---|---|---|
| **driver** | agent | Qwen3 235B (`qwen/qwen3-235b-a22b-2507`) | OpenRouter | every turn (foreground) |
| **memory** | agent | Qwen3 235B (`qwen/qwen3-235b-a22b-2507`) | OpenRouter | after each reply (background) |
| **mood** | agent | Qwen3 235B (`qwen/qwen3-235b-a22b-2507`) | OpenRouter | after memory (background) |
| **introspection** | agent | Qwen3 235B (`qwen/qwen3-235b-a22b-2507`) | OpenRouter | after mood (background) |
| **dream** | agent | Qwen3 235B (`qwen/qwen3-235b-a22b-2507`) | OpenRouter | nightly dream cycle (04:00 local) |
| **persona** | agent | Qwen3 235B (`qwen/qwen3-235b-a22b-2507`) | OpenRouter | nightly dream cycle (after dream) |
| reply | tool LLM | Kimi K2 (`moonshotai/kimi-k2-0905`) | OpenRouter → Groq | called by driver via the `reply` tool |
| classifier | tool LLM | Gemini 3.1 Flash Lite (`google/gemini-3.1-flash-lite`) | OpenRouter | called by memory + mood to gate writes (fail-open) |
| vision | tool LLM | Gemini 3 Flash (`google/gemini-3-flash-preview`) | OpenRouter | called by driver via `view_image` |
| embeddings | local | Nomic Embed Text | Local (Ollama) | semantic dedup for memories + moods |
| STT | local | Parakeet | Local | voice memo → text |
| TTS | local | Piper | Local | text → voice reply |

Each agent registers itself with the **trace inbox** (`trace/`) at init time — adding a new agent is one `trace.Register(...)` call from its package.

## What Mira Can Do

### Memory & Learning
Mira doesn't just respond — she remembers. A background agent extracts facts from your conversations and stores them in SQLite, using local embeddings to avoid duplicates. Before any memory hits the database, a classifier LLM checks it for quality (catching fictional content, low-value facts, or inferred information that wasn't actually stated). 

Memories are organized into **memory cards** — think of them as folders with summaries. The dream agent maintains these automatically, grouping related memories and keeping a "table of contents" so Mira knows what she knows.

### Self-Awareness
After each conversation, the introspection agent generates **self-memories**: Mira's own reflections about the conversation, the relationship, and her behavior. These get auto-injected into future context, so her self-awareness compounds over time. During the nightly dream cycle, the persona agent uses these reflections to rewrite `persona.md` — Mira's personality description. Trait changes are damped to keep evolution gradual and grounded.

### Mood Tracking
Inspired by Apple's State of Mind feature, Mira tracks emotional patterns with a dedicated agent that infers valence (1–7 scale), labels (anxious, excited, content, etc.), and life-area associations. High-confidence moods auto-log; medium-confidence moods send a Telegram proposal with inline buttons for you to approve or refine; low-confidence moods drop silently. Embedding-based dedup prevents redundant entries. You can view charts with `/mood week`, `/mood month`, or `/mood year` — PNG graphs color-coded by valence.

### Real-World Integration
- **Web search** — Tavily integration for current information (free tier: 1000 searches/month)
- **Book lookup** — Open Library search for titles, authors, ISBNs (no API key needed)
- **Places search** — Foursquare API for finding nearby restaurants, cafes, bars, shops, etc. (free tier: 10k calls/month)
- **Weather** — Current conditions via Open-Meteo. The `set_location` tool geocodes addresses and persists your home location
- **Vision** — Send images and Mira describes them via Gemini Flash
- **Voice** — Local speech-to-text (Parakeet) and text-to-speech (Piper) — your audio never leaves your machine
- **Calendar** — Apple Calendar integration via EventKit (macOS only) — read events, create shifts, manage schedules

### Privacy First
Hard identifiers (SSN, credit cards) are redacted before reaching any LLM. Contact info (phone, email) is tokenized and deanonymized in responses. Names and context pass through for conversational coherence. The mood agent only ever sees PII-scrubbed text — same firewall as the chat model. Voice processing runs entirely locally.

### Developer Features
- **Trace inbox** — `/traces` shows you the whole pipeline in one Telegram message: main → memory → mood → introspection, updating in real-time as each agent finishes
- **Sim harness** — YAML-defined test suites for regression testing and model comparison
- **Dual compaction** — Separate compaction streams for chat history and agent action history, each with independent token budgets
- **D1 sync** — Optional Cloudflare D1 mirroring for cross-machine shared state (laptop ↔ server)
- **Full observability** — Every agent turn, tool call, classifier verdict, and mood inference is stored in SQLite

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
| `/traces` | Toggle trace inbox visibility (main/memory/mood/introspection in one shared message) |
| `/mood` | Manual mood entry — multi-step wizard (valence → labels → associations → optional note) |
| `/mood week` | PNG chart of the last 7 days of mood entries |
| `/mood month` | PNG chart of the last 30 days |
| `/mood year` | PNG chart of the last 365 days |

## CLI Commands

```bash
her run       # Start the bot (foreground, with TUI)
her dev       # Development mode (webhook + cloudflared tunnel)
her start     # Start as a background launchd service
her stop      # Stop the background service
her status    # Check service status
her sim       # Run scripted simulation suites (supports run_dream + run_rollup flags)
her shape     # Show what fills each model's context window (per-layer token breakdown)
her sync      # D1 cross-machine sync (push/pull)
her logs      # Tail logs (--stderr, --lines N)
her tunnel    # Cloudflare Tunnel management (setup, start)
her setup     # Interactive first-run setup
```

## Resource Requirements

Mira runs on surprisingly modest hardware. The main process uses about 35–50 MB of RAM, and Ollama (for embeddings) adds another 50 MB. You can run the entire stack on a Raspberry Pi or similar single-board computer with as little as 1 GB of RAM.

### Minimal Setup (1 GB RAM / Low-Power SBC)

This configuration runs successfully on a Le Potato (ARM64, 2 GB RAM, similar to Raspberry Pi 3):

**What runs locally:**
- `her-go` binary (35 MB RAM)
- Ollama + nomic-embed-text (50 MB RAM)

**What runs remotely:**
- All LLM calls (driver, chat, memory agents) → OpenRouter
- Voice transcription → OpenRouter (`nvidia/parakeet-tdt-0.6b-v3`)
- Voice synthesis → skipped (TTS disabled to save resources)

**Config tweaks for low-memory setups:**
```yaml
voice:
  enabled: true
  stt:
    engine: "whisper"  # use remote STT instead of local Parakeet
    base_url: "https://openrouter.ai/api/v1"
    model: "nvidia/parakeet-tdt-0.6b-v3"  # or "openai/whisper-large-v3-turbo"
  tts:
    enabled: false  # disable TTS to save RAM, or use a remote service

embed:
  base_url: "http://localhost:11434/v1"
  model: "nomic-embed-text"  # lightweight 768-dim embeddings (50 MB)
```

### Standard Setup (8+ GB RAM / Desktop/Laptop)

**What runs locally:**
- `her-go` binary
- Ollama + nomic-embed-text
- Parakeet STT (Apple Silicon only, ~200 MB)
- Piper TTS (~100 MB)

**What runs remotely:**
- LLM calls → OpenRouter

This gives you full privacy for voice processing while keeping the heavy lifting (LLM inference) in the cloud.

### Resource Summary

| Component | RAM | Notes |
|---|---|---|
| `her-go` binary | 35–50 MB | Main process |
| Ollama (nomic-embed-text) | 50 MB | Semantic search embeddings |
| Parakeet STT (local) | ~200 MB | Apple Silicon only, optional |
| Piper TTS (local) | ~100 MB | Optional, can use remote TTS instead |
| **Total (minimal)** | **~100 MB** | Remote voice via OpenRouter |
| **Total (full local voice)** | **~400 MB** | All voice processing on-device |

LLM inference happens remotely via OpenRouter, so even a tiny SBC can run a fully-featured instance. The only constraint is network latency — each turn makes 1–3 LLM calls depending on whether the driver agent needs to think, search, or use tools.

## Voice Setup

Mira supports both voice input (speech-to-text) and voice output (text-to-speech). Voice processing can run entirely locally for privacy, or you can use remote APIs for lower resource usage. Voice is optional — the bot works fine with text-only.

### Speech-to-Text (STT)

Two engines are available:

**Parakeet (local)** — recommended for macOS/Apple Silicon with 4+ GB RAM:
- Local processing via MLX framework (~200 MB RAM)
- Automatically spawned as a sidecar when you run `her run`
- No API key needed, fully private
- Install: `her setup` handles this automatically

**Whisper or remote Parakeet** — for any platform, or low-memory setups:
- Remote processing via OpenRouter or OpenAI
- Works on Raspberry Pi, VPS, or any platform
- Minimal memory footprint (no local models)
- Requires API key (can reuse your OpenRouter key)
- OpenRouter offers both Whisper and NVIDIA's Parakeet models

To enable STT, edit `config.yaml`:
```yaml
voice:
  enabled: true
  stt:
    engine: "parakeet"  # or "whisper" for remote
    # ... other settings auto-configured
```

### Text-to-Speech (TTS)

Mira uses **Piper** for TTS — a fast, privacy-first local engine with natural-sounding voices.

**Setup:**

1. **Run `her setup`** to download the default voice model:
   - Model: `en_GB-southern_english_female-low` (British English, female voice)
   - Downloads to `scripts/piper-voices/`
   - Files: `.onnx` model weights (22 MB) + `.onnx.json` config
   - This happens automatically during setup alongside other dependencies

2. **Enable TTS in `config.yaml`**:
   ```yaml
   voice:
     tts:
       enabled: true
       model: "en_GB-southern_english_female-low"
       reply_mode: "voice"  # always reply with voice
       # or "match" to reply in same format as input
   ```

**Choosing a different voice:**

Piper has 100+ voices in many languages. Browse them at [rhasspy/piper-voices on HuggingFace](https://huggingface.co/rhasspy/piper-voices).

To add a new voice:
1. Download both the `.onnx` and `.onnx.json` files to `scripts/piper-voices/`
2. Update `config.yaml`:
   ```yaml
   voice:
     tts:
       model: "en_US-amy-medium"  # example: US English
   ```

The model name should match the filename without the `.onnx` extension.

**How it works:**

When you start `her run`, the bot automatically spawns a Piper TTS server as a sidecar process (listening on `localhost:8766` by default). The server loads your chosen voice model and accepts text-to-speech requests via an OpenAI-compatible API. When Mira generates a reply, she synthesizes it to audio and sends you an Opus-encoded voice message on Telegram.

## Configuration

The config file (`config.yaml`) controls everything from API keys to model selection to feature toggles. Copy `config.yaml.example` to get started:

```bash
cp config.yaml.example config.yaml
```

### Essential Settings (minimum viable config)

You need these to get started:

- **`telegram.token`** — Get this from [@BotFather](https://t.me/BotFather) on Telegram. Create a new bot and it'll give you a token. **Required.**
- **`openrouter.api_key`** — Sign up at [openrouter.ai](https://openrouter.ai) for access to 200+ models via one API. Free credits to start. **Required.**
- **`search.tavily_api_key`** — Optional but recommended for web search. Free tier: 1000 searches/month. Sign up at [tavily.com](https://tavily.com).
- **`foursquare.api_key`** — Optional for places search (restaurants, cafes, etc.). Free tier: 10k calls/month. Generate a Service API Key at [foursquare.com/developers](https://foursquare.com/developers).

That's it. The defaults for everything else (models, thresholds, etc.) are already set to sensible values.

### Model Configuration

Mira uses different models for different jobs. The defaults use MiMo v2.5 for most tasks (via the `xiaomi/fp8` provider for excellent prompt caching — ~80% cache hit rate). You can change these if you want different cost/quality tradeoffs:

- **`chat.model`** — Generates the natural-language replies you see in Telegram (default: `xiaomi/mimo-v2.5-pro`)
- **`driver.model`** — Orchestrates each turn, decides when to think/search/recall/reply (default: `xiaomi/mimo-v2.5`)
- **`memory_agent.model`** — Extracts facts in the background after each reply (default: `xiaomi/mimo-v2.5`)
- **`classifier.model`** — Fast safety gate for memory/mood writes (default: `google/gemini-3.1-flash-lite`)
- **`vision.model`** — Describes images (default: `xiaomi/mimo-v2.5`)

The other agents (`mood_agent`, `introspection_agent`, `dream_agent`, `persona_agent`) fall back to the memory agent model if you don't specify them explicitly.

### Local Services

These run on your machine for privacy-first processing:

- **`embed.base_url`** — Embedding server for semantic search. Defaults to Ollama at `http://localhost:11434/v1`. Run `ollama pull nomic-embed-text` to download the model.
- **`voice.stt` / `voice.tts`** — Speech engines. See [Voice Setup](#voice-setup) above.

### Feature Toggles

- **`mood_agent.model`** — Set to empty string (`""`) to completely disable mood tracking
- **`voice.enabled`** — Set to `true` to accept voice memos
- **`voice.tts.enabled`** — Set to `true` to reply with voice
- **`dream.enabled`** — Set to `false` to disable nightly dream cycle (memory consolidation + persona evolution)
- **`background_agents.substance_gate`** — Set to `false` to run memory/mood/introspection on *every* turn instead of batching

### Advanced Configuration

See `config.yaml.example` for the full reference with comments. Notable sections:

- **`memory.recall.*`** — Tune the blended retrieval formula (similarity vs. importance vs. recency)
- **`mood.*`** — Confidence thresholds, dedup settings, daily rollup time
- **`dream.forgetting.*`** — Conservative forgetting rules (disabled by default)
- **`d1_database_id`** — Enable Cloudflare D1 sync for cross-machine shared state
- **`calendar.jobs`** — Define your work schedules for shift tracking
- **`gateway.adapters`** — Multi-adapter transport (Telegram + Gradio dev UI)

## Editable Files (no recompilation needed)

| File | Purpose | Who edits it |
|---|---|---|
| `prompt.md` | Mira's personality, tone, boundaries | You |
| `driver_agent_prompt.md` | Driver agent orchestration rules, tool usage patterns | You |
| `memory_agent_prompt.md` | Memory agent instructions, fact quality rules | You |
| `mood_agent_prompt.md` | Mood agent inference rules | You |
| `introspection_agent_prompt.md` | Introspection agent self-reflection rules | You |
| `mood/vocab.yaml` | Apple-style mood vocab (valence buckets, labels, associations) | You |
| `mood/task.yaml` | Daily rollup cron + retry config | You |
| `persona.md` | Mira's evolving self-image | Mira (automatically) |

All prompts are hot-reloaded from disk on every message.

## Project Structure

```
her-go/
├── cmd/              # CLI commands (Cobra): run, dev, sim, shape, logs, sync, tunnel, etc.
├── agent/            # Multi-agent orchestrator: driver, memory, mood, introspection, persona, dream
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
├── trace/            # Trace inbox (Stream registry + Board); agents share one message
├── tui/              # Terminal UI event bus and rendering
├── turn/             # Turn phase tracking (driver → memory → mood → introspection)
├── vision/           # Image understanding via Gemini Flash
├── voice/            # Parakeet STT + Piper TTS clients
├── weather/          # Current conditions via Open-Meteo (no API key)
├── worker/           # Cloudflare Worker for webhook routing (Wrangler project)
├── sims/             # Simulation suites + results
├── docs/             # Architecture docs
├── _junkdrawer/      # Archived code (old skills system, deprecated tools)
├── prompt.md                    # Mira's personality
├── driver_agent_prompt.md       # Driver agent behavior rules
├── memory_agent_prompt.md       # Memory agent instructions
├── mood_agent_prompt.md         # Mood agent instructions
├── introspection_agent_prompt.md # Introspection agent self-reflection rules
├── persona.md                   # Mira's evolving self-image (bot-authored)
└── config.yaml                  # Your configuration (gitignored)
```

## Privacy

Your data stays on your machine. Everything — conversation history, extracted memories, mood entries, persona versions — lives in a local SQLite database (`her.db`, gitignored). No cloud sync unless you explicitly enable D1 mirroring.

**PII scrubbing happens in three tiers:**
1. **Hard identifiers** (SSN, credit cards, government IDs) are redacted before any text reaches an LLM
2. **Contact info** (phone numbers, email addresses) is tokenized and stored in a local vault, then deanonymized in Mira's responses
3. **Names and context** pass through for conversational coherence — Mira needs to know your name to talk to you naturally

The mood agent only ever sees PII-scrubbed text — same firewall as the chat model. Voice processing (both STT and TTS) runs entirely locally via Parakeet and Piper, so your audio never leaves your machine.

**External services** are only used when their matching tool is invoked:
- **Tavily** — web search (needs API key, free tier: 1000 searches/month)
- **Foursquare** — places search (needs API key, free tier: 10k calls/month)
- **Open Library** — book lookup (no key needed)
- **Open-Meteo** — weather (no key needed)
- **Nominatim** — geocoding (no key needed)

All five are free or have generous free tiers. Everything else stays local.

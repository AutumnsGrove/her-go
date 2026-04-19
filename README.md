# her

A privacy-first personal companion chatbot, inspired by the movie *Her*. Built in Go.

Mira is an AI companion you chat with on Telegram. She remembers your conversations, evolves her personality over time, searches the web, understands images and voice, and keeps your private data local. Everything runs through a three-model agent system that separates orchestration, conversation, and memory.

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
You (Telegram) → her binary → Qwen3 (agent, orchestrates the turn)
                                 ├── think (reason about what to do)
                                 ├── recall_memories (semantic fact search)
                                 ├── use_tools → search  (web_search, web_read, search_books)
                                 │             / vision  (view_image)
                                 │             / context (get_weather, set_location)
                                 ├── reply (calls Deepseek, sends response)
                                 └── done (signals turn complete)
                               ↓ (after reply sent)
                               Kimi K2.5 (memory agent, background goroutine)
                                 ├── save_fact / save_self_fact
                                 ├── update_fact / remove_fact
                                 └── done
```

Every message goes through the **Qwen3** agent first — it decides whether to think, search, recall memories, or reply. The conversational model (Deepseek V3.2) generates the actual natural language response when the agent calls `reply`. After the reply is sent, a separate **Kimi K2** memory agent reviews the turn in the background and extracts facts worth keeping. The user never waits for memory processing.

For a deep dive into all model calls, data flow, and the dual compaction system, see [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

### Model Stack

| Role | Model | Via |
|---|---|---|
| Agent (orchestration) | Qwen3 235B (`qwen/qwen3-235b-a22b-2507`) | OpenRouter |
| Chat (responses) | Deepseek V3.2 (`deepseek/deepseek-v3.2`) | OpenRouter |
| Memory (post-turn facts) | Kimi K2 (`moonshotai/kimi-k2-0905`) | OpenRouter → Groq |
| Vision (images) | Gemini 3 Flash (`google/gemini-3-flash-preview`) | OpenRouter |
| Classifier (memory quality) | Claude Haiku 4.5 (`anthropic/claude-haiku-4.5`) | OpenRouter |
| Embeddings | Nomic Embed Text v1.5 | Local (LM Studio/Ollama) |
| STT (speech-to-text) | Parakeet | Local |
| TTS (text-to-speech) | Piper | Local |

## Features

- **Three-model pipeline** — Qwen3 orchestrates, Deepseek converses, Kimi remembers. Each model does what it does best.
- **Memory system** — Facts extracted by a background agent, stored in SQLite, semantically deduplicated via local embeddings, quality-gated by a classifier LLM
- **Persona evolution** — Mira reflects on conversations and rewrites her own personality description over time, with trait tracking and damped updates
- **Thinking traces** — Optional `/traces` command shows the agent's tool calls and the memory agent's fact extraction in a single Telegram message above the reply
- **Web search** — Tavily integration for real-time information, loaded on demand via `use_tools`
- **Book search** — Open Library lookup for titles, authors, topics, or ISBNs. No API key, loaded on demand alongside web search
- **Weather + location** — On-demand current conditions via Open-Meteo. `set_location` geocodes a city, address, landmark, or raw coords (via Nominatim) and persists to both `config.yaml` and the `location_history` table so Mira remembers where "home" is across restarts
- **Vision** — Image understanding via Gemini Flash, loaded on demand via `use_tools`
- **Voice** — Local speech-to-text (Parakeet) and text-to-speech (Piper)
- **PII scrubbing** — Tiered: hard identifiers redacted, contact info tokenized + deanonymized, names pass through
- **Dual compaction** — Separate compaction streams for chat (conversation flow) and agent (tool call history), each with independent budgets and summaries
- **Sim harness** — Scripted message suites for regression testing, model comparison, and threshold tuning
- **Full observability** — Agent turns, tool calls, classifier verdicts, metrics, all stored in SQLite

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
| `/traces` | Toggle thinking trace visibility |

## CLI Commands

```bash
her run      # Start the bot (foreground, with TUI)
her sim      # Run scripted simulation suites
her shape    # Show what fills each model's context window (per-layer token breakdown)
her logs     # Tail logs (--stderr, --lines N)
```

## Configuration

Copy `config.yaml.example` to `config.yaml` and fill in:

- `telegram.token` — from @BotFather on Telegram
- `llm.api_key` — from OpenRouter
- `llm.model` — conversational model (default: `deepseek/deepseek-v3.2`)
- `agent.model` — orchestration model (default: `qwen/qwen3-235b-a22b-2507`)
- `memory_agent.model` — memory extraction model (default: `moonshotai/kimi-k2.5`)
- `search.tavily_api_key` — from Tavily (free tier: 1000 searches/month)
- `embed.base_url` — local embedding server (LM Studio, Ollama)
- `embed.model` — embedding model name
- `voice` — paths to local Parakeet STT and Piper TTS binaries
- `location` — home coordinates + unit prefs (`fahrenheit`/`celsius`, `mph`/`kmh`). Populated automatically by the `set_location` tool — you rarely need to edit this by hand.

## Editable Prompts (no recompilation needed)

| File | Purpose | Who edits it |
|---|---|---|
| `prompt.md` | Mira's personality, tone, boundaries | You |
| `main_agent_prompt.md` | Agent orchestration rules, tool usage patterns | You |
| `memory_agent_prompt.md` | Memory agent instructions, fact quality rules | You |
| `persona.md` | Mira's evolving self-image | Mira (automatically) |

All are hot-reloaded from disk on every message.

## Project Structure

```
her-go/
├── cmd/              # CLI commands (Cobra): run, sim, shape, logs
├── agent/            # Agent orchestrator, memory agent, classifier, tool dispatch
│   └── layers/       # Prompt layer registry (one file per layer for agent + chat)
├── bot/              # Telegram bot + message pipeline
├── compact/          # Dual compaction (chat conversations + agent action history)
├── config/           # YAML config loading + env var substitution
├── embed/            # Local embedding client for semantic similarity
├── integrate/        # External integrations (Nominatim geocoding)
├── llm/              # OpenAI-compatible LLM client (fallback, cost tracking)
├── logger/           # Structured logging (charmbracelet/log)
├── memory/           # SQLite store: messages, facts, summaries, metrics, vault, location_history
├── persona/          # Reflection, persona evolution, trait tracking, dreaming
├── scrub/            # Tiered PII detection + deanonymization
├── search/           # Tavily web search + Open Library book search
├── tools/            # Tool YAML manifests + handlers (init-registered)
├── tui/              # Terminal UI events and rendering
├── vision/           # Image understanding via Gemini Flash
├── voice/            # Parakeet STT + Piper TTS
├── weather/          # Current conditions via Open-Meteo (no API key)
├── sims/             # Simulation suites + results
├── docs/             # Architecture docs (model calls, data flow, compaction)
├── prompt.md         # Mira's personality
├── main_agent_prompt.md  # Agent behavior rules
├── persona.md        # Mira's evolving self-image (bot-authored)
└── config.yaml       # Your configuration (gitignored)
```

## Privacy

- All conversation data stays in local SQLite (`her.db`, gitignored)
- Hard identifiers (SSN, credit cards) are redacted before reaching any LLM
- Contact info (phone, email) is tokenized and deanonymized in responses
- Names and context pass through for conversational coherence
- Voice processing runs entirely locally via Parakeet and Piper
- External services used only when the matching tool is invoked: Tavily (web search), Open Library (book search), Open-Meteo (weather), Nominatim (geocoding). All four are free; only Tavily needs a key. Everything else stays on your machine.

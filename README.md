# her

A privacy-first personal companion chatbot, inspired by the movie *Her*. Built in Go.

Mira is an AI companion you chat with on Telegram. She remembers your conversations, evolves her personality over time, searches the web, understands images and voice, manages your schedule, and keeps your private data local. Everything runs through a tool-calling agent that decides what to do on every turn.

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
You (Telegram) → her binary → Mercury (agent model, orchestrates everything)
                                 ├── think (reason about what to do)
                                 ├── find_skill / run_skill (sandboxed skill execution)
                                 ├── web_search / book_search (via skills or built-in)
                                 ├── view_image (Gemini Flash vision)
                                 ├── reply (calls Deepseek, sends response)
                                 ├── save_fact / recall_memories (memory management)
                                 ├── create_reminder / create_schedule (scheduling)
                                 └── done (signals turn complete)
```

Every message goes through the **Mercury** agent first — a diffusion LLM (`inception/mercury-2`) that handles all tool-calling and orchestration. Mercury decides whether to search, remember, schedule, or call a skill. The conversational model (Deepseek V3.2) generates the actual natural language response when the agent calls `reply`. A separate vision model (Gemini Flash) handles image understanding.

### Model Stack

| Role | Model | Via |
|---|---|---|
| Agent (orchestration) | Mercury 2 (`inception/mercury-2`) | OpenRouter |
| Chat (responses) | Deepseek V3.2 (`deepseek/deepseek-v3.2`) | OpenRouter |
| Vision (images) | Gemini 3 Flash (`google/gemini-3-flash-preview`) | OpenRouter |
| OCR (text extraction) | Apple Vision (primary), GLM-OCR (fallback) | Local |
| Embeddings | Nomic Embed Text v1.5 | Local (LM Studio/Ollama) |
| STT (speech-to-text) | Parakeet | Local |
| TTS (text-to-speech) | Piper | Local |

## Features

- **Agent-first pipeline** — Mercury orchestrates every turn: think, search, reply, remember, schedule
- **Skills system** — Extensible sandboxed skills (Go or Python) with a 4-tier trust model, network proxy, and per-skill sidecar databases
- **Memory system** — Facts extracted and stored in SQLite, semantic deduplication via local embeddings, quality gates that reject AI writing tics
- **Persona evolution** — Mira reflects on conversations and rewrites her own personality description over time, with trait tracking and damped updates
- **Scheduling** — Cron-based reminders, mood check-ins, morning briefings, medication reminders, auto-journaling, with quiet hours and rate limiting
- **Web search** — Tavily integration for real-time information
- **Book search** — Open Library integration for book lookups
- **Vision** — Image understanding via Gemini Flash, OCR via Apple Vision
- **Voice** — Local speech-to-text (Parakeet) and text-to-speech (Piper)
- **Weather** — Open-Meteo integration (no API key needed)
- **PII scrubbing** — Tiered: hard identifiers redacted, contact info tokenized + deanonymized, names pass through
- **Conversation compaction** — Older messages summarized to stay within token budget
- **Thinking traces** — Optional `/traces` command shows the agent's decision-making before each reply
- **Full observability** — Agent turns, search queries, skill executions, metrics, all stored in SQLite

## Skills

Skills are self-contained tools (written in Go or Python) that the agent can discover and execute at runtime. They live in the `skills/` directory and are defined by a `skill.md` manifest.

### Trust Tiers

Skills run in a sandbox with permissions based on trust level:

| Tier | What | Network | Timeout | Sidecar DB |
|---|---|---|---|---|
| **1st-party** | Compiled into binary | Full | None | Full DB access |
| **2nd-party** | Hash matches `skill.md` | Direct | 30s | Read/write |
| **3rd-party** | Agent-modified skill | Proxied | 10s | Read-only |
| **4th-party** | Agent-created skill | Proxied | 5s | None |

The **network proxy** provides SSRF protection — it blocks loopback/private IPs and restricts domains to what the skill declares. Skills that haven't been verified get routed through the proxy automatically.

Each skill gets its own **sidecar SQLite database** that stores execution history (args, output, duration, embeddings), enabling the agent to search past results before re-running expensive operations.

### Built-in Skills

- `web_search` — Tavily web search
- `web_read` — Fetch and extract content from URLs
- `book_search` — Open Library book lookups

## Commands (Telegram)

| Command | Description |
|---|---|
| `/status` | Uptime, models, services, session stats |
| `/restart` | Restart via launchd (or clean exit in dev) |
| `/stats` | Detailed usage: messages, tokens, cost |
| `/facts` | List all active facts with IDs |
| `/forget <id>` | Soft-delete a fact |
| `/reflect` | Trigger Mira to write a reflection |
| `/persona` | View current personality description |
| `/persona history` | View past persona versions |
| `/compact` | Force conversation compaction |
| `/clear` | Reset conversation context |
| `/traces` | Toggle thinking trace visibility |

## CLI Commands

```bash
her run      # Start the bot (foreground)
her setup    # Build binary, generate launchd plist, install service
her start    # Start launchd service (runs setup if needed)
her stop     # Stop launchd service
her status   # Show service status
her logs     # Tail stdout logs (--stderr, --lines N)
her trust    # Manage skill trust (hash verification)
```

## Production Deployment (Mac)

```bash
her setup    # one command does everything
```

This builds the binary, generates a device-specific launchd plist, installs it to `~/Library/LaunchAgents`, and starts the service. Mira restarts automatically on crash or reboot.

## Configuration

Copy `config.yaml.example` to `config.yaml` and fill in:

- `telegram.token` — from @BotFather on Telegram
- `llm.api_key` — from OpenRouter
- `llm.model` — conversational model (default: `deepseek/deepseek-v3.2`)
- `agent.model` — tool-calling model (default: `inception/mercury-2`)
- `search.tavily_api_key` — from Tavily (free tier: 1000 searches/month)
- `embed.base_url` — local embedding server (LM Studio, Ollama)
- `embed.model` — embedding model name
- `weather` — location coordinates for Open-Meteo (no API key needed)
- `voice` — paths to local Parakeet STT and Piper TTS binaries
- `scheduler` — timezone, quiet hours, max proactive messages per day

## Editable Prompts (no recompilation needed)

| File | Purpose | Who edits it |
|---|---|---|
| `prompt.md` | Mira's personality, tone, boundaries | You |
| `agent_prompt.md` | Agent orchestration rules, tool usage patterns | You |
| `persona.md` | Mira's evolving self-image | Mira (automatically) |

All three are hot-reloaded from disk on every message.

## Project Structure

```
her-go/
├── cmd/              # CLI commands (Cobra): run, setup, start, stop, status, logs, trust
├── agent/            # Agent orchestrator, tool dispatch, skills integration
├── bot/              # Telegram bot + message pipeline
├── compact/          # Conversation history compaction
├── config/           # YAML config loading + env var substitution
├── embed/            # Local embedding client for semantic similarity
├── llm/              # OpenAI-compatible LLM client (streaming, multi-modal)
├── logger/           # Structured logging (charmbracelet/log)
├── memory/           # SQLite store: messages, facts, summaries, metrics, vault
├── ocr/              # Apple Vision + GLM-OCR text extraction
├── persona/          # Reflection, persona evolution, trait tracking
├── scheduler/        # Cron-based task runner (reminders, check-ins, journaling)
├── scrub/            # Tiered PII detection + deanonymization
├── search/           # Tavily web search + Open Library books
├── skills/
│   ├── loader/       # Skill discovery, trust verification, sandbox, proxy, sidecar DB
│   ├── skillkit/     # Shared libraries for Go and Python skills
│   ├── web_search/   # Web search skill
│   ├── web_read/     # URL content extraction skill
│   └── book_search/  # Book search skill
├── vision/           # Image understanding via Gemini Flash
├── voice/            # Parakeet STT + Piper TTS
├── weather/          # Open-Meteo weather integration
├── prompt.md         # Mira's personality
├── agent_prompt.md   # Agent behavior rules
├── persona.md        # Mira's evolving self-image (bot-authored)
└── config.yaml       # Your configuration (gitignored)
```

## Privacy

- All conversation data stays in local SQLite (`her.db`, gitignored)
- Hard identifiers (SSN, credit cards) are redacted before reaching any LLM
- Contact info (phone, email) is tokenized and deanonymized in responses
- Names and context pass through for conversational coherence
- Skills run in a sandbox with domain-restricted network access and SSRF protection
- Voice processing runs entirely locally via Parakeet and Piper
- Search queries go to Tavily; everything else stays on your machine

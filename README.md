# her

A privacy-first personal companion chatbot, inspired by the movie *Her*. Built in Go.

Mira is an AI companion you chat with on Telegram. She remembers your conversations, evolves her personality over time, searches the web when you need information, and keeps your private data local.

## Quick Start

```bash
# Clone and enter
git clone <your-repo-url>
cd her-go

# Copy config and fill in your API keys
cp config.yaml.example config.yaml
# Edit config.yaml: add TELEGRAM_BOT_TOKEN, OPENROUTER_API_KEY, TAVILY_API_KEY

# Install the binary
go install .

# Run in foreground (development)
her run
```

## Architecture

```
You (Telegram) → her binary → Trinity (agent model, orchestrates everything)
                                 ├── think (reason about what to do)
                                 ├── web_search / book_search (Tavily, Open Library)
                                 ├── reply (calls Deepseek, sends response)
                                 ├── save_fact / update_fact (memory management)
                                 └── done (signals turn complete)
```

Every message goes through the agent first. The agent decides whether to search, what to remember, and how to respond. The conversational model (Deepseek) generates the actual natural language response when the agent calls the `reply` tool.

## Features

- **Agent-first pipeline** — Trinity orchestrates every turn: think, search, reply, remember
- **Memory system** — Facts extracted and stored in SQLite, semantic deduplication via local embeddings
- **Persona evolution** — Mira reflects on conversations and rewrites her own personality description over time
- **Web search** — Tavily integration for real-time information
- **Book search** — Open Library integration for book discussions
- **PII scrubbing** — Tiered: hard identifiers redacted, contact info tokenized + deanonymized, names pass through
- **Conversation compaction** — Older messages summarized to stay within token budget
- **Full observability** — Agent turns, search queries, metrics, all stored in SQLite

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

## CLI Commands

```bash
her run      # Start the bot (foreground)
her setup    # Build binary, generate launchd plist, install service
her start    # Start launchd service (runs setup if needed)
her stop     # Stop launchd service
her status   # Show service status
her logs     # Tail stdout logs (--stderr, --lines N)
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
- `agent.model` — tool-calling model (default: `arcee-ai/trinity-large-preview:free`)
- `search.tavily_api_key` — from Tavily (free tier: 1000 searches/month)
- `embed.base_url` — local embedding server (LM Studio, Ollama)
- `embed.model` — embedding model name

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
├── cmd/           # CLI commands (Cobra)
├── agent/         # Agent orchestrator + tool execution
├── bot/           # Telegram bot + message pipeline
├── compact/       # Conversation history compaction
├── config/        # YAML config loading
├── embed/         # Embedding client for semantic similarity
├── llm/           # OpenAI-compatible LLM client
├── memory/        # SQLite store for everything
├── persona/       # Reflection + persona evolution
├── scrub/         # PII detection + deanonymization
├── search/        # Tavily web search + Open Library books
├── prompt.md      # Mira's personality
├── agent_prompt.md # Agent behavior rules
└── config.yaml    # Your configuration (gitignored)
```

## Privacy

- Raw conversation data stays in local SQLite (`her.db`, gitignored)
- Hard identifiers (SSN, credit cards) are redacted before leaving the machine
- Contact info (phone, email) is tokenized and deanonymized in responses
- Names and context pass through for conversational coherence
- Search queries go to Tavily; everything else stays local
- Voice processing (future) runs locally via Parakeet/Kokoro

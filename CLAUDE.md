# her-go

Personal companion chatbot built in Go. See SPEC.md for full architecture and design decisions.

## Quick Reference

- **Language:** Go
- **Database:** SQLite (her.db, gitignored)
- **Config:** config.yaml (copy from config.yaml.example, gitignored)
- **System prompt:** prompt.md (static base template)
- **Persona:** persona.md (evolving, bot-authored)

## Running

```bash
# Copy config and fill in API keys
cp config.yaml.example config.yaml

# Run
go run main.go
```

## Key Design Decisions

- **Privacy first:** Tiered PII scrubbing. Hard identifiers (SSN, cards) redacted. Contact info tokenized + deanonymized in responses. Names/context pass through.
- **Persona evolution:** Bot rewrites its own persona.md every ~20 conversations. Reflections triggered by memory-dense conversations. Changes are gradual (damped).
- **Everything in SQLite:** Messages, facts, metrics, persona versions, traits, PII vault. One file, fully portable.
- **Model agnostic:** OpenRouter API (OpenAI-compatible). Swap models by changing config.

## Project Structure

See SPEC.md § Project Structure for full layout.

Core packages:
- `bot/` — Telegram handler
- `llm/` — OpenRouter client
- `memory/` — SQLite store, fact extraction, context building
- `persona/` — Evolution system, trait tracking
- `scrub/` — Tiered PII detection + deanonymization
- `scheduler/` — Reminder delivery
- `config/` — YAML + env var loading

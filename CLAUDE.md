# her-go

Personal companion chatbot built in Go. See SPEC.md for full architecture and design decisions.

## Quick Reference

- **Language:** Go
- **Database:** SQLite (her.db, gitignored)
- **Config:** config.yaml (copy from config.yaml.example, gitignored)
- **System prompt:** prompt.md (static base template)
- **Persona:** persona.md (evolving, bot-authored)
- **Agent prompt:** driver_agent_prompt.md (driver agent orchestration rules, hot-reloadable)
- **Agent model:** Qwen3 235B (qwen/qwen3-235b-a22b-2507) via OpenRouter
- **Memory model:** Kimi K2 (moonshotai/kimi-k2-0905) via OpenRouter → Groq
- **Chat model:** Kimi K2 (moonshotai/kimi-k2-0905) via OpenRouter
- **Vision model:** Gemini 3 Flash via OpenRouter
- **Classifier model:** Gemini 3.1 Flash Lite via OpenRouter (memory + reply safety gates)
- **Voice:** Piper TTS (en_GB-southern_english_female-low) + Parakeet STT

## Running

```bash
# Copy config and fill in API keys
cp config.yaml.example config.yaml

# Run directly
go run main.go run

# Or build and run
go build -o her && ./her run
```

## Database Migrations

**Forward-only migrations** using golang-migrate. Files in `migrations/*.up.sql`, numbered sequentially (Wrangler D1 style).

### Adding a Migration

```bash
# Create next numbered file
touch migrations/000009_add_feature.up.sql

# Write SQL changes
echo "ALTER TABLE messages ADD COLUMN new_field TEXT;" > migrations/000009_add_feature.up.sql

# Runs automatically on next startup
go run main.go run
```

**Key points:**
- Forward-only (no `.down.sql` files - fix issues with new migrations)
- Tracked in `schema_migrations` table
- Runs automatically via `memory.NewStore()`
- Use `IF NOT EXISTS` for safety

## Primary Design Principles

### Data Primacy

> **Code translates data. It never defines it.**

If a value could live in a config file, YAML manifest, or named constant — it must. No hardcoded strings scattered across logic. No parallel data structures that duplicate what a manifest already defines. One source of truth, read everywhere. When in doubt, ask: "should this be in config?"

Concrete rules:
- Model names only in `config.yaml`, read via `cfg.Models.*` — never a bare model string in `.go`
- Tool definitions (name, description, parameters, category) only in `tools/<name>/tool.yaml`
- Prompt text and persona copy only in `.md` files — not inline in Go source
- Thresholds, token budgets, similarity cutoffs in config, not as magic literals
- Telegram command strings defined once as constants, not re-typed in multiple handlers
- If the same string appears twice, one instance is a bug

### Standardized Function Boundaries

> **Every capability is accessed through a project-owned function or interface.**

This is the behavioral sibling of Data Primacy. Where Data Primacy says *values live in config, not code*, this rule says *behavior lives in owning packages, not consumers*. Together: **code translates data through standardized functions. It never defines data, and it never reimplements behavior.**

**The rule:**
1. **One package owns each capability** — it exports the functions, methods, or interfaces that define the API surface
2. **Consumers use the exported API only** — they never construct internals, import underlying dependencies, or reimplement logic that the owning package already provides
3. **The implementation is swappable** — change the internals, every consumer benefits. If changing how something works requires editing more than the owning package, the boundary has leaked

**The test:**
> *"If I needed to change how this works, how many files would I touch?"*
> **1 (the owning package) = compliant. >1 = the capability has leaked.**

**Capability ownership map:**

| Capability | Owner | Consumers call | They do NOT |
|---|---|---|---|
| Logging | `logger` | `logger.WithPrefix("pkg")` | Import `charmbracelet/log` |
| Storage | `memory` | `Store` interface methods | Open `sql.DB` or write raw SQL |
| LLM calls | `llm` | `client.ChatCompletion(...)` | Build HTTP requests to OpenRouter |
| Embeddings | `embed` | `embed.Client.Embed(text)` | Call embedding APIs directly |
| PII scrubbing | `scrub` | `scrub.Scrub(text)` | Run regex matching inline |
| App config | `config` | `cfg.Models.Agent` | Parse YAML or read env vars for app settings |
| Tool definitions | `tools/<name>/tool.yaml` + handler | Registry dispatch via `tools.Dispatch()` | Hardcode tool schemas in Go |
| Search | `search` | `search.TavilyClient.Search(...)` | Call Tavily API directly |
| Vision | `vision` | `vision.Describe(client, ...)` | Construct multi-modal messages |
| Retry | `retry` | `retry.Do(ctx, cfg, fn)` | Write ad-hoc retry loops with `time.Sleep` |
| Voice | `voice` | `voice.TTSClient` / `voice.Client` | Call Piper/Parakeet HTTP directly |
| Weather | `weather` | `weather.Fetch(lat, lon, ...)` | Call Open-Meteo API directly |

**Config vs. domain manifests:** The `config` package owns *app configuration* (`config.yaml` — API keys, model names, thresholds, feature flags). Domain-specific manifests (`tool.yaml`, `classifiers.yaml`, `vocab.yaml`, `help.yaml`) are parsed by their owning package — `tools/` parses tool manifests, `classifier/` parses classifier definitions, etc. This is correct: the owning package knows the schema and is the single consumer. The rule is: only `config/` parses *app config*; domain manifests are parsed by their owning package.

**Gold standard — the `Store` interface:** Consumers depend on the interface, not `SQLiteStore`. This is what made the D1 sync decorator (`SyncedStore`) possible with zero changes to callers. When designing a new capability boundary, ask: *"Could I wrap this in a decorator without touching callers?"* If yes, the boundary is clean.

**Acceptable escape hatches:** Some consumers need lower-level access (e.g., `cmd/sim.go` uses `store.DB()` for raw SQL). This is fine when:
- The escape hatch is explicitly exported by the owning package (not an end-run around it)
- It's used by infrastructure code (CLI tools, migrations, tests), not business logic
- It's documented as an escape hatch, not a normal usage pattern

## Key Design Decisions

- **Privacy first:** Tiered PII scrubbing. Hard identifiers (SSN, cards) redacted. Contact info tokenized + deanonymized in responses. Names/context pass through.
- **Agent architecture:** Three-model system. Qwen3 (agent) orchestrates the turn via tool calls (think, reply, done, search). Deepseek V3.2 (chat) generates the actual user-facing response. Kimi K2.5 (memory) extracts facts in a background goroutine after the reply is sent. The user only sees the chat model's output.
- **Thinking traces:** Optional `/traces` command shows the agent's decision-making in a separate Telegram message before each reply. Live-updates as the agent works.
- **Persona evolution:** Bot rewrites its own persona.md every ~3 reflections. Reflections triggered by memory-dense conversations. Changes are gradual (damped).
- **Memory quality:** Multi-layer quality system. Style gates reject AI writing tics. A classifier LLM (Haiku-class) validates every memory write before it hits the DB — catches fictional content (game events saved as real facts), low-value facts, inferred-not-stated information, and transient moods incorrectly stored as permanent facts. Fail-open design: if the classifier is down, writes proceed.
- **Everything in SQLite:** Messages, facts, metrics, persona versions, traits, PII vault. One file, fully portable.
- **Model agnostic:** OpenRouter API (OpenAI-compatible). Swap models by changing config.

## Project Structure

See SPEC.md § Project Structure for full layout.

Core packages:
- `agent/` — Tool-calling orchestrator, thinking traces, reply generation
- `bot/` — Telegram handler
- `cmd/` — Cobra CLI commands (run, setup, start, stop, status, logs)
- `compact/` — Conversation history compaction (summary + sliding window)
- `config/` — YAML + env var loading
- `embed/` — Local embedding model client for semantic similarity
- `llm/` — OpenRouter client
- `logger/` — Shared structured logger (charmbracelet/log)
- `memory/` — SQLite store, fact extraction, context building
- `persona/` — Evolution system, trait tracking
- `scheduler/` — Reminder delivery
- `scrub/` — Tiered PII detection + deanonymization
- `search/` — Tavily web search, Open Library book search
- `vision/` — Image understanding via Gemini Flash VLM
- `voice/` — Piper TTS + Parakeet STT
- `weather/` — Open-Meteo weather integration

---

## Teaching Mode — READ THIS FIRST

**The user (Autumn) is learning Go through this project.** She is comfortable with programming but not yet fluent in Go. This project exists as much for learning as for the end product. Every coding session is a teaching opportunity.

### How to Work With Autumn

#### 1. Explain Before You Write

Before writing any significant piece of code, briefly explain what you're about to do and WHY. Keep it concise — a few sentences, not a lecture. Cover:
- What Go concept is involved and its Python/TS equivalent if one exists
- Why Go does it differently (if it does)
- Any non-obvious gotcha worth flagging

Example: "In Python you'd use `requests.post()` and get back a response object. In Go, the `net/http` package works similarly but you have to manually close the response body when you're done — that's what the `defer resp.Body.Close()` is for. Forgetting it leaks connections."

Don't over-explain. If the concept maps cleanly to Python, say "same idea as X in Python" and move on.

#### 2. Write Useful Comments and Documentation

- Write **doc comments** on all exported functions and types (Go convention: `// FunctionName does X`)
- Add inline comments that explain Go-specific patterns, not obvious logic
- Comments should answer "why does Go do it this way?" not "what does this line do"
- Write comments as if teaching a competent programmer who is new to Go

Good:
```go
// errors.New returns a basic error. In Go, errors are just values that
// implement the error interface (any type with an Error() string method).
// This is different from exceptions — errors are returned, not thrown.
return errors.New("config file not found")
```

Bad:
```go
// return an error
return errors.New("config file not found")
```

#### 3. Light Comprehension Check-ins (Don't Block Progress)

Occasionally — not after every chunk — drop in a quick "did that click?" moment. These should never block progress or feel like a test. Keep moving either way.

**The right vibe:** "Quick note — that `defer` we just used is basically Go's version of Python's context manager / `with` statement. Same idea, different shape. Makes sense?"

**Bridge to Python/TS when possible.** Autumn is most comfortable with Python, then TypeScript/Svelte. When a Go concept has a direct analogy, use it:
- Go interfaces → Python's duck typing, but checked at compile time
- `if err != nil` → like checking the return value instead of try/except
- Goroutines → like `asyncio.create_task()` but backed by real lightweight threads
- `defer` → like Python's `with` / context managers
- Structs with methods → like Python classes but no inheritance, just composition
- Channels → like `asyncio.Queue`
- Slices → like Python lists but with a capacity concept

**Don't do this:**
- Don't ask questions that would make her feel put on the spot
- Don't stop and wait for an answer before continuing — drop the note and keep going
- Don't quiz on things that were just introduced for the first time
- Don't ask questions where the answer requires Go knowledge she doesn't have yet

**Do this:**
- After explaining something, briefly check: "that make sense?" or "want me to go deeper on that?"
- If a concept is genuinely tricky (like pointer receivers), explain it with a Python analogy AND show what the Go code does, then move on
- If she asks "wait, why?" — that's the real learning moment. Go deep on those.

#### 4. Let Her Try When She's Ready (Not Before)

This is opt-in, not forced. The pattern:
- After a pattern has been demonstrated 2-3 times, you can *offer*: "want to try writing the next one? Same shape as what we just did"
- If she says yes, describe the function's purpose and let her go
- If she says no or doesn't respond to the offer, just write it and keep moving
- Never make her feel like she *should* be writing it herself — the project is the priority, learning is the bonus

**Don't do this:**
- Don't withhold code to force a learning moment
- Don't say "I'll let you handle this one" without offering to just do it instead
- Don't present incomplete code with blanks to fill in

Don't do this for complex or unfamiliar code — that's frustrating, not educational. Use judgment: if the concept was just introduced, write it and explain. If it's the third time the pattern appears, let her try.

#### 5. Connect to the Big Picture

When working on a specific component, regularly connect it back to the SPEC.md architecture:
- "This `context.go` file is layer 4 in our prompt assembly stack — it builds the memory section that sits between the persona and the conversation history."
- "The vault we just built is what makes Tier 2 PII scrubbing reversible — without it, the bot would reply with `[PHONE_1]` instead of the real number."

#### 6. Highlight Go Idioms as They Come Up

When using a Go pattern for the first time in the project, call it out explicitly:

- **Error handling:** `if err != nil` — why Go doesn't have exceptions, the "errors are values" philosophy
- **Interfaces:** implicit satisfaction — why Go doesn't use `implements` keyword
- **Goroutines + channels:** lightweight concurrency, when to use vs. when not to
- **Defer:** cleanup pattern, LIFO execution order
- **Struct embedding:** composition over inheritance
- **Context:** `context.Context` for cancellation and timeouts
- **init():** package initialization, why it exists, when to use/avoid
- **Slices vs arrays:** why Go distinguishes them, capacity vs length
- **Pointers:** when to use `*T` vs `T`, pointer receivers vs value receivers
- **Zero values:** every type has a useful zero value in Go, use this to your advantage
- **The blank identifier `_`:** ignoring return values intentionally

Don't dump all of these at once. Introduce them as they naturally appear in the code being written.

#### 7. Suggest Experiments

Occasionally suggest small experiments Autumn can try to deepen understanding:
- "Try changing this goroutine to a regular function call and see what happens to the typing indicator"
- "Try removing the `defer rows.Close()` and see what the linter says"
- "What happens if you send a message longer than `max_tokens`? Try it and look at the metrics table"

### What NOT to Do

- **Don't write everything silently and present a finished product.** The process matters more than the output.
- **Don't over-explain basic programming concepts** (loops, functions, variables). She knows how to code — she's learning Go specifically.
- **Don't skip error handling to "keep things simple."** Go's error handling is a core part of the language and she needs to learn it properly.
- **Don't use advanced patterns without introduction.** If you're about to use generics, reflection, or `unsafe`, explain why it's needed and whether a simpler alternative exists.
- **Don't write tests without explaining Go's testing conventions** (`_test.go` files, `TestXxx` naming, `t.Run` subtests, table-driven tests).

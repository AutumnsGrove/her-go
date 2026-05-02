# Go PR Audit

Comprehensive PR audit for her-go. Combines generic Go best practices with her-go-specific architecture checks — tool registration patterns, PII boundary enforcement, memory classifier flow, and boundary validation across all external inputs and outputs.

## When to Use

- Before merging any feature branch into main
- After a large refactor to catch regressions in established patterns
- When reviewing a PR for correctness, security, and architecture fit
- As a pre-push quality gate during development

---

## Audit Protocol

### Step 1 — Scope the Branch

```bash
git log main...HEAD --oneline
git diff main...HEAD --stat
git diff main...HEAD
```

Identify:
- Which packages changed (new packages? existing ones modified?)
- Whether `go.mod` / `go.sum` changed (dependency audit required)
- Rough change size (small patch vs. large refactor)

### Step 2 — Spawn Go Reviewer

Spawn the `go-reviewer` sub-agent. It will:
- Run C0–C8 compliance checks — starting with **C0: Data Primacy** (single source of truth, no hardcoded values, manifest-driven behavior) then error handling, concurrency, interfaces, security, resources, clarity, testing, formatting
- Apply STRIDE threat modeling to changed code
- Identify logic errors and dead code
- Report PASS/WARN/FAIL with `file:line` evidence

Wait for the go-reviewer to complete before continuing to Step 3.

### Step 3 — Her-go Architecture Checks

Read each changed file. Apply the following checklists:

#### Data Primacy — Her-go Specific

> **Code translates data. It never defines it.** This is the project's primary design principle.

Check that the diff does not violate the single-source-of-truth rule in any of these her-go-specific ways:

- **Model names** — model identifiers (e.g. `qwen/qwen3-235b-a22b-2507`) appear only in `config.yaml` and are read from `cfg.Models.*` in code. A model string hardcoded anywhere in `.go` files is a FAIL.
- **Tool definitions** — the authoritative description of a tool (name, description, parameters, category) lives in `tools/<name>/tool.yaml`. If any of that information is duplicated in Go code, the YAML is not the source of truth.
- **Prompt and message text** — user-facing strings, persona copy, and prompt templates belong in `.md` or `.yaml` files. Inline prose in `.go` source is a FAIL unless it is a short, stable error message with no user-facing copy.
- **Thresholds and tuning values** — token budgets, similarity cutoffs, retry counts, rate limits must be config fields, not bare literals. A `0.75` or `4096` appearing as a magic number in logic is a FAIL.
- **Command and trigger strings** — Telegram command names (e.g. `/traces`, `/reflect`) must be defined once (as a constant or config key) and referenced everywhere else. Repeated string literals for the same command are a FAIL.
- **PII patterns** — scrub rules and regex patterns must live in one place. If the same pattern appears in two files, one is a copy — find which is canonical.
- **Status/label strings** — classifier verdicts, memory types, tool categories must be defined as typed constants or read from the manifest. No bare `"SAVE"` or `"RECALL"` strings scattered across the codebase.

#### Standardized Function Boundaries

> **Every capability is accessed through a project-owned function or interface.**

This is the behavioral sibling of Data Primacy. Where Data Primacy governs *values*, this rule governs *behavior*. Together: code translates data through standardized functions — it never defines data, and it never reimplements behavior.

**The test:** *"If I needed to change how this works, how many files would I touch?"* 1 (the owning package) = compliant. >1 = the capability has leaked.

Check that the diff does not violate function boundaries:

- **Logging** — consumers use `logger.WithPrefix("pkg")`, never import `charmbracelet/log` directly. No `fmt.Println` or stdlib `log.Print`. FAIL if any `.go` file outside `logger/` imports the underlying log library.
- **Storage** — consumers use the `memory.Store` interface, never `*sql.DB` or raw SQL. Exception: explicitly exported escape hatches (e.g., `store.DB()`) used only by infrastructure code (CLI tools, migrations, tests). FAIL if business logic bypasses the Store interface.
- **LLM calls** — consumers use `llm.Client` methods (`ChatCompletion`, `ChatCompletionWithTools`, `ChatCompletionStreaming`), never build HTTP requests to OpenRouter/OpenAI directly. FAIL if any package outside `llm/` constructs API requests.
- **Embeddings** — consumers use `embed.Client.Embed()`, `embed.CosineSimilarity()`, `embed.FindBestMatch()`. FAIL if vector math or embedding API calls are implemented outside `embed/`.
- **PII scrubbing** — consumers use `scrub.Scrub()` and `scrub.Deanonymize()`. FAIL if regex-based PII detection is implemented outside `scrub/`.
- **Config** — consumers read `cfg.Models.*`, `cfg.Memory.*`, etc. FAIL if any package parses YAML, reads env vars, or opens config files directly.
- **Tool definitions** — tool schema lives in `tools/<name>/tool.yaml`, dispatch via `tools.Dispatch()`. FAIL if tool schemas are hardcoded in Go source.
- **Search** — consumers use `search.TavilyClient`. FAIL if Tavily API calls exist outside `search/`.
- **Vision** — consumers use `vision.Describe()`. FAIL if multi-modal message construction happens outside `vision/`.
- **Voice** — consumers use `voice.TTSClient` / `voice.Client`. FAIL if Piper/Parakeet HTTP calls exist outside `voice/`.
- **Weather** — consumers use `weather.Fetch()`. FAIL if Open-Meteo API calls exist outside `weather/`.

**Gold standard check:** Could the changed code be wrapped in a decorator without touching callers? If a new capability doesn't follow the Store interface pattern (consumers depend on interface, not implementation), flag it as a WARN.

**Escape hatch check:** If code reaches past an owning package's API:
- Is the escape hatch explicitly exported by the owning package? (OK if yes)
- Is it used by infrastructure code only, not business logic? (OK if yes)
- Is it documented? (WARN if no)
- If none of the above: FAIL.

#### Tool Registration Pattern
New tools must follow the established registration pattern:
- Handler lives in `tools/<name>/handler.go`
- Manifest lives in `tools/<name>/tool.yaml`
- Registers via `tools.Register("name", Handle)` inside `func init()`
- Blank-imported in `agent/agent.go` (or equivalent loader) with `import _ "her/tools/<name>"`
- YAML manifest defines `name`, `description`, `parameters`, and `category`

#### Context Bundle Usage (`tools.Context`)
- New tool handlers receive `tools.Context` and use its fields — no direct dependency construction inside handlers
- Callbacks (`StatusCallback`, `SendCallback`, `TraceCallback`) are threaded through from the caller, not created inline
- `tools.Context` not reconstructed inside handler functions — it comes from the agent orchestration layer

#### PII Scrubbing at Boundaries
- All Telegram message text passes through `scrub.Scrub()` before reaching the agent
- LLM responses that contain `[TOKEN_N]` placeholders are deanonymized before sending to the user
- Logging calls do not log raw message content that may contain PII
- New Telegram handlers follow the established scrub-before-process flow

#### Memory Classifier Flow
- New memory writes go through the classifier before hitting the DB
- Classifier bypass (fail-open) is only acceptable when the classifier is unreachable, not for convenience
- Memory write functions accept the classifier client as a dependency — no hardwired bypass
- Facts are not written as permanent if they are transient states (mood, current activity)

#### Logger Pattern
- Each package uses `var log = logger.WithPrefix("package-name")` at package level
- No `fmt.Println` or `log.Print` (stdlib logger) — always use the project logger
- Log levels are appropriate: `log.Debug` for verbose tracing, `log.Info` for significant events, `log.Warn` for recoverable anomalies, `log.Error` for failures
- API keys, tokens, and PII are never passed to any logging call

#### Error Wrapping Convention
- Errors use `fmt.Errorf("verb noun: %w", err)` — lowercase, no trailing punctuation, operation-first
- Example: `fmt.Errorf("opening database: %w", err)` not `fmt.Errorf("Error opening DB: %w", err)`
- Each layer wraps with its own context so the chain reads like a call stack

#### Hot-Reloadable Prompts
- Prompt changes use `replaceBetweenMarkers()` — not full file replacement
- Markers in prompt files are preserved: `<!-- BEGIN MARKER --> ... <!-- END MARKER -->`
- New prompt sections that should survive hot-reload use the marker pattern

---

### Step 4 — Boundary Validation Check

Her-go's equivalent of Zod — every external input/output boundary must be explicitly handled:

| Boundary | Check |
|----------|-------|
| Telegram message input | Scrubbed before agent; length/type not assumed |
| LLM API responses | HTTP status checked; JSON parsed with explicit error handling; not assumed to match schema |
| Tool call arguments | Parsed with `json.Unmarshal` + `if err != nil`; required fields checked; not assumed present |
| SQLite query results | `rows.Err()` checked after iteration; `rows.Scan()` error checked |
| Config YAML | Required fields validated at startup — missing critical config fails loudly, not silently |
| External APIs (Tavily, vision, STT) | Non-200 responses handled; response body not assumed to be well-formed |
| Telegram callback data | Validated before acting — not assumed to be a known command |
| File reads (prompt.md, persona.md) | File-not-found handled; empty file handled; not assumed to always exist |
| Embedding API responses | Vector dimension checked against configured `EmbedDimension` before storage |

For each boundary touched by the diff, mark: **✅ Validated** / **⚠️ Partial** / **❌ Missing**

---

### Step 5 — Dependency Audit (if go.mod changed)

- **New dependency**: Is it justified? Could stdlib handle it? Does it have known CVEs? (`govulncheck ./...`)
- **Removed dependency**: Are all import sites cleaned up? Does `go mod tidy` pass?
- **Version bump**: Has the changelog been reviewed for breaking changes?
- **Indirect dependencies**: Does the change introduce a transitive dependency with a security advisory?

```bash
go mod tidy
govulncheck ./...
```

### Step 6 — Test Coverage Check

For each changed package:

```bash
go test ./... -count=1
go test ./... -race
```

Check:
- Does the new logic have at least one test?
- Are existing tests still green?
- Does the race detector find issues?
- For new tool handlers: is there a dispatch test?
- For new memory operations: is there a store test against a real temp SQLite DB?

---

## Report Format

```
# PR Audit: [branch-name]
Date: [today]
Packages changed: [list]

## Summary
[2–3 sentences: what changed, what the overall quality looks like, any blocking issues]

## Go Code Review
[Full go-reviewer output — compliance table + all findings]

## Her-go Architecture

| Check | Status | Notes |
|-------|--------|-------|
| **Data primacy — no hardcoded model names** | ✅ / ⚠️ / ❌ / N/A | |
| **Data primacy — tool definitions in YAML only** | | |
| **Data primacy — no inline prompt/copy in .go files** | | |
| **Data primacy — thresholds/tuning in config** | | |
| **Data primacy — command strings defined once** | | |
| **Function boundaries — logging via `logger` only** | | |
| **Function boundaries — storage via `Store` interface** | | |
| **Function boundaries — LLM via `llm.Client` only** | | |
| **Function boundaries — embeddings via `embed` only** | | |
| **Function boundaries — no leaked dependencies** | | |
| **Function boundaries — escape hatches documented** | | |
| Tool registration pattern | | |
| Context bundle usage | | |
| PII scrubbing at boundaries | | |
| Memory classifier flow | | |
| Logger pattern | | |
| Error wrapping convention | | |
| Hot-reload prompt markers | | |

## Boundary Validation

| Boundary | Status | Notes |
|----------|--------|-------|
[one row per boundary touched by the diff]

## Security (Combined)
[STRIDE table from go-reviewer + any her-go-specific additions]

## Dependencies
[go.mod findings or "No dependency changes"]

## Test Coverage
[What has tests, what's missing, race detector result]

## Verdict
READY TO MERGE / NEEDS MINOR FIXES / CHANGES REQUIRED

## Required Before Merge
[Ordered list — empty if READY TO MERGE]

## Non-Blocking Suggestions
[Optional improvements that can be addressed later]
```

---

## Principles

- **Code translates data. It never defines it.** If a value could live in a config, manifest, or constant, it must. Hardcoded values scattered through logic are a design smell, not just a style issue. — Autumn Grove
- Audit is read-only. Never modify files during an audit run.
- Evidence required for every non-passing finding (`file:line`).
- N/A is valid and honest.
- A clean, well-implemented PR deserves a clear "this is good" verdict — not hedged praise.
- The goal is confidence, not gatekeeping.

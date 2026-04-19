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
- Run C1–C8 compliance checks (error handling, concurrency, interfaces, security, resources, clarity, testing, formatting)
- Apply STRIDE threat modeling to changed code
- Identify logic errors and dead code
- Report PASS/WARN/FAIL with `file:line` evidence

Wait for the go-reviewer to complete before continuing to Step 3.

### Step 3 — Her-go Architecture Checks

Read each changed file. Apply the following checklists:

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
| Tool registration pattern | ✅ / ⚠️ / ❌ / N/A | |
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

- Audit is read-only. Never modify files during an audit run.
- Evidence required for every non-passing finding (`file:line`).
- N/A is valid and honest.
- A clean, well-implemented PR deserves a clear "this is good" verdict — not hedged praise.
- The goal is confidence, not gatekeeping.

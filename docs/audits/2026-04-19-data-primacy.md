# Data Primacy Audit — 2026-04-19

**Auditor:** go-audit / Claude  
**Branch:** claude/go-code-review-tools-4zC9p  
**Scope:** Full codebase audit against the primary design principle: *code translates data, never defines it.*

---

## Summary

The codebase architecture is fundamentally sound. Tool definitions, classifier verdicts, Telegram commands, and configuration tunables are all properly manifest-driven. The audit surfaced one category of systemic violations: **prompt text embedded as Go constants** across three packages. Eight prompt strings were living in source code instead of `.md` files. All were fixed in this session.

Secondary violations: a duplicated magic number (`0.75`) and a duplicated model string literal across two call sites in the simulation command. Also fixed.

A latent correctness bug was found in `cmd/run.go`: code-level fallback values for agent temperature (`0.1`) and max tokens (`512`) contradicted the documented defaults in `config.yaml.example` (`0.4` and `4096`). These were dead code but created a silent discrepancy between documentation and behavior. Removed in favour of trusting `config.yaml.example` as the single source of truth.

---

## Findings

### HIGH — Prompt Text Defined in Go Source

**Principle violated:** Prompt text and persona copy belong in `.md` files, not inline in Go source.

| File | Constant | Status |
|------|----------|--------|
| `compact/compact.go` | `summaryPromptTmpl` | Fixed → `compact/summary_prompt.md` |
| `compact/compact.go` | `agentSummaryPromptTmpl` | Fixed → `compact/agent_summary_prompt.md` |
| `persona/evolution.go` | `reflectionPromptTmpl` | Fixed → `persona/reflection_prompt.md` |
| `persona/evolution.go` | `rewritePromptTmpl` | Fixed → `persona/rewrite_prompt.md` |
| `persona/evolution.go` | `traitExtractionPrompt` | Fixed → `persona/trait_extraction_prompt.md` |
| `persona/evolution.go` | `nightlyReflectPromptTmpl` | Fixed → `persona/nightly_reflect_prompt.md` |
| `persona/evolution.go` | `gatedRewritePromptTmpl` | Fixed → `persona/gated_rewrite_prompt.md` |
| `memory/extract.go` | `extractionPrompt` | Fixed → `memory/extraction_prompt.md` |

**Fix applied:** Each constant replaced with a `//go:embed` variable declaration. The prompt text moved to a `.md` file alongside the Go source. Calling code (`fmt.Sprintf(...)`) unchanged — only the storage location changed.

**Note on the two "fallback" prompts** (`agent/agent.go:defaultAgentPrompt`, `agent/memory_agent.go:defaultMemoryAgentPrompt`): these are minimal one-liner fallbacks used only when the real prompt file cannot be loaded. They are intentionally inline as a fail-safe and are not violations of the principle.

---

### MEDIUM — Duplicate Magic Number (`0.75` compaction threshold)

**File:** `compact/compact.go`  
**Issue:** `float64(maxHistoryTokens) * 0.75` appeared in two separate functions (`MaybeCompact` and `MaybeCompactAgent`). One instance is a bug waiting to happen.  
**Fix applied:** Extracted to `const compactionThreshold = 0.75` with a doc comment explaining why it is an architectural constant rather than a config value.

---

### MEDIUM — Duplicate Model String Literal in `cmd/sim.go`

**File:** `cmd/sim.go:431` and `cmd/sim.go:1106`  
**Issue:** `"liquid/lfm-2.5-1.2b-instruct:free"` appeared twice — once when recording the run and once when generating the report.  
**Fix applied:** Extracted to `const fallbackSimAgentModel`. Both sites now reference the constant.

---

### MEDIUM — Code Fallbacks Contradict `config.yaml.example` (Correctness Bug)

**File:** `cmd/run.go`  
**Issue:** Code-level fallback defaults for LLM clients disagreed with the documented defaults in `config.yaml.example`:

| Setting | Code fallback | config.yaml.example |
|---------|--------------|---------------------|
| `agent.temperature` | `0.1` | `0.4` |
| `agent.max_tokens` | `512` | `4096` |

These values were reached only when `config.yaml.example` was absent, making them rare but surprising. The existence of two conflicting default sources is itself the violation — `config.yaml.example` is the single source of truth.

**Fix applied:** Removed all code-level fallback guards for `agent`, `vision`, `classifier`, and `memory_agent` LLM client construction. All four now pass config values directly. `config.yaml.example` provides the defaults via the config loader; missing fields produce Go zero values, which cause fast-fail LLM errors rather than silent wrong-model behavior.

---

## What Was Already Correct

The following were checked and found compliant — no changes needed:

- **Tool definitions** — 100% YAML-driven (`tools/<name>/tool.yaml`). No tool name, description, parameter, or category defined in Go.
- **Classifier verdicts** — `SAVE`, `SPLIT`, `LOW_VALUE`, `STYLE_ISSUE`, etc. defined once in `classifier/classifiers.yaml`. Go code parses them; it does not define them.
- **Model names in config** — All production model identifiers (`qwen/qwen3-235b-a22b-2507`, `moonshotai/kimi-k2-0905`, etc.) live only in `config.yaml.example` and the user's `config.yaml`. None appear as bare strings in `.go` files (excluding the sim fallback label addressed above).
- **Telegram commands** — Centralized in `bot/handlers_commands.go`. No command string duplicated across handlers.
- **Thresholds and tunables** — `max_history_tokens`, `agent_context_budget`, `auto_link_threshold`, `max_memory_length`, etc. all in config. Named constants used for the few architectural invariants (`compactionThreshold`, `maxTelegramLen`, etc.).
- **Logger pattern** — `var log = logger.WithPrefix("package")` consistent across all packages.
- **Error wrapping** — `fmt.Errorf("context: %w", err)` pattern used consistently throughout.

---

## Remaining Items (Not Fixed This Session)

None are blocking. All are low-priority observations.

- `compact/compact.go:128` — `maxHistoryTokens = 8000` and `compact/compact.go:372` — `agentContextBudget = 16000` are guarded fallbacks that fire only when the caller passes `<= 0`. Since `config.yaml.example` sets both, these are redundant but harmless. Could be extracted to named constants or removed if callers are tightened up.
- `memory/context.go:30` — `conversationRedundancyThreshold = 0.60` is a local constant. Acceptable as an architectural invariant; consider adding to config if users need to tune it.
- `cmd/setup.go`, `cmd/sim.go` — Use `fmt.Printf` / `fmt.Println` directly rather than the project logger. Acceptable for CLI output where structured logging adds no value.

# her-go Refactor Plan

> Written 2026-04-14. This document captures every decision from the scope review conversation.
> Nothing here is implemented yet. This is the blueprint.

---

## Goals

1. **Reliability over features.** The core agent loop must work consistently before anything else gets added.
2. **Strip to essentials.** Remove everything that isn't core to: think → recall → search → reply → done.
3. **Separate concerns.** Memory writes move to a post-turn background agent. The main agent only orchestrates and replies.
4. **Observability.** If you can't see what's happening, you can't fix it. Full context dumps, structured logging, failed tool visibility.
5. **Testability.** Unit tests on core components, integration tests on the agent loop, sim tests only after the harness is stable.

---

## Architecture: Three-Model System

The program glues together multiple models, each doing what it does best.

### Agent Model (main orchestrator)
- **Current:** Trinity Large Thinking (`trinity-large-thinking`) via OpenRouter
- **Future candidate:** Qwen3 235B (`qwen/qwen3-235b-a22b-2507`) — faster, cheaper, worth testing once harness is stable
- **Job:** Orchestrate the turn. Think, recall facts, search the web, call reply, call done. That's it.
- **Does NOT:** Save facts, update facts, manage memory in any way.

### Memory Model (post-turn background agent)
- **Model:** Kimi K2.5 (`moonshotai/kimi-k2.5`) — best at nuance and fact extraction
- **Job:** After the main agent finishes, review the turn (user message + agent thinking + reply) and extract/update/remove facts.
- **Runs:** In a background goroutine, never blocks the user.
- **Has access to:** The conversation turn, existing facts for dedup, classifier gate.

### Chat Model (reply generator)
- **Model:** Deepseek V3.2 via OpenRouter
- **Job:** Generate the actual user-facing response inside the `reply` tool.
- **Receives:** Slimmed system prompt, recent messages, agent's instruction + search context.

### Classifier Model (memory quality gate)
- **Model:** Claude Haiku 4.5 via OpenRouter
- **Job:** Validate every memory write from the memory model. Fail-open (if down, writes proceed).
- **Simplified to 3 verdicts:** MOOD_NOT_FACT, LOW_VALUE, FICTIONAL (simplified rules).
- **Removed verdicts:** INFERRED (too aggressive — reasonable inference is fine), EXTERNAL (edge case).

### Dream Model (persona evolution)
- **Model:** Same as memory model (Kimi K2.5) or configurable
- **Job:** Nightly reflection + gated persona rewrite. See Dreaming section.

---

## Tool Split

### Agent Hot Tools (5)

These are always available to the main agent:

| Tool | Purpose |
|------|---------|
| `think` | Agent scratchpad, surfaces in traces |
| `reply` | Calls chat model to generate response |
| `done` | Signals end of turn |
| `recall_memories` | Semantic search over facts (read-only) |
| `use_tools` | Load deferred tool categories on demand |

### Deferred Tools (loaded via `use_tools`)

| Category | Tools |
|----------|-------|
| `search` | `web_search`, `web_read` (Tavily — reverted from skill to direct tool) |
| `vision` | `view_image` |

Search is on-demand, not every turn. Keeping it deferred saves token overhead on the majority of turns that don't need it.

### Memory Agent Tools (4, post-turn only)

| Tool | Purpose |
|------|---------|
| `save_fact` | Save new user fact |
| `save_self_fact` | Save bot self-observation (feeds into dreaming) |
| `update_fact` | Update existing fact (creates new, supersedes old) |
| `remove_fact` | Soft-delete a fact |

---

## Junk Drawer

Everything below moves to `_junkdrawer/` at the repo root. It stays in the repo, just out of the build path. Git history is preserved. These can be brought back individually once the core harness is rock-solid.

### Entire packages
- `skills/` — Skill discovery, runner, sandbox, trust tiers, proxied DB/web, skillkit SDK
- `scheduler/` — Cron job scheduler, reminder delivery, quiet hours, proactive features
- `ocr/` — macOS Vision OCR + GLM-OCR fallback
- `weather/` — Open-Meteo integration
- `search/books.go` — Open Library book search

### Tools (move to `_junkdrawer/tools/`)
- `no_action/` — Useless signal
- `reply_confirm/` — Good concept, needs better prompting, not essential now
- `log_mood/`, `update_mood/` — Mood tracking
- `scan_receipt/`, `query_expenses/`, `delete_expense/`, `update_expense/` — Expense tracking
- `todoist_create/`, `todoist_list/`, `todoist_update/`, `todoist_complete/` — Todoist integration
- `nearby_search/` — Foursquare places
- `github_create_issue/`, `github_list_issues/` — GitHub integration
- `search_history/` — Conversation history search
- `set_location/` — Location management
- `get_current_time/` — Time as a tool (time is injected as a layer instead)
- `create_reminder/`, `create_schedule/`, `list_schedules/`, `update_schedule/`, `delete_schedule/` — Scheduler tools

### Agent layers (move to `_junkdrawer/layers/`)
- `chat_mood.go` — Mood trend injection
- `chat_weather.go` — Weather injection
- `chat_expenses.go` — Expense context injection
- `chat_traits.go` — Personality traits injection (redundant with persona.md)

### Config sections to remove
- `scheduler` (all proactive features)
- `todoist`
- `github`
- `foursquare`
- `weather`
- `ocr`

---

## Core Agent Loop

### Current problems
- Hard cap of 10 iterations causes 25% of complex turns to end without `reply` or `done`
- `use_tools()` loading deferred tools consumes iteration budget
- Nudge logic (force `tool_choice="required"`) is Mercury-era, counterproductive for thinking models
- Memory tool calls burn 2-3 iterations per turn on fact saves that belong elsewhere

### New design

**Base loop: 15 iterations.** With memory tools removed from the agent, 15 is generous for: think → recall → load search → search → think → reply → done (7 calls typical).

**Continuation windows (issue #48):** When the agent runs out of iterations without calling `done`:
1. Inject a system message summarizing what the agent accomplished so far
2. Restart the loop for another 15 iterations
3. The agent MUST call `reply` first in the continuation to update the user on progress
4. Max 3 continuations = 60 total calls (hard safety cap against runaway costs)

**Remove the nudge.** Trinity and Qwen3 are thinking models. If the model returns text instead of tool calls, that's a prompting problem. Don't paper over it with forced tool_choice.

**Tool resilience (issue #51):** Per-tool YAML failures become log-and-skip instead of panic. The bot starts with whatever tools loaded cleanly. Infrastructure-level failures (embedded YAMLs unreadable, categories.yaml broken) stay as panics.

Specific panic sites to convert to log-and-skip:
- `tools/trace.go:110` — bad trace template
- `tools/trace.go:123` — bad reject trace template
- `tools/registry.go:35` — duplicate handler registration
- `tools/loader.go:139` — per-tool YAML parse failure
- `tools/loader.go:146` — loader sanity check

Keep as panics (infrastructure-level):
- `tools/loader.go:120` — embedded YAMLs unreadable
- `tools/loader.go:195` — categories.yaml parse failure

---

## Memory Agent (Post-Turn)

### How it works

After the main agent loop completes and the user has their reply:

1. A background goroutine spawns the memory agent
2. It receives: the full turn transcript (user message, agent tool calls + thinking, reply sent)
3. It also receives: access to the fact store for dedup checking
4. It calls Kimi K2.5 with a focused prompt: "Review this conversation turn. Extract facts worth remembering."
5. The memory model has 4 tools: `save_fact`, `save_self_fact`, `update_fact`, `remove_fact`
6. Each save goes through the classifier gate (Haiku 4.5)
7. Results are logged but never block the user

### What the memory agent sees

- The user's message (scrubbed)
- The agent's thinking traces (what the agent was reasoning about)
- The reply that was sent
- Existing facts (for dedup context)
- The agent prompt for the memory model (focused on: what to save, what not to save, quality rules)

### What the memory agent does NOT see

- Full conversation history (it doesn't need it — just the current turn)
- The reply model's system prompt
- Weather, mood, traits, or any contextual layers

### Self-facts and dreaming

The memory agent saves `save_self_fact` when it observes patterns in how the bot responded:
- "Responded with humor when Autumn was stressed — she seemed to appreciate it"
- "Asked a follow-up about the project and Autumn went deep on it"

These self-facts feed into the nightly dreaming system for persona evolution.

### Model choice rationale

Kimi K2.5 was chosen because:
- Best at nuanced fact extraction in sim comparisons
- Fewer incomplete turns than Trinity in sim runs
- Cost is higher per call, but the memory agent makes 1 call per turn (not 10)
- Can be swapped via config without code changes

---

## Reply Model Context (Simplified)

### The problem

The reply model's chat history budget is 3,000 tokens. `prompt.md` alone is ~1,700 tokens — 57% of the budget consumed before a single message. After persona + time + one exchange, you're at ~2,200 tokens, which triggers compaction at 2,250 (75% of 3,000). Compaction fires on practically the second message.

### Fixes

**1. Trim `prompt.md` from ~1,700 to ~600-700 tokens.**

The current prompt says the same thing 3-4 times across 4 sections:
- "Communication Style" says: be short, casual, curious, have takes
- "Stay in the Conversation" says: be curious, pull on threads, have takes
- "How to Sound Like a Real Person" says: be specific, curious, short, have personality
- "What to avoid" says: don't use AI words, don't summarize

Consolidate into one tight section. Draft the trimmed version, test in sim, adopt when validated.

**2. Compaction trigger counts only message tokens, not system prompt.**

The system prompt is fixed overhead — it's always there. Counting it toward the compaction threshold means the conversation barely has room to exist before getting compacted. Fix: subtract estimated system prompt tokens from the API's reported prompt_tokens before comparing to the threshold.

**3. Raise conversation budget to 8,000 tokens.**

With the system prompt excluded from the trigger math, 8,000 tokens gives room for ~15-20 exchanges before compaction. This can be tuned in the sim once everything is working.

### What the reply model receives (after refactor)

| Component | ~Tokens | Notes |
|-----------|---------|-------|
| `prompt.md` (trimmed) | ~600-700 | Consolidated identity + style rules |
| `persona.md` | ~275 | Bot's evolved self-image |
| Current time | ~30 | Timezone-aware readable format |
| Semantic facts | ~400-1,200 | Keep for now, test removing in sim |
| Conversation summary | 0-2,000 | Only present after compaction |
| Recent messages (6) | ~300-600 | Reduced from 10 to 6 |
| Agent instruction + search context | ~100-700 | What to say + reference material |
| Current user message | ~50-300 | PII-scrubbed |
| **Total** | **~1,800-5,800** | Down from ~3,050-8,200 |

### What's removed from the reply model

- **Traits** — Redundant with persona.md
- **Mood, weather, expenses** — Junk drawered
- **Direct fact injection** — Keep for now. Test in sim whether removing facts (relying on agent instruction alone) degrades reply quality. If it doesn't, remove. The agent currently doesn't pass contextual info in its instructions because it's never been told to — this needs prompt work first.

---

## Compaction (CRUSH-style)

### Current problems
- Dual-signal trigger (real token count vs. estimation fallback) is fragile
- 75% budget threshold with system prompt counted in = triggers too early
- Running summaries that build on each other add complexity
- Chat and agent have separate compaction systems with separate budgets

### New design

Adopt CRUSH's approach: simple, single-pass summarization.

**Chat history compaction:**
- Budget: 8,000 tokens (configurable)
- Trigger: when message-only token count exceeds 75% of budget (6,000 tokens)
- System prompt tokens are NOT counted toward this threshold
- When triggered: summarize all messages older than the last 6 into a single summary
- Summary is stored in DB (append-only, for audit trail and looking back in time)
- Summary becomes part of the reply model's context on future turns
- Identity pinning in summary prompt: "The user's name is {name}. Always refer to them as {name}."

**Agent action compaction:**
- Budget: 16,000 tokens (configurable)
- Trigger: same 75% rule (12,000 tokens)
- Compacts the agent's tool call history (what it did in previous turns)
- Keeps last 10 actions in full fidelity
- Summarizes older actions

**Memory agent:** No compaction needed. It processes one turn at a time.

**Key simplification:** One compaction system for messages, one for agent actions. No dual signals, no estimation fallback, no percentage-of-budget math that includes fixed overhead. Just: count the variable tokens, compare to threshold, summarize if over.

---

## Observability

### Current problems
- `her.log` is a single file that grows to 12K+ lines with no rotation or session separation
- Failed tool calls are invisible in the TUI — only successes show
- Trinity thinks natively but its internal reasoning is never surfaced
- Can't see what the agent API call or chat API call actually receives (facts, summaries, persona — all invisible at runtime)
- TUI attaches voice memo / photo turns to previous turn when `SaveMessage` fails and `msgID = 0`

### Logging overhaul

**Lumberjack log rotation:**
- 10MB per file
- Keep ALL files for now (no deletion — Autumn needs long-term context)
- Future: 30-day retention window once stable
- New file per session (or per 10MB, whichever comes first)

**Dual format:**
- File: JSON structured logging (machine-parseable, grep-friendly)
- TUI: Human-readable (charmbracelet/log, same as now)

**Session markers:** Write a clear `SESSION_START` entry with timestamp, config hash, and model names at boot. Makes it easy to find where one run ends and another begins.

### Debug mode

New `debug: true` config flag. When enabled:

- **Full context dump:** Log the exact messages array sent to every API call — agent, chat, classifier, memory agent. Includes system prompt, conversation history, tool schemas, everything. This is the single most important observability feature. You need to see what's going in.
- **Token breakdown per layer:** Log how many tokens each layer contributed to the system prompt
- **Embedding calls:** Log every embed() call with the text being embedded and the latency
- **API response metadata:** Full usage stats (prompt_tokens, completion_tokens, cost) per call

When `debug: false` (default): standard operational logging only. No token dumps, no context dumps. Clean output for end users.

### TUI fixes

**Failed tool calls:** Surface tool errors in the TUI turn section, not just successes. Format: `❌ save_fact: rejected — too similar (92%) to existing fact ID=47`

**Thinking traces:** If the API response includes thinking/reasoning content (provider-dependent), surface it in the TUI under a `💭 thinking` line, same as the `think` tool output. This way you see the model's reasoning whether it uses the tool or thinks natively.

**Turn attachment bug:** Add early-return error handling in `handlePhoto` and `handleVoice` (`bot/handlers_media.go`) when `SaveMessage` fails or returns `msgID == 0`. Don't call `runAgent` with an invalid trigger ID.

**Voice memo / photo turn creation:** Emit `TurnStartEvent` BEFORE processing (transcription, OCR), not after. The TUI should show "processing voice memo..." in its own turn section from the start, then update as the agent runs.

---

## Embedding Sidecar

### Current problem
The embed client (`embed/embed.go`) is a bare HTTP client that assumes the external embedding server (LM Studio) is already running. No health check, no auto-start, no stale process cleanup. If LM Studio isn't loaded, embedding silently fails and semantic search degrades to nothing — with no clear signal to the user.

Voice sidecars (STT/TTS) "just work" because they have explicit lifecycle management: `killStaleProcess()`, `IsAvailable()` health check, graceful shutdown.

### New design

Give the embedding client the same lifecycle treatment as voice:

**1. Health check (`IsAvailable()`):**
```
POST {base_url}/embeddings with a tiny test input
Return true if 2xx within 2 seconds, false otherwise
```

**2. Start command (config field):**
```yaml
embed:
  base_url: "http://localhost:1234/v1"
  model: "nomic-embed-text-v1.5"
  dimension: 768
  start_command: "lms load nomic-embed-text-v1.5"  # NEW
```

**3. Startup sequence (in `cmd/run.go`):**
1. Create embed client
2. Health check — if already healthy, done
3. If unhealthy and `start_command` is set: run it
4. Poll for health (up to 30 seconds, 1-second intervals)
5. If still unhealthy: log warning, continue with degraded recall (FTS5 only)

**4. Stale process cleanup:** Same `killStaleProcess()` pattern as voice sidecars, if applicable for the embedding server.

**5. Graceful degradation:** If the sidecar goes down mid-session, `recall_memories` falls back to FTS5 lexical search with a `degraded: true` flag. The TUI shows a warning. No crash, no silent failure.

---

## Dreaming System (New)

Ported from her-pi's design. Two-stage, time-based persona evolution that replaces the current density-based reflection triggers.

### Stage 1: Nightly Reflection (always runs)

**Trigger:** Simple goroutine timer, runs at 04:00 local time. Also runs on startup if >20 hours since last reflection (catch-up).

**Input to dream model:**
- Current persona.md
- Recent self-facts (primary input — how the bot has been responding)
- Recent conversation history (secondary — what topics came up)
- Recent user facts (weighted less — this is about the bot's growth, not the user's details)
- Base identity from prompt.md (immutable anchor)

**Output:** Either:
- 2-4 sentence observation about patterns, growth, or changes ("I've been using more humor lately and Autumn seems to respond well to it")
- The literal string `NOTHING_NOTABLE` (no observation worth recording)

**Storage:** `reflections` table — `id`, `text`, `trigger` ('dream' or 'manual'), `consumed_in_persona_version` (FK), `created_at`

### Stage 2: Persona Rewrite (gated)

**Cooldown gates (both must pass):**
- `daysSinceLastRewrite >= 7` (configurable, default 7)
- `unconsumedReflections >= 3` (configurable, default 3)

**Input to dream model:**
- Current persona.md
- All unconsumed reflections (with dates)
- Base identity from prompt.md (immutable — rewrite must not contradict this)

**Output:** Either:
- `UNCHANGED` — nothing shifted enough to warrant a rewrite
- `CHANGE_SUMMARY: <one sentence>\n---\n<new full persona>` — proceed with rewrite

**Storage:** `persona_versions` table (already exists in her-go) — append-only, full text snapshot per version. `persona_state` single-row table tracks active version + cooldown timestamps.

**Disk mirror:** Write new persona to `persona.md` on disk (best-effort, failure doesn't rollback).

### Manual trigger

`/dream` Telegram command bypasses all cooldowns. Sets `trigger='manual'` on rows. For debugging and after particularly significant conversations.

### Why this replaces the current system

The current density-based reflection triggers almost never fire naturally — Autumn has only seen them trigger manually. The nightly cron ensures reflections happen reliably regardless of conversation patterns. The 7-day + 3-reflection gate prevents personality whiplash while still allowing gradual evolution.

---

## Classifier Simplification

### Current state: 5 verdicts
- FICTIONAL — in-game events saved as real facts
- MOOD_NOT_FACT — transient emotions as permanent facts
- INFERRED — agent editorializing beyond stated facts
- LOW_VALUE — generic or trivially obvious facts
- EXTERNAL — character emotions from fiction mistaken for user moods

### New state: 3 verdicts

**Keep:**
- **MOOD_NOT_FACT** — Clear, useful, unambiguous. "User feels tired today" is not a fact.
- **LOW_VALUE** — Clear, useful. "User enjoys food" is not worth storing.
- **FICTIONAL** — Simplified rules. Instead of trying to detect mixed game/real content (which most models struggle with), simplify to: "Does the user's message explicitly state this about themselves in real life? Save. Is it about a game character's actions? Skip." No clever mixed-content analysis.

**Remove:**
- **INFERRED** — Too aggressive. Reasonable inference from stated facts is fine. "User adopted their cat from a Portland shelter" is a valid inference from "I got Bean from that shelter in Portland." The current gate rejects this.
- **EXTERNAL** — Edge case that overlaps with FICTIONAL. Not worth the complexity.

---

## Fact Save Pipeline Simplification

### Current state: 7 gates
1. Self-fact blocklist
2. Style blocklist (~30 patterns)
3. Length gate (200 chars)
4. Timestamp stripping
5. Retry budget (max 2 per turn, cosine 0.75)
6. Duplicate check (tag 0.85, text 0.85, context 0.70)
7. Classifier gate (5 verdicts)

### New state: 4 gates

**Keep:**
1. **Style blocklist** — Curate the list. Remove natural phrases like "it's important to" that trigger false positives. Keep em dashes, obvious AI slop ("delve", "tapestry", "leverage"), and grandiose phrasing. Audit the list based on classifier_log data.
2. **Length gate** — 200 chars, unchanged. Simple and effective.
3. **Duplicate check** — Consider relaxing from 0.85 to 0.80 threshold. The current threshold catches minor rephrases ("User likes X" vs "User enjoys X") as duplicates when they might be legitimate refinements.
4. **Classifier gate** — Simplified to 3 verdicts (see above).

**Remove:**
- **Self-fact blocklist** — The memory agent doesn't have the same prompt-regurgitation problem that the main agent had. It sees the conversation, not its own system prompt.
- **Retry budget** — The memory agent runs post-turn in the background. Retries don't burn the user's time or the main agent's iteration budget. If a save fails, the memory agent can just try a different phrasing without pressure.
- **Timestamp stripping** — Move to the memory agent's prompt instructions ("don't include temporal references like 'today' or 'last Tuesday' in facts") instead of code-level stripping. The model should learn to write good facts, not have them mechanically cleaned.

---

## Testing Strategy

### Priority 1: Unit tests (write first)

Core components that can be tested in isolation:

- **Compaction trigger logic:** Given N messages totaling X tokens with a budget of Y, does compaction fire? Does it correctly exclude system prompt tokens?
- **Tool dispatch:** Given a tool call JSON, does the right handler get called? Does a missing handler log-and-skip instead of panicking?
- **Tool loader resilience:** Given a broken tool.yaml, does the loader skip it and load the rest?
- **Fact quality gates:** Given a fact string, does the style blocklist catch it? Does the length gate reject it? Does the duplicate checker flag it at the right threshold?
- **Classifier verdict parsing:** Given classifier output text, is the verdict correctly parsed?
- **Embedding client health check:** Does `IsAvailable()` return false when the server is down, true when it's up?
- **Agent continuation logic:** Given a loop that ran out of iterations without `done`, does it correctly inject a summary and restart?

### Priority 2: Integration tests (write second)

Agent loop with a mock LLM:

- **Basic turn:** Agent calls think → reply → done. Verify the reply reaches the send callback.
- **Search turn:** Agent calls use_tools(search) → web_search → think → reply → done. Verify deferred tools load correctly.
- **Continuation turn:** Agent uses all 15 iterations without calling done. Verify continuation fires, progress reply sent, loop restarts.
- **Tool failure:** One tool handler returns an error. Verify the agent sees the error message and continues (doesn't crash).
- **Memory agent:** After main agent completes, verify memory agent goroutine runs, receives correct turn transcript, and can call save_fact.

### Priority 3: Sim tests (write after harness is stable)

Use the existing sim framework (`cmd/sim.go` + `sims/*.yaml`):

- **Regression suite:** Re-run classifier-stress, fact-a-thon, compaction-stress with the new system
- **New sims:** Continuation window stress test, memory agent fact quality test, prompt.md trim A/B test
- **Sim isolation fix:** Ensure sim runs use isolated DBs, not the central sim.db during execution (discovered bug: sim runs were reading previous run facts, causing identity confusion)

---

## Implementation Order

This is not a sprint plan — it's a dependency graph. Each phase builds on the previous.

### Phase 0: Observability (do first — see before you fix)
1. Lumberjack log rotation
2. Debug mode config flag + full context dumps
3. Fix TUI turn attachment bug (msgID == 0)
4. Failed tool call visibility in TUI

### Phase 1: Junk drawer (reduce scope before changing anything)
1. Move all junk drawer items to `_junkdrawer/`
2. Remove their imports from `agent/agent.go` and `cmd/run.go`
3. Remove their config sections from `config.yaml.example`
4. Remove their agent layers
5. Verify the bot still compiles and starts with the stripped set

### Phase 2: Core agent loop fixes
1. Raise loop limit to 15
2. Remove nudge logic
3. Implement continuation windows (issue #48)
4. Implement tool resilience / log-and-skip (issue #51)
5. Revert web_search and web_read from skills to direct tools
6. Unit tests for tool dispatch, loader resilience, continuation logic

### Phase 3: Compaction rewrite
1. Rewrite chat compaction (CRUSH-style, 8K budget, system prompt excluded)
2. Simplify agent action compaction (same pattern)
3. Store summaries in DB (append-only)
4. Identity pinning in summary prompt
5. Unit tests for compaction triggers

### Phase 4: Memory agent
1. Build post-turn memory agent (Kimi K2.5, background goroutine)
2. Remove memory tools from main agent's hot tools
3. Simplify fact pipeline (4 gates instead of 7)
4. Simplify classifier (3 verdicts instead of 5)
5. Unit tests for fact gates, classifier parsing
6. Integration tests for memory agent flow

### Phase 5: Reply model cleanup
1. Audit and trim prompt.md (~1,700 → ~600-700 tokens)
2. Reduce recent messages from 10 to 6
3. Remove traits layer
4. Test in sim: compare reply quality before/after

### Phase 6: Embedding sidecar
1. Add `IsAvailable()` health check
2. Add `start_command` config field
3. Implement startup lifecycle (health check → start → poll → degrade gracefully)
4. Stale process cleanup

### Phase 7: Dreaming system
1. Add `reflections` table and `persona_state` table
2. Implement nightly reflection (goroutine timer)
3. Implement gated persona rewrite
4. `/dream` manual trigger
5. Remove old density-based reflection triggers

### Phase 8: Testing and stabilization
1. Fill in remaining unit tests
2. Integration test suite
3. Fix sim isolation (isolated DBs per run)
4. Run regression sim suite
5. Tune thresholds based on sim results

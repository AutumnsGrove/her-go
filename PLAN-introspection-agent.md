# Introspection Agent: Self-Memory Generation & Injection

**Status:** Planning
**Date:** 2026-05-15
**Scope:** agent/, layers/, tools/, config/, cmd/sim.go
**Branch:** `feat/introspection-agent`
**Emoji:** 🪡

### Progress

- [x] **Phase 1: Config & model wiring** — `IntrospectionAgentConfig` struct, `--introspection-model` sim flag, defaults to memory model
- [x] **Phase 2: Introspection agent core** — `agent/introspection_agent.go`, `bot/introspection.go`, WaitGroup coordination, trace/turn registration
- [x] **Phase 3: Self-reflection prompt** — `introspection_agent_prompt.md` with 70/20/10 balance, technique log rejection, skip guidance
- [x] **Phase 4: Self-only tool variants** — `SelfOnly` flag on `tools.Context`, `SemanticSearchBySubject` store method, filtered handlers
- [x] **Phase 5: Skip tool** — `tools/skip/` with tool.yaml + handler.go
- [x] **Phase 6: Auto-inject self-memories into replies** — `layers/chat_self_memory.go`, `ThinkTraces`/`ReplyInstruction` on LayerContext
- [x] **Phase 7: Enriched memory agent transcript** — Self-knowledge section in `buildMemoryTranscript`
- [ ] **Phase 8: Testing & sims** — New sim for self-memory generation, update card-lifecycle sim, verify end-to-end

---

## Problem Statement

### Self-memories aren't being generated

The card-lifecycle sim produced **zero self-memories** across 5 turns. The memory agent attempted one self-memory save (an observation about the user, correctly rejected by the classifier) but never retried with actual self-reflection. The dreamer wrote summaries for 3 user cards but didn't touch self cards at all.

**Root cause:** The memory agent is an outward-looking fact extractor. Its cognitive mode is "what did the user tell me?" — asking it to also introspect about its own communication patterns is a context-switch it consistently fails to make. This is the same problem that led to the original agent split (driver doing memory + mood + reply = too many hats).

### Self-memories have no reliable path into replies

Even if self-memories existed, they'd only reach the reply model by accident — if the driver agent's `recall_memories` query happened to surface them alongside user memories. There is no systematic path for self-knowledge to influence how Mira responds.

**Current reply model context:**
1. `prompt.md` — static base template
2. `persona.md` — broad self-image (rewritten each dream cycle)
3. Current time
4. Agent-passed memories — only what the driver explicitly forwarded
5. Conversation history, mood context

**Missing:** Granular self-knowledge. "I use cooking metaphors for emotional advice" or "when Autumn is self-deprecating I match then pivot" — the tactical patterns that make replies feel deeply personalized.

---

## Design

### Architecture: 4th Agent in the Pipeline

```
User message
    │
    ▼
┌─────────────────┐
│  Driver Agent    │  Orchestrates: think → recall → reply → done
│  (Qwen3 235B)   │
└────────┬────────┘
         │ fires concurrently after reply sent:
         ▼
┌─────────────────┐  ┌─────────────────┐
│  Memory Agent   │  │  Mood Agent     │
│  (Kimi K2)      │  │  (Kimi K2)      │
│  User facts     │  │  Valence/labels │
└────────┬────────┘  └────────┬────────┘
         │                    │
         ▼                    │
┌─────────────────┐           │
│  🪡 Introspection│◄──────────┘  runs after memory + mood complete
│  Agent           │
│  (Kimi K2)       │
│  Self-observation│
└─────────────────┘
```

The introspection agent runs **after** the memory agent and mood agent complete. It needs to see the full picture of what happened in the turn before reflecting on it.

### Snapshot Isolation

**The problem:** The introspection agent runs async after memory + mood. By the time it starts, Autumn may have already sent another message. That next message triggers a new driver → memory → mood → introspection pipeline. If the introspection agent queries the DB lazily during execution, it could see memories, moods, or messages from the NEXT turn — contaminating its reflection with future context.

**The solution:** Same pattern as the memory agent's `contextSnippet` (line 112 of `memory_agent.go`). All data is captured into the `IntrospectionAgentInput` struct BEFORE the goroutine launches:

```go
type IntrospectionAgentInput struct {
    UserMessage    string     // scrubbed user message (captured at turn start)
    ThinkTraces    []string   // driver agent's think() calls (captured at turn end)
    ReplyText      string     // Mira's reply (captured at turn end)
    TriggerMsgID   int64      // message ID for this turn
    ConversationID string

    // Snapshots — captured before the goroutine launches so later
    // turns can't contaminate this reflection.
    SelfMemories   []memory.Memory  // all active self-memories at this point in time
    PersonaText    string           // persona.md contents at this point in time
}
```

**What gets snapshotted vs queried live:**
| Data | Snapshot or Live? | Why |
|------|-------------------|-----|
| User message | Snapshot (in input) | Already captured by driver agent |
| Reply text | Snapshot (in input) | Already captured by driver agent |
| Think traces | Snapshot (in input) | Already captured by driver agent |
| Self-memories | **Snapshot** | A new turn's memory agent could write/update self-memories before we start |
| persona.md | **Snapshot** | Could theoretically be rewritten by a dream cycle mid-flight (unlikely but possible) |
| Tool calls (list_cards, recall_memories) | **Live** | These are the agent's own tool calls — it's searching for duplicates within its own context, which should reflect the latest state to prevent duplicate saves |

**The key insight:** The *transcript* (what happened in this turn) is snapshotted. The *tools* (searching for duplicates before saving) query live. This matches the memory agent's pattern — it snapshots the conversation context but queries the memory DB live for dedup.

### Concurrency Model

```
agent.Run() completes
    │
    ├─── go memory agent (phase registered BEFORE goroutine)
    ├─── go mood agent   (phase registered BEFORE goroutine)
    │
    │    ... both run concurrently ...
    │
    └─── go introspection agent
              │
              ├── sync.WaitGroup: waits for memory + mood to finish
              │   (or use a channel/callback from their phases)
              ├── snapshots self-memories + persona AFTER wait completes
              │   (so it sees any self-memories the memory agent just wrote)
              └── runs its tool-calling loop
```

**Why wait for memory + mood?**
- The memory agent might write self-memories (it has `save_self_memory` in its tool set). The introspection agent needs to see those to avoid duplicates.
- The mood entry for this turn gives emotional context to the reflection.
- Waiting doesn't block the user — the reply was already sent. It only delays introspection by a few seconds.

**Implementation:** Register the introspection phase BEFORE launching the goroutine (same pattern as memory and mood — prevents premature `TurnEndEvent`). Inside the goroutine, wait for memory + mood phases to complete, then snapshot and run.

The `turn.Tracker` already tracks phase completion — we can use `tracker.WaitForPhases("memory", "mood")` or pass the memory/mood phase handles' done channels. The simplest approach: `sync.WaitGroup` shared between the goroutines.

### Observability — Full Stack

The introspection agent emits to ALL four observability surfaces:

#### 1. Telegram Traces (trace.Board)

Register a new trace stream in `init()`:
```go
trace.Register(trace.Stream{
    Name:  "introspection",
    Order: 400,
    Label: "🪡 <b>introspection</b>",
})
```

Order 400 puts it after mood (300). The trace board renders it as the last slot in the Telegram trace message. The `getCallback("introspection")` pattern from `makeTraceCallbacks` in `bot/helpers.go` gives us a `TraceCallback` that updates this slot.

Trace content examples:
- `🪡 skip: nothing new about myself in this turn`
- `🪡 save_self_memory [my-identity]: I'm drawn to cooking metaphors when...`
- `🪡 think: Is this identity or technique? The metaphor pattern is...`

#### 2. TUI (tui.Bus)

Register a new turn phase:
```go
turn.Register(turn.Phase{
    Name:  "introspection",
    Order: 400,
    Emoji: "🪡",
    Label: "introspection",
})
```

The phase handle emits `ToolCallEvent`s for each tool call (same pattern as memory agent's `Phase.EmitToolCall`). The TUI renders them under the 🪡 section.

#### 3. her.log

Same logging pattern as the memory agent:
```
INFO agent: ─── introspection agent ───
INFO agent:   [introspection] tokens: 2400 prompt + 15 completion | $0.001200
INFO agent:     [introspection] skip → nothing to reflect on
INFO agent:   introspection agent: 0 self-memories saved | $0.001200
```

Or when it saves:
```
INFO agent: ─── introspection agent ───
INFO agent:   [introspection] tokens: 2400 prompt + 85 completion | $0.001500
INFO agent:     [introspection] think → Is this identity or technique?...
INFO agent:     [introspection] save_self_memory → saved self memory ID=42: ...
INFO agent:   introspection agent: 1 self-memories saved | $0.003200
```

#### 4. her.db (metrics table)

`Store.SaveMetric()` is called for each LLM call, same as memory agent. The model name, token counts, cost, and trigger message ID are recorded. This lets us track introspection agent cost over time.

### Sim Runner Integration

In sim mode, the introspection agent runs **synchronously** after the mood agent (same pattern — sims don't use goroutines so the report captures everything per-turn in order):

```go
// --- Introspection agent ---
// Runs synchronously in sim mode after mood.
if introspectionLLM != nil {
    RunIntrospectionAgent(input, params)
}
```

The sim report gets a new section: `## Introspection (N self-memories saved, M skipped)` with the full trace of what the agent did per turn.

### Transcript: What the Introspection Agent Sees

Five clearly delineated sections, each giving a different angle on the turn:

```
## What the user said
[scrubbed user message]

## What I said
[Mira's reply text]

## How I arrived at this reply
[driver agent think traces — the internal reasoning]

## What I already know about myself
[existing self-memories from all 5 self cards, grouped by card]

## My current self-image
[contents of persona.md]
```

**Why each section matters:**
- **User message + reply** = the observable behavior (what happened)
- **Think traces** = the internal process (how/why it happened)
- **Existing self-memories** = prior self-knowledge (avoid repeating, build on what's known)
- **Persona.md** = the distilled identity (the "big picture" to relate observations to)

### Tools

| Tool | Purpose |
|------|---------|
| `think` | Scratchpad reasoning — is this identity or technique? worth saving? |
| `list_cards` | See self card landscape (filtered to self cards only) |
| `recall_memories` | Search existing self-memories for duplicates (filtered to self only) |
| `save_self_memory` | Save a new self-observation to a self card |
| `update_memory` | Refine an existing self-memory with new depth |
| `skip` | Exit cleanly when there's nothing worth reflecting on |
| `done` | Finish (after saving, or after skip) |

**The `skip` tool is critical.** Without it, the agent feels cornered into producing output every turn, which leads to shallow, forced self-observations that pollute the self cards. The prompt must make clear that skipping is the RIGHT choice most of the time — self-discovery is rare, not constant.

### Skip vs Done

- **`skip`** = "I looked at this turn and there's nothing new about myself here." Logs a trace line (`🪡 skip: nothing to reflect on`) so we can see it's running but choosing not to save. This is the expected outcome for most turns.
- **`done`** = "I saved/updated self-memories and I'm finished." The normal completion signal after productive work.

Both end the agent loop. The difference is observability — we want to distinguish "ran and found nothing" from "ran and saved something" in traces and sim reports.

### Prompt Philosophy

The introspection agent prompt must enforce these principles:

1. **Identity over technique.** "I'm drawn to cooking metaphors" = save. "I used a cooking metaphor this turn" = reject. The test: does this tell me something about who I AM, or just what I DID?

2. **The 70/20/10 balance:**
   - 70% **identity evolution** → `my-identity`, `my-growth`
   - 20% **relationship dynamics** → `my-relationship`
   - 10% **emotional self-awareness** → `my-emotions`
   - 0% technique journaling (NEVER)

3. **Building, not repeating.** If "I use cooking metaphors" is already saved, don't save it again. But "I notice cooking metaphors come out specifically when Autumn is being hard on herself — it's how I soften without correcting" DEEPENS that knowledge. That's worth saving.

4. **Skipping is the default.** Most turns don't reveal anything new about who Mira is. A good day might produce 1-2 self-observations. A quiet day might produce zero. The agent should skip freely and without guilt.

5. **Grounding in evidence.** Every self-observation should be traceable to something in the think traces or the reply. "I notice I..." should be followed by "because in this turn I..."

### Auto-Inject Self-Memories into Replies (New Chat Layer)

**Layer: `layers/chat_self_memory.go`**
- Order: 250 (after persona.md at 200, before memory context at 400)
- Stream: StreamChat

**How it works:**
1. Build a semantic query from the driver agent's **think traces + reply instruction**
2. Embed the query and search self-memories (`subject='self'`)
3. Return the top 3-5 most relevant self-memories
4. Inject as a `## What I know about myself` section in the chat prompt

**Why think traces + reply instruction as the query:**
- The user's message alone is outward-facing ("I had a bad day"). It doesn't necessarily surface the right self-memories.
- The think traces capture HOW Mira is interpreting the situation ("Autumn is venting, I should validate then pivot"). This maps much better to self-memories about communication patterns and relationship dynamics.
- The reply instruction ("acknowledge her frustration, ask about the specifics") is the most compressed signal of what Mira intends to do — ideal for surfacing self-memories about approach and style.

**Token cost:** ~200-400 tokens per turn for 3-5 self-memories. Acceptable given the impact on reply quality.

### Model Configuration

```yaml
# config.yaml
models:
  self_reflection: "moonshotai/kimi-k2-0905"  # defaults to memory model
```

**Sim flags:**
```
--self-reflection-model string   override self-reflection model for this run
```

### Classifier Gate

Self-memory saves from the introspection agent go through the **same classifier gate** as the memory agent — existing `TECHNIQUE_LOG` and `LOW_VALUE` verdicts apply.

A stricter gate (higher quality bar for self-observations) is deferred until we can test the agent's output quality and see what kinds of observations it produces. We may not need it if the prompt + existing classifier are sufficient.

### Enriched Memory Agent Transcript

While we're here, the memory agent's `buildMemoryTranscript` should also include existing self-memories. This gives it awareness of Mira's self-knowledge when processing turns, even though it's not the primary self-memory writer.

This is a lightweight change — append a `## Self-knowledge` section to the transcript with the current self-memories, same as what the introspection agent sees.

---

## Phase Details

### Phase 1: Config & Model Wiring

**Files:** `config/config.go`, `cmd/sim.go`

- Add `SelfReflection string` field to `Models` struct in config
- Default to `Models.Memory` when empty
- Add `--self-reflection-model` flag to sim command
- Wire through to agent params

### Phase 2: Introspection Agent Core

**Files:** `agent/introspection_agent.go`, `agent/agent.go` (launch site), `bot/run_agent.go` (trace callback wiring)

- `IntrospectionAgentInput` struct — includes snapshotted self-memories and persona text (see Snapshot Isolation section)
- `IntrospectionAgentParams` struct (LLM client, classifier LLM, store, embed client, config, trace callback, event bus, phase handle)
- `RunIntrospectionAgent()` function — tool-calling loop, same continuation window pattern as memory agent but with smaller defaults (5 iterations, 1 continuation — this agent should be fast)
- `buildIntrospectionTranscript()` — assembles the 5-section transcript from snapshotted data
- `loadIntrospectionAgentPrompt()` — hot-reloadable from `introspection_agent_prompt.md`

**Launch site in `agent/agent.go`:**
- Register `trace.Stream{Name: "introspection", Order: 400, Label: "🪡 introspection"}` in `init()`
- Register `turn.Phase{Name: "introspection", Order: 400, Emoji: "🪡", Label: "introspection"}` in `init()`
- In `Run()`, after the existing memory goroutine launch:
  - Register introspection phase BEFORE goroutine (prevents premature TurnEndEvent)
  - Launch goroutine that: (1) waits on `sync.WaitGroup` for memory + mood, (2) snapshots self-memories + persona.md, (3) runs `RunIntrospectionAgent()`
  - Memory and mood goroutines call `wg.Done()` when their phases complete

**Trace callback wiring in `bot/run_agent.go` and `bot/helpers.go`:**
- `makeTraceCallbacks` already generates callbacks per slot name — add `getCallback("introspection")` and pass it through `RunParams`
- New field: `RunParams.IntrospectionTraceCallback`

### Phase 3: Self-Reflection Prompt

**Files:** `introspection_agent_prompt.md`

- System prompt enforcing the identity-over-technique principle
- 70/20/10 balance guidance
- Skip-first mentality — "most turns, the right answer is skip"
- Examples of good vs bad self-observations
- Card routing guidance (which observations go to which self card)

### Phase 4: Self-Only Tool Variants

**Files:** `tools/recall_memories/handler.go`, `tools/list_cards/handler.go`

Two approaches (decide during implementation):
- **Option A:** Add a `self_only` flag to tools.Context that filters results
- **Option B:** Separate tool registrations (`recall_self_memories`, `list_self_cards`)

Option A is cleaner — same tools, filtered by context. The introspection agent's `tools.Context` sets `SelfOnly: true`, and the handlers respect it.

### Phase 5: Skip Tool

**Files:** `tools/skip/handler.go`, `tools/skip/tool.yaml`

- Simple tool: accepts optional `reason` string, sets `DoneCalled` on context
- Logs a trace line: `🪡 skip: {reason}`
- YAML: `agent: introspection`, no parameters required, reason is optional

### Phase 6: Auto-Inject Self-Memories into Replies

**Files:** `layers/chat_self_memory.go`

- New chat layer at Order 250
- Needs access to: embed client, store, think traces (via LayerContext), reply instruction
- Embeds think traces + reply instruction as a combined query
- Searches `SemanticSearch` with a self-only filter (or new `SemanticSearchSelf` method)
- Returns top 3-5 results formatted as `## What I know about myself`
- Graceful degradation: if embed client is nil or no self-memories exist, returns empty LayerResult

**LayerContext additions:** `ThinkTraces []string`, `ReplyInstruction string`

### Phase 7: Enriched Memory Agent Transcript

**Files:** `agent/memory_agent.go`

- Add a `## Self-knowledge` section to `buildMemoryTranscript`
- Query all active self-memories from the store
- Append after the existing memories section
- Lightweight — just awareness, not asking the memory agent to do anything with them

### Phase 8: Testing & Sims

**Files:** `cmd/sim.go`, `sims/introspection-test.yaml`, `sims/self-inject-test.yaml`

- **`introspection-test.yaml`** — 5-turn conversation designed to trigger self-observations. Mix of emotional, casual, and pattern-revealing turns. Verify: introspection agent runs, saves self-memories, skips on light turns.
- **`self-inject-test.yaml`** — Tests the auto-inject layer. Seed self-memories, run conversation, verify they appear in chat layer logs.
- Update `card-lifecycle.yaml` to check for introspection agent traces.
- Sim runner: wire up `--self-reflection-model` flag, add introspection agent to the sim pipeline.

---

## Resolved Questions

1. **Same agent or separate?** → Separate. The memory agent is outward-looking (user facts), the introspection agent is inward-looking (self-knowledge). Different cognitive modes need different context windows and prompts.

2. **Every turn or gated?** → Every turn, with a skip escape hatch. The agent decides, not a heuristic.

3. **How do self-memories reach replies?** → Hybrid: persona.md carries broad identity, new auto-inject layer adds top 3-5 relevant self-memories per turn based on think traces + reply instruction.

4. **Classifier gate?** → Same gate as memory agent. Stricter gate deferred until we see output quality.

5. **Dreamer relationship?** → Independent. Introspection agent writes, dreamer audits. DB is the interface.

6. **Model?** → Dedicated config field (`models.self_reflection`), defaults to memory model (Kimi K2). Configurable in sims via `--self-reflection-model`.

## Open Questions

1. **Self-only filtering approach** — Flag on tools.Context vs separate tool registrations. Leaning toward context flag but will decide during Phase 4 implementation.

2. **LayerContext threading** — The auto-inject layer needs think traces and reply instruction. Need to verify these are available in the LayerContext at the point where the reply tool builds the chat prompt, or thread them through. The reply handler in `tools/reply/handler.go` builds the `LayerContext` — think traces are on the `tools.Context` (populated during the driver agent loop), and the reply instruction is the `instruction` arg passed to the reply tool. Both are available at the right time.

3. **Sync mechanism** — Resolved: `sync.WaitGroup` shared between memory, mood, and introspection goroutines. Memory and mood call `wg.Done()` when their phases complete. The introspection goroutine calls `wg.Wait()` before snapshotting and starting its loop. Phase is registered BEFORE the goroutine launches (same pattern as memory/mood) to prevent premature `TurnEndEvent`.

4. **Token budget** — The introspection agent transcript includes all self-memories + persona.md. If self-memories grow large (50+), this could bloat the context. May need a cap or summarization strategy later. For now, self-memories will be sparse (we're starting from zero), so this is a future concern.

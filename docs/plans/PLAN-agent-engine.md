# Agent Engine Extraction Plan

## 1. Context

The her-go codebase has 5 agentic agents (driver, memory, introspection, worker, dream) that each independently implement the same tool-calling loop pattern: build messages, call LLM with tools, execute tool calls, append results, check for done, handle continuations. This produces ~950 lines of duplicated loop logic spread across 4 packages (`agent/`, `workeragent/`, `persona/`).

Extracting a shared `agent_engine` package eliminates this duplication while preserving each agent's unique behavior through a config-with-callbacks pattern. The engine owns the loop skeleton; agents own their identity, hooks, and pre/post logic.

Simultaneously, the memory write pipeline (style gate, length gate, dedup, classifier, safety classifier) is duplicated across `save_memory`, `update_memory`, and `merge_memories`. Extracting it to `tools/memgate/` makes it a single shared middleware.

**Estimated net reduction: ~470 lines** (350 new, 820 removed duplication).

## 2. Architecture

```
agent_engine/           NEW - shared loop engine
  engine.go             EngineConfig, LoopResult, RunLoop()
  engine_test.go        Unit tests with mock LLM
  trace.go              Trace formatting helpers (moved from agent/)

tools/memgate/          NEW - memory write pipeline middleware
  pipeline.go           Gate type, RunPipeline(), gate implementations
  pipeline_test.go      Unit tests with mock classifier
```

### Dependency graph (post-extraction)

```
bot/run_agent.go
  -> agent/agent.go (driver)           -> agent_engine.RunLoop()
  -> agent/memory_agent.go             -> agent_engine.RunLoop()
  -> agent/introspection_agent.go      -> agent_engine.RunLoop()

scheduler/
  -> workeragent/worker.go             -> agent_engine.RunLoop()

persona/dream_cycle.go
  -> persona/memory_dreamer.go         -> agent_engine.RunLoop()

tools/save_memory/     -> tools/memgate.RunPipeline()
tools/save_self_memory -> tools/memgate.RunPipeline() (via ExecSaveMemory)
tools/update_memory/   -> tools/memgate.RunPipeline()
tools/merge_memories/  -> tools/memgate.RunPipeline()
```

The `agent_engine` package imports: `her/llm`, `her/memory`, `her/tools`, `her/tui`, `her/turn`, `her/logger`. It does NOT import `her/agent`, `her/workeragent`, or `her/persona` -- the dependency arrow always points inward.

## 3. Design Pattern: Config Struct with Callbacks

Each agent creates an `EngineConfig` with its dependencies and hook functions, then calls `RunLoop()`. Hooks are nil-safe -- nil means "use default" or "skip." This avoids inheritance (Go doesn't have it) and avoids interfaces (which force stub methods for hooks the agent doesn't care about).

The Python analogy: it's like passing `key=` and `default=` kwargs to a library function, not like subclassing a base class.

## 4. EngineConfig Struct

```go
package agent_engine

// EngineConfig defines a single agent run. The engine owns the loop;
// the caller owns identity, prompt construction, and behavioral hooks.
type EngineConfig struct {
    // -- Identity --
    Name       string // "driver", "memory", "introspection", "worker", "dream"
    MetricRole string // memory.RoleDriver, memory.RoleMemory, etc.

    // -- Core dependencies --
    LLM          *llm.Client
    Store        memory.Store
    ToolDefs     []llm.ToolDef
    ToolCtx      *tools.Context   // pre-built by caller
    Messages     []llm.ChatMessage // initial [system, user] messages
    TriggerMsgID int64

    // -- Loop tuning --
    IterationsPerWindow int // default 15, hard cap 50
    MaxContinuations    int // default 2, hard cap 10

    // -- Observability (built-in, not hooks) --
    // The engine owns ALL trace emission. Agents never format or send
    // trace lines — the engine does it automatically using tools.FormatTrace().
    // This guarantees consistent formatting, HTML escaping, truncation,
    // and slot rendering across all agents.
    TraceCallback tools.TraceCallback // nil = no tracing
    LiteToolHook  func(toolName string) // nil = no lite tracing
    EventBus      *tui.Bus              // nil-safe
    Phase         *turn.PhaseHandle     // nil-safe

    // -- Level 1 Hooks (all nil-safe) --

    // ToolChoiceFirst: if non-nil, used as tool_choice on iteration 0
    // of window 0. Typically "required" for the driver agent.
    ToolChoiceFirst interface{}

    // ContinuationMsg builds the system message injected when a
    // continuation window opens. nil = engine uses a sensible default.
    ContinuationMsg func(window, maxWindows int, summary string) string

    // PreTool fires before each tool execution. Return skip=true to
    // prevent execution (engine appends skipResult as the tool result).
    // Use case: dream agent's maxOps safety cap, dry-run interception.
    PreTool func(tc llm.ToolCall, tctx *tools.Context) (skipResult string, skip bool)

    // PostTool fires after each tool execution. Called AFTER the engine
    // has already handled tracing and TUI events. This hook is only for
    // agent-specific concerns (SaveAgentTurn, think capture, op counting).
    PostTool func(tc llm.ToolCall, result string, isError bool)

    // PreIteration fires before each LLM call.
    PreIteration func(iteration, window int)

    // PostIteration fires after each LLM response, before tool
    // execution. Return true to break the loop.
    // Use case: driver loop detection (repeated think calls).
    PostIteration func(iteration, window int, resp *llm.ChatResponse) (breakLoop bool)

    // OnNoToolCalls fires when the LLM returns no tool calls.
    // Return true to suppress the default "break outer" behavior.
    // Use case: driver "done" text detection, agentFinalText capture.
    OnNoToolCalls func(resp *llm.ChatResponse) (handled bool)

    // OnLoopExit fires when the loop ends for any reason.
    // Use case: driver fallback reply path, placeholder cleanup.
    OnLoopExit func(reason string, messages []llm.ChatMessage)

    // ActiveToolGuard validates tool calls before execution. Return
    // errResult to reject. Used by driver for ActiveTools whitelist.
    ActiveToolGuard func(tc llm.ToolCall) (errResult string, reject bool)
}
```

## 5. LoopResult Struct

```go
type LoopResult struct {
    Messages   []llm.ChatMessage // final message history
    TotalCost  float64
    ToolCalls  int
    Iterations int
    ExitReason string // "done", "no_tool_calls", "max_iterations",
                      // "max_continuations", "error", "hook_break",
                      // "finish_reason_stop"
    TraceLines []string
}
```

## 6. RunLoop Algorithm (with hook injection points)

```go
func RunLoop(cfg EngineConfig) (*LoopResult, error) {
    iterationsPerWindow := coerce(cfg.IterationsPerWindow, 15, 50)
    maxContinuations    := coerce(cfg.MaxContinuations, 2, 10)

    messages   := cfg.Messages
    var totalCost, totalTools, totalIters int/float
    var traceLines []string
    tracing := cfg.TraceCallback != nil

    exitReason := "max_continuations"

outer:
    for window := 0; window <= maxContinuations; window++ {

        // >> HOOK: ContinuationMsg (window > 0)
        if window > 0 {
            summary := buildContinuationSummary(traceLines)
            if cfg.ContinuationMsg != nil {
                contMsg = cfg.ContinuationMsg(window, maxContinuations, summary)
            } else {
                contMsg = defaultContinuationMsg(...)
            }
            messages = append(messages, system message)
        }

        for i := 0; i < iterationsPerWindow; i++ {
            totalIters++

            // >> HOOK: PreIteration
            if cfg.PreIteration != nil { cfg.PreIteration(i, window) }

            // Tool choice (ToolChoiceFirst on iter 0, window 0)
            toolChoice := nil
            if i == 0 && window == 0 && cfg.ToolChoiceFirst != nil {
                toolChoice = cfg.ToolChoiceFirst
            }

            // LLM call
            resp, err := cfg.LLM.ChatCompletionWithTools(messages, cfg.ToolDefs, toolChoice)
            if err != nil { exitReason = "error"; break outer }

            // Metrics + cost accumulation + TUI event emission
            cfg.Store.SaveMetric(...)
            totalCost += resp.CostUSD

            // >> HOOK: PostIteration (before tool execution)
            if cfg.PostIteration != nil {
                if cfg.PostIteration(i, window, resp) {
                    exitReason = "hook_break"; break outer
                }
            }

            // No tool calls -> exit
            if len(resp.ToolCalls) == 0 {
                // >> HOOK: OnNoToolCalls
                if cfg.OnNoToolCalls != nil && cfg.OnNoToolCalls(resp) {
                    continue  // hook handled it
                }
                exitReason = "no_tool_calls"; break outer
            }

            // Append assistant message
            messages = append(messages, assistant msg with ToolCalls)

            // Execute each tool call
            for _, tc := range resp.ToolCalls {
                if tc.Function.Name == "" { continue }

                // >> HOOK: ActiveToolGuard
                if cfg.ActiveToolGuard != nil {
                    if errResult, reject := cfg.ActiveToolGuard(tc); reject {
                        append error result; continue
                    }
                }

                // >> HOOK: PreTool (dry-run, safety caps)
                var result string
                if cfg.PreTool != nil {
                    if skipResult, skip := cfg.PreTool(tc, cfg.ToolCtx); skip {
                        result = skipResult
                    } else {
                        result = tools.Execute(tc, cfg.ToolCtx)
                    }
                } else {
                    result = tools.Execute(tc, cfg.ToolCtx)
                }

                totalTools++
                isError := strings.HasPrefix(result, "error:") || ...

                // == BUILT-IN: Trace emission (not a hook) ==
                // Every agent gets this for free. Consistent formatting,
                // HTML escaping, and truncation via tools.FormatTrace().
                if tracing {
                    line := tools.FormatTrace(tc.Function.Name, tc.Function.Arguments, result)
                    traceLines = append(traceLines, line)
                    sendTrace()
                }

                // == BUILT-IN: Lite trace (not a hook) ==
                if cfg.LiteToolHook != nil {
                    cfg.LiteToolHook(tc.Function.Name)
                }

                // == BUILT-IN: TUI event emission ==
                emitToolCallEvent(cfg, tc, result, isError)

                // >> HOOK: PostTool (agent-specific only: SaveAgentTurn, think capture, etc.)
                if cfg.PostTool != nil { cfg.PostTool(tc, result, isError) }

                // Append tool result to messages
            }

            // Done check
            if cfg.ToolCtx.DoneCalled { exitReason = "done"; break outer }

            // finish_reason=stop exit
            if resp.FinishReason == "stop" {
                exitReason = "finish_reason_stop"; break outer
            }
        }

        if window == maxContinuations { break outer }
    }

    // >> HOOK: OnLoopExit
    if cfg.OnLoopExit != nil { cfg.OnLoopExit(exitReason, messages) }

    return &LoopResult{...}, nil
}
```

### Hook injection point summary

| Hook | When | Use Case |
|------|------|----------|
| `ToolChoiceFirst` | iter 0, window 0 | Driver: `"required"` to force tool use |
| `ContinuationMsg` | window > 0 | Custom continuation prompts per agent |
| `PreIteration` | before each LLM call | Introspection: latency tracking |
| `PostIteration` | after LLM response, before tools | Driver: loop detection (repeated think) |
| `OnNoToolCalls` | LLM returns no tool calls | Driver: "done" text detection, agentFinalText |
| `ActiveToolGuard` | before each tool execution | Driver: ActiveTools whitelist |
| `PreTool` | before tools.Execute | Dream: dry-run interception, maxOps cap |
| `PostTool` | after tools.Execute (after built-in tracing) | Driver: SaveAgentTurn, think capture, op counting |
| `OnLoopExit` | after loop ends | Driver: fallback reply, placeholder cleanup |

### What's built-in vs what's a hook

The engine handles these **automatically** for every agent (not hooks):
- **Full trace emission**: `tools.FormatTrace()` → `traceLines` → `sendTrace()` — consistent HTML, emoji, truncation
- **Lite trace emission**: `LiteToolHook(toolName)` — tool sequence tracking for compact view
- **TUI event emission**: `Phase.EmitToolCall()` / `EventBus.Emit(ToolCallEvent{})` — nil-safe
- **Iteration events**: `AgentIterEvent` with tokens, cost, finish reason
- **Continuation headers**: `"🔄 continuation window N/M"` trace line
- **Error/fallback traces**: `"❌ error: ..."`, `"⚡ fallback: model"`
- **Metric saving**: `Store.SaveMetric()` with role tag

Agent hooks are only for **agent-specific concerns** that differ between agents.

## 7. Trace Unification

### Problem

The current trace system has 7 inconsistencies across agents:

| Issue | Impact |
|-------|--------|
| Introspection doesn't use `tools.FormatTrace()` — manual plain-text formatting | Traces look different in Telegram (no emoji, no HTML) |
| Introspection truncates to 60 chars, others use 80+ | Results cut short |
| Memory dreamer registers a trace slot but never writes to it | Dream cycle is invisible |
| Worker has no lite-mode integration in `liteTraceState` | Worker traces always verbose |
| Each agent reimplements `traceLines []string` + `sendTrace()` closure | 5 copies of identical boilerplate |
| Continuation summary HTML stripping is hardcoded per-agent | New tags break stripping |
| TUI event emission pattern varies (Phase vs EventBus vs neither) | Inconsistent observability |

### Solution

The engine owns all trace emission as built-in behavior. After every tool call, the engine:

```
1. tools.FormatTrace(name, args, result)    // consistent formatting
2. traceLines = append(traceLines, line)    // accumulate
3. sendTrace()                              // push to Board slot
4. LiteToolHook(name)                       // lite mode tracking
5. EmitToolCallEvent(...)                   // TUI event
```

No agent reimplements any of this. The driver's one special case (reply fallback annotation) is handled in its PostTool hook, which can *append* an extra trace line — but the engine already emitted the standard one.

### What this fixes

- **Introspection**: automatically gets `tools.FormatTrace()` formatting, correct truncation, HTML
- **Dream agent**: automatically gets traces just by being migrated to the engine — the registered `persona` slot starts receiving data
- **Worker**: gets lite-mode support automatically via the engine's `LiteToolHook` field
- **All agents**: one implementation of `traceLines`, `sendTrace()`, continuation headers, error traces
- **Future agents**: get full trace support by default — zero trace code needed

### What stays agent-specific

- **Driver**: PostTool hook adds reply fallback annotation (`"⚡ reply(fallback → model)"`)
- **Mood agent**: not a tool-loop agent, keeps its own `traceBuf` (this is correct — it traces decisions, not tool calls)

## 8. memgate Pipeline Design (Level 2)

```go
package memgate

type Verdict struct {
    Allowed  bool
    Reason   string   // human-readable rejection reason
    Rewrite  string   // classifier-suggested rewrite (empty if none)
    Splits   []string // SPLIT verdict sub-memories
}

type PipelineInput struct {
    Text      string // the memory text to validate
    Subject   string // "user" or "self"
    Tags      string
    Category  string
    Context   string // optional "why this matters" context
    CardID    int64
    OldText   string // non-empty for updates (shows classifier the delta)
}

type PipelineDeps struct {
    Store           memory.Store
    EmbedClient     *embed.Client
    ClassifierLLM   *llm.Client
    Threshold       float64
    MaxLength       int
    ConversationID  string
    TriggerMsgID    int64
    Snippet         []memory.Message // pre-captured context for classifier
    PreApproved     map[string]bool  // bypass on classifier-suggested rewrites
    SkipDedup       bool             // merge_memories skips dedup
}

// RunPipeline validates a memory write through all gates in order.
// Returns early on the first rejection.
//
// Gate order (cheapest first):
//   1. Style blocklist  (string matching, ~0ms)
//   2. Length gate       (len() check, ~0ms)
//   3. Embedding dedup   (two-vector tag+text, ~50ms, skipped if SkipDedup)
//   4. Classifier LLM    (~200-500ms)
//   5. Safety classifier (self-subject only, ~200-500ms)
func RunPipeline(input PipelineInput, deps PipelineDeps) Verdict { ... }
```

### Callers after extraction

| Tool | Before | After |
|------|--------|-------|
| `save_memory` / `save_self_memory` | `ExecSaveMemory()` in `tools/memory_helpers.go` -- inline pipeline ~170 lines | `memgate.RunPipeline()` then `Store.SaveMemory()` |
| `update_memory` | Inline style+length+classifier ~60 lines | `memgate.RunPipeline(SkipDedup: true, OldText: old.Content)` then `Store.SaveMemory()` + `Store.SupersedeMemory()` |
| `merge_memories` | Inline style+length+classifier+safety ~90 lines | `memgate.RunPipeline(SkipDedup: true)` then `Store.SaveMemory()` + supersede chains |

## 9. Migration Steps

All in one branch, one commit per logical step.

### Step 1: Create `agent_engine/engine.go`
Create the package with `EngineConfig`, `LoopResult`, `RunLoop()`, and helper functions (`buildContinuationSummary`, `truncateLog`). No callers yet.

**Files created:** `agent_engine/engine.go`, `agent_engine/engine_test.go`

### Step 2: Create `tools/memgate/pipeline.go`
Extract the 5-gate pipeline from `tools/memory_helpers.go`. Pure function, no callers yet.

**Files created:** `tools/memgate/pipeline.go`, `tools/memgate/pipeline_test.go`

### Step 3: Migrate memory write callers to memgate
Replace inline pipelines in `tools/memory_helpers.go`, `tools/update_memory/handler.go`, and `tools/merge_memories/handler.go` with `memgate.RunPipeline()` calls. External APIs stay identical.

### Step 4: Migrate Dream agent (`persona/memory_dreamer.go`)
Simplest agentic loop. ~120 lines of loop -> ~40 lines of config + hooks.

**Hooks used:** `PreTool` (dry-run + maxOps cap), `PostTool` (operation counting), `ContinuationMsg` (includes opCount).

### Step 5: Migrate Worker agent (`workeragent/worker.go`)
~110 lines of loop -> ~35 lines of config + hooks.

**Hooks used:** `PostTool` (LiteToolHook, TUI events), `ContinuationMsg` ("Finish up and call done").

### Step 6: Migrate Memory agent (`agent/memory_agent.go`)
~110 lines of loop -> ~30 lines of config + hooks.

**Hooks used:** `PostTool` (TUI events via Phase/EventBus fallback), `ContinuationMsg` ("Continue... call done or notify_agent").

### Step 7: Migrate Introspection agent (`agent/introspection_agent.go`)
~90 lines of loop -> ~35 lines of config + hooks.

**Hooks used:** `PreIteration` (latency timer), `PostTool` (TUI events), `ContinuationMsg` ("Call skip or done now").

**Watch for:** Introspection appends messages differently (one assistant+tool pair per tool call instead of batch). If behavior diverges in testing, add a `SingleToolCallMessages bool` field to EngineConfig.

### Step 8: Migrate Driver agent (`agent/agent.go`)
Most complex. ~340 lines of loop -> ~80 lines of config + hooks.

**Hooks used:** All of them.
- `ToolChoiceFirst`: `"required"` (conditionally from config)
- `PostIteration`: loop detection (repeated think calls)
- `OnNoToolCalls`: "done" text detection, agentFinalText capture
- `ActiveToolGuard`: ActiveTools whitelist
- `PostTool`: SaveAgentTurn, think trace capture, LiteToolHook, tool sequence tracking, reply fallback trace annotation
- `OnLoopExit`: fallback reply paths, orphan placeholder cleanup, auto-done for diffusion models

### Step 9: Cleanup
- Remove `buildContinuationSummary()` from `agent/agent.go` (now in engine)
- Remove duplicate `truncateLog()` from `workeragent/worker.go` and `persona/memory_dreamer.go`
- Remove `executeTool()` from `agent/agent.go` (engine calls `tools.Execute` directly)
- Verify no dead code remains

## 10. Files Summary

### New files
| File | Purpose |
|------|---------|
| `agent_engine/engine.go` | EngineConfig, LoopResult, RunLoop, helpers |
| `agent_engine/engine_test.go` | Unit tests with mock LLM |
| `tools/memgate/pipeline.go` | RunPipeline, Verdict, gate implementations |
| `tools/memgate/pipeline_test.go` | Unit tests with mock classifier |

### Modified files
| File | Change |
|------|--------|
| `agent/agent.go` | Replace ~340-line loop with RunLoop + hooks (~80 lines) |
| `agent/memory_agent.go` | Replace ~110-line loop with RunLoop + hooks (~30 lines) |
| `agent/introspection_agent.go` | Replace ~90-line loop with RunLoop + hooks (~35 lines) |
| `workeragent/worker.go` | Replace ~110-line loop with RunLoop + hooks (~35 lines) |
| `persona/memory_dreamer.go` | Replace ~120-line loop with RunLoop + hooks (~40 lines) |
| `tools/memory_helpers.go` | Replace ~100-line inline pipeline with memgate call (~15 lines) |
| `tools/update_memory/handler.go` | Replace ~40-line inline gates with memgate call |
| `tools/merge_memories/handler.go` | Replace ~50-line inline gates with memgate call |

## 11. Verification

### Unit tests (Steps 1-2)
- **`agent_engine/engine_test.go`** -- mock LLM with scripted responses:
  - Basic loop: 3 tool calls then done -> verify LoopResult
  - Continuation windows: exhaust iterations -> verify injection
  - Max continuations: exhaust all windows -> verify exit reason
  - ToolChoiceFirst: verify first call passes "required", second passes nil
  - PreTool skip: return skip=true -> verify tool not executed
  - PostIteration break: return true -> verify exit reason "hook_break"
  - OnNoToolCalls: LLM returns text -> verify hook called
  - Error handling: LLM returns error -> verify exit reason "error"

- **`tools/memgate/pipeline_test.go`** -- mock classifier and embed client:
  - Style gate: blocked pattern -> rejected
  - Length gate: oversized text -> rejected
  - Dedup: similar embedding -> rejected with existing ID
  - Classifier REJECT -> rejected
  - Classifier SPLIT -> splits returned
  - Self-memory safety -> rejected for sycophancy
  - Happy path: all gates pass -> allowed

### Integration tests (Steps 3-8)
After each agent migration, run sim tests:
```bash
go test ./sims/... -run TestSim
```

### Manual verification checklist
- [ ] Driver: tool calls work, reply delivered, traces appear, fallback reply fires
- [ ] Memory: facts saved after turn, classifier rejects bad writes, traces appear
- [ ] Introspection: self-memories saved when warranted, skip on casual turns
- [ ] Worker: briefing report generated, file written to reports/
- [ ] Dream: dry-run shows planned operations, real run executes merges/rewrites

### Regression signals
- Cost per turn stays the same (no extra LLM calls)
- Tool call counts per turn stay the same
- Trace output is identical (diff Telegram trace messages before/after)
- Continuation windows fire at the same iteration thresholds

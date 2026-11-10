# Plan: Migrate her-go's agent loop onto Nova (and bump to MiMo 2.6)

> Status: **DRAFT v1: pending answers to the "Open questions" section.** Every question has a
> default, so an agent can start without answers and still do the right thing.
> Written 2026-11-10 against her-go `316a748` and Nova `v0.3.5`.

## 0. How to use this document (read first, executing agent)

You are doing a multi-hour migration unattended. These rules override your instincts:

1. **Work on branch `claude/nova-hergo-migration-plan-4lvpwn`. Never push to `main`.** The Le Potato
   host runs `./her update`, which pulls `origin/main`, so anything on `main` goes live.
2. **Small commits.** One concern per commit; commit the moment the gate for that step is green.
   Gate = `go build ./... && go vet ./... && go test ./...` (plus the step's own gate below).
3. **Never weaken or delete an existing test to get green.** If a parity test fails, the new code is wrong
   or the test documents an intentional delta that you record in §5 and in the progress log.
4. **Keep a progress log** at `docs/plans/NOVA-MIGRATION-LOG.md`. After each step append: step id,
   commit hash, gate results, sim cost, surprises. This is how Autumn re-enters the work.
5. **Record Nova gaps, don't patch Nova.** The Nova repo is read-only for you. Workaround in her-go and add
   a row to `docs/plans/NOVA-GAPS.md` (symptom, workaround, suggested Nova issue title).
6. **Stop and write a blocker note** (in the log, then end your turn) instead of improvising on:
   DB migrations, removing/renaming existing `config.yaml` keys, anything touching a real `her.db`,
   or any step whose gate fails twice for reasons you do not understand.
7. **Follow `CLAUDE.md`, including Teaching Mode.** Every new file gets doc comments that explain Go idioms
   for a Python/TS programmer. Data Primacy and the capability-ownership map apply. LLM calls stay owned
   by `llm/`, so no consumer imports Nova's provider packages except `llm/` and the new bridge code in §3.
8. **Sim budget:** see §4.4. Check cumulative sim spend in the log before every live sim run.

## 1. Goal and non-goals

**Goal:** `agent_engine.RunLoop`, her-go's hand-rolled tool-calling loop used by all five agents, runs on
`github.com/AutumnsGrove/nova/agent` instead, with identical observable behaviour, the same sims passing,
and the old loop deleted at the end. Then switch to the MiMo 2.6 models.

**Non-goals for this chunk:** voice (Nova has no TTS/STT yet; `voice/` stays untouched and the seam
stays where it is), Telegram/gateway changes, memory/mood/persona logic changes, DB schema changes,
adopting Nova's `session/` or `compaction/` (her-go's `compact/` stays), Nova's TUI.

## 2. What I found (verified, not assumed)

### her-go side
- One loop: `agent_engine/engine.go` (670 lines), `EngineConfig` → `RunLoop` → `LoopResult`. Exactly
  **5 call sites**: `agent/agent.go` (driver), `agent/memory_agent.go`, `agent/introspection_agent.go`,
  `workeragent/worker.go`, `persona/memory_dreamer.go`.
- Hooks actually used by callers (grep-verified): driver uses `ToolChoiceFirst`, `PostIteration`
  (think-loop detection), `OnNoToolCalls` (always returns false), `ActiveToolGuard`, `PostTool`,
  `OnLoopExit`, `ContinuationMsg`. Introspection: `PreIteration`, `PostIteration`, `PostToolResult`.
  Memory: `PostToolResult`. Dream: `PreTool` (dry-run). Worker: `PostTool`, `ContinuationMsg`.
  **Nobody uses `OnError`, `OnLoopStart` is trivial.** That shrinks the port.
- `engine_test.go` (752 lines) drives the loop against an `httptest` SSE mock through a real
  `llm.Client`. That is a ready-made **parity spec**: run the same table against both backends.
- Tools: handlers are `func(argsJSON string, ctx *tools.Context) string`; schemas/agent scoping/hot-vs-deferred/trace
  specs come from `tools/<name>/tool.yaml`, parsed by `tools/loader.go` into `llm.ToolDef`.
  `tools.Context` is the big dependency bundle (~45 fields). It becomes `ToolData` unchanged.
- LLM: `llm/client.go` (1031 lines) is a hand-written OpenRouter client with fallback model, provider
  routing (`order: ["xiaomi/fp8"]`), `session_id` sticky routing, reasoning toggle, streaming, usage/cost/cache
  parsing. It has **28 call sites** outside the loop (reply tool streaming, classifier, mood, vision, compact,
  fast path), across `cmd`, `persona`, `mood`, `agent`, `workeragent`, `tools`, `bot`, `vision`, `memory`,
  `compact`, `classifier`, `gateway`.

### Nova side
- Library use is first-class and documented: `docs/INTEGRATION.md` has a her-go section, and
  `examples/embed` is a compiling reference (harness layer only). Nova is `v0.3.5` (tags exist, so we can
  pin a tag instead of a `replace`).
- Mapping that INTEGRATION.md proposes, which I checked against the real hook signatures:
  `PreTool`→`Hooks.ToolCall`, `PostToolResult`→`Hooks.ToolResult`, continuation/stop→`Hooks.TurnEnd`
  (`End`, `Continue`, `Inject`), observers→`Emit`, `ToolChoiceFirst`→`Hooks.BeforeProviderRequest`.
  `req.Tools = cfg.Tools.Defs()` is recomputed **every turn** (`agent/turn.go:55`), so deferred
  tool loading (`use_tools`) works by letting `BeforeProviderRequest` set `req.Tools` from `ActiveTools`.
- `provider/openrouter` takes `ProviderOptions` merged into the wire body, which is how `provider.order`,
  `session_id`, and `reasoning` get through. `openrouter.Routing{Order: ...}.Options()` renders `provider.order`.
- `provider.Provider.Stream` returns a `Response` carrying `Model` and `Provider`, and Nova's
  `OnMessage`/`usage` events do **not** carry them. her-go's `SaveMetric` needs both (and `IsFallback`).
  Fix: a small recording decorator around the provider (§3, step 1.4), with no Nova change.
- Nova's loop **runs tools in parallel by default**. her-go's is strictly sequential. We set
  `ToolExecution: agent.Sequential` to preserve behaviour (see §8 for the optional later win).
- Tool args are schema-validated by Nova before dispatch. Error text differs from her-go's
  `error: malformed JSON in arguments...`. Sims/prompts may depend on exact strings, so §3 step 2 keeps
  the guard strings inside the wrapper.

### Feasibility spike (done, in a scratch copy, nothing committed)
Added `require github.com/AutumnsGrove/nova v0.3.5`, bumped `go 1.25.8 → 1.26.6` (Nova requires it), ran
`go mod tidy`, imported `nova/agent`, `provider/openrouter`, `provider/route`, `tool`:
**`go build ./...` and `go test ./...` pass.** Side effect: `go mod tidy` bumps charm.land/*,
`x/sync`, `x/term` and a few indirects. **Deploy implication:** the Le Potato needs Go 1.26.6 (the `go`
directive auto-downloads the toolchain when `GOTOOLCHAIN=auto`, but it needs network on first build).

### MiMo 2.6 (queried live from OpenRouter)
| Model | Context | $/M in / out | cache read $/M | Input | `xiaomi/fp8` endpoint |
|---|---|---|---|---|---|
| `xiaomi/mimo-v2.6-pro` | 1.05M | 0.435 / 0.87 | 0.0036 | text,image,video,audio | **yes** |
| `xiaomi/mimo-v2.6-flash` | 1.05M | 0.14 / 0.28 | 0.0028 | text,image,video,audio | **yes** |
| (now) `mimo-v2.5-pro` | 1.02M | 0.48 / 1.80 | 0.096 | text only | yes |
| (now) `mimo-v2.5` | 1.0M | 0.40 / 2.00 | 0.08 | text,image,audio,video | yes |

So the pin to `xiaomi/fp8` still works, cached-read is ~25–30× cheaper, and Flash is multimodal (it can
replace v2.5 for the vision agent too). Caveat: `mimo-v2.6-pro` does not list `parallel_tool_calls`
(irrelevant while sequential). There is also a `mimo-v2.6-pro-ultraspeed` variant (10× price), which I'd skip.

## 3. Strategy: strangler fig, two seams, flag-guarded

Two independent seams, in this order, each behind a gate. Everything else keeps its current API, so the
diff stays reviewable and the sims keep working at every step.

```
 Seam 1 (provider)             Seam 2 (loop)
 llm.Client  ──facade──►  nova provider     agent_engine.RunLoop ──facade──► nova agent.Agent
 (28 callers unchanged)                      (5 callers unchanged; EngineConfig/LoopResult unchanged)
```

Why this shape (and not "rewrite the 5 agents to Nova's API"): the 5 agents' bespoke logic is in their
*hooks and prompts*, not in loop mechanics. Keeping `EngineConfig` as the contract means the migration is
one file's internals plus converters, the agents don't change in the same commit as the loop, and rollback is a
config flag. Once everything is green and soaked, call sites can adopt Nova types directly if you want
(§8). Configuration: `engine.backend: legacy | nova` (default `legacy` until step 5; per-agent override
`engine.backend_overrides: {worker: nova}` for the staged cutover).

### Step list (each step = ≥1 commit; gate must pass before the next)

**Step 0: Preflight and baselines** (no behaviour change)
- 0.1 Branch hygiene: `git fetch`, rebase onto `origin/main` if it moved. Run baseline
  `go build/vet/test` on Go 1.25.8 and record it in the log.
- 0.2 Create `config.yaml` for sims (see §4.4 for what the environment can and cannot provide). Never commit it.
- 0.3 **Capture baselines on the CURRENT code and models (2.5)** with the sim suite in §4.2. Save each
  run's `reports/*.md` summary plus a machine-readable digest into `docs/plans/baselines/` (tool sequence per
  turn, exit reason, total cost, turn count, memories saved). Committing digests, not full reports.
  *This is the most important step: without it nothing later is comparable.*
- 0.4 Write the digest extractor as `cmd/` or `scripts/` code if the sim report format makes it
  non-trivial (the report writers are in `cmd/sim_gw.go`, not the legacy `cmd/sim.go`; see CLAUDE.md "Sims").

**Step 1: Seam 1, `llm.Client` runs on Nova's provider**
- 1.1 Go bump + dependency: `go 1.26.6`, `require github.com/AutumnsGrove/nova v0.3.5`, `go mod tidy`. One commit, only
  go.mod/go.sum (+ a `docs/` note about the Le Potato toolchain).
- 1.2 Characterization tests first: extend `llm/streaming_test.go` to pin current behaviour for: usage + cached/
  cache-write tokens, cost, `provider` attribution, reasoning toggle body, `session_id` body field, provider order
  body, fallback-on-retriable, no-fallback-on-401/403, streaming chunk callback, tool-call assembly,
  `tool_choice`, empty tool name tolerance. These pass on the OLD client (red→green is "they already pass"; the point
  is the safety net).
- 1.3 New file `llm/nova.go` (owning package stays `llm`): build `openrouter.New(...)` once per `Client`;
  convert `ChatMessage`⇄`message.Message` (incl. `ContentParts` ⇄ `Parts` for vision); map `ProviderRouting` →
  `openrouter.Routing.Options()`; `session_id` + `reasoning` → `ProviderOptions`; stream chunks → `onChunk`.
  Reimplement `ChatCompletion`, `ChatCompletionStreaming`, `ChatCompletionWithTools` on top of `Provider.Stream`
  behind `llm.backend: legacy|nova`. **Retries/fallback:** `chat.Complete` has no retry loop, so keep her-go's
  `retry` package + `isRetriable` + `WithFallback` semantics at the `Client` level (the capability-ownership map
  says retry belongs to `retry`), and do not adopt `provider/route` unless parity tests demand it.
- 1.4 Recording decorator: wraps the provider; after each `Stream` returns, hands `(model, provider, usage,
  fallback?)` to the existing `SaveMetric` path, which preserves `metrics.provider`/`is_fallback`.
- 1.5 Gate: all of 1.2 green on **both** backends; then run the sims that exercise only non-loop LLM paths
  (`classifier-stress`, `mood-dedup-test`, `vision-test`, `compaction-quick`) with `llm.backend: nova` and compare digests
  to baseline (§4.3). Flip default to `nova` only when green.

**Step 2: Tool bridge** (still no loop change)
- 2.1 `tools/nova.go`: `NovaTools(agent string, cfg) []tool.Tool` wraps each registered handler:
  `Info` from the manifest (`Parameters` map reused from `llm.ToolDef`), `Parallel: false`, `Run` calls the
  existing handler with `tctx := tool.Context.Data.(*tools.Context)`, maps the returned string to
  `tool.Result` (`IsError` when it starts with `error:` / `rejected:`, same rule as the engine).
  The malformed-JSON guard and the `ActiveTools` guard live **inside** the wrapper so the exact error strings
  models see today are unchanged. Nova's own schema validation runs first; record in
  `NOVA-GAPS.md` if it rejects anything her-go accepts (e.g. integer-as-string).
- 2.2 Tests: for every tool.yaml, assert the bridged `Info` matches the legacy `llm.ToolDef` JSON byte-for-byte
  (guards prompt-cache stability: a changed tool schema invalidates the cache).
  Table-driven; this also catches property-ordering differences in the schema map.
- 2.3 Deferred loading: keep `use_tools` and `tctx.ActiveTools`; the loop facade sets `req.Tools` from
  `ActiveTools` in `BeforeProviderRequest`. No new concept.

**Step 3: Seam 2, `RunLoop` runs on Nova's agent** (behind `engine.backend`)
- 3.1 `agent_engine/nova.go` implements `runNova(cfg EngineConfig) (*LoopResult, error)`. Mapping table:

  | `EngineConfig` | Nova |
  |---|---|
  | `LLM` + `ToolDefs` | `agent.Config{Provider (via llm), Model, Tools: registry view}` |
  | `Messages` (`[system,user]`) | `System` = first, `history`=[], user msg passed to `Run` |
  | `IterationsPerWindow`,`MaxContinuations` | `MaxTurns = iters×(cont+1)`; `Hooks.TurnEnd` injects the continuation message at each window boundary |
  | `ToolChoiceFirst` | `BeforeProviderRequest` sets `req.ToolChoice` when turn == 0 (only if `len(req.Tools)>0`) |
  | `PreIteration` / `PostIteration` | `BeforeTurn` / `MessageEnd` (see delta D1) |
  | `PreTool`, `ActiveToolGuard` | inside the bridged tool wrapper (string-identical) |
  | `PostToolResult` | `Hooks.ToolResult` |
  | `PostTool`, trace lines, `LiteToolHook`, TUI `ToolCallEvent` | `Hooks.ToolResult` (has call + result; keeps ordering) |
  | `ToolCtx.DoneCalled` | `TurnEnd` → `End: true` after the batch; fires `OnDone` |
  | `OnLoopExit` | after `Run` returns |
  | `AgentIterEvent`, `SaveMetric` | recording decorator (step 1.4) + `Emit(usage)` |
  | `<think>` extraction | `MessageEnd` fallback if the model returns inline tags rather than `Reasoning` |
  | `LoopResult.Messages` | `Result.Messages` converted back to `[]llm.ChatMessage` (callers read them: `extractDoneSummary`, `extractLastThink`) |
  | exit reasons | `ExitDone` / `ExitMaxContinuations` / `ExitError` / `ExitHookBreak` / `ExitNoToolCalls` derived from `Result.Reason` + which hook ended the run |

- 3.2 **Parity suite:** parametrize `engine_test.go` over both backends (`for _, be := range []string{"legacy","nova"}`).
  Every existing test must pass for `nova` without edits to assertions. Where it can't, that is a delta, and it gets
  an entry in §5 and an explicit `t.Skip`-free alternative assertion with a comment explaining why.
- 3.3 Add new parity tests for things the old suite doesn't cover but the callers rely on: `PostToolResult` can
  change `isError`, multiple tool calls in one assistant message run in order, tool error does not abort the
  loop, malformed args string, empty tool name skipped, context cancel mid-tool, panic in a handler becomes an error
  result, no tool calls on iteration 0 with `tool_choice: required`.
- 3.4 Gate: legacy and nova both green. Default stays `legacy`.

**Step 4: Staged cutover per agent** (least coupled first; per-agent override flag)
Order and the sim(s) that gate each. A step is done when the sim's digest matches baseline within the §4.3 tolerances.
1. worker (`worker-agent-test`, `worker-briefing-narrate`, `tool-a-thon`)
2. introspection (`introspection-test`, `self-memory-quality`)
3. memory agent (`memory-a-thon`, `contradiction-canary`, `card-lifecycle`)
4. dream / memory dreamer (`dream-consolidation`, `dreaming-test`, `quick-loop-and-dream`)
5. **driver last** (`core-loop-and-dream`, `schedule-crud`, `context-and-search`, `safety-stress`, `full-stress-canary`)
   Commit per agent, with the sim cost and digest delta in the commit message.

**Step 5: Default to Nova, soak, delete legacy**
- 5.1 Flip `engine.backend` default to `nova` for all. Run the full gate suite (§4.2 "full") twice back to back.
- 5.2 Remove the legacy loop body and the flag, keeping `EngineConfig`/`LoopResult`/exit constants and
  `BuildContinuationSummary`/`TruncateLog` if still used. Delete dead tests only if they test deleted code (record
  which in the log). Remove `llm` legacy backend only if 1.5 is also soaked.
- 5.3 Docs: `CLAUDE.md` (architecture + Nova dependency + Go 1.26.6 + update procedure note), `docs/ARCHITECTURE.md`,
  `PLAN-agent-engine.md` status line, and a short "Nova bridge" doc.

**Step 6: MiMo 2.6 bump** (after migration so any behaviour change is attributable to the model)
- 6.1 Update `config.yaml.example`, `CLAUDE.md`, `README.md`: chat → `xiaomi/mimo-v2.6-pro`; driver/memory/mood/
  introspection/persona/dream/vision → `xiaomi/mimo-v2.6-flash` (default recommendation; see Q5). Keep
  `provider.order: ["xiaomi/fp8"]`. `grep -rn "mimo-v2.5"` across `*.go`, `*.yaml`, `*.md`, `sims/`.
- 6.2 Pricing: if there is a local cost table or fallback pricing in code/config, update (Data Primacy: should be config).
- 6.3 A/B: run the "core" sim set on 2.6 vs the Step 5 digests; log cost, tool-sequence differences, any
  `<think>` format differences, cache hit rate. `sims/mimo-benchmark.yaml` and `model-compare.yaml` exist for this.
- 6.4 Commit config/doc changes separately from any prompt tweaks that 2.6 turns out to need.

## 4. Testing strategy

### 4.1 Three layers
1. **Deterministic (no network, runs every commit):** scripted mock-SSE tests (existing `httptest` pattern in
   `agent_engine/engine_test.go` and `llm/streaming_test.go`). These carry the parity contract.
2. **Bridge invariants (no network):** tool-schema byte equality, message round-trip (`ChatMessage`⇄`message.Message`
   including images, tool calls, empty content), usage/cost mapping.
3. **Live sims (costs money, nondeterministic):** `go run main.go sim <suite>` (this is `simGWCmd`, the real
   gateway path; **not** `sim-legacy`). Used as regression detectors, never as the only gate.

### 4.2 Sim sets
- **smoke** (~cents): `schedule-crud`, `worker-agent-test`, `vision-test`, `compaction-quick`, `mood-dedup-test`
- **core** (a few dollars): smoke + `core-loop-and-dream`, `memory-a-thon`, `card-lifecycle`, `introspection-test`, `dream-consolidation`
- **full** (the most): core + `full-stress-canary`, `safety-stress`, `context-and-search`, `contradiction-canary`, `tool-a-thon`
Agent: do not invent sim names; list `sims/` and confirm each exists, substituting the nearest equivalent and noting it.

### 4.3 What "matches baseline" means (LLM output is not deterministic)
Compare **structure, not prose**: per-turn tool sequence set (reply and done are present on every turn,
`think` precedes `reply` where the baseline had it), exit reason distribution, turn completion (no truncated turns),
memories-saved count within ±30%, total cost within ±25%, no new error-prefixed tool results, no new
`UsedFallback`, cache read tokens > 0 on turn ≥ 2. Run a flaky-looking failure **twice** before treating it as
a regression, and a regression that reproduces gets a deterministic test added before the fix.

### 4.4 Environment caveats for the executing agent
- `config.yaml`/`her.db` are gitignored. Create `config.yaml` from the example. The **OpenRouter key is
  proxy-injected in this cloud environment** (dummy value is fine). Tavily, Telegram, Gmail, Foursquare, D1
  keys are absent, so skip any sim that needs them (search/place/email/calendar) and record the skip.
- `embed:` points at a local Ollama (`localhost:11434`, `nomic-embed-text`). **That won't exist in a cloud
  container.** See Q3: if the run is remote, either route embeddings through OpenRouter (a compatible
  `/embeddings` model) *for sims only*, or skip embedding-dependent sims. Note sqlite-vec dimensions are fixed
  per DB, so use a fresh sim DB (the sims already seed a new one).
- Budget: default cap **$15 total** for the session; the digest includes cost, and the log keeps a running sum.
  Prefer `smoke`/`core` on every step; `full` only at Step 5 and Step 6.

## 5. Known behavioural deltas to expect (decide, don't discover)

| Id | Delta | Handling |
|---|---|---|
| D1 | `PostIteration` can break *before* tools run in the old engine. Nova's `MessageEnd` fires before tools but cannot end the run; only `TurnEnd.End` ends after the batch. The driver's think-loop detector triggers on repeated `think`, which is a no-op tool. | Accept ending after the (harmless) batch; assert in a parity test. |
| D2 | Continuation messages are injected as `role: system` mid-conversation. Nova `Inject` messages go through the provider's role handling. | Test against the mock first; if the wire rewrites it, use a `user` message with the same text, and verify on one live sim for each agent. |
| D3 | Old engine also exits on `finish_reason == "stop"` after executing tools (OpenCode #14972 pattern). `agent.Turn` doesn't expose finish reason. | Probably skip (the next turn returns no tool calls and ends). Verify with a mock: assistant message with tool calls + `finish_reason:"stop"`. Gap row if it loops. |
| D4 | Nova ends a run when an assistant message has no tool calls; old engine had `OnNoToolCalls` (unused in practice but the driver uses it to capture fallback text). | `MessageEnd` captures `agentFinalText`; `done`-as-text detection moves there. |
| D5 | Tool errors from the registry (unknown tool, schema failure) use Nova's wording (`unknown tool`, `blocked:`). | String-guard in the wrapper (step 2.1) for the two strings prompts rely on; others accepted + logged. |
| D6 | Reasoning arrives as `Message.Reasoning` and/or inline `<think>`. | Keep `extractThinkBlock` as fallback; add a live check per model on 2.5 and 2.6. |
| D7 | Cache stability: tool schema ordering, `content: ""` on tool-call-only assistant messages. | Byte-equal schema test (2.2); set `EmptyToolCallContent: true` on the openrouter config if the legacy wire sent `""`. |

## 6. Nova gaps (starting list; the agent appends to `NOVA-GAPS.md`)
1. Per-call model/provider attribution isn't on `OnMessage`/usage events (worked around by the provider decorator).
2. No built-in deferred tool loading (INTEGRATION.md checklist says "Not built"; our `ActiveTools` approach is the workaround).
3. No trace-spec equivalent for `tool.yaml` trace formats (we keep `tools.FormatTrace`).
4. `Turn` doesn't expose `finish_reason`.
5. No `tool.yaml` manifest loader exists despite INTEGRATION.md saying "Nova's manifest loader": we keep ours.
6. Voice (TTS/STT): coming in Nova (#85/#86); explicitly out of scope here.

## 7. Risks
| Risk | Likelihood | Mitigation |
|---|---|---|
| Prompt-cache hit rate drops (byte drift in tools/messages) | Med | D7 tests; watch `cache_read_tokens` in digests; this is the costliest silent regression |
| Go 1.26.6 on the Le Potato | Med | Document; first deploy builds with `GOTOOLCHAIN=auto`; verify `go version` on the host before `her update` |
| Dependency churn from `go mod tidy` (charm.land bumps) breaks the TUI/Telegraph | Low | spike built and tested clean; run `go vet ./tui/...` and launch `her sim` |
| Sims nondeterminism → false alarms | High | structural comparison + rerun-twice rule |
| Agent runs out of budget or time mid-step | Med | each step is independently shippable; log is the handoff; flag defaults keep `main`-equivalent behaviour |

## 8. After this chunk (don't do today)
- Voice via Nova when it lands (`voice/` owner stays; consumers call Nova's role-based TTS/STT).
- Adopt Nova `embed` roles (replaces the local Ollama dependency) and `decide` roles for the classifier gates.
- Opt read-only tools into `Parallel: true` (recall, read_card, get_time...) for latency.
- Replace the `llm` facade at the call sites with Nova types directly, then delete the facade.
- Evaluate Nova `compaction/` vs `compact/`.

## 9. Open questions (defaults apply if unanswered)
| # | Question | Default |
|---|---|---|
| Q1 | Seam 1 too: put `llm.Client` on Nova's provider (28 callers, facade), or loop only? | **Both**, facade first |
| Q2 | Pin Nova by tag `v0.3.5` or local `replace ../nova`? | **Tag**, gaps logged, never edit Nova from here |
| Q3 | Where does the all-day agent run, and how do sims get embeddings? | Cloud container; embeddings via OpenRouter for sims only |
| Q4 | Sim budget for the day? | $15 |
| Q5 | 2.6 mapping: Pro for chat, Flash for driver/memory/mood/introspection/persona/dream/vision? | **Yes** |
| Q6 | Open a PR at the end, or leave the branch for you to review? | Branch only, no PR |
| Q7 | OK to bump her-go to Go 1.26.6 and the charm.land minor versions? | **Yes** |
| Q8 | Migration before the 2.6 bump (my recommendation, so regressions are attributable)? | **Yes** |

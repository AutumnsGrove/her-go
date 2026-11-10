# The Nova bridge

her-go runs its agent loop and its LLM transport on
[`github.com/AutumnsGrove/nova`](https://github.com/AutumnsGrove/nova) (pinned by tag in `go.mod`). Nova is a
library here, not a framework: her-go keeps its own contracts, its own tools, and its own agent behaviour.

This document is the map. The reasoning, the decisions and the honest list of behavioural differences are in
[`plans/PLAN-nova-migration.md`](plans/PLAN-nova-migration.md) and
[`plans/NOVA-MIGRATION-LOG.md`](plans/NOVA-MIGRATION-LOG.md).

## Where the seam is

```
  agent/  workeragent/  persona/            ← unchanged: prompts, hooks, behaviour
        │
        ▼
  agent_engine.EngineConfig                 ← unchanged contract (5 call sites untouched)
        │
        ├── agent_engine/nova.go            → nova agent.Agent.Run
        │        │
        │        └── tools/nova.go          → tools.Execute (same handlers, same strings)
        │
        └── llm.Client                      ← unchanged API for 28 non-loop call sites
                 └── llm/nova.go            → nova provider/openrouter
```

Two project-owned contracts absorb the migration:

- **`EngineConfig` / `LoopResult`** (`agent_engine`) — the loop's inputs and outputs. The five agents built
  their hooks against this, so nothing in `agent/`, `workeragent/` or `persona/` changed.
- **`llm.Client`** — the LLM entry point. Its three methods (`ChatCompletion`, `ChatCompletionStreaming`,
  `ChatCompletionWithTools`) are now answered by Nova's provider, plus a `Stream` method so a `*llm.Client` is
  a Nova `provider.Provider` for the loop.

## Config

```yaml
llm:
  backend: "nova"    # nova | legacy — the LLM transport
engine:
  backend: "nova"    # nova | legacy — the agent loop
  backend_overrides: {}   # e.g. {worker: nova} during a staged rollout
```

`legacy` is a rollback lever, not a supported long-term mode: it is the pre-migration hand-written
implementation, kept until the Nova path has soaked in production.

## How the pieces map

| her-go | Nova | Notes |
|---|---|---|
| `EngineConfig.Messages` | `Config.System` + `Run(history, prompt)` | first system message is hoisted |
| `IterationsPerWindow`, `MaxContinuations` | `MaxTurns` + `Hooks.TurnEnd` injections | windows become a turn budget |
| `PreIteration` | `Hooks.BeforeTurn` | |
| `PostIteration` | `Hooks.MessageEnd` | cannot end the run (delta D1) |
| `OnNoToolCalls` | `Hooks.MessageEnd` | text capture only (delta D4) |
| `ToolChoiceFirst` | `Hooks.BeforeProviderRequest` | first request only |
| `ActiveToolGuard`, `PreTool` | a `guardedTool` wrapper | exact error strings preserved |
| `PostToolResult`, `PostTool`, traces, TUI events | `Hooks.ToolResult` | |
| `ToolCtx.DoneCalled` | `Hooks.TurnEnd` → `End` | |
| `OnLoopStart` / `OnLoopExit` | around `Run` | |
| `SaveMetric` per LLM call | `llm.Client.ResponseHook` | Nova reports per-run usage only |
| `tool.yaml` manifests | `tools.NovaTools` | schema bytes unchanged (prompt cache) |

## Things that are deliberately different

Each has a regression test pinning the new behaviour, and a paragraph in the migration log:

- A hook that wants to stop before a tool batch stops just after it instead (`PostIteration`).
- Truncated tool arguments become a normal tool error the model can correct, rather than a retriable
  transport failure (the legacy client failed the call).
- Tool arguments are schema-validated by Nova first, so that error's wording is Nova's.
- `metrics.provider` records `openrouter`, not the upstream infrastructure name.
- Newly `use_tools`-loaded tools are actually advertised in the schema from the next turn (the legacy loop
  sent its starting set for ever). This one is an improvement.

## Working on it

- Nova is **read-only** for this repository. If a gap blocks something, write it in
  [`plans/NOVA-GAPS.md`](plans/NOVA-GAPS.md) with the workaround and a suggested upstream issue title.
- Parity lives in tests, not in review: `llm/parity_test.go` (transport) and
  `agent_engine/nova_parity_test.go` + the backend-parameterised `engine_test.go` (loop) run the same cases
  against both implementations while both exist.
- `her sim --engine-backend nova --llm-backend nova` runs a simulation on the Nova path without editing
  `config.yaml`; `her digest` turns reports into structure so runs can be compared without diffing prose.

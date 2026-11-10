# Nova gaps found while migrating her-go

Running list, appended to as the migration proceeds. Each row is a place where Nova (v0.3.5, read-only for
this work) does not give her-go what it needs, the workaround shipped in her-go, and a suggested upstream
issue title.

Rule from the plan (§0.5): **record the gap, don't patch Nova.**

| # | Symptom | her-go workaround | Suggested Nova issue |
|---|---|---|---|
| 1 | `OnMessage` / usage events do not carry the per-response `model` or `provider`. her-go's `SaveMetric` needs both, plus whether the response came from a fallback model. | A recording decorator wraps the provider (`llm/nova.go`): after `Stream` returns it captures model/provider/usage and hands them to the existing `SaveMetric` path. | "Expose resolved model and provider on usage/OnMessage events" |
| 2 | No built-in deferred/lazy tool loading. `INTEGRATION.md`'s checklist marks it "Not built". | her-go keeps `use_tools` + `tools.Context.ActiveTools` and rewrites `req.Tools` from `ActiveTools` inside `Hooks.BeforeProviderRequest`. | "Support dynamic tool sets between turns" |
| 3 | No equivalent of `tool.yaml`'s per-tool trace spec; `tools.FormatTrace` has no Nova counterpart. | `tools.FormatTrace` stays in her-go and is called from the bridged `Hooks.ToolResult`. | "Tool result rendering hooks / trace formatters" |
| 4 | `agent.Turn` does not expose the provider's `finish_reason`, so her-go's "exit on `finish_reason == stop` even after tool calls" rule (OpenCode #14972) cannot be reproduced exactly. | Accepted as a delta (plan §5 D3): the run ends on the next turn anyway. Verified with a mock that a `stop`-with-tool-calls response does not loop. | "Expose finish_reason on Turn" |
| 5 | No `tool.yaml` manifest loader, despite `INTEGRATION.md` referring to "Nova's manifest loader". | her-go keeps `tools/loader.go` and bridges the parsed definitions into `[]tool.Tool`. | "Ship a manifest-driven tool loader" |
| 6 | Voice (TTS/STT) not in Nova yet (#85/#86 upstream). | Out of scope for this chunk; `voice/` is untouched. | (tracked upstream already) |

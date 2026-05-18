# Handoff: Sim Observability & Post-Run Fixes

**Date:** 2026-05-16
**Branch:** main (all work here is on main unless scoped otherwise)
**Context:** Just shipped the tool-agent-registry refactor (PR #82) and ran the full 32-turn `everything-everywhere-all-at-once` sim. The sim exposed observability gaps in the report and several bugs.

---

## Architecture Quick Reference

- **4-agent pipeline per turn:** driver (Qwen3) → memory (Kimi K2) + mood (Kimi K2) + introspection (Kimi K2), all running after driver replies
- **Dream cycle:** runs between "days" in sims — consolidates memory cards, rewrites persona.md
- **Sim runner:** `cmd/sim.go` — `runSim()` orchestrates turns, `writeTurnReport()` generates markdown
- **Sim report:** generated in `cmd/sim.go`, writes to `sims/results/<name>-run<N>.md`
- **Tool definitions:** `tools/<name>/tool.yaml` with `agent:` field driving which agents get which tools
- **Reply pipeline:** driver calls `reply` tool → chat model generates response → style/safety classifiers → send

## Latest sim report

`sims/results/everything-everywhere-all-at-once-run121.md` — 32 turns, $1.09, 66 memories (36 self-memories from introspection agent)

---

## Task List (use task tracking — tasks already created)

### Priority 1: Investigation

**#13 — Dreamer not merging/expiring memories**
The dream agent did 8 card rewrites in cycle 1 and 1 in cycle 2, but zero merges and zero expires. Self-memories are accumulating with overlapping content (multiple "reframing" observations, multiple "I instinctively reach for..." patterns). The dreamer has `merge_memories` and `remove_memory` in its tool set (confirmed via `agent: [dream]` in those tool.yaml files). Check `persona/memory_dreamer.go` for the prompt and tool loop. The dreamer prompt (`memory_dreamer_prompt.md` or embedded) may not be instructing it to merge/expire aggressively enough, or the card summaries may not surface the redundancy.

**#22 — Turn 18 hallucination (fabricated shipping crisis)**
Reply says "Remember when you rerouted that whole shipment crisis during the snowstorm?" — this never happened in the sim conversation. Turn 21 repeats it. Likely the chat model fabricated it and it stuck. Check: did the driver agent pass this as a memory string to the reply tool? Did it get saved as an actual memory? Or did compaction introduce it? The compaction summary (Turn 20, visible in report) doesn't mention it, so it's probably a chat model hallucination that the driver agent reinforced by referencing it in the `reply` instruction.

**#23 — Turn 9 barber shop in nearby results**
User asked for "a good coffee shop nearby" — Foursquare returned a barber shop alongside coffee shops. Check `tools/nearby_search/handler.go` for how the Foursquare query is constructed. The `query` param may not be filtering by category, or Foursquare's API may be returning broad results. The driver agent's search query may also be too generic.

### Priority 2: Sim Report Observability

All of these are in `cmd/sim.go` — the report generation code.

**#14 — Add memory agent traces to sim report**
Mood traces show inline (`> mood: logged valence=3 ...`). Introspection traces show inline (`> introspection: saved 1 self-memories`). Memory agent traces are completely absent. The memory agent runs in `agent/memory_agent.go` and uses trace callbacks, but `cmd/sim.go` doesn't capture or render them. Goal: show what the memory agent did each turn (what it saved, what cards it touched, what the classifier rejected). Ideally, architect this so adding a new agent's traces is data-driven (like tool registration), not hardcoded per-agent.

**#15 — Show introspection self-memory content inline**
Currently just says "saved 1 self-memories" — need to show the actual text, e.g. `> introspection: saved self-memory: "I instinctively reach for dark humor..."`. The data exists (it's in the trace callback), it's just not rendered in the report.

**#16 — Show compaction events inline**
Compaction happens between Turn 20 and Turn 21 but only appears in a section at the bottom. Should appear inline: `> [Compaction: 33 messages summarized → "Autumn's been stuck in a fog..."]`

**#17 — Per-turn cost tracking**
Currently cost is bundled by model at the bottom. Need:
1. Per-turn cost inline (sum of driver + memory + mood + introspection for that turn)
2. Cost broken down by agent role, not by model (since Kimi K2 is used by 4 agents, the current table is useless for identifying which agent costs most)
3. Classifier cost is missing entirely — not logged anywhere in the cost summary
4. Inline cost for special operations (compaction, view_image, web_search)

**#18 — Fix supersession ID mismatch**
Supersession chains show "superseded by #22" but the memory table uses sim.db internal IDs (e.g., #566). These need to be consistent. Check `cmd/sim.go` report generation — the supersession tracking likely stores the original per-run IDs while the memory table uses the DB-assigned IDs after insertion.

### Priority 3: Feature Improvements

**#19 — Em dash regex replacement**
Replies are overloaded with em dashes (` — `). The old style classifier gate was too aggressive (rewrote replies mid-stream while user was reading). A lighter approach: simple regex replacement post-reply, before sending. Most em dashes can become periods or commas. Check `tools/reply/handler.go` for where the reply text is finalized before sending.

**#20 — Reply memories as IDs not strings**
Driver agent passes full memory strings to `reply` tool. Should pass memory IDs (ints) that get looked up automatically. Current approach: driver paraphrases or truncates memory text, which is fragile. Check `tools/reply/handler.go` for the `memories` parameter definition and how it's consumed. The `reply` tool.yaml defines the parameter schema. Keep string input as optional escape hatch.

**#21 — Model cost optimization**
Target: $0.25-0.30 per 20 messages (currently $1.09 for 32 turns). Test instruct-only models:
- Chat: Mistral Nemo (cheap, instruct)
- Memory/mood/introspection: Qwen3 (instruct mode, strong tool-calling)
- Classifier: Gemini 3.1 Flash Lite (already used, could expand)
- Driver: Trinity Large Preview (instruct, previously used successfully)
- Key constraint: MUST be instruct models — thinking/reasoning models break the pipeline (skip think tool, burn invisible tokens, hide CoT)
- Find a fast sim suite for baseline. Run current config first, then swap models one at a time.

---

## Key Files

| Area | File |
|------|------|
| Sim runner + report gen | `cmd/sim.go` |
| Driver agent | `agent/agent.go` |
| Memory agent | `agent/memory_agent.go` |
| Introspection agent | `agent/introspection_agent.go` |
| Dream agent | `persona/memory_dreamer.go` |
| Dream prompt | `memory_dreamer_prompt.md` (or look for embedded prompt in memory_dreamer.go) |
| Reply tool | `tools/reply/handler.go` + `tools/reply/tool.yaml` |
| Nearby search | `tools/nearby_search/handler.go` |
| Tool registry | `tools/loader.go` |
| Trace system | `trace/` package |
| Mood agent | `bot/mood.go` |
| Sim suites | `sims/*.yaml` |
| Latest full report | `sims/results/everything-everywhere-all-at-once-run121.md` |

## Running Sims

```bash
# Full stress test (32 turns, ~1 hour, ~$1)
go run main.go sim --suite sims/everything-everywhere-all-at-once.yaml

# Quick introspection test (5 turns, ~5 min, ~$0.12)
go run main.go sim --suite sims/introspection-test.yaml

# Override models for testing
go run main.go sim --suite sims/introspection-test.yaml --chat-model mistralai/mistral-nemo --memory-model qwen/qwen3-235b-a22b-2507
```

## Important Constraints

- **Instruct models only** — reasoning/thinking models break the tool-calling pipeline
- **Data Primacy** — model names only in config.yaml, never hardcoded in .go files
- **Run sims as background Bash** — never in subagents (they timeout)
- **Don't use Opus 4.7** — stick with proven models (4.6 family)

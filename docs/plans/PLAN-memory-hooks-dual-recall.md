---
title: "Memory Hooks & Dual-Mode Recall"
status: planning
created: 2026-07-25
updated: 2026-07-25
category: memory
priority: high
branch: feat/memory-hooks
---

# Plan: Memory Hooks & Dual-Mode Recall

## Problem

`recall_memories` is currently the *only* path memories reach the chat model
(see `[[project_semantic_injection_tradeoff]]` — auto-injection was removed
2026-05-02 for the same reason: it gave the agent a false sense of context
completeness). But data pulled from D1 and three diagnostic sims run on
`experiment/card-index-recall` show the tool itself has three separable
failure modes, not one:

1. **Gating** — the driver has to decide "is this worth searching for"
   *before* it searches, and across historical D1 turns it skipped that
   decision on ~80% of turns before a 2026-05-02 prompt change, and still
   skipped it entirely at both of a canary sim's oblique trigger turns
   (`sims/card-index-canary.yaml`).
2. **Ranking** — of 55 historical `recall_memories` calls that returned
   results, only 15 (28%) had a returned memory actually used in the reply.
   Unused memories averaged **higher** cosine similarity (60.9%) than used
   ones (56.5%) — similarity doesn't predict usefulness, it's closer to
   noise with respect to it.
3. **Reach** — even with every card summary visible (the always-visible
   card-index layer prototyped on this branch), the driver never once
   connected two topically-distant facts across cards (a Thursday
   presentation + migraines triggered by fluorescent lighting/long
   meetings), across 4 separate opportunities in `sims/full-stress-canary.yaml`.

The card-index layer (`layers/agent_card_index.go`, already merged into this
branch) fixed failure mode 1 outright and partially fixed failure mode 3 when
two relevant facts happened to live in adjacent, similarly-scoped cards (a
decoy-card test: seeing both "interests" and "hobbies" cards side by side let
the driver mention facts from both, where the pre-layer baseline only
surfaced one). It did not — and structurally cannot — fix failure mode 2,
because it doesn't change how `recall_memories` ranks results once called.

Throwing a reranker LLM call in front of `recall_memories` was considered and
rejected: it doesn't fix the underlying issue, it just adds another model
call making the same kind of unreliable judgment one layer deeper.

## Design Decisions (from discussion)

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Where hooks are written | **Write time**, by the agent saving the memory | The agent that just wrote a memory knows what's notable about it. A dreamer reconstructing that later from bare prose, in a batch, is guessing. Removes the dependency on a dream cycle ever having run for a memory to be well-indexed. |
| Hook shape | **One field**, no fact/trigger split | Splitting into `hook_fact` + `hook_trigger` adds a seam nothing needs yet and makes "what goes where" its own decision burden — the opposite of the goal (memory should stay simple: text that surfaces when the situation needs it). |
| Hook content | Trigger-oriented, not just compressed fact | The Donette-bit sim canary showed the memory's own vocabulary ("babe," "sweetheart") was never going to bridge to future conversation via search. A hook like *"call her this when a stranger uses a pet name on her"* is written by the one party with time to anticipate retrieval — the agent, at write time — rather than expecting the driver to notice the connection cold, mid-conversation. |
| Primary key | **Existing integer `id`** stays canonical | SQLite already forces this; lean into it rather than inventing a new identity scheme. |
| Slug | Secondary, human-readable, indexed column resolving to the same row | Not a replacement key — collisions just fall back to `id` lookup. Lets a lookup read like `recall_memories(slug="donette-bit")` without needing global uniqueness guarantees. |
| Card summaries | Dreamer curates a **list of hooks**, not prose | Reuses the existing `update_card` tool and the `Summary` text field as-is — no schema change. Only the *content* the dreamer is instructed to write changes (a bulleted hook list instead of a paragraph). The card-index layer (`layers/agent_card_index.go`) needs zero code changes — it already renders `Summary` verbatim. |
| Self-memories | Always-visible **flat index of every active self-memory's hook**, not gated behind card browsing | Self-memories are a small, actively curated set (the dream cycle already merges/expires them) — unlike user memories, which are open-ended and can't be flattened without eventually becoming unreadable. This is the direct fix for `[[feedback_self_memory_gap]]`. |
| `recall_memories` tool shape | **One dual-mode tool**, not split into two tools | It's one capability ("give me memory content") with two entry points, not two capabilities. Relies on prompting to steer mode selection correctly — explicitly agreed no need to split. |
| Recall priority | Lookup (`id`/`slug`) is primary; semantic `search` is the **fallback** | Inverts today's only-option search into an escape hatch for the genuine "I don't know what I'm looking for yet" case — which should be rare once browsing (card index + self index) is cheap and reliable. |
| Backfill | One-time batch pass, not lazy | D1 audit shows only ~40-50 *active* memories exist (most of the ~500 rows are stale/superseded/inactive) — a batch backfill is cheap enough to just do up front rather than engineering lazy on-touch generation. |

## What This Does Not Fix

Failure mode 3 (reach across topically-distant cards) is **not** solved by
this plan. Even with every hook visible, nothing forces the driver to scan a
`health` hook while reasoning about a `work` topic unless it's already
inclined to look. That's a model-attention/prompting problem, not a
retrieval-architecture problem — nothing in the retrieval layer can compel a
cross-domain leap the model isn't inclined to make on its own. Out of scope
here; worth a separate, narrower experiment (e.g. explicit prompting to scan
hooks outside the obvious category) if it turns out to matter in practice.

## Architecture

### Schema

```sql
ALTER TABLE memories ADD COLUMN slug TEXT;
ALTER TABLE memories ADD COLUMN hook TEXT;
CREATE INDEX IF NOT EXISTS idx_memories_slug ON memories(slug);
```

Both nullable — old, untouched memories degrade to search-only until backfilled
or naturally superseded.

### Write-time hook generation

`save_memory`, `update_memory`, `merge_memories`, and `split_memory` all gain
a required `hook` parameter (and derive a `slug` from it — lowercase,
hyphenated, truncated, de-duplicated against existing slugs by suffixing
`-2`, `-3`, etc. if needed). Prompt changes needed in `main_agent_prompt.md`
(or wherever `save_memory` is documented for the memory agent) explaining the
trigger-oriented framing with the two worked examples from this design
discussion (Donette bit / over-hedges-feedback) as few-shot models.

### Backfill

New one-off command (`her backfill-hooks` or similar, following the
`cmd/sim.go`-style escape-hatch pattern for infra scripts) that:
1. Loads all active memories lacking a `hook`
2. One LLM call per memory (or small batches) to generate `hook` + `slug`
   retroactively, using the same trigger-oriented prompt as write-time
   generation
3. ~40-50 memories at current D1 state — cheap, one-time, run manually

### Dual-mode `recall_memories`

`tools/recall_memories/tool.yaml` gains optional `id` (integer) and `slug`
(string) parameters alongside the existing `query`. Handler logic:

```
if id or slug provided:
    direct DB fetch (Store.GetMemoryByID / Store.GetMemoryBySlug) — no embedding call
else:
    existing semantic search path (now the fallback, not the default)
```

Store interface additions: `GetMemoryByID(id int64) (*Memory, error)`,
`GetMemoryBySlug(slug string) (*Memory, error)`.

Prompt changes to whichever prompt documents `recall_memories` for the
driver: primary usage is "you saw a hook in the card index or self index —
fetch it by id/slug"; `query`-based search is explicitly framed as the
fallback for when nothing in the visible index matches.

### Card summaries → hook lists

No code change to `layers/agent_card_index.go` — it already renders
`MemoryCard.Summary` verbatim. Only `memory_dreamer_prompt.md` changes: the
dreamer is now instructed to write `Summary` as a short bulleted list of
child hooks with IDs (`- donette-bit (#502): call her this when...`) rather
than a compressed prose paragraph. The dreamer's job also gets easier under
this design — since hooks already exist per-memory at write time, dream-cycle
work shifts from lossy compression to curation (which hooks belong in this
card, are any two actually duplicates, should anything retire) — a more
tractable task than reconstructing "what mattered" from bare prose after the
fact.

### New: self-memory index layer

`layers/agent_self_memory_index.go` (new file, same registration pattern as
`agent_card_index.go`) — queries all active `subject="self"` memories and
renders one hook line each, always visible, not gated behind browsing a
card. Needs a new Store method (`ActiveSelfMemories() ([]Memory, error)` or
reuse existing self-memory queries if one already fits). Order it near the
existing card-index layer (~Order 385) so both indexes sit together in the
driver's context.

## Implementation Phases

### Phase 1: Schema + Store
**Files:** `migrations/0000XX_memory_hooks.up.sql`, `memory/store.go`, `memory/store_facts.go`

1. Add `slug`, `hook` columns + slug index
2. `GetMemoryByID`, `GetMemoryBySlug` store methods
3. `ActiveSelfMemories` store method (or confirm an existing query covers it)

### Phase 2: Write-time hook generation
**Files:** `tools/save_memory/`, `tools/update_memory/`, `tools/merge_memories/`, `tools/split_memory/`, `main_agent_prompt.md`

1. Add required `hook` param to each tool's YAML + handler
2. Slug derivation + collision suffixing helper (likely `memory/slug.go`)
3. Prompt updates with the two worked examples (Donette bit, over-hedges-feedback) as few-shot models

### Phase 3: Backfill
**Files:** new `cmd/backfill_hooks.go` (or similar)

1. One-off command, run manually once against the real `her.db`
2. Batch LLM pass over the ~40-50 active, hook-less memories

### Phase 4: Dual-mode recall_memories
**Files:** `tools/recall_memories/tool.yaml`, `tools/recall_memories/handler.go`, driver prompt docs

1. Add optional `id`/`slug` params
2. Handler branches: direct fetch vs. existing semantic search
3. Reframe prompt docs: lookup primary, search fallback

### Phase 5: Card summaries → hook lists
**Files:** `persona/memory_dreamer_prompt.md`

1. Rewrite dreamer instructions: curate a hook list, not prose
2. No code changes needed — `layers/agent_card_index.go` already renders `Summary` as-is

### Phase 6: Self-memory index layer
**Files:** `layers/agent_self_memory_index.go` (new)

1. New layer, same registration pattern as the card index
2. Wire in near Order 385

### Phase 7: Regression coverage
**Files:** `sims/card-index-canary.yaml`, `sims/contradiction-canary.yaml`, `sims/full-stress-canary.yaml` (adapt existing), possibly a new `sims/memory-hooks.yaml`

1. Re-run the three existing experiment suites against the new dual-mode recall to confirm no regression on what the card-index layer already fixed
2. New suite specifically exercising id/slug lookup vs. search fallback selection

## Files Changed (Summary)

| File | Change |
|------|--------|
| `migrations/0000XX_memory_hooks.up.sql` | New — `slug`, `hook` columns + index |
| `memory/store.go` | Interface additions |
| `memory/store_facts.go` | `GetMemoryByID`, `GetMemoryBySlug`, `ActiveSelfMemories` |
| `memory/slug.go` | **New** — slug derivation + collision handling |
| `tools/save_memory/` | Required `hook` param |
| `tools/update_memory/` | Required `hook` param |
| `tools/merge_memories/` | Required `hook` param |
| `tools/split_memory/` | Required `hook` param |
| `tools/recall_memories/tool.yaml` | Optional `id`/`slug` params |
| `tools/recall_memories/handler.go` | Dual-mode branch |
| `main_agent_prompt.md` | Hook-writing guidance, trigger-oriented framing |
| `persona/memory_dreamer_prompt.md` | Hook-list curation instead of prose summarization |
| `layers/agent_self_memory_index.go` | **New** — always-visible self-memory hook index |
| `cmd/backfill_hooks.go` | **New** — one-off backfill command |
| `sims/*.yaml` | Adapt existing three suites; possibly one new suite |

## Relationship to `experiment/card-index-recall`

This plan builds directly on top of work already merged into this branch:
`layers/agent_card_index.go`, the `seed_custom_cards` sim schema addition,
and the `CardIndexTokens` reporting plumbing all stay as-is. The three
diagnostic sims already written (`card-index-canary`, `contradiction-canary`,
`full-stress-canary`) become the regression baseline for Phase 7 rather than
one-off experiments.

## Future (out of scope here)

- Cross-domain "reach" (failure mode 3) — separate, narrower experiment if it
  proves to matter in real usage, likely a prompting change rather than an
  architecture change
- Hook quality auditing — spot-check whether trigger-oriented hooks actually
  read differently from fact-restating ones once real agents are writing them
  at scale, not just in the two hand-crafted examples from this design pass
- Possible hook re-generation during dream-cycle merges (a merged memory
  needs a new hook — covered implicitly by `merge_memories`' existing
  required-param change in Phase 2, but worth explicit test coverage)

---
title: "Embedding Similarity Consolidation"
status: completed
created: 2026-03-31
updated: 2026-04-23
completed: 2026-04-23
category: refactor
priority: low
branch: refactor/similarity-consolidation
commits: a3c2094, 39ebb2b, 4e6ea3c
---

# Plan: Consolidate Embedding Similarity Helpers

## ✅ COMPLETED — 2026-04-23

All three phases complete on branch `refactor/similarity-consolidation`. All similarity comparison patterns now use centralized helpers with explicit performance vs accuracy tradeoffs.

## Problem

Five places in the codebase do the same thing: embed text → compute cosine similarity → make a decision. Each reimplements the pattern independently with its own loop, threshold handling, and error recovery. As we add more similarity checks (retry budget tracking, classifier rewrite matching), this duplication will keep growing.

## Current Call Sites

| # | What | File | Comparing | Threshold | Embeddings Kept? |
|---|------|------|-----------|-----------|-----------------|
| 1 | Fact dedup (tags) | `tools/fact_helpers.go:298` | New fact tags vs existing fact tags | 0.85 (0.70 for context) | Backfilled if missing |
| 2 | Fact dedup (text) | `tools/fact_helpers.go:317` | New fact text vs existing fact text | 0.85 (0.70 for context) | Backfilled if missing |
| 3 | Conversation echo | `memory/context.go:96` | Fact embedding vs recent message | 0.60 | Messages discarded |
| 4 | Skill search | `skills/loader/registry.go:172` | Query vs skill descriptions | configurable | Skills cached in-memory |
| 5 | Mood dedup | `tools/run_skill/handler.go:187` | New mood note vs recent mood notes | 0.75 | Both discarded |

Plus incoming from #43 (retry budget): will need to compare rejected fact text against previous rejections in the same turn.

### Shared Pattern

Every call site does:
```
1. Get or compute vector A (embed text if needed)
2. Get or compute vector B (embed text if needed)
3. sim := embed.CosineSimilarity(A, B)
4. if sim >= threshold → take action
```

The differences are:
- **Where vectors come from:** cached in DB, cached in memory, or computed fresh
- **What threshold:** 0.60 to 0.85
- **What action:** reject fact, filter from context, include in results, reject mood

### What's NOT Duplicated

- `embed.CosineSimilarity()` itself — one implementation in `embed/embed.go:141`, used everywhere. That's fine.
- The sqlite-vec KNN search path (`SemanticSearch`, `recall_memories`) — this uses the vec_facts virtual table, not manual cosine sim. Different pattern entirely.

## What to Consolidate

### Tier 1: `embed.SimilarText()` — fire-and-forget comparison

For cases where both texts need fresh embedding and vectors are discarded:

```go
// SimilarText embeds two strings and returns their cosine similarity.
// Convenience wrapper for one-shot comparisons where vectors aren't stored.
func (c *Client) SimilarText(a, b string) (float64, error)
```

**Would simplify:**
- Mood dedup (site #5) — currently embeds both notes manually, compares, discards
- Retry budget tracking (new) — compare rejected fact vs previous rejections
- Conversation echo fallback (site #3) — the `factVec` fallback path where fact has no cached embedding

**Would NOT replace:**
- Fact dedup (sites #1, #2) — needs to handle cached embeddings, backfilling, and two-vector comparison (tags + text)
- Skill search (site #4) — queries against pre-cached skill embeddings
- Conversation echo main path (site #3) — uses cached `f.EmbeddingText`

### Tier 2: `embed.BestMatch()` — find highest similarity in a set

For cases where you compare one vector against many:

```go
// BestMatch compares a vector against a set and returns the best match.
// Returns the ID, similarity score, and whether it exceeded the threshold.
func BestMatch(query []float32, candidates map[int64][]float32, threshold float64) (bestID int64, bestSim float64, matched bool)
```

**Would simplify:**
- Fact dedup loop (iterating over all facts, tracking best sim)
- Mood dedup loop (iterating over recent moods)
- Conversation echo loop (iterating over message vectors)

This is the bigger consolidation — all three sites have the same "loop, compare, track best" structure.

### Tier 3: Threshold constants in one place

Currently scattered:
```
conversationRedundancyThreshold = 0.60  (memory/context.go:31)
sameDayContextThreshold = 0.70          (tools/fact_helpers.go:102)
moodSimilarityThreshold = 0.75          (tools/run_skill/handler.go:35)
SimilarityThreshold = 0.85              (config.go, configurable)
```

Could move the hardcoded ones to config or at minimum to `embed/` as named constants, so all thresholds are visible in one place. The configurable one (`SimilarityThreshold`) stays in config.

## Terminology Inconsistency

Worth noting: cosine **similarity** (1.0 = identical) vs cosine **distance** (0.0 = identical) are used interchangeably in different parts of the code. sqlite-vec returns distance; `CosineSimilarity` returns similarity. Linked facts convert between them (`f.Distance = 1 - sim`). This isn't broken but is confusing — could add a doc comment to `CosineSimilarity` clarifying the convention.

## What NOT to Touch

- `embed.CosineSimilarity()` itself — it's fine as is, one implementation, well-written
- The sqlite-vec KNN path — different architecture (DB-side search), not manual comparison
- Fact dedup's backfill logic — this is specific to the dedup use case (populating missing embeddings on old facts). Don't try to generalize it.
- Skill registry's pre-caching — loading skill embeddings at startup is a different pattern from on-the-fly comparison

## Implementation Summary

### Phase 1: Foundation (Commit a3c2094)
**Added helpers and consolidated thresholds:**
- ✅ Added threshold constants: `DefaultSimilarityThreshold` (0.85), `ContextMemorySimilarityThreshold` (0.70), `ConversationRedundancyThreshold` (0.60)
- ✅ Added `FindBestMatch(query, candidates, threshold, earlyExit)` — consolidates "loop and track best" pattern
- ✅ Added `SimilarText(a, b)` — fire-and-forget comparison wrapper
- ✅ Updated `memory/context.go` to use centralized constant
- ✅ Created `embed/similarity_test.go` with comprehensive coverage (15 tests)
- **Stats:** +373 lines, -10 lines across 3 files

### Phase 2: Fact Dedup Migration (Commit 39ebb2b)
**Refactored memory deduplication:**
- ✅ Refactored `checkMemoryDuplicate()` to build candidate maps with backfill
- ✅ Uses `FindBestMatch()` for both tag and text comparisons
- ✅ Added `lookupMemoryContent()` helper to avoid N² lookups
- ✅ Replaced `sameDayContextThreshold` constant with centralized version
- ✅ Preserved all backfill side effects (critical!)
- **Stats:** +51 lines, -29 lines in `tools/memory_helpers.go`

### Phase 3: Conversation Echo + Early Exit (Commit 4e6ea3c)
**Enhanced helper and refactored conversation filtering:**
- ✅ Added `earlyExit` parameter to `FindBestMatch()` for performance optimization
- ✅ Refactored `FilterRedundantMemories` to use `FindBestMatch` with early exit
- ✅ Fixed efficiency: build message candidate map once, reuse for all memories
- ✅ Added tests for early-exit behavior
- ✅ Updated all existing callers to pass `earlyExit` parameter
- **Stats:** +108 lines, -23 lines across 4 files

### Final Scope Adjustment

**Original plan:** All 5 call sites  
**Actual implementation:** 3 active call sites (sites #4 and #5 moved to `_junkdrawer/` — deprecated code, skipped)

**Active sites consolidated:**
1. ✅ Fact dedup (tags) — `tools/memory_helpers.go`
2. ✅ Fact dedup (text) — `tools/memory_helpers.go`
3. ✅ Conversation echo — `memory/context.go`

### What Was Delivered

**New API:**
```go
// Threshold constants (centralized)
embed.DefaultSimilarityThreshold = 0.85
embed.ContextMemorySimilarityThreshold = 0.70
embed.ConversationRedundancyThreshold = 0.60

// Find best match with optional early exit
id, sim, matched := embed.FindBestMatch(query, candidates, threshold, earlyExit)

// Fire-and-forget comparison
sim, err := embedClient.SimilarText("text A", "text B")
```

**Benefits:**
- All similarity patterns use centralized, tested helpers
- Performance vs accuracy tradeoff is explicit (`earlyExit` parameter)
- Thresholds documented in one place with clear semantics
- Future features (retry budget) can reuse without reimplementing
- Zero behavior changes — all backfill preserved, thresholds identical

## Decisions

- **Thresholds:** Hybrid — hardcoded defaults in code, config.yaml can override. Works out of the box, tunable when needed. ✅ Implemented
- **Scope:** Active code only (3 sites) after discovering 2 sites moved to junkdrawer. ✅ Completed
- **Early exit:** Added as optional parameter to support both performance and accuracy use cases. ✅ Enhanced beyond original plan

## Implementation Order

1. **`SimilarText()`** — smallest, most reusable, needed by retry budget (#43). Do this first.
2. **`BestMatch()`** — refactors the loop pattern. Migrate mood dedup and conversation echo first (simpler), then fact dedup (complex, has backfill side effects).
3. **Threshold organization** — hybrid approach: named defaults in `embed/`, config overrides in `MemoryConfig`.

## Files That Would Change

| File | Change |
|------|--------|
| `embed/embed.go` | Add `SimilarText()`, `BestMatch()`, threshold constants |
| `tools/run_skill/handler.go` | Mood dedup → use `SimilarText` or `BestMatch` |
| `memory/context.go` | Conversation echo → use `BestMatch` for the loop |
| `tools/fact_helpers.go` | Fact dedup → use `BestMatch` for the loop (careful with backfill) |
| `skills/loader/registry.go` | Skill search → could use `BestMatch` but low priority (already clean) |

## Verification

1. `go build` — no regressions
2. Run `fact-a-thon` sim — dedup behavior unchanged
3. Run `classifier-stress-test` sim — same rejection patterns
4. Spot-check mood dedup manually — same 0.75 threshold behavior

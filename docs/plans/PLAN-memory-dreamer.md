---
title: "Memory Dreamer — Autonomous Memory Consolidation"
status: planning
created: 2026-05-07
updated: 2026-05-07
category: features
priority: high
branch: feat/memory-dreamer
---

# Plan: Memory Dreamer

## Problem

The memory system catches bad writes on the way **in** (dedup, classifier gate, style gate) but never reviews what's already stored. Over time, the memory store accumulates:

- **Redundant clusters** — 5 sobriety entries, 4+ deadnaming entries, 3+ financial stress entries scattered across categories
- **Stale mood snapshots** — "User feels low after therapy cancelled" stored as permanent facts
- **Point-in-time events** — entries that had contextual relevance for a day but are now noise, OR contain a durable pattern buried in temporal framing that should be promoted to a timeless fact
- **Supersede churn** — chains like ID 93→94→95→96→97 where the same fact gets slightly reworded five times instead of merged once

Human REM sleep solves this: memories are consolidated, reorganized, and pruned during sleep. The dream cycle already runs nightly but only handles persona evolution. This plan extends it with a **memory dreamer** — an autonomous tool-calling agent that tidies the memory store before persona reflection begins.

## Design Decisions (from interview)

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Operations (v1) | Merge, expire, promote | Merge redundant clusters; expire stale moods/events; promote point-in-time entries to timeless facts |
| Architecture | Tool-calling agent | Same pattern as memory agent — flexible, auditable per-action, reuses existing infra |
| Scoping | Embedding clusters + lonely pass | Cluster by cosine similarity for redundancy; unclustered memories get staleness review |
| Timing | Before persona reflection | Clean house first, then reflect on clean data |
| Context | Memories + metadata only (v1) | Content, category, importance, tags, timestamp, subject. No conversation lookups — add in v2 if needed |
| Model | New `dream_agent` config slot, Kimi K2 for now | Dedicated slot for independent tuning, falls back to memory agent model if empty |
| Safety | Audit log, soft-delete only, dry-run mode, sim support | Every operation logged; deactivated never deleted; config flag for dry runs; sim suite coverage |

## Operations

### Merge Redundant Memories

**When:** Embedding cluster contains 2+ memories about the same topic.

**How:** New `merge_memories` tool takes source IDs + merged text. Atomically:
1. Saves the consolidated memory with fresh embedding
2. Deactivates all source memories
3. Creates supersession chain (each source → new ID)
4. Auto-links the new memory
5. Logs to audit table

**Example:**
```
INPUT (cluster):
  #108: "Autumn quit heavy THC use because it was counteracting antidepressants"
  #109: "Autumn is maintaining sobriety from weed using environmental friction..."
  #110: "Autumn is healing dopamine receptor damage from heavy weed use"
  #140: "Autumn is maintaining 6 weeks of sobriety from weed"

OUTPUT (merged):
  #NEW: "Autumn quit daily THC use after 3 years because it counteracted her
         antidepressants. She maintains sobriety using environmental friction
         (storing supplies in a storage unit) and is healing dopamine receptor
         damage from prolonged use."

  #108, #109, #110, #140 → deactivated, superseded_by = #NEW
```

### Expire Stale Entries

**When:** Memory is a point-in-time mood snapshot or time-bound event that no longer holds relevance.

**How:** Uses existing `remove_memory` tool to deactivate. Audit log captures the reasoning.

**Examples of expirable memories:**
- `#40: "User is feeling 'kind of nothing' today after therapy got cancelled"` — ephemeral mood
- `#25: "User has plans to meet their friend Reid on Thursday"` — past event
- `#60: "User feels overwhelmed by the complexity of the skill trust system"` — transient reaction to a coding session

### Promote Point-in-Time to Durable

**When:** A temporal entry contains a real, recurring pattern worth preserving — just wrapped in "today/right now" framing.

**How:** Uses existing `update_memory` tool to reword the entry into a timeless fact.

**Example:**
```
BEFORE: #17 "User is experiencing daily frustration and aimlessness, feeling stuck
              in a cycle where not knowing what to do leads to doing nothing"
AFTER:  #17 "Autumn experiences recurring cycles of frustration and aimlessness where
              not knowing what to do leads to inaction — a pattern tied to executive
              dysfunction"
```

## Architecture

### Memory Dreamer Agent

A tool-calling agent following the same pattern as `RunMemoryAgent` in `agent/memory_agent.go`:

```
persona/memory_dreamer.go
├── RunMemoryDreamer(params MemoryDreamerParams) MemoryDreamerResult
│   ├── Load all active memories + embeddings
│   ├── Cluster by cosine similarity (threshold ~0.7)
│   ├── Build transcript: clusters + lonely memories + metadata
│   ├── Tool-calling loop (same continuation window pattern)
│   └── Return result with audit summary
```

**Tools available to the memory dreamer:**

| Tool | Source | Purpose |
|------|--------|---------|
| `think` | Existing | Scratchpad for reasoning about clusters |
| `recall_memories` | Existing | Semantic search for additional context |
| `update_memory` | Existing | Reword point-in-time → durable |
| `remove_memory` | Existing | Expire stale entries (batch deactivate) |
| `split_memory` | Existing | Break compound memories during consolidation |
| `merge_memories` | **New** | Merge a cluster into one consolidated memory |
| `done` | Existing | Signal completion |

### Scoping: Embedding Clusters + Lonely Pass

Before the agent runs, a preprocessing step groups memories:

```go
// ClusterMemories groups active memories by embedding similarity.
// Returns tight clusters (potential redundancy) and lonely memories
// (unclustered — candidates for staleness review).
func ClusterMemories(memories []memory.Memory, threshold float64) (clusters [][]memory.Memory, lonely []memory.Memory)
```

**Algorithm (simple, no external deps):**
1. Load all active memories with embeddings from `AllActiveMemories()`
2. Build a similarity graph: for each pair, if cosine similarity ≥ threshold → edge
3. Connected components = clusters (memories reachable from each other via similarity edges)
4. Single-node components = lonely memories
5. Filter: only pass clusters of size ≥ 2 to the agent (singletons in clusters aren't actionable for merging)

At 94 memories this is 94×94 = 8,836 cosine similarity computations on 384-dim vectors — sub-millisecond in Go. At 1,000 memories it's ~500K comparisons — still under a second. No need for approximate methods at this scale.

**Cluster threshold:** Start at 0.70 (same as auto-link). Configurable via `dream.cluster_threshold` in config.

### Transcript Format

The agent receives a structured transcript with clusters and lonely memories:

```
## Cluster 1 (4 memories, topic: sobriety)
- [ID=108, cat=health, imp=5, age=22d] Autumn quit heavy THC use because...
- [ID=109, cat=health, imp=5, age=22d] Autumn is maintaining sobriety from weed...
- [ID=110, cat=health, imp=5, age=22d] Autumn is healing dopamine receptor damage...
- [ID=140, cat=health, imp=5, age=8d] Autumn is maintaining 6 weeks of sobriety...

## Cluster 2 (3 memories, topic: financial stress)
- [ID=72, cat=event, imp=9, age=35d] User has 60 days to find new housing...
- [ID=89, cat=work, imp=5, age=14d] Autumn is working two jobs to slowly pay down...
- [ID=134, cat=health, imp=5, age=10d] Autumn is drowning in debt...

## Lonely memories (staleness review)
- [ID=25, cat=event, imp=8, age=45d] User has plans to meet their friend Reid on Thursday
- [ID=40, cat=mood, imp=9, age=40d] User is feeling 'kind of nothing' today after therapy...
- [ID=46, cat=mood, imp=9, age=38d] Feeling lonely seeing couples at coffee shop...
```

### New Tool: `merge_memories`

```
tools/merge_memories/
├── tool.yaml
└── handler.go
```

**tool.yaml:**
```yaml
name: merge_memories
agent: dream
description: >
  Merge multiple redundant memories into one consolidated memory.
  Provide the IDs of memories to merge and the new combined text.
  All source memories will be deactivated and superseded by the new one.
hot: true
category: dream
parameters:
  type: object
  properties:
    memory_ids:
      type: array
      items:
        type: integer
      description: IDs of the memories to merge (minimum 2)
    merged_text:
      type: string
      description: The consolidated memory text combining all sources
    category:
      type: string
      description: Category for the merged memory
    tags:
      type: string
      description: Comma-separated tags for the merged memory
  required:
    - memory_ids
    - merged_text
    - category
trace:
  emoji: "🔀"
  format: "merged {{len .MemoryIDs}} → new"
```

### Audit Log

New table for tracking all dream operations:

```sql
CREATE TABLE IF NOT EXISTS dream_audit (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
    operation TEXT NOT NULL,          -- 'merge', 'expire', 'promote', 'split'
    source_ids TEXT NOT NULL,         -- JSON array of affected memory IDs
    result_id INTEGER,                -- new memory ID (for merge), or updated ID (for promote)
    before_text TEXT,                 -- original text (for promote) or summary (for merge)
    after_text TEXT,                  -- new text
    reason TEXT,                      -- LLM's reasoning for the operation
    dry_run BOOLEAN DEFAULT 0         -- 1 if this was a dry-run (no DB changes made)
);
```

**Store interface additions:**

```go
// Dream audit
SaveDreamAudit(op string, sourceIDs []int64, resultID int64, before, after, reason string, dryRun bool) error
RecentDreamAudits(limit int) ([]DreamAudit, error)
```

### Config

```yaml
# New config section
dream:
  cluster_threshold: 0.70      # cosine similarity for clustering
  max_operations: 20            # cap per cycle (safety)
  dry_run: false                # log-only mode, no DB changes
  enabled: true                 # master switch

# New model config (falls back to memory_agent if empty)
dream_agent:
  model: "moonshotai/kimi-k2-0905"
  temperature: 0.3
  max_tokens: 4096
  timeout: 120
```

**Config struct:**

```go
// DreamConfig controls the memory dreamer's consolidation behavior.
type DreamConfig struct {
    ClusterThreshold float64 `yaml:"cluster_threshold"` // 0 = 0.70 default
    MaxOperations    int     `yaml:"max_operations"`    // 0 = 20 default
    DryRun           bool    `yaml:"dry_run"`           // log-only mode
    Enabled          bool    `yaml:"enabled"`           // master switch, default true
}

// DreamAgentConfig holds the LLM settings for the memory dreamer.
// Falls back to MemoryAgentConfig if Model is empty.
type DreamAgentConfig struct {
    Model       string          `yaml:"model"`
    Temperature float64         `yaml:"temperature"`
    MaxTokens   int             `yaml:"max_tokens"`
    Timeout     int             `yaml:"timeout"`
    Provider    *ProviderConfig `yaml:"provider,omitempty"`
    Fallback    *FallbackConfig `yaml:"fallback,omitempty"`
}
```

## Integration with Dream Cycle

The dreamer goroutine (`persona/dreamer.go`) gains a new first step:

```go
func runDream(ctx context.Context, p DreamerParams) {
    select {
    case <-ctx.Done():
        return
    default:
    }

    log.Info("dreamer: running dream cycle")

    // NEW — Step 0: Memory consolidation (clean house before reflecting).
    if p.Cfg.Dream.Enabled {
        result := RunMemoryDreamer(MemoryDreamerParams{
            LLM:      p.DreamLLM,  // new field — dedicated dream model
            Store:    p.Store,
            Embed:    p.Embed,
            Cfg:      p.Cfg,
            EventBus: p.EventBus,
        })
        if result.Error != nil {
            log.Error("dreamer: memory consolidation failed", "err", result.Error)
        } else {
            log.Infof("dreamer: consolidated %d merges, %d expires, %d promotes",
                result.Merges, result.Expires, result.Promotes)
            emitPersonaEvent(p.EventBus, "dream_consolidate",
                fmt.Sprintf("%d merges, %d expires, %d promotes",
                    result.Merges, result.Expires, result.Promotes))
        }
    }

    // Step 1: Nightly reflection (existing) — now sees clean memory state.
    if err := NightlyReflect(p.LLM, p.Store, p.Cfg, ...); err != nil {
        ...
    }

    // Step 2: Gated rewrite (existing).
    ...
}
```

## Implementation Phases

### Phase 1: Foundation — Config, Schema, Store Methods
**Files:** `config/config.go`, `migrations/000XXX_dream_audit.up.sql`, `memory/store.go`, `memory/store_dream.go`

1. Add `DreamConfig` and `DreamAgentConfig` structs to config
2. Add `dream` and `dream_agent` sections to `config.yaml.example`
3. Create `dream_audit` migration
4. Add `DreamAudit` type and store methods (`SaveDreamAudit`, `RecentDreamAudits`)
5. Add store interface methods

### Phase 2: Clustering Engine
**Files:** `persona/cluster.go`, `persona/cluster_test.go`

1. Implement `ClusterMemories(memories, threshold) → (clusters, lonely)`
2. Uses `embed.CosineSimilarity` (already exists) for pairwise comparison
3. Connected-components via union-find (simple, well-known algorithm)
4. Table-driven tests: known clusters, edge cases (empty set, all identical, all unique)

### Phase 3: merge_memories Tool
**Files:** `tools/merge_memories/tool.yaml`, `tools/merge_memories/handler.go`

1. Create tool directory with YAML definition
2. Handler: validate IDs, save merged memory with embedding, deactivate sources, create supersession chains, auto-link, log to dream_audit
3. Dry-run support: if `cfg.Dream.DryRun`, log the audit but skip DB mutations
4. Wire into tool registry (blank import)

### Phase 4: Memory Dreamer Agent
**Files:** `persona/memory_dreamer.go`, `persona/memory_dreamer_prompt.md`

1. `MemoryDreamerParams` struct (follows `MemoryAgentParams` pattern)
2. `MemoryDreamerResult` struct (merges, expires, promotes, cost, error)
3. `RunMemoryDreamer` function:
   - Load all active memories
   - Run `ClusterMemories` to scope the work
   - Build transcript (clusters + lonely memories with metadata)
   - Tool-calling loop with continuation windows (reuse pattern from memory agent)
   - Return result summary
4. Prompt file: instructions for the dream agent covering merge/expire/promote decisions
5. Blank imports for all tools the dreamer uses
6. Max operations safety cap (count tool executions, stop at limit)

### Phase 5: Integration — Wire into Dream Cycle
**Files:** `persona/dreamer.go`, `cmd/run.go`, `cmd/sim.go`

1. Add `DreamLLM` field to `DreamerParams`
2. Insert `RunMemoryDreamer` as Step 0 in `runDream()`
3. Wire up LLM client creation in `cmd/run.go` (read `dream_agent` config, fall back to memory agent)
4. Add dream consolidation to sim's `runDreamCycle()`
5. Emit persona events for TUI rendering

### Phase 6: Telegram + Observability
**Files:** `bot/handlers_persona.go`, `tui/event.go`

1. Extend `/dream` command to show consolidation results
2. Add `/dreamlog` command to show recent audit entries
3. Add `dream_consolidate` to persona event actions for TUI

### Phase 7: Sim Coverage
**Files:** `sims/dream-consolidation.yaml`

1. Build a sim that:
   - Pre-loads messy memories (redundant, stale, point-in-time)
   - Runs the dream consolidation cycle
   - Verifies merge/expire/promote operations via audit log
2. Add dream consolidation flag to existing `core-loop-and-dream.yaml`

## Files Changed (Summary)

| File | Change |
|------|--------|
| `config/config.go` | Add `DreamConfig`, `DreamAgentConfig` |
| `config.yaml.example` | Add `dream` and `dream_agent` sections |
| `migrations/000XXX_dream_audit.up.sql` | New table |
| `memory/store.go` | Interface additions |
| `memory/store_dream.go` | **New** — audit CRUD |
| `persona/cluster.go` | **New** — embedding clustering |
| `persona/cluster_test.go` | **New** — clustering tests |
| `tools/merge_memories/tool.yaml` | **New** — tool definition |
| `tools/merge_memories/handler.go` | **New** — merge handler |
| `persona/memory_dreamer.go` | **New** — agent loop |
| `persona/memory_dreamer_prompt.md` | **New** — agent instructions |
| `persona/dreamer.go` | Insert Step 0 |
| `cmd/run.go` | Wire dream LLM client |
| `cmd/sim.go` | Add consolidation to sim dream cycle |
| `bot/handlers_persona.go` | Extend `/dream`, add `/dreamlog` |
| `tui/event.go` | New event action |
| `sims/dream-consolidation.yaml` | **New** — test suite |

## Future (v2+)

- **Re-weight importance** — dream agent adjusts importance scores based on recency, reference frequency, emotional weight
- **Re-categorize** — normalize inconsistent categories
- **Source message lookups** — pull original conversation context via `source_message_id` for better staleness decisions
- **Cross-session pattern detection** — identify recurring themes across multiple conversations and surface them as meta-memories
- **Forgetting curve** — memories that haven't been recalled in N days get flagged for review (spaced repetition in reverse)
- **Memory graph visualization** — TUI or web view showing clusters, links, and supersession chains

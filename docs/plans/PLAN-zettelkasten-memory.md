# Plan: Zettelkasten-Style Memory Improvements

Inspired by [ScottRBK/forgetful](https://github.com/ScottRBK/forgetful) — a Zettelkasten-based MCP memory server.

**Goal:** Evolve our flat fact store into a self-organizing knowledge graph. Facts become linked nodes rather than isolated rows. Retrieval gets smarter — pulling in related facts even when they don't directly match the query embedding.

**Status of what we already have:**

| Concept | Status | Where |
|---------|--------|-------|
| Atomic facts (200 char cap) | Done | `memory/extract.go` |
| Style gates (reject AI tics) | Done | extraction prompt |
| Importance scoring (1-10) | Done | `facts.importance` column |
| Embeddings + vec search | Done | `embed/`, `vec_facts` virtual table |
| Soft delete | Done | `facts.active` column |
| Deduplication via cosine sim | Done | agent tool, `similarityThreshold` config |
| Fact linking | Done | `fact_links` table, `AutoLinkFact()`, 1-hop traversal in `SemanticSearch()` |
| Supersession chains | Done | `SupersedeFact()`, `FactHistory()`, `update_fact` creates chain |
| Token budget (adaptive context) | Superseded | Dual-compactor (`chat_context_budget` + `agent_context_budget`) handles history/actions; fact count cap fine at current scale |
| Query context for retrieval | Done | Conversation-concat in `agent.go` before embedding (no LLM call) |
| Cross-encoder reranking | **Missing** | — |
| Fact context field ("why") | Done | `facts.context` column, optional param on save/update tools |

---

## Phase 1: Fact Linking (The Zettelkasten Core)

**What:** When a new fact is saved, auto-link it to the most similar existing facts. Links are bidirectional. During retrieval, do 1-hop traversal to pull in neighbors.

**Why:** "What does she like to cook?" currently only finds facts whose embeddings match that query. With linking, it also pulls in related facts about her kitchen, dietary preferences, favorite recipes — things that are semantically close to *other* matching facts, not necessarily to the query itself.

### Schema Change

```sql
-- New table in memory/store.go initDB()
CREATE TABLE IF NOT EXISTS fact_links (
    source_id INTEGER NOT NULL,
    target_id INTEGER NOT NULL,
    similarity REAL NOT NULL,       -- cosine similarity at link time
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (source_id, target_id),
    FOREIGN KEY (source_id) REFERENCES facts(id),
    FOREIGN KEY (target_id) REFERENCES facts(id)
);
CREATE INDEX IF NOT EXISTS idx_fact_links_source ON fact_links(source_id);
CREATE INDEX IF NOT EXISTS idx_fact_links_target ON fact_links(target_id);
```

**Normalization trick from forgetful:** Always store `(min(id), max(id))` as `(source_id, target_id)`. This prevents duplicate links in both directions and simplifies lookups.

### Store Methods to Add (`memory/store.go`)

```go
// LinkFacts creates a bidirectional link between two facts.
// IDs are normalized (min, max) to prevent duplicates.
func (s *Store) LinkFacts(id1, id2 int64, similarity float64) error

// LinkedFacts returns facts linked to the given fact ID (1-hop).
// Looks up both directions since links are normalized.
func (s *Store) LinkedFacts(factID int64, limit int) ([]Fact, error)

// AutoLinkFact finds the top-N most similar existing facts and links them.
// Called after saving a new fact that has an embedding.
// Uses vec_facts KNN search, filters by similarity >= threshold.
func (s *Store) AutoLinkFact(factID int64, embedding []float32, numLinks int, minSimilarity float64) error
```

### Config Additions (`config/config.yaml`)

```yaml
memory:
  auto_link_count: 3        # max links per new fact (0 = disabled)
  auto_link_threshold: 0.7  # minimum cosine similarity to create link
```

### Integration Points

1. **`SaveFact()`** — after inserting the fact and its embedding, call `AutoLinkFact()` if embedding is non-nil
2. **`SemanticSearch()`** — after getting primary results, do 1-hop link traversal:
   - For each primary result, call `LinkedFacts(id, 5)`
   - Deduplicate against primary results
   - Merge into final result set (linked facts come after primary hits)
3. **`DeactivateFact()`** — no change needed. Linked facts pointing to deactivated facts just won't appear (the `active=1` filter handles it in `LinkedFacts`)

### Teaching Notes

- The `fact_links` table is a classic **adjacency list** — same pattern used for social graphs, dependency trees, etc. Go doesn't have a built-in graph type, so we model it with a table and queries.
- The normalization trick (`min/max`) is worth calling out — it's a neat way to avoid storing the same relationship twice.
- `LinkedFacts` query will use a UNION to search both directions:
  ```sql
  SELECT f.* FROM facts f
  JOIN fact_links fl ON fl.target_id = f.id WHERE fl.source_id = ? AND f.active = 1
  UNION
  SELECT f.* FROM facts f
  JOIN fact_links fl ON fl.source_id = f.id WHERE fl.target_id = ? AND f.active = 1
  ```

---

## Phase 2: Supersession Chains ✅

**Status:** Complete as of 2026-03-31.

**What:** When a fact is replaced by a newer version, record *which* fact replaced it and *why*. Supersession chains let the agent trace knowledge evolution: "you used to work at Company A."

**Implemented:**
- `SupersedeFact()` — marks old fact inactive, records `superseded_by` and `supersede_reason`
- `FactHistory()` — walks the chain both directions (predecessors + successors), cycle-safe, capped at 20 hops
- `GetFact()` — reads full fact by ID (including inactive ones, with supersession fields)
- `update_fact` tool — now creates a new fact via `SaveFact()` and supersedes the old one (instead of overwriting in-place). Gets embedding, auto-linking, and classifier gate for free.
- `remove_fact` tool — already had `replaced_by` param wired to `SupersedeFact()` since Phase 1

---

## Phase 3: Token Budget for Context Assembly

**What:** Replace fixed `maxFacts` count with a token budget. Fill context importance-first, then recency, stopping when the budget is reached.

**Why:** A 200-char fact and a 50-char fact shouldn't count the same. Token budgeting lets us pack more short facts or fewer long ones, adapting to what's actually stored.

### Changes to `memory/context.go`

```go
// BuildMemoryContext currently takes maxFacts int.
// Change signature to:
func BuildMemoryContext(store *Store, tokenBudget int, relevantFacts []Fact, userName string) (string, error)

// Inside, instead of slicing to maxFacts:
// 1. Sort merged facts by importance DESC, timestamp DESC
// 2. Accumulate facts until tokenBudget would be exceeded
// 3. Track how many were dropped for logging
```

### Token Counting

We already have `token_count` on messages. For facts, we can estimate ~1 token per 4 chars (rough but good enough), or use a proper tokenizer if we want precision. The rough estimate is fine — this is a budget, not a hard limit.

### Config

```yaml
memory:
  context_token_budget: 2000  # replaces max_facts_in_context for context assembly
  max_facts_in_context: 30    # keep as hard ceiling to prevent runaway queries
```

---

## Phase 4: Query Context ("Why Am I Searching?") ✅

**Status:** Complete as of 2026-03-31.

**What:** Embedding just the latest message misses conversational intent. Now we prepend up to 2 prior user messages before embedding, so "vet says it might be his kidneys" also captures the earlier "my dog max has been sick" context.

**Implemented:** Conversation-concat approach in `agent/agent.go` — no LLM call, no config, no schema. Prior user messages joined with ` | ` separator before embedding.

### Future: Cross-Encoder Reranking

Add a reranking step like forgetful does (dense search → cross-encoder → top-K). Requires a local cross-encoder model. Lower priority.

---

## Phase 5: Fact Context Field ✅

**Status:** Complete as of 2026-03-31.

**What:** Optional `context` column on facts — a 1-2 sentence note explaining *why* a fact matters or how it connects to other knowledge.

**Implemented:**
- Schema: `ALTER TABLE facts ADD COLUMN context TEXT`
- `Fact.Context` field on the struct
- `save_fact`, `save_self_fact`, `update_fact` tools accept optional `context` param
- Text embedding enriched with context when present (tag embedding unchanged)
- Rendered in chat prompt as `- [Mar 31] Fact text (context note)`
- `GetFact()` reads context for supersession chain traversal

---

## Implementation Order

```
Phase 1: Fact Linking          ← highest value, enables the knowledge graph
Phase 2: Supersession Chains   ← small change, big payoff for knowledge evolution
Phase 3: Token Budget          ← improves context quality as fact count grows
Phase 4: Query Context         ← lightweight retrieval improvement
Phase 5: Fact Context Field    ← stretch goal, depends on extraction quality
```

Phases 1 and 2 can be done in the same session (they touch different parts of the schema). Phase 3 is independent. Phases 4 and 5 are refinements that build on having more facts in the system.

---

## Files That Will Change

| File | Phases | What Changes |
|------|--------|-------------|
| `memory/store.go` | 1, 2, 3, 5 | Schema, new methods (LinkFacts, AutoLinkFact, SupersedeFact, etc.) |
| `memory/context.go` | 3 | Token budget logic replaces fixed count |
| `memory/extract.go` | 5 | Extraction prompt gets optional context field |
| `config/config.go` | 1, 3 | New config fields (auto_link_count, threshold, token_budget) |
| `config/config.yaml.example` | 1, 3 | Document new config options |
| `agent/agent.go` | 1, 4 | Semantic search uses linked facts; query context concatenation |
| `embed/embed.go` | — | No changes needed (already sufficient) |

## Reference

- Forgetful repo: https://github.com/ScottRBK/forgetful
- Key files studied: `app/services/memory_service.py`, `app/repositories/sqlite/memory_repository.py`, `app/models/memory_models.py`
- Their auto-link threshold: 0.7 cosine similarity, top 3 links per memory
- Their token budget default: 8000 tokens, max 20 memories
- Their cross-encoder: Xenova/ms-marco-MiniLM-L-12-v2

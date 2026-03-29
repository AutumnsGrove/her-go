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
| Fact linking | **Missing** | — |
| Supersession chains | **Missing** | — |
| Token budget (adaptive context) | **Missing** | fixed `maxFacts` count today |
| Query context for retrieval | **Missing** | — |
| Cross-encoder reranking | **Missing** | — |
| Fact context field ("why") | **Missing** | — |

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

## Phase 2: Supersession Chains

**What:** When a fact is deactivated because a newer version exists, record *which* fact replaced it and *why*.

**Why:** "She works at Company A" → superseded by → "She left Company A, now at Company B." Currently we just deactivate the old fact and the history is lost. Supersession chains let us trace knowledge evolution, and the agent could use them to say "oh, you used to work at Company A" naturally.

### Schema Change

```sql
-- Add columns to existing facts table
ALTER TABLE facts ADD COLUMN superseded_by INTEGER REFERENCES facts(id);
ALTER TABLE facts ADD COLUMN supersede_reason TEXT;
```

### Store Methods

```go
// SupersedeFact marks a fact as inactive and records what replaced it.
func (s *Store) SupersedeFact(oldID, newID int64, reason string) error

// FactHistory returns the supersession chain for a fact (follow superseded_by links).
func (s *Store) FactHistory(factID int64) ([]Fact, error)
```

### Integration

- The agent's `update_fact` / `delete_fact` tools should use `SupersedeFact()` instead of bare `DeactivateFact()` when a replacement fact exists.
- During extraction, if the LLM detects a fact that contradicts an existing one, it could return both the new fact and a reference to the old one. (This is a stretch goal — may require extraction prompt changes.)

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

## Phase 4: Query Context ("Why Am I Searching?")

**What:** Pass a short "why" string alongside the embedding query so retrieval can consider intent, not just content.

**Why:** "Tell me about her dog" and "She's feeling sad about her dog" have overlapping embeddings but very different retrieval needs. The first wants pet facts; the second wants emotional context + pet facts.

### How It Works

This is lightweight. In `agent/agent.go` where we call `SemanticSearch()`:

1. Before embedding the user message, have the agent generate a 1-sentence query context: "User is asking about X because Y"
2. Concatenate: `queryText = userMessage + " | Context: " + queryContext`
3. Embed the concatenated string
4. Pass to `SemanticSearch()` as usual

The embedding now captures intent, not just content. No schema changes needed.

### Alternative (More Involved)

Add a cross-encoder reranking step like forgetful does:
1. Dense search returns 20 candidates
2. Cross-encoder scores each (query, fact) pair
3. Return top-K

This requires a local cross-encoder model (e.g., ms-marco-MiniLM via our embed service). Worth exploring but lower priority than the other phases.

---

## Phase 5: Fact Context Field (Stretch Goal)

**What:** Add a `context` column to facts — a short (500 char max) note explaining *why* this fact matters or *how* it relates to other knowledge.

**Example:**
- Fact: "Autumn is learning Go"
- Context: "This is her primary project language. She's comfortable with Python/TS but new to Go. Teaching mode is important."

### Schema

```sql
ALTER TABLE facts ADD COLUMN context TEXT;
```

### Extraction Change

Update the extraction prompt to optionally produce context alongside each fact. Keep it optional — most facts won't need it.

### Retrieval Impact

When building embedding text for a fact, include context:
```go
embeddingText := fact.Fact + " " + fact.Context  // if context is non-empty
```

This makes semantic search aware of the *why*, not just the *what*.

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

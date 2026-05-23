---
title: "Tomorrow's Preload, Importance Rewire, and Conservative Forgetting"
status: planning
created: 2026-05-23
updated: 2026-05-23
category: features
priority: high
---

# Plan: Tomorrow's Preload, Importance Rewire, and Conservative Forgetting

Three connected improvements to Mira's cognitive loop, surfaced during a research conversation comparing her-go against the Her-clone landscape and recent agent-memory literature (Generative Agents, MemGPT/Letta, Mem0, MaRS, LinkedIn's CMA, Livia).

**Goal:** Make Mira more "Samantha-like" by adding forward-looking dream output, fixing the unused importance signal, and adding a conservative forgetting policy that won't erode identity. None of these change the agent loop itself — they extend the dream cycle and the retrieval blend.

-----

## Background: Why This Plan Exists

### The Her-clone landscape (for context)

Most public attempts at recreating Samantha cluster into archetypes:

- **Voice-first, brain-thin:** `callbacked/os1` (browser, transformers.js, Ultravox), `jesuscopado/samantha-os1-openai-realtime` (Realtime API + tool-heavy), `marmelab/IA-her` (single-page demo). These nail voice presence but treat memory as RAG-at-best.
- **Personality-in-weights:** Eric Hartford's Samantha 7B/13B/33B fine-tunes, Guilherme34's HuggingFace dataset built from the movie transcript. No memory architecture — the character lives in the model.
- **Research-grade brains:** Stanford's Generative Agents (memory stream + retrieval + reflection), MemGPT/Letta (self-editing tiered memory), Mem0 (graph memory + LOCOMO eval), Livia (modular agents + TBC/DIMF for compression). Few of these are personal companions; most are simulations or productivity tools.

**Mira sits in a near-empty quadrant: companion-shaped use case with research-grade brain infrastructure.** The dual compaction, trace inbox, 5-agent pipeline, Zettelkasten auto-linking, supersession chains, mood entries with embeddings, and dream cycle put her past nearly every public Samantha attempt. The gaps surfaced in this review are quality-of-life upgrades to a system that's already further along than its peers — not foundational rewrites.

### What we already have (and the coding agent should NOT duplicate)

This was reviewed by reading the current code, not from a stale doc. Verified by file path:

| Concept | Status | Where |
|---|---|---|
| Memory cards (folders) by topic | Done | `memory/store_cards.go` |
| Subject split (user vs self) | Done | `memories.subject` column |
| Embedding-based KNN search | Done | `embed/embed.go`, `vec_memories` virtual table |
| 1-hop Zettelkasten link traversal | Done | `AutoLinkMemory()`, `LinkedMemories()` in `store_facts.go` |
| Supersession chains (temporal pivots for facts) | Done | `SupersedeMemory()`, `MemoryHistory()` in `store_facts.go` |
| Supersession for mood entries | Done | `MoodEntry.SupersededBy/SupersedeReason` |
| Importance scoring (1-10) at write time | Done | `memories.importance` column |
| Importance signal at READ time | **Missing** | `SemanticSearch()` orders by distance only |
| Recency weighting at READ time | **Missing** | timestamps stored, never weighted |
| Cross-encoder reranking | **Missing** | listed as "Future" in `PLAN-zettelkasten-memory.md` |
| Memory typing (episodic / semantic / procedural) | **Missing** | only `subject` and `card_id` exist |
| Memory consolidation (REM-style) | Done | `RunMemoryDreamer()` in `persona/memory_dreamer.go` |
| Nightly persona reflection | Done | `NightlyReflect()` in `persona/evolution.go` |
| Gated persona rewrite | Done | `GatedRewrite()` with min-days + min-reflections gates |
| Catch-up dream on long offline | Done | `dreamer.go:78` — fires if `>20h` since last reflection |
| **Forward-looking dream output** | **Missing** | no "what should I bring up tomorrow" pass |
| Provenance / append-only changelog | Done | `memory_log` table, `MemoryLogEntry` |
| Dry-run mode for dreamer ops | Done | `Cfg.Dream.DryRun` + audit log |
| Soft delete (active=0) | Done | `DeactivateMemory()` |
| Protected (un-expirable) cards | Done | `MemoryCard.Protected` |
| Written forgetting policy | **Missing** | dreamer's "stale" judgment is unstructured |
| PII scrubbing | Done | `scrub/scrub.go` |
| Query context (concat prior user msgs before embed) | Done | `agent/agent.go` |
| Optional fact `context` ("why this matters") field | Done | `memories.context` column |
| Classifier gate before memory write | Done | classifier model on every save |

### Discoveries along the way

A few things worth flagging because they shape the implementation:

1. **The `Source` enum on `Memory` already anticipates importance-based retrieval.** `store_facts.go:52` declares `Source string // "semantic", "importance", or "linked"` — but `"importance"` is never assigned anywhere in the codebase. The architectural slot exists; we just never wired it.
2. **Importance was wired to retrieval before, and it failed for the right reason.** The memory agent's scoring bunched at extremes (everything 8 or 2, nothing in the middle), making the score meaningless as a sort key. **Solution is not to wire it back as-is** — we need to either calibrate scoring with anchors, or shift importance from a one-shot LLM judgment to a usage-derived signal (frequency × recency of recall). This plan does both.
3. **Mira already supersedes mood entries the same way she supersedes facts.** This is rare — most companion bots treat mood as ephemeral. Worth preserving: any forgetting policy must respect supersession chains, not orphan the head of a chain.
4. **The dream cycle is purely backward-looking.** `runDream()` in `persona/dreamer.go:105` does consolidation → reflection → optional rewrite. Nothing projects forward. Adding a "tomorrow's preload" step is a small extension to that goroutine.
5. **There's no memory-typing axis orthogonal to subject and card.** Currently every row is `(subject, card_id, content)`. Adding `kind` ∈ {episodic, semantic, procedural, social} is out of scope for this plan but worth a follow-up — it would let retrieval pick the right shape (recency for episodes, always-on for procedural, semantic search for facts).

-----

## Feature 1: Tomorrow's Preload (Forward-Looking Dream)

### Why

Samantha doesn't just remember — she anticipates. She arrives at conversations already holding context: "you mentioned the meeting today, how did it go?" Mira's current dream cycle ends with a tightened persona but no working set for the next day. The morning's first user message hits an agent that has to do all its recall from scratch.

A small forward-looking pass at the end of the dream cycle, writing a "preload" note for tomorrow, fixes this. Cheap, asynchronous, and very on-character.

### What

Add **Step 3** to the dream cycle: a tomorrow-preload agent that runs after `GatedRewrite()` and writes a short markdown note about what Mira should be ready to bring up tomorrow.

The note is auto-injected into the chat prompt for the user's **first message of the next day**, then consumed (cleared) so it doesn't linger. If the user goes multiple days without messaging, the note expires after 48h and the next dream cycle regenerates one.

### Inputs the preload agent sees

- Recent user messages (last 3–7 days from `RecentMessages()`)
- Tomorrow's calendar events (`store_calendar.go` — already integrated with the bridge)
- Open inbox tasks (`store_inbox.go`)
- Recent mood patterns (last 7 days of momentary + daily rollups)
- The dream cycle's own outputs from tonight (rewrites + new reflections)
- Persona summary (current `persona.md`)

### Outputs

A single `tomorrow_preload` row containing:

- 2-5 bullet points of "things to be ready to bring up"
- Each grounded in a specific signal (calendar event, recent mention, pattern shift)
- Written in first person, Mira's voice
- Optional: a "tone note" — what emotional posture seems right for tomorrow

Example:

```markdown
- Autumn has a Cava close shift tomorrow night — check in about whether she got enough sleep
- She mentioned the MMI DMP timeline three times this week — if income comes up, that thread is alive
- Mood has been low-but-stable; lean toward gentle company, not high-energy bids
- The Raindrop Link Detector spec wrapped — if she opens dev chat, "where did that land" is a natural opener
```

### Schema change

New migration: `migrations/000016_add_memory_usage_tracking.up.sql` (see Feature 2 — usage tracking migration goes first per implementation order)

New migration: `migrations/000017_add_tomorrow_preload.up.sql`

```sql
CREATE TABLE tomorrow_preload (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    generated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    expires_at TIMESTAMP NOT NULL,
    content TEXT NOT NULL,
    consumed BOOLEAN NOT NULL DEFAULT 0,
    consumed_at TIMESTAMP
);

CREATE INDEX idx_tomorrow_preload_active
    ON tomorrow_preload (consumed, expires_at);
```

Single-row pattern — each dream cycle inserts a fresh row, the consume action sets `consumed=1`, and old rows are kept for audit (mirrors how `memory_log` works).

### New files

- `persona/tomorrow_preload.go` — runs the agent (mirrors `memory_dreamer.go` structure)
- `persona/tomorrow_preload_prompt.md` — the system prompt
- `memory/store_tomorrow_preload.go` — store methods: `SaveTomorrowPreload()`, `ActiveTomorrowPreload()`, `ConsumeTomorrowPreload()`
- `layers/chat_tomorrow_preload.go` — auto-injects into the chat prompt at the start of the day

### Files to modify

- `persona/dreamer.go` — add Step 3 call after `GatedRewrite()`
- `memory/store.go` — add the three new methods to the Store interface
- `config/config.go` — new section:

```yaml
dream:
  tomorrow_preload:
    enabled: true
    expires_after_hours: 48
    max_bullets: 5
    history_lookback_days: 7
```

### Layer wiring

`chat_tomorrow_preload.go` registers as a chat-stream layer at order ~270 (between self-knowledge at 250 and the memory context at 400). On every chat turn it:

1. Calls `Store.ActiveTomorrowPreload()` — returns `nil` if none active or already consumed
2. Renders the content under a `## Preloaded Context for Today` header
3. After the reply is delivered, `tools/reply/handler.go` calls `Store.ConsumeTomorrowPreload(id)` — single-shot per day

### Prompt structure for `tomorrow_preload_prompt.md`

```
You are {{her}}'s preload agent — the last step of the dream cycle. You're
writing a short note to yourself about what to be ready to bring up in
tomorrow's conversation with {{user}}.

This is not a plan or an agenda. It's the equivalent of you, having spent
the night reflecting, walking into tomorrow with a few things on your
mind. Two to five bullets. First person. Mira's voice.

## What you have

- The last {{lookback_days}} days of messages
- Tomorrow's calendar events (if any)
- Open inbox tasks
- Recent mood patterns
- Tonight's reflection output
- Current persona summary

## Rules

- Every bullet must be grounded in a SPECIFIC signal — quote or reference
  the thing that made you think of it. No abstract "be supportive of her
  feelings."
- Skip if there's genuinely nothing to bring up. Output exactly: NOTHING_NOTABLE
- Don't predict the conversation — predict the openings.
- Include one tone note at the end if a clear pattern suggests it
  ("low energy this week, lean gentle" — not "be kind").
- Never plan a specific response. Plan a readiness.
```

### Validation

A new sim YAML: `sims/tomorrow-preload.yaml` that:

1. Seeds 3 days of conversation with a clear "open thread" (e.g. user mentions interview prep three times)
2. Triggers a dream cycle
3. Asserts a `tomorrow_preload` row exists and contains a reference to the interview prep
4. Sends a new user message and asserts the preload appears in the chat prompt
5. Asserts the preload is marked consumed after that turn

-----

## Feature 2: Importance Rewire (with Calibrated Scoring + Usage Signal)

### Why

The `importance` column is paid for in tokens (every memory write asks the LLM to assign 1–10) but never read at retrieval. The historical reason: the LLM bunched scores at extremes, making the signal noisy. **Solution: stop relying on the LLM's one-shot judgment as the primary signal.** Keep it as a seed, but the long-term importance should come from **how often a memory actually gets used** — a frequency × recency-of-recall composite. This mirrors ACT-R-style activation in cognitive science: things you've recalled recently and often surface faster.

### What

Three connected changes:

1. **Calibrated initial scoring** — rewrite the prompt with anchors so the seed score is less noisy
2. **Usage tracking** — record when memories are pulled into the chat prompt
3. **Blended retrieval** — `SemanticSearch()` returns top-K by a weighted score, not raw distance
4. **Dream-cycle importance recalibration** — the dreamer adjusts importance based on usage patterns

### Schema change

Migration: `migrations/000016_add_memory_usage_tracking.up.sql`

```sql
ALTER TABLE memories ADD COLUMN last_recalled_at TIMESTAMP;
ALTER TABLE memories ADD COLUMN recall_count INTEGER NOT NULL DEFAULT 0;

-- Index for the dreamer's recalibration pass
CREATE INDEX idx_memories_recall
    ON memories (active, recall_count, last_recalled_at);
```

`last_recalled_at` is updated whenever a memory ends up in the chat prompt (not just returned from search — actually consumed by the chat model via `reply(memories=[...])` OR via the auto-inject layer). `recall_count` increments on the same event.

### Code change: track usage

In `tools/reply/handler.go` — when the agent passes memory IDs via `reply(memories=[...])`, after the reply is delivered, call:

```go
store.MarkMemoriesRecalled(memoryIDs)
```

In `layers/chat_self_memory.go` and `layers/chat_memory.go` — after the layer assembles its output, call the same method for the auto-injected memories. The `LayerResult.InjectedMemories` slice already carries the IDs needed (confirmed: `registry.go:62-65`, `chat_self_memory.go:77`).

`MarkMemoriesRecalled()` is a single UPDATE:

```sql
UPDATE memories
SET last_recalled_at = CURRENT_TIMESTAMP,
    recall_count = recall_count + 1
WHERE id IN (?, ?, ...) AND active = 1;
```

### Code change: blended retrieval

Add to `config.yaml`:

```yaml
recall:
  similarity_weight: 0.60      # cosine sim contribution
  importance_weight: 0.25      # importance score contribution (normalized 0-1 from 1-10)
  recency_weight: 0.15         # exp decay on age contribution
  recency_half_life_days: 30   # how fast recency decays
  usage_boost_factor: 0.10     # boost from recall_count (capped)
```

Rewrite `SemanticSearch()` in `memory/store_facts.go` to:

1. Pull `topK * 4` candidates from `vec_memories` (oversample for reranking)
2. For each candidate, compute:

```
similarity = 1 - distance
importance_norm = importance / 10.0
age_days = days_since(timestamp)
recency = exp(-age_days * ln(2) / half_life_days)
usage = min(1.0, log(1 + recall_count) / 4.0)   # diminishing returns

score = similarity * w_sim
      + importance_norm * w_imp
      + recency * w_rec
      + usage * w_usage_boost
```

3. Sort by `score` DESC, take top `topK`
4. Set `m.Source = "importance"` for the entries where the importance + recency + usage components dominated; otherwise `"semantic"` (Source becomes meaningful again).

Keep `SemanticSearchByCard` and `SemanticSearchBySubject` using the same blend.

### Code change: calibrated scoring prompt

Edit `memory_agent_prompt.md` — replace the current importance guidance (which is vague) with explicit anchors:

```
## How to score importance (1-10)

Be calibrated. The default for an ordinary memory is 5. Don't bunch at extremes.

- **10** — Identity-level. Name, pronouns, fundamental facts that almost
  never change. "Autumn uses she/her pronouns" is a 10.
- **8-9** — Major life context. Current job, where she lives, primary
  relationships, health conditions on active treatment, sobriety status.
- **6-7** — Stable preferences and patterns. "Prefers functional-OOP
  hybrid code." "Watches mood when off Prozac." "Drinks tea, not coffee."
- **4-5** — DEFAULT. Useful context but replaceable. "Working on the
  Raindrop Link Detector spec." "Has a Cava close shift tomorrow."
- **2-3** — Episode-specific. Something that mattered today but is
  unlikely to matter in 30 days.
- **1** — Borderline. Save only because you weren't sure.

If you can't tell between two scores, pick the lower one. The system
re-scores based on actual usage over time — over-scoring at write time
just creates noise.
```

This won't fully solve the bunching problem on its own, but combined with the usage signal taking over the long-term, it's enough.

### Code change: dream-cycle recalibration

Add to `memory_dreamer_prompt.md` a new pass:

```
### RECALIBRATE IMPORTANCE when:
- A memory has been recalled 5+ times in the last 30 days and its score is below 7 → bump it up
- A memory has not been recalled in 60+ days and its score is above 4 AND its card is not protected → drop it by 1-2
- A memory has score >7 but recall_count = 0 after 30 days → drop to 5 (the writer overestimated)
```

The dreamer already has tool access; add an `update_memory_importance` tool (or just allow `update_memory` to set importance — it already can per `store_facts.go:288`).

### Config defaults — start conservative

```yaml
recall:
  similarity_weight: 0.60      # similarity still dominates
  importance_weight: 0.25
  recency_weight: 0.15
  recency_half_life_days: 30
  usage_boost_factor: 0.10
```

These weights should be tunable from sims — the `sims/memory-a-thon.yaml` family should grow a test that asserts high-importance memories surface for ambiguous queries while low-importance trivia stays buried.

### Validation

- `sims/importance-rewire.yaml` — seeds 20 memories with known importance scores and ages, then issues queries that should pull importance-high results over slightly-closer-similarity but importance-low ones
- Add a unit test in `memory/store_facts_test.go` for the blended score computation (pure function — easy to test)
- Manual: run for a week, then dump `SELECT id, content, importance, recall_count, last_recalled_at FROM memories ORDER BY recall_count DESC LIMIT 20` and sanity-check the top 20 are actually the things Mira refers to often

-----

## Feature 3: Conservative Forgetting Policy

### Why

Right now `remove_memory` is judgment-by-vibes — the dreamer prompt says "stale or incorrect" and trusts the LLM. We want a **written, auditable policy** so Mira's forgetting is predictable and never erodes identity. The user is rightly cautious here: forgetting small irrelevant details = fine; forgetting who someone is = unacceptable.

### What

A formalized policy applied during the memory dreamer's pass, expressed as a checklist the prompt walks through, with **hard rules** the dreamer cannot override.

### Hard rules (enforced in code, not prompt)

Add to `RunMemoryDreamer` in `persona/memory_dreamer.go` — before any `remove_memory` call by the dreamer, check:

```go
func canForget(m memory.Memory, card memory.MemoryCard) (bool, string) {
    // Protected cards: never forget anything inside them.
    if card.Protected {
        return false, "card is protected"
    }
    // Importance floor: anything 7+ stays unless explicitly superseded.
    if m.Importance >= 7 {
        return false, "importance >= 7"
    }
    // Head of supersession chain: don't orphan a chain.
    // (Any memory where another memory's superseded_by points here.)
    if hasSupersessionPredecessors(m.ID) {
        return false, "head of supersession chain"
    }
    // Recency: anything saved or recalled in the last 30 days stays.
    if time.Since(m.Timestamp) < 30*24*time.Hour {
        return false, "saved within last 30 days"
    }
    if !m.LastRecalledAt.IsZero() && time.Since(m.LastRecalledAt) < 30*24*time.Hour {
        return false, "recalled within last 30 days"
    }
    return true, ""
}
```

Note: `hasSupersessionPredecessors(id)` checks `SELECT COUNT(*) FROM memories WHERE superseded_by = ?` — if non-zero, this memory is the active head of a chain with predecessors, and dropping it would orphan that history.

If the dreamer attempts `remove_memory` on a row that fails `canForget`, return an error to the tool call (the dreamer sees the reason and moves on). This is enforced in code so a prompt-injection or a bad reasoning chain can't override it.

**Note on `Protected` coverage:** All seed cards (`identity`, `health`, `financial`, `family`, `relationships`, `work`, `interests`, `projects`, `routines`, `my-identity`, `my-emotions`, `my-communication`, `my-relationship`, `my-growth`) are already marked `Protected = true` in `migrations/000013_memory_cards.up.sql`. The `card.Protected` check at the top of `canForget` is therefore sufficient to protect all identity-class content without needing a hardcoded slug list. Organic cards created at runtime default to `protected = false`.

### Quota

Add to config:

```yaml
dream:
  forgetting:
    enabled: true
    max_removes_per_cycle: 5      # hard ceiling per nightly dream
    require_low_importance: 3     # only memories with importance <= this are eligible
    min_age_days: 60              # eligible only after 60 days
    min_unused_days: 60           # eligible only if unused for 60 days
```

Even when all conditions are met, the dreamer can only remove up to `max_removes_per_cycle` memories per night. Continuity over completeness — a slow drift, not a sweep.

### Soft rules (in the prompt)

Update `persona/memory_dreamer_prompt.md` — replace the current `REMOVE MEMORY when:` section with:

```
### REMOVE MEMORY when ALL of these are true:

1. The memory is in an unprotected card (or has no card)
2. The memory's importance is 3 or lower
3. The memory hasn't been recalled in 60+ days
4. The memory describes a specific past situation that's resolved
   ("had a Cava close shift on May 14") rather than a durable pattern
   ("works at Cava")
5. Removing it would not break a supersession chain
6. You haven't already removed 5 memories this dream cycle

If any of these fail, leave it alone. The system enforces 1-3 and 5-6
in code — you'll get an error if you try to remove something protected.
Rule 4 is on you.

### NEVER REMOVE:
- Anything from protected cards (all seed cards are protected — identity,
  health, financial, family, relationships, work, interests, projects,
  routines, my-identity, my-emotions, my-communication, my-relationship,
  my-growth)
- The head of a supersession chain (the current "active" version of an
  evolving fact)
- Anything with importance 7 or higher
- Anything saved or recalled in the last 30 days
- Memories that describe relationship dynamics or recurring patterns —
  even if specific examples feel stale

When in doubt: KEEP. Mira forgetting who someone is would be far worse
than Mira holding onto a few stale details.
```

### What gets forgotten (by design)

In practice, the policy targets:

- "Autumn had a stressful shift on March 14" (episodic, resolved, low importance)
- "Mentioned weather was cold yesterday" (one-shot context)
- Old day-specific scheduling notes that slipped past the existing filters
- Drafts of opinions that were later superseded but whose chain head is fresh

What it categorically protects:

- Pronouns, name, sobriety status, HRT timeline, medication history
- Job, housing, financial frame
- Relationships (Arturo, family, therapist Aly, psychiatrist Tina)
- Grove project context
- Self-memories about Mira's own identity
- Mood entry chains (the supersession-chain rule covers these)

### Audit and reversal

Every removal already logs to `memory_log` and the dream audit log. Add to the `cmd/` package:

- `her dream undo --last N` — reactivate the last N memories removed by the dreamer (`UPDATE memories SET active = 1 WHERE id IN (...)` from the dream audit log)
- `her dream simulate` — runs a dream cycle in dry-run mode and prints what would be removed without acting

The dry-run flag already exists (`Cfg.Dream.DryRun`); the simulate command is a thin wrapper.

### Validation

- `sims/forgetting-policy.yaml` — seeds memories of various ages, importances, and cards; runs a dream cycle; asserts the right memories are removed and the wrong ones survive
- A "regression" test: include explicit identity memories and assert they're still there after 10 simulated dream cycles
- Add a metric to the dream audit log summary printed at end of cycle: `"forgetting: removed N (cap=5), refused M (reasons: ...)"`

-----

## Implementation Order

```
Step 1: Importance rewire — schema + calibrated prompt + usage tracking
        (gives us the recall_count + last_recalled_at signal we need for everything else)

Step 2: Tomorrow's preload — schema + agent + layer + dream-cycle Step 3
        (independent of forgetting, but easier to validate once usage tracking is in)

Step 3: Conservative forgetting — code guard + prompt update + quota config
        (depends on Step 1's last_recalled_at column)
```

Steps 1 and 2 are independent and can be parallelized if convenient. Step 3 depends on Step 1.

Each step ships behind a config flag (`recall.use_blended_score`, `dream.tomorrow_preload.enabled`, `dream.forgetting.enabled`) so they can be enabled one at a time in production and rolled back if something feels off.

-----

## Files Likely to Change

```
New:
  migrations/000016_add_memory_usage_tracking.up.sql
  migrations/000017_add_tomorrow_preload.up.sql
  memory/store_tomorrow_preload.go
  persona/tomorrow_preload.go
  persona/tomorrow_preload_prompt.md
  layers/chat_tomorrow_preload.go
  sims/importance-rewire.yaml
  sims/tomorrow-preload.yaml
  sims/forgetting-policy.yaml

Modified:
  memory/store_facts.go            — blended retrieval, MarkMemoriesRecalled
  memory/store.go                  — interface additions
  memory_agent_prompt.md           — calibrated importance anchors
  persona/dreamer.go               — add Step 3 (preload)
  persona/memory_dreamer.go        — canForget guard, recalibration pass
  persona/memory_dreamer_prompt.md — forgetting policy section, importance recalibration
  tools/reply/handler.go           — mark recalled on reply, consume preload
  layers/chat_memory.go            — mark recalled on inject
  layers/chat_self_memory.go       — mark recalled on inject
  config/config.go                 — three new config blocks
  config.yaml.example              — document the new options
  cmd/run.go                       — wire preload agent client (mirrors dreamAgentClient setup)
  docs/ARCHITECTURE.md             — update the "Journey of a Message" diagram
```

-----

## Out of Scope (Follow-up Plans)

These came up in the conversation but aren't part of this plan:

- **Cross-encoder reranking** — already a "Future" item in `PLAN-zettelkasten-memory.md`
- **Cognitive memory typing** — add `kind` ∈ {episodic, semantic, procedural, social} to the memories table. Probably its own plan, ~200 lines of work
- **Graph traversal beyond 1-hop** — current zettelkasten does 1-hop; multi-hop with falloff is a separate research question
- **Voice-first/ambient mode** — Telegram is text-first; "Samantha-in-your-ear" would be a separate program

-----

## References

- **Livia** — Xi, R., & Wang, X. (2025). *Livia: An Emotion-Aware AR Companion Powered by Modular AI Agents and Progressive Memory Compression.* arXiv:2509.05298. <https://arxiv.org/abs/2509.05298> — Read for TBC (Temporal Binary Compression) and especially **DIMF (Dynamic Importance Memory Filter)**, which is the formal version of what Feature 2 does.
- **Generative Agents** — Park, J. S., O'Brien, J., Cai, C. J., Morris, M. R., Liang, P., & Bernstein, M. S. (2023). arXiv:2304.03442. The memory stream + reflection + retrieval architecture; weighting by recency × relevance × importance comes from this paper's ablation.
- **MaRS** — Alqithami, S. (2025). *Forgetful but Faithful: A Cognitive Memory Architecture and Benchmark for Privacy-Aware Generative Agents.* arXiv:2512.12856. Source for the named forgetting policies (FIFO / LRU / Priority Decay / Reflection-Summary). Feature 3 is essentially Priority Decay + identity-protected allowlist.
- **MemGPT/Letta** — <https://docs.letta.com> — Self-editing memory tiers and the "sleep-time compute" paradigm that maps onto Mira's dream cycle. Useful background for why moving work to idle time matters.
- **LinkedIn Cognitive Memory Agent** — InfoQ writeup, April 2026 — Temporal pivot summaries for evolving user state. Mira already does this via `SupersedeMemory()`; cited here so a future reader can see we're in good company.
- **Mem0** — Chhikara, P. et al. (2025). arXiv:2504.19413. Graph memory + LOCOMO benchmark. If Mira ever needs an objective number to compare against, LOCOMO is the standard.

-----

## Acceptance Criteria

This plan is done when:

- [ ] All three feature flags default to `enabled: true` in `config.yaml.example`
- [ ] Sims pass: `importance-rewire`, `tomorrow-preload`, `forgetting-policy`
- [ ] After 7 days of real use: top 20 memories by `recall_count` are intuitively the ones Mira refers to most often
- [ ] After 7 days of real use: at least one `tomorrow_preload` row shows up in the chat prompt and references a real signal from the prior day
- [ ] After 7 days of real use: `forgetting.refused` count > `forgetting.removed` count (the policy is biased toward keeping)
- [ ] Zero identity-class memories removed across 30 days of simulated dream cycles

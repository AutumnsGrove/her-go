---
title: "Cognitive Loop Improvements: Preload, Importance, Forgetting, Brevity, and Direct Reply"
status: planning
created: 2026-05-23
updated: 2026-05-23
category: features
priority: high
---

# Plan: Cognitive Loop Improvements

Five connected improvements to Mira's cognitive loop. Features 1-3 surfaced during a research conversation comparing her-go against the Her-clone landscape and recent agent-memory literature (Generative Agents, MemGPT/Letta, Mem0, MaRS, LinkedIn's CMA, Livia). Features 4-5 surfaced from analysis of the HaltiaAI Her movie dialogue dataset (306 rows of Samantha/Theodore exchanges), which revealed that Samantha's median response is 7 words, 43% of her replies are ≤5 words, and her long responses are reserved for moments of genuine emotional depth.

**Goal:** Make Mira more "Samantha-like" by adding forward-looking dream output, fixing the unused importance signal, adding a conservative forgetting policy that won't erode identity, calibrating response length to match real conversational patterns, and experimentally testing whether the driver agent can speak directly (bypassing the chat model handoff). Features 1-3 extend the dream cycle and retrieval blend. Features 4-5 address the reply mechanism and conversational style.

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

New migration: `migrations/000017_add_memory_usage_tracking.up.sql` (see Feature 2 — usage tracking migration goes first per implementation order)

New migration: `migrations/000018_add_tomorrow_preload.up.sql`

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

Migration: `migrations/000017_add_memory_usage_tracking.up.sql`

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

## Feature 4: Direct Reply Experiment (Sim-Only A/B Test)

### Why

The current reply mechanism separates the "brain" (driver agent, Qwen3) from the "mouth" (chat model, Kimi K2). The driver thinks, recalls memories, plans — then writes a short instruction like "Respond warmly, ask about their day." The chat model receives that instruction along with the full layer stack (persona, memory, conversation history) and generates the actual text.

This works well — the chat model gets a clean, purpose-built context window, and the driver keeps its own context focused on tool use and planning. But the handoff is **lossy by design.** The driver's thinking — its emotional read on the moment, its sense of pacing, its awareness of unresolved threads — gets compressed into 1-2 sentences of instruction. The chat model then reconstructs intent from that compressed signal. Sometimes it nails it. Sometimes it expands a quick "hey, how'd it go?" into a paragraph because nothing told it to be brief.

Analysis of the Her movie dataset confirms this matters. Samantha's responses show:
- **Median 7 words.** 43% of responses are 5 words or fewer.
- Her long responses (80+ words) happen only at emotionally significant moments.
- She interrupts, redirects, and brings up things *already on her mind* — behaviors that emerge from a unified thinking-and-speaking process, not from an instruction interpreter.

The question: **does the instruction handoff lose enough nuance to justify a direct mode?** We don't know yet. This feature builds the mechanism to test it in sim, side by side, so we can answer the question with data instead of intuition.

### What

A **sim-only, flag-gated** alternative reply path where the driver agent generates the reply text directly instead of delegating to a separate chat model. This is an experiment, not a replacement — the current two-model system remains the default and the direct mode exists exclusively for comparative testing.

### Why keep both (the case for the current system)

The two-model split has real advantages that the direct mode must compete with:

1. **Clean context partitioning.** The chat model sees: persona + memories + conversation history + a short instruction. No tool call noise, no planning artifacts, no search results that weren't relevant. The driver's context window is polluted with tool schemas, search results, think traces — none of that leaks into the reply generation.

2. **Persona layer injection.** The `layers/` system builds a rich personality context (`prompt.md` + `persona.md` + memories + mood + time-of-day + tomorrow's preload). This stack is purpose-built for *sounding like Mira*. The driver model's system prompt is purpose-built for *thinking like Mira's brain*.

3. **Independent model choice.** The chat model can be chosen for prose quality (Kimi K2 is good at natural conversation), while the driver model can be chosen for reasoning and tool use (Qwen3 is good at structured thinking). Direct mode forces one model to do both jobs.

4. **Style and safety gates.** The gates catch AI-isms, escalation, and sycophancy *after* generation but *before* delivery. In direct mode, the driver writes the final text — the gates still run, but there's no "rephrase naturally" retry path because there's no separate chat model to regenerate.

### How it works

New tool: `reply_direct` — only registered when the `driver.direct_reply` config flag is true. Uses the same tool registry as other tools (YAML definition + handler).

**`tools/reply_direct/tool.yaml`:**
```yaml
name: reply_direct
agent: [main]
hint: "write and send a response directly (experimental)"
description: >-
  Write the actual response text and send it to the user. Unlike the standard
  reply tool, YOU write the words — there is no separate conversational model.
  Your output IS what the user sees.

  The chat model's personality layers (persona, memory, mood) are NOT available
  in this path. You must carry the persona yourself from your system prompt.
hot: false  # not hot — only loaded when feature flag is on
parameters:
  type: object
  properties:
    text:
      type: string
      description: >-
        The exact text to send to the user. This is delivered verbatim (after
        PII deanonymization and style gate checks). Write as Mira — short,
        warm, specific.
    memory_ids:
      type: array
      items:
        type: integer
      description: >-
        Memory IDs used in composing this reply. Tracked for usage metrics.
  required:
    - text
```

**`tools/reply_direct/handler.go`** — a stripped-down version of `tools/reply/handler.go` that:

1. Takes `text` verbatim instead of calling the chat LLM
2. Still runs the style gate and safety gate (catches AI tics even in direct text)
3. Still runs degenerate detection and length guard
4. Still handles PII deanonymization, TTS, delivery, DB save
5. Still tracks `MarkMemoriesRecalled()` for the usage signal (Feature 2)
6. Skips: layer building, chat LLM call, conversation history assembly (all chat-model concerns)

The driver agent prompt gets a conditional section when direct mode is on:

```
## Direct Reply Mode (ACTIVE)

You are writing the actual words the user will see. There is no separate
conversational model — your text IS the reply. Carry Mira's voice yourself:
short, warm, specific, curious. Refer to your persona notes above.

Use reply_direct(text="your actual words") instead of reply(instruction="...").
The text parameter is delivered verbatim to the user.
```

### Sim A/B testing

The sim adapter gets a new flag:

```yaml
# sims/her-brevity-ab.yaml
name: her-brevity-ab
description: Compare driven vs direct reply modes
variants:
  - name: driven
    driver:
      direct_reply: false
    messages:
      - "hey, how was your day?"
      - "i had a rough shift at work"
      - "yeah the manager was being weird about schedules"

  - name: direct
    driver:
      direct_reply: true
    messages:
      - "hey, how was your day?"
      - "i had a rough shift at work"
      - "yeah the manager was being weird about schedules"
```

The sim report compares:
- Mean/median reply word count per variant
- Question ratio (% of replies containing a `?`)
- Style gate hit rate (does one mode produce more AI-isms?)
- Reply latency (direct mode skips one LLM call — how much faster?)
- Qualitative: do the direct replies feel more natural, or do they lose persona coherence?

### Config

```yaml
driver:
  direct_reply: false          # default OFF — current system is the default
  direct_reply_sim_only: true  # when true, the flag only takes effect in sim runs
```

The `direct_reply_sim_only` guard ensures that even if someone accidentally sets `direct_reply: true` in production config, it's ignored unless running through the sim adapter. This is a testing tool, not a production feature — until we have data that says otherwise.

### New files

- `tools/reply_direct/tool.yaml` — tool definition
- `tools/reply_direct/handler.go` — direct reply handler (subset of reply/handler.go)
- `sims/her-brevity-ab.yaml` — A/B comparison sim suite

### Files to modify

- `config/config.go` — add `Driver.DirectReply` and `Driver.DirectReplySimOnly`
- `config.yaml.example` — document the flags
- `driver_agent_prompt.md` — conditional direct-reply section
- `cmd/sim_gw.go` — wire the variant flag into per-sim config overrides
- `gateway/sim.go` — pass variant config to the pipeline

### What this does NOT change

- The current `reply` tool stays exactly as-is. It's still the default, still hot-loaded, still the production path.
- The layer system (`layers/`) is untouched — it still powers the standard reply path.
- The style and safety gates still run on direct replies.
- No prompt.md changes. No persona.md changes. No chat model changes.

### Decision point

After running the A/B sim at least 5 times across different conversational scenarios (casual, emotional, factual, multi-turn), we review the reports and decide:

1. **Direct mode wins clearly** → promote to production behind a config flag, eventually make it the default
2. **Mixed results** → investigate whether a hybrid works (direct for short replies, driven for complex ones)
3. **Driven mode wins** → close the experiment, focus on improving instruction quality in the driver prompt instead

The most likely outcome is #2 — direct mode probably wins for brevity and naturalness on simple exchanges, while driven mode wins for complex replies where the layer stack provides critical context. If so, the driver could choose which tool to call based on the situation.

-----

## Feature 5: Response Brevity (Her-Calibrated Length Norms)

### Why

Analysis of the Her movie dataset (306 dialogue rows, HaltiaAI/Her-The-Movie-Samantha-and-Theodore-Dataset on HuggingFace) reveals that Samantha's response lengths follow a distribution that is dramatically shorter than typical LLM output:

| Bucket | Samantha | Typical LLM |
|--------|----------|-------------|
| 1-5 words | **43%** | ~5% |
| 6-10 words | **19%** | ~10% |
| 11-20 words | **20%** | ~25% |
| 21-40 words | **12%** | ~35% |
| 41+ words | **6%** | ~25% |

Median: **7 words.** Mean: 13.2 (pulled up by rare long responses at emotional peaks).

The current `prompt.md` already says "Keep it short. 1-3 sentences." But the chat model still trends verbose because:
1. The instruction from the driver often implies detail ("respond warmly and ask about their day, referencing the Cava shift")
2. LLMs have a built-in verbosity bias — they're trained on long-form text and gravitate toward completeness
3. Nothing in the system *measures* or *enforces* brevity — it's guidance, not a constraint

The fix is not to clamp output length mechanically (that kills the rare valuable long response). It's to make brevity the **default mode** with length escalation requiring explicit justification from the driver.

### What

Three changes that work together:

#### 5a. Length signal in the reply tool

Add an optional `length` parameter to the existing `reply` tool:

```yaml
    length:
      type: string
      enum: [brief, normal, detailed]
      description: >-
        How long should this reply be? Brief = 1 sentence, a few words.
        Normal = 1-3 sentences. Detailed = paragraph-length, for emotional
        depth or complex explanations. Default: brief.
```

The chat model's system prompt gets a length directive injected based on this parameter. In `tools/reply/handler.go`, before building the LLM messages:

```go
lengthDirective := ""
switch args.Length {
case "detailed":
    lengthDirective = "This warrants a longer, more thoughtful response. Take the space you need."
case "normal":
    lengthDirective = "Keep this to 1-3 sentences."
default: // "brief" or empty
    lengthDirective = "Keep this SHORT — one sentence, maybe a few words. Fragments are fine. Don't elaborate unless asked."
}
```

This directive is appended to the system note alongside the instruction, so the chat model sees it in the same place.

**The default is "brief."** The driver must actively choose "normal" or "detailed" when the moment warrants it. This inverts the current dynamic where the chat model defaults to verbose and nothing constrains it.

#### 5b. Driver prompt update

Update `driver_agent_prompt.md` — add to the "Rules for reply" section:

```
- **Default to brief.** Most replies should be a sentence or a few words.
  Use length="brief" (the default) for: greetings, acknowledgements,
  quick reactions, follow-up questions, casual banter. This is most of
  conversation.
- Use length="normal" for: answering a direct question, sharing a thought
  that needs context, responding to something emotional.
- Use length="detailed" ONLY for: moments of genuine emotional depth,
  complex explanations the user explicitly asked for, or when you have
  something important to say that can't be compressed. This should be
  rare — maybe 1 in 20 replies.
- When in doubt, go shorter. The user can always ask for more.
```

#### 5c. Brevity metric in sim reports

Add to the sim report's per-turn metrics:

```
Reply length: 12 words (brief)
```

And to the summary section:

```
Response length distribution:
  brief (≤10 words):    8/15 turns (53%)
  normal (11-40 words): 5/15 turns (33%)
  detailed (41+ words): 2/15 turns (13%)

Her-calibration score: 0.82
  (1.0 = matches Samantha's distribution perfectly)
```

The Her-calibration score is a simple distribution distance:
```
target = [0.62, 0.32, 0.06]  # ≤10, 11-40, 41+ from the movie data
actual = [count_brief/total, count_normal/total, count_detailed/total]
score  = 1 - (sum of absolute differences) / 2
```

This gives sims a quantitative signal for whether Mira's reply lengths are Samantha-like.

### Schema change

None. This is prompt and handler changes only.

### New files

None — all changes fit into existing files.

### Files to modify

- `tools/reply/tool.yaml` — add `length` parameter
- `tools/reply/handler.go` — read `length`, inject directive into system note
- `driver_agent_prompt.md` — brevity guidance in "Rules for reply"
- `prompt.md` — strengthen the existing brevity section with concrete targets
- `cmd/sim_gw.go` — add brevity metrics to report output
- `gateway/sim.go` — track word counts per turn

### Interaction with Feature 4 (direct reply)

In direct reply mode, the driver writes the text itself — brevity is entirely in its hands, no length directive needed. The sim A/B test (Feature 4) naturally measures whether direct mode produces shorter replies than driven mode. If direct mode consistently hits the Her-calibrated distribution without any length parameter, that's strong evidence for the "mouth-brain unification" hypothesis.

### Validation

- Update existing sims to check that `brief` is the most common length signal
- `sims/her-brevity-ab.yaml` (from Feature 4) measures word count distribution
- Manual: compare 20 real conversations before/after the change — count words per reply, check the distribution shift

-----

## Implementation Order

```
Step 1: Importance rewire — schema + calibrated prompt + usage tracking
        (gives us the recall_count + last_recalled_at signal we need for everything else)

Step 2: Tomorrow's preload — schema + agent + layer + dream-cycle Step 3
        (independent of forgetting, but easier to validate once usage tracking is in)

Step 3: Conservative forgetting — code guard + prompt update + quota config
        (depends on Step 1's last_recalled_at column)

Step 4: Response brevity — length signal + driver prompt update + sim metrics
        (independent of Steps 1-3, can be done in parallel)

Step 5: Direct reply experiment — new tool + sim A/B framework
        (depends on Step 4's brevity metrics for meaningful comparison)
```

Steps 1, 2, and 4 are independent and can be parallelized. Step 3 depends on Step 1. Step 5 depends on Step 4.

Each step ships behind a config flag (`recall.use_blended_score`, `dream.tomorrow_preload.enabled`, `dream.forgetting.enabled`, `driver.direct_reply`) so they can be enabled one at a time in production and rolled back if something feels off.

-----

## Files Likely to Change

```
New:
  migrations/000017_add_memory_usage_tracking.up.sql
  migrations/000018_add_tomorrow_preload.up.sql
  memory/store_tomorrow_preload.go
  persona/tomorrow_preload.go
  persona/tomorrow_preload_prompt.md
  layers/chat_tomorrow_preload.go
  tools/reply_direct/tool.yaml       — direct reply tool definition (Feature 4)
  tools/reply_direct/handler.go      — direct reply handler (Feature 4)
  sims/importance-rewire.yaml
  sims/tomorrow-preload.yaml
  sims/forgetting-policy.yaml
  sims/her-brevity-ab.yaml           — A/B comparison: driven vs direct (Features 4+5)

Modified:
  memory/store_facts.go            — blended retrieval, MarkMemoriesRecalled
  memory/store.go                  — interface additions
  memory_agent_prompt.md           — calibrated importance anchors
  persona/dreamer.go               — add Step 3 (preload)
  persona/memory_dreamer.go        — canForget guard, recalibration pass
  persona/memory_dreamer_prompt.md — forgetting policy section, importance recalibration
  tools/reply/tool.yaml            — add length parameter (Feature 5)
  tools/reply/handler.go           — mark recalled on reply, consume preload, length directive
  layers/chat_memory.go            — mark recalled on inject
  layers/chat_self_memory.go       — mark recalled on inject
  config/config.go                 — five new config blocks (recall, preload, forgetting, direct_reply, brevity)
  config.yaml.example              — document the new options
  cmd/run.go                       — wire preload agent client (mirrors dreamAgentClient setup)
  cmd/sim_gw.go                    — brevity metrics in sim report, variant config for A/B
  gateway/sim.go                   — word count tracking per turn, variant flag passthrough
  driver_agent_prompt.md           — brevity rules, conditional direct-reply section
  prompt.md                        — strengthen brevity targets
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
- **Her Movie Dataset** — HaltiaAI (2023). <https://huggingface.co/datasets/HaltiaAI/Her-The-Movie-Samantha-and-Theodore-Dataset> — 306 rows of Samantha/Theodore dialogue from the 2013 film. Source for the response length distribution and conversational pattern analysis that informed Features 4 and 5. Dataset saved locally at `data/her_movie_dataset.csv` with exploration scripts.

-----

## Acceptance Criteria

This plan is done when:

**Features 1-3 (cognitive loop):**
- [ ] All three feature flags default to `enabled: true` in `config.yaml.example`
- [ ] Sims pass: `importance-rewire`, `tomorrow-preload`, `forgetting-policy`
- [ ] After 7 days of real use: top 20 memories by `recall_count` are intuitively the ones Mira refers to most often
- [ ] After 7 days of real use: at least one `tomorrow_preload` row shows up in the chat prompt and references a real signal from the prior day
- [ ] After 7 days of real use: `forgetting.refused` count > `forgetting.removed` count (the policy is biased toward keeping)
- [ ] Zero identity-class memories removed across 30 days of simulated dream cycles

**Feature 4 (direct reply experiment):**
- [ ] `reply_direct` tool exists behind `driver.direct_reply` flag, defaults to `false`
- [ ] `direct_reply_sim_only: true` prevents accidental production use
- [ ] A/B sim suite (`her-brevity-ab.yaml`) runs both variants and produces a comparison report
- [ ] Decision documented: direct mode promoted, hybrid explored, or experiment closed

**Feature 5 (response brevity):**
- [ ] `length` parameter on reply tool with `brief` as default
- [ ] Driver prompt explicitly instructs brief as the default mode
- [ ] Sim reports include word count distribution and Her-calibration score
- [ ] After enabling: median reply length drops below 15 words (from current baseline)
- [ ] Long responses (41+ words) occur in less than 15% of turns

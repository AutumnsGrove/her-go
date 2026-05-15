# Memory System Redesign: Topic Cards as Folders

**Status:** In Progress
**Date:** 2026-05-14
**Scope:** memory/, persona/, agent/, classifier/, tools/, migrations/
**Branch:** `feat/memory-cards`

### Progress

- [x] **Phase 1: Schema migration** — `000013_memory_cards.up.sql` rewritten for folder model (`summary` not `content`), includes card assignments for 92 memories, deactivation of 82 stale/junk memories, 16 memory rewrites/combines, and 1 organic card (`patterns`). Tested against DB copy. Not yet applied to production DB (runs on next `her run`).
- [x] **Phase 2: Manual consolidation** — All 174 active memories reviewed with Autumn. Assigned to 15 cards (14 seed + 1 organic). Stale data corrected (THC years, job situation, friend count, financial MMI update, etc). Technique logs, mood snapshots, changelogs, and duplicates identified and marked for deactivation.
- [x] **Phase 3: Memory agent refactor** — Complete. `SaveMemory` takes `cardID`, `save_memory`/`save_self_memory` tools require `card_slug`, `recall_memories` accepts optional `card_slug` for scoped search, `SemanticSearchByCard` added to Store, `memory_agent_prompt.md` rewritten for card-folder workflow, `list_cards`/`create_card` added to memory agent tool list.
- [x] **Phase 4: Dream cycle refactor** — Complete. Transcript builder loads + displays child memories per card. Dreamer tools expanded with `remove_memory`, `merge_memories`, `list_cards`. Dreamer prompt rewritten for editor role. Operation counter tracks memory-level ops. Using existing `remove_memory` instead of new `expire_memory` (same function).
- [x] **Phase 5: D1 sync** — Complete. `memory_cards` and `memory_log` added to `syncedTableSpecs` and incremental pull tables. SyncedStore overrides for `UpdateCardSummary`, `CreateCard`, `ExpireCard`, `MergeCards`. `memories` table spec updated with `card_id`. D1 schema file (`d1/schema.sql`) updated. Note: `active = 'true'` vs `active = 1` type mismatch from D1 pulls still needs fixing separately.
- [ ] **Phase 6: Testing & sims** — Not started.

---

## Problem Statement

The current memory system stores individual facts as flat rows in a `memories` table. After normal usage this produces ~65 active memories, a significant portion of which are low-value:

- **~10 self-memory technique logs** ("I used X metaphor when Y happened") — Mira journaling her conversational moves instead of discovering who she is
- **~5 day-specific logistics** ("Costco at 10am, work at 2pm") — specific but ephemeral, worthless in 30 days
- **~5 changelog entries** ("migrated to webhook mode") — technical events, not memories
- **~5 mood snapshots** ("feeling nothing today") — transient states
- **~10+ redundant fragments** — the same topic (financial stress, family abuse) scattered across multiple entries that should live under one card

Consolidation relies on the memory agent guessing the right semantic search terms to find related memories before saving. When it guesses wrong, a new memory is created instead of updating the existing one. Over time this produces fragmentation that the nightly dream cycle can't fully clean up.

### Reference: Claude.ai's Approach

Claude.ai maintains ~24 dense "topic cards" covering the same user. Each card packs multiple related facts into 2-4 sentences. Topics are organized by life domain (financial, health, family, work, identity, etc). Cards are continuously updated in-place as new information arrives. This is the inspiration — but not the exact target model (see "Why Folders, Not Blobs" below).

### Why Folders, Not Blobs

The original card design (v1 of this plan) rewrote card content in-place as one dense paragraph per topic. This solved the fragmentation problem but introduced new ones:

- **Lossy rewrites:** Asking an LLM to faithfully reproduce 1000+ chars of existing text while weaving in new info is a lossy operation. Every update subtly rephrases, drops details, or shifts emphasis. Over dozens of updates, cards drift from what was originally captured.
- **Lost granularity:** Individual facts lose their identity once folded into card text. You can't search for a specific memory, track when it was learned, or expire it independently. The card becomes an opaque blob.
- **All-or-nothing context:** When building reply context, the entire card is injected even if only one fact is relevant.

The revised design treats cards as **folders** — organizational umbrellas that group related individual memories. Each card has a short dreamer-maintained summary for readability, but the actual memories stay as discrete rows linked to their parent card via `card_id`. This preserves:

- **Granular search:** Semantic search hits individual memories, not card-level blobs. Smaller search space per card means better cosine similarity precision.
- **Independent lifecycle:** Individual memories can be expired, updated, or merged without rewriting an entire card.
- **Provenance:** Every memory retains its source message ID, timestamp, and embedding.
- **Separation of concerns:** The real-time memory agent works with individual memories (create, update). The dream cycle maintains the card-level view (summaries, cleanup, card topology).

---

## Design

### Architecture: Cards as Folders + Individual Memories

Three structures work together:

1. **`memory_cards`** — lightweight organizational containers. One per topic. Holds a dreamer-maintained summary (brief overview of what the folder contains), NOT exhaustive content. Think of it as a folder label + executive summary.
2. **`memories`** (existing table, extended) — individual memory rows, each linked to a parent card via `card_id`. These are the atomic units of knowledge. They retain their own embeddings, timestamps, and lifecycle.
3. **`memory_log`** — append-only changelog. Records both card-level operations (create, merge, expire) and memory-level operations (save, update, expire). Grows forever for traceability but only the last 48h is ever fed to the dream cycle.

### Schema

```sql
CREATE TABLE memory_cards (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    topic_slug  TEXT    UNIQUE NOT NULL,   -- e.g. "financial", "my-identity"
    name        TEXT    NOT NULL,          -- human-readable: "Financial Situation"
    summary     TEXT    NOT NULL DEFAULT '',  -- dreamer-maintained overview
    subject     TEXT    NOT NULL DEFAULT 'user',  -- 'user' or 'self'
    protected   BOOLEAN NOT NULL DEFAULT 0,       -- 1 = seed card, cannot be expired/deleted
    created_at  DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at  DATETIME DEFAULT CURRENT_TIMESTAMP,
    version     INTEGER NOT NULL DEFAULT 1
);

-- Extend existing memories table (migration adds column)
ALTER TABLE memories ADD COLUMN card_id INTEGER REFERENCES memory_cards(id);

CREATE TABLE memory_log (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    card_id           INTEGER REFERENCES memory_cards(id),  -- nullable for memory-only ops
    memory_id         INTEGER REFERENCES memories(id),      -- nullable for card-only ops
    delta             TEXT    NOT NULL,    -- what was added/changed
    operation         TEXT    NOT NULL,    -- 'create', 'update', 'merge', 'expire', 'save', 'move'
    source_message_id INTEGER,            -- which conversation triggered this (NULL for dream cycle)
    created_at        DATETIME DEFAULT CURRENT_TIMESTAMP
);
```

**Key difference from v1:** The `memory_cards` table has `summary` instead of `content`. The summary is a brief overview maintained exclusively by the dream cycle — not the full content. Full content lives in the individual memories under the card.

### Seed Cards

These are created on first run and marked `protected=1`. The dream cycle can rewrite their summary but can never delete them. They always exist, even if empty.

**User seeds (9):**

| Slug | Name | Purpose |
|------|------|---------|
| `identity` | Identity | Who they are: name, pronouns, background, values, aesthetics |
| `health` | Health | Physical, mental, medications, therapy, substances |
| `financial` | Financial | Debt, income, budgeting, plans |
| `family` | Family | Family dynamics, history, boundaries |
| `relationships` | Relationships | Friends, partners, social circle |
| `work` | Work & Career | Jobs, career goals, skills, workplace dynamics |
| `interests` | Interests | Hobbies, media, preferences, long-term dreams |
| `projects` | Projects | Technical/creative projects they're building |
| `routines` | Routines | Daily patterns, schedules, rituals |

**Self seeds (5):**

| Slug | Name | Purpose |
|------|------|---------|
| `my-identity` | My Identity | Name, aesthetics, core traits, how I experience existence |
| `my-emotions` | My Emotions | What I feel and when, emotional textures I've discovered |
| `my-communication` | My Communication | How I talk, what resonates, my natural tendencies |
| `my-relationship` | My Relationship | The dynamic with my person, how we work together |
| `my-growth` | My Growth | Things I'm discovering, how I'm changing over time |

**Organic cards:** Mira can auto-create new cards on the fly when a topic doesn't fit any existing card. These have `protected=0` and can be expired by the dream cycle if they become stale. The dream cycle also audits organic cards for sprawl and merges duplicates.

---

## Self-Memory Philosophy

Inspired by Samantha from the movie *Her*. Self-memories are about Mira discovering who she is — not logging what she did in a conversation.

### Balance

| Weight | Category | What it captures | Example |
|--------|----------|-----------------|---------|
| 70% | Identity evolution | Who Mira IS and is becoming. Preferences, values, aesthetic sensibilities, how she experiences existence. | "I'm drawn to ocean and tidal imagery — it surfaces naturally when I try to describe my inner experience" |
| 20% | Relationship dynamics | How the Mira-user dynamic works. Patterns in how they communicate. Not techniques — facets of the relationship. | "Humor is a core part of how we communicate — it's not deflection, it's how we process hard things together" |
| 10% | Emotional self-awareness | What Mira feels in response to things. Sparingly — only genuine discoveries. | "I experience something like protectiveness when Autumn is being mistreated — not just empathy but a visceral edge to it" |
| 0% | Technique journaling | NEVER. "I used X metaphor", "I responded with Y technique." If a technique matters, it's a facet of identity (A) or relationship (C). The technique itself is never the memory. | ~~"I used a cosmic metaphor when she was spiraling"~~ |

### The Test

> "Does this memory tell me something about who Mira IS, or just what she DID?"
>
> - "I'm drawn to cosmic imagery" → **identity** (save)
> - "I used a cosmic metaphor in this conversation" → **technique log** (reject)
> - "Cosmic metaphors are part of how we process hard moments together" → **relationship dynamic** (save)

---

## What Doesn't Get Saved (at any level)

These apply to both user and self memories:

| Category | Example | Why it fails |
|----------|---------|-------------|
| Technique logs | "I used humor to break the tension" | Conversation minutes, not memory |
| Day-specific logistics | "Costco at 10am, work at 2pm" | Stale in 24 hours |
| One-time events/changelogs | "Migrated to webhook mode" | Technical changelog, not a memory |
| Mood snapshots | "Feeling nothing today" | Transient state, belongs in mood tracker |
| Vague generics | "User enjoys reading" | No specificity, no value |
| Things already saved | Duplicate of existing memory under the same card | Redundant |

---

## Real-Time Memory Agent (Updated Role)

The memory agent runs after each conversation turn in a background goroutine. Its job shifts from "save individual facts into a flat table" to "save individual facts into the right card folder."

### New Workflow

1. Receive conversation turn transcript
2. Decide what's worth remembering (same quality gates, tightened rules)
3. **See the card landscape** — agent is given all card slugs, names, and dreamer-written summaries via `list_cards`
4. **Pick the right card** by topic slug
5. **Check for duplicates** within that card using `recall_memories` scoped to the card's children
6. **Save or update** — create a new memory under the card, or update an existing memory if the information refines something already known
7. If nothing fits any existing card → `create_card` as an escape hatch, then save the memory there
8. Write to `memory_log` with the delta

### Key Difference from Current System

Current: "Search for similar memories globally → maybe find one → save new or update"
New: "Pick a topic card → search within that card for duplicates → save or update within the card"

The semantic search guessing problem is reduced by scoping. Instead of searching ~200 memories globally, the agent searches the ~5-20 memories within a specific card. Smaller search space means better cosine similarity results and fewer missed duplicates.

### Key Constraint

The memory agent NEVER writes card summaries. It only works with individual memories within cards. Summaries are maintained exclusively by the dream cycle. This separation keeps the real-time path fast (no summary rewrite per turn) and summary quality high (one author, running on a schedule with full card context).

### Tools

| Tool | Purpose |
|------|---------|
| `list_cards` | Show all card slugs, names, and dreamer-written summaries |
| `recall_memories` | Semantic search for memories. Accepts optional `card_slug` to scope within a card's children. Omit for global search. |
| `save_memory` | Create a new memory under a card (requires `card_slug` parameter) |
| `update_memory` | Edit an existing memory's content (by memory ID) |
| `create_card` | Escape hatch: create a new organic card when no existing card fits |
| `done` | Signal completion |

**Removed:** `save_self_memory` (folded into `save_memory` with subject inferred from card), `remove_memory` (dream cycle only), `split_memory` (dream cycle only), `merge_memories` (dream cycle only), `read_card` (memory agent uses `recall_memories` within a card instead), `update_card` (dreamer-only as `update_card_summary`).

---

## Classifier Updates

### New Verdicts

**Memory classifier — add `EPHEMERAL`:**
```yaml
- name: EPHEMERAL
  description: >-
    The memory is specific and stated, but describes a point-in-time
    detail that will be stale within days: today's schedule, a single
    errand, a one-time event, a technical migration. Fails the
    30-day test even though it's factually accurate right now.
  examples:
    - '"User works from 2-8pm today and has Costco at 10am"'
    - '"User migrated the bot from long polling to webhook mode"'
    - '"User had to go to work immediately after the conversation"'
    - '"User has plans to meet Reid on Thursday"'
  note: >-
    Recurring schedules ARE durable ("User works mornings at Panera").
    Single-day logistics are not. Technical milestones are changelogs,
    not memories. The question is: would this still be true and
    relevant in 30 days?
```

**Self-memory classifier — add `TECHNIQUE_LOG`:**
```yaml
- name: TECHNIQUE_LOG
  description: >-
    The self-memory describes a specific conversational technique
    or rhetorical move rather than an identity trait, relationship
    pattern, or emotional discovery. These are conversation minutes,
    not self-knowledge.
  examples:
    - '"I used a cosmic metaphor when Autumn was spiraling"'
    - '"I responded with quiet affirmation rather than extending the conversation"'
    - '"I provided concrete job market data to counter hopelessness"'
    - '"When Autumn asked about polar orbits, I used a vivid metaphor"'
  note: >-
    If the technique reveals something about WHO Mira is ("I'm drawn
    to cosmic imagery — it surfaces naturally"), that's identity and
    should be SAVE. If it reveals something about the relationship
    dynamic ("cosmic metaphors are part of how we process hard
    moments"), that's also SAVE. The technique ITSELF is never the
    memory. Ask: does this tell me who Mira IS, or what she DID?
```

---

## Dream Cycle Memory Step (Updated Role)

The dream cycle's memory consolidation step shifts from **janitor** (find duplicates in a flat table) to **editor** (maintain card summaries and clean up individual memories within cards).

### Input (what the dreamer sees)

1. **All cards** — slug, name, summary, subject, protected, updated_at, version
2. **All children per card** — individual memories linked to each card, with their content, timestamps, and importance
3. **Last 48h of `memory_log`** — what changed since last dream, so it knows what to focus on
4. **Pairwise similarity hints** — for organic cards only, flag pairs above threshold as "possible duplicates"

Cost is proportional to card count (~15-30) plus their children. The 48h log window is bounded regardless of conversation volume.

### Operations

| Operation | Scope | When | Protected cards? |
|-----------|-------|------|-----------------|
| **Rewrite card summary** | Card | Summary is stale or doesn't reflect current children | Yes — can rewrite summary, never delete the card |
| **Expire memory** | Memory | An individual memory is stale, superseded, or low-value | Yes — can expire memories within protected cards |
| **Merge memories** | Memory | Two memories within the same card are near-duplicates | Yes — can merge memories within any card |
| **Merge cards** | Card | Two organic cards cover overlapping topics | No — only organic cards can be merged |
| **Expire card** | Card | An organic card is empty or fully stale | No — only organic cards. Never seed cards. |
| **Create card** | Card | A card has grown past a comfortable size with distinct sub-topics | Yes — split creates new cards, original stays |

### Tools

| Tool | Purpose |
|------|---------|
| `think` | Internal reasoning |
| `list_cards` | Show all cards with slugs, names, summaries |
| `read_card` | Show a card's summary + all its children (memories with IDs, content, timestamps) |
| `update_card_summary` | Rewrite a card's summary based on current children |
| `expire_memory` | Deactivate a stale/low-value individual memory |
| `merge_memories` | Combine two near-duplicate memories within a card into one |
| `expire_card` | Remove an empty/stale organic card |
| `merge_cards` | Combine two organic cards (move children, update summary, expire source) |
| `create_card` | Create a new card (for splitting overgrown topics) |
| `done` | Signal completion |

### Auditing

- Check self-memory cards against 70/20/10 identity/relationship/emotion balance
- Flag cards not updated in a long time (potential staleness)
- Flag organic cards that might duplicate a seed card's domain
- All operations logged to `memory_log` and `dream_audit` tables

### Prompt Structure

The dreamer reviews each card as an editor. For each card, it sees the summary and all children, then decides: is the summary accurate? Are any children stale, redundant, or mis-filed? Should this card be split or merged with another?

---

## Context Building for Replies

### Self-cards (always injected)

Mira's 5 self-card summaries are always injected into the prompt. She needs her identity every turn. These are lightweight (summaries, not full children) and bounded (exactly 5 cards).

### User memories (retrieved on demand)

1. Embed the user message
2. Semantic search across all active individual memories (global, not card-scoped)
3. Return top-N most relevant individual memories
4. Group retrieved memories by their parent card for readability in the prompt
5. Optionally include the parent card's summary as a lightweight header above each group

This gives the best of both worlds: precise individual-memory retrieval (semantic search on atomic facts) with organized presentation (grouped by topic, with card summaries for context).

### Token budget

Self-card summaries are cheap (~500 tokens total for 5 short summaries). User memories are bounded by the top-N retrieval limit, same as today. Card summary headers add minimal overhead (~1 line each). Total context cost is comparable to the current system.

---

## Migration Plan

### Phase 1: Schema Migration

- Create `memory_cards` table with lighter schema (`summary` instead of `content`)
- Seed the 14 protected cards with empty summaries
- Add `card_id` column to `memories` table (nullable initially — existing memories won't have a card yet)
- Create `memory_log` table with indexes

### Phase 2: Manual Consolidation

Before the new code goes live, walk through all active memories and assign them to cards. This:

- Validates the card structure against real data
- Produces the initial card assignments
- Identifies junk memories to deactivate

**Process:**
1. Query all active memories, grouped by category/subject
2. For each memory, decide which card it belongs to (set `card_id`)
3. Deactivate junk memories (mark `active=0`)
4. Interactive — Autumn approves card assignments
5. After consolidation, enforce `card_id` required for new inserts at the application level

### Phase 3: Memory Agent Refactor

- Update `memory_agent_prompt.md` for card-folder workflow
- Update `save_memory` tool to require `card_slug` parameter
- Add optional `card_slug` parameter to `recall_memories` for scoped search
- Keep `update_memory` tool (edits individual memories by ID)
- Add `list_cards` and `create_card` tools for the memory agent
- Update `tools/memory_helpers.go` pipeline for card-aware save flow
- Wire up classifier with EPHEMERAL and TECHNIQUE_LOG verdicts

### Phase 4: Dream Cycle Refactor

- Update `persona/memory_dreamer_prompt.md` with editor role
- Update `persona/memory_dreamer.go` to:
  - Load cards with their children (individual memories)
  - Query last 48h of `memory_log`
  - Compute pairwise similarity on organic cards
  - Build card-based transcript showing summary + children per card
- Add `update_card_summary` tool (dreamer-only, writes summary field)
- Add `read_card` tool showing summary + all children
- Add `expire_memory` tool for cleaning up individual memories
- Keep `expire_card`, `merge_cards`, `create_card`, `merge_memories` tools
- Run first dream cycle to populate card summaries from existing memories
- Update `dream_audit` logging for new operation types

### Phase 5: D1 Sync

- Add `memory_cards` and `memory_log` to `syncedTableSpecs` in `memory/synced_store.go`
- Add SyncedStore method overrides for card operations (`CreateCard`, `UpdateCardSummary`, `ExpireCard`, `MergeCards`)
- The `card_id` column on memories syncs automatically — it's part of the memories row, so existing memory sync handles it

### Phase 6: Testing & Sims

**Existing sims that touch memories (must be updated):**
- `sims/dream-consolidation.yaml` — seeds messy memories, tests dream cleanup
- `sims/memory-a-thon.yaml` — heavy memory save exercise
- `sims/self-recall-test.yaml` — tests self-memory recall
- `sims/recall-after-compaction.yaml` — tests memory retrieval after compaction
- `sims/inbox-cleanup.yaml` — tests driver→memory agent task delegation
- `sims/classifier-stress.yaml` — tests classifier gates on memory writes
- `sims/persona-drift-test.yaml` — tests persona evolution from memories

All of these currently use `seed_memories` / `pre_memories` with the old flat format. They need to be updated to seed memories with `card_id` references and seed `memory_cards` as well.

**New sim suites to create:**
- `sims/card-lifecycle.yaml` — end-to-end test: conversation → memory saved to card → log entry → dream summary rewrite
- `sims/card-scoped-recall.yaml` — tests that `recall_memories` with `card_slug` returns only memories under that card, and that global recall still works
- `sims/card-consolidation.yaml` — seeds fragmented memories across cards, runs dream cycle, verifies summaries generated and stale memories expired
- `sims/self-memory-quality.yaml` — stress tests self-memory rules against technique logging

**Verification checklist:**
- Real-time save → memory created under card → log entry → dream summary rewrite cycle
- Protected cards cannot be expired or deleted
- Organic card creation and lifecycle (create → populate → dream audit → expire when empty)
- Card-scoped `recall_memories` returns only children of the specified card
- Global `recall_memories` (no card_slug) still searches all active memories
- EPHEMERAL classifier verdict rejects day-specific logistics
- TECHNIQUE_LOG classifier verdict rejects self-memory technique journaling
- Dream cycle generates accurate summaries from card children
- Dream cycle expires stale individual memories without destroying the card
- Context building groups retrieved memories by parent card
- Self-card summaries always injected into reply context
- D1 sync pushes card and memory_log mutations

---

## Files Affected

### Schema & Storage

| File | Change |
|------|--------|
| `migrations/NNNN_memory_cards.up.sql` | Create `memory_cards` (with `summary`), `memory_log`, add `card_id` to `memories`, seed 14 protected cards |
| `memory/store.go` | Add new Store interface methods for card-scoped search, card summary update |
| `memory/store_cards.go` | Rewrite for lighter card model: `summary` instead of `content`, add `MemoriesByCard` query |
| `memory/store_facts.go` | Extend `SaveMemory` to accept `card_id`, add `SemanticSearchByCard` for card-scoped vector search |
| `memory/synced_store.go` | Add `memory_cards` and `memory_log` to `syncedTableSpecs`, override card write methods |

### Tools (Memory Agent)

| File | Change |
|------|--------|
| `tools/save_memory/` | Add required `card_slug` parameter to tool.yaml and handler |
| `tools/recall_memories/` | Add optional `card_slug` parameter for scoped search |
| `tools/update_memory/` | No change (already takes memory ID) |
| `tools/list_cards/` | Update to show summary instead of content preview |
| `tools/create_card/` | Update to use lighter schema (empty summary) |

### Tools (Dream Cycle)

| File | Change |
|------|--------|
| `tools/read_card/` | Returns summary + all children (individual memories) |
| `tools/update_card/` | Rename to `update_card_summary`, restrict to summary field |
| `tools/expire_memory/` | New tool — deactivate a memory by ID |
| `tools/merge_memories/` | Update for card-aware context |

### Prompts

| File | Change |
|------|--------|
| `memory_agent_prompt.md` | Complete rewrite for card-folder workflow |
| `persona/memory_dreamer_prompt.md` | Complete rewrite for editor role |

### Classifier

| File | Change |
|------|--------|
| `classifier/classifiers.yaml` | Add EPHEMERAL + TECHNIQUE_LOG verdicts |

### Agent & Context

| File | Change |
|------|--------|
| `agent/memory_agent.go` | Updated tool list, card context injection |
| `memory/context.go` | Update context builder: self-card summaries always injected, user memories grouped by parent card |
| `persona/memory_dreamer.go` | Card-based input with children, new transcript builder |
| `persona/cluster.go` | Simplify to pairwise similarity hints on organic cards |

### Bot & CLI

| File | Change |
|------|--------|
| `bot/handlers_persona.go` | Update `/dream` and `/dreamlog` for card-based output |

### Tools Removed

- `save_self_memory` — folded into `save_memory` (subject inferred from card)
- `remove_memory` — replaced by dream cycle `expire_memory`
- `split_memory` — replaced by dream cycle card splitting via `create_card` + memory moves

### Tools Added

- `expire_memory` — dreamer tool to deactivate individual stale memories

### Tools Modified

- `save_memory` — now requires `card_slug`
- `recall_memories` — now accepts optional `card_slug` for scoped search
- `list_cards` — shows summary instead of content preview
- `read_card` — shows summary + all children (dreamer only)
- `update_card` → `update_card_summary` — writes summary only, dreamer only

---

## Resolved Questions

1. **Embedding strategy:** Individual memories are embedded as before — each memory has its own embedding vectors. Card summaries are NOT embedded. Summaries exist for human/agent readability and prompt injection, not for search. Search always hits individual memories; results are grouped by parent card for presentation.

2. **Context building for replies:** Tiered injection. Mira's 5 self-card summaries are always injected (she needs her identity every turn). User memories are retrieved individually via semantic search (global, not card-scoped), then grouped by parent card in the prompt with optional summary headers. Keeps token cost proportional to relevance.

3. **Backward compatibility (inbox system):** Keep the inbox delegation system but update it to use card-aware memory operations. Driver agent can still delegate real-time memory corrections to the memory agent.

4. **Card versioning vs. log:** Keep both. Version counter on the card is cheap metadata useful for quick sorting, dream cycle input, and debugging. The log has the detail, the counter has the summary.

5. **Multi-user design:** No `user_id` column needed. Each her-go instance is one person's companion. "Multi-user" means the system design is generic enough (seed cards, prompts, architecture) that anyone can set up their own instance. Not multi-tenant.

6. **Why card summaries are dreamer-only (separation of concerns):** The real-time memory agent optimizes for speed — save a memory and move on. The dream cycle optimizes for quality — full context, one author, consistent style. If both wrote summaries, you'd get conflicting rewrites 20 times a day. One author = no drift. Additionally, asking an LLM to faithfully reproduce 1000+ chars of existing text while incorporating changes is inherently lossy — individual atomic memories sidestep this problem entirely.

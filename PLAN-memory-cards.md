# Memory System Redesign: Topic Cards

**Status:** Planning
**Date:** 2026-05-13
**Scope:** memory/, persona/, agent/, classifier/, tools/, migrations/

---

## Problem Statement

The current memory system stores individual facts as flat rows in a `memories` table. After normal usage this produces ~200 active memories, roughly half of which are low-value:

- **~30 self-memory technique logs** ("I used X metaphor when Y happened") — Mira journaling her conversational moves instead of discovering who she is
- **~10 day-specific logistics** ("Costco at 10am, work at 2pm") — specific but ephemeral, worthless in 30 days
- **~7 changelog entries** ("migrated to webhook mode") — technical events, not memories
- **~7 mood snapshots** ("feeling nothing today") — transient states
- **~40+ redundant fragments** — the same topic (financial stress, family abuse, Costco pursuit) scattered across 5-12 entries that should be one dense card

Consolidation relies on the memory agent guessing the right semantic search terms to find related memories before saving. When it guesses wrong, a new memory is created instead of updating the existing one. Over time this produces fragmentation that the nightly dream cycle can't fully clean up.

### Reference: Claude.ai's Approach

Claude.ai maintains ~24 dense "topic cards" covering the same user. Each card packs multiple related facts into 2-4 sentences. Topics are organized by life domain (financial, health, family, work, identity, etc). Cards are continuously updated in-place as new information arrives. This is the target density and organization model.

---

## Design

### Architecture: Topic Cards + Append Log

Replace the flat `memories` table with two new structures:

1. **`memory_cards`** — source of truth. One dense row per topic. Updated in-place.
2. **`memory_log`** — append-only changelog. Every change recorded with timestamp and source. Grows forever for traceability but only the last 48h is ever fed to the dream cycle.

### Schema

```sql
CREATE TABLE memory_cards (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    topic_slug  TEXT    UNIQUE NOT NULL,   -- e.g. "financial", "my-identity"
    name        TEXT    NOT NULL,          -- human-readable: "Financial Situation"
    content     TEXT    NOT NULL,          -- dense card text, ~1000 char soft cap
    subject     TEXT    NOT NULL DEFAULT 'user',  -- 'user' or 'self'
    protected   BOOLEAN NOT NULL DEFAULT 0,       -- 1 = seed card, cannot be expired/deleted
    created_at  DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at  DATETIME DEFAULT CURRENT_TIMESTAMP,
    version     INTEGER NOT NULL DEFAULT 1
);

CREATE TABLE memory_log (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    card_id           INTEGER NOT NULL REFERENCES memory_cards(id),
    delta             TEXT    NOT NULL,    -- what was added/changed
    operation         TEXT    NOT NULL,    -- 'create', 'update', 'merge', 'expire', 'rewrite'
    source_message_id INTEGER,            -- which conversation triggered this (NULL for dream cycle)
    created_at        DATETIME DEFAULT CURRENT_TIMESTAMP
);
```

### Seed Cards

These are created on first run and marked `protected=1`. The dream cycle can rewrite their content but can never delete them. They always exist, even if empty.

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
| Things already on the card | Duplicate of existing card content | Redundant |

---

## Real-Time Memory Agent (Updated Role)

The memory agent runs after each conversation turn in a background goroutine. Its job changes from "save individual facts" to "update the right card."

### New Workflow

1. Receive conversation turn transcript
2. Decide what's worth remembering (same quality gates, tightened rules)
3. **Find the right card by topic slug** — deterministic lookup, not semantic search guessing
   - Agent is given the list of all card slugs + names
   - Picks the best match
   - If nothing fits → auto-create a new organic card
4. **Read the current card content** (pulled automatically when card is selected)
5. **Rewrite the card** to incorporate the new information
6. Write to `memory_log` with the delta (what was added/changed)

### Key Difference from Current System

Current: "Search for similar memories → maybe find one → save new or update"
New: "Pick a topic card → read it → rewrite it with new info"

The semantic search guessing problem is eliminated. The agent always knows what cards exist and picks one by name.

### Tools (Updated)

| Tool | Purpose |
|------|---------|
| `list_cards` | Show all card slugs + names + first ~100 chars of content |
| `read_card` | Read full content of a specific card |
| `update_card` | Rewrite a card's content to incorporate new info. Logs delta to memory_log |
| `create_card` | Create a new organic card (slug, name, initial content) |
| `done` | Signal completion |

Removed: `save_memory`, `save_self_memory`, `recall_memories`, `update_memory`, `remove_memory`, `split_memory`

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

The dream cycle's memory consolidation step shifts from **janitor** (find duplicates in 200 flat memories) to **editor + janitor** (maintain and improve ~30 topic cards).

### Input (what the dreamer sees)

1. **All cards** — full content + metadata (slug, subject, protected, updated_at, version)
2. **Last 48h of `memory_log`** — what changed since last dream, so it knows what to focus on
3. **Pairwise similarity hints** — for organic cards only, flag pairs above threshold as "possible duplicates"

Cost is proportional to card count (~30-40), not conversation history. The 48h log window is bounded regardless of conversation volume.

### Operations

| Operation | When | Protected cards? |
|-----------|------|-----------------|
| **Rewrite** | Card prose is loose, stale details remain, density can improve | Yes — can rewrite content, never delete the card |
| **Merge cards** | Two organic cards cover overlapping topics | No — only organic cards can be merged |
| **Split card** | A card has grown past ~1000 chars with genuinely distinct sub-topics | Yes — split creates new cards, original stays |
| **Expire card** | An organic card's topic is fully stale or absorbed elsewhere | No — only organic cards. Never seed cards. |
| **File fragments** | Recent log shows info that wasn't properly incorporated | Yes |

### Auditing

- Check self-memory cards against 70/20/10 identity/relationship/emotion balance
- Flag cards not updated in a long time (potential staleness)
- Flag organic cards that might duplicate a seed card's domain
- All operations logged to `dream_audit` table (existing)

### Prompt Structure (Updated)

The dreamer prompt shifts from "review clusters and lonely memories" to "review each card as an editor." The decision framework changes from merge/expire/promote/leave-alone to rewrite/merge-cards/split/expire/file-fragments.

---

## Migration Plan

### Phase 1: Manual Consolidation of Existing Memories

Before building any new code, consolidate the current ~200 memories into ~25-30 topic cards manually. This:
- Validates the card structure against real data
- Produces the initial card set that the new system will maintain
- Gives us a concrete baseline to test against

**Process:** Walk through all 200 active memories together, decide which card each belongs to, draft the consolidated card text.

### Phase 2: Schema Migration

- Create `memory_cards` and `memory_log` tables
- Seed the 14 protected cards (9 user + 5 self)
- Populate cards with content from Phase 1
- Keep old `memories` table intact (soft deprecation, not deletion)

### Phase 3: Memory Agent Refactor

- Update `memory_agent_prompt.md` with new card-based workflow
- Replace save_memory/save_self_memory/recall_memories tools with list_cards/read_card/update_card/create_card
- Update `tools/memory_helpers.go` pipeline for card-based flow
- Update classifier with EPHEMERAL and TECHNIQUE_LOG verdicts

### Phase 4: Dream Cycle Refactor

- Update `persona/memory_dreamer_prompt.md` with editor role
- Update `persona/memory_dreamer.go` to:
  - Load cards instead of flat memories
  - Query last 48h of memory_log
  - Compute pairwise similarity on organic cards
  - Build card-based transcript
- Replace merge_memories tool with card-level merge/rewrite/expire tools
- Update dream_audit logging for new operation types

### Phase 5: Testing & Sims

**Existing sims that touch memories (must be updated):**
- `sims/dream-consolidation.yaml` — seeds messy memories, tests dream cleanup
- `sims/memory-a-thon.yaml` — heavy memory save exercise
- `sims/self-recall-test.yaml` — tests self-memory recall
- `sims/recall-after-compaction.yaml` — tests memory retrieval after compaction
- `sims/inbox-cleanup.yaml` — tests driver→memory agent task delegation
- `sims/classifier-stress.yaml` — tests classifier gates on memory writes
- `sims/persona-drift-test.yaml` — tests persona evolution from memories

All of these currently use `seed_memories` / `pre_memories` with the old flat format. They need to be updated to seed `memory_cards` instead.

**New sim suites to create:**
- `sims/card-lifecycle.yaml` — end-to-end card test: conversation → card update → log entry → dream rewrite. Tests card creation, update, organic card auto-creation, and protected card immutability.
- `sims/card-consolidation.yaml` — seeds deliberately fragmented cards and recent log entries, runs dream cycle, verifies cards are tightened and organic duplicates are merged.
- `sims/self-memory-quality.yaml` — stress tests the self-memory rules: sends conversations that would trigger technique logging and verifies they are rejected. Sends conversations that should trigger identity/relationship/emotion saves and verifies they land in the right self cards.

**Verification checklist:**
- Real-time save → card update → log entry → dream rewrite cycle
- Protected cards cannot be expired or deleted
- Organic card creation and lifecycle (create → update → dream audit → expire)
- EPHEMERAL classifier verdict rejects day-specific logistics
- TECHNIQUE_LOG classifier verdict rejects self-memory technique journaling
- Card content stays under ~1000 char soft cap
- Dream cycle only sees last 48h of memory_log

---

## Files Affected

| File | Change |
|------|--------|
| `migrations/NNNN_memory_cards.up.sql` | New tables + seed cards |
| `memory/store.go` | New Store interface methods for cards + log |
| `memory/store_sqlite.go` | Card CRUD, log queries, 48h window query |
| `memory_agent_prompt.md` | Complete rewrite for card-based workflow |
| `agent/memory_agent.go` | Updated tool list, card context injection |
| `tools/memory_helpers.go` | Card-based save pipeline |
| `tools/list_cards/` | New tool |
| `tools/read_card/` | New tool |
| `tools/update_card/` | New tool |
| `tools/create_card/` | New tool |
| `classifier/classifiers.yaml` | Add EPHEMERAL + TECHNIQUE_LOG verdicts |
| `persona/memory_dreamer_prompt.md` | Complete rewrite for editor role |
| `persona/memory_dreamer.go` | Card-based input, new transcript builder |
| `persona/cluster.go` | Simplify to pairwise similarity hints |
| `sims/dream-consolidation.yaml` | Update for card-based operations |
| `sims/core-loop-and-dream.yaml` | Update for end-to-end card flow |
| `bot/handlers_persona.go` | Update /dream and /dreamlog for cards |

### Tools Removed
- `save_memory` — replaced by `update_card`
- `save_self_memory` — replaced by `update_card` (subject=self)
- `recall_memories` — no longer needed (deterministic card lookup)
- `update_memory` — replaced by `update_card`
- `remove_memory` — replaced by dream cycle card expiry
- `split_memory` — replaced by dream cycle card splitting
- `merge_memories` — replaced by dream cycle card merging

### Tools Added
- `list_cards` — show all cards with slugs, names, preview
- `read_card` — read full card content
- `update_card` — rewrite card content, log delta
- `create_card` — create new organic card

---

## Resolved Questions

1. **Embedding strategy for cards:** Chunk cards at the sentence level for embedding, linked back to the parent card. Search matches individual chunks for precision, but retrieval pulls the full card for context. Best of both: precise search, full context injection.

2. **Context building for replies:** Tiered injection. Mira's 5 self cards are always injected (she needs her identity every turn). User cards are retrieved on demand — embed the user message, find the top-N most relevant cards via chunk matching, inject those full cards. Keeps token cost proportional to relevance.

3. **Backward compatibility (inbox system):** Keep the inbox delegation system but update it to use card-level operations (update_card, merge cards, etc). Driver agent can still delegate real-time memory corrections to the memory agent.

4. **Card versioning vs. log:** Keep both. Version counter is cheap metadata useful for quick sorting, dream cycle input, and debugging. The log has the detail, the counter has the summary.

5. **Multi-user design:** No user_id column needed. Each her-go instance is one person's companion. "Multi-user" means the system design is generic enough (seed cards, prompts, architecture) that anyone can set up their own instance. Not multi-tenant.

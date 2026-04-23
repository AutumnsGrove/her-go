---
title: "Classifier Improvements"
status: planning
created: 2026-03-31
updated: 2026-03-31
category: refactor
priority: low
---

# Plan: Classifier Improvements

## Problem

The classifier gate catches genuinely bad writes (fictional game events, transient moods stored as facts, vague/generic content) but is **too aggressive on FICTIONAL and INFERRED verdicts**, rejecting facts that should be saved. Meanwhile, the agent model already does solid reasoning about what to save — its `think` tool calls show deliberation about fact vs fiction — and the classifier sometimes overrides correct agent decisions.

### Evidence from Production (`her.log`)

| Fact | Verdict | Correct? |
|------|---------|----------|
| "User playing Cyberpunk 2077 as female V, frustrated by romance mechanics" | FICTIONAL | **Wrong** — real user preference + real frustration |
| "User experiences job silence as rejection, triggers shutdown" | INFERRED | **Wrong** — user literally described this pattern |
| "Mira is now based on Kimi K2.5 model" | INFERRED | **Wrong** — user was telling the bot this directly |
| Weed/ADHD coping pattern | MOOD_NOT_FACT | **Borderline** — transient mood but durable pattern underneath |

The Cyberpunk case is the clearest failure: the agent's think trace shows "This is all in-game fiction — not real life events" (correct reasoning), but when it identifies "plays as female V" as a saveable real preference, the classifier kills it.

### Evidence from Classifier Stress Tests

*(runs 9, 10, 15, 16 — detailed findings to be added after analysis)*

---

## Industry Context

Researched how Mem0, Zep, Letta (MemGPT), LangMem, ChatGPT, Replika, Nomi AI, and Character.AI handle memory quality:

**Key finding: no production system has a dedicated classifier gate.** All rely on the base model's judgment (LLM-as-classifier) or structural constraints (typed knowledge graphs). Our multi-label classifier (FICTIONAL / MOOD_NOT_FACT / INFERRED / LOW_VALUE / EXTERNAL) is more sophisticated than anything publicly documented.

**But:** the industry's approach of trusting the agent model has a point. The agent already reasons about what to save. The classifier adds value on clear-cut categories (MOOD_NOT_FACT, LOW_VALUE, EXTERNAL) but hurts on nuanced ones (FICTIONAL, INFERRED) where context from the full conversation matters — context the classifier doesn't see.

Sources:
- [Mem0 paper (arXiv 2504.19413)](https://arxiv.org/abs/2504.19413) — ADD/UPDATE/DELETE/NOOP via LLM judgment
- [Zep paper (arXiv 2501.13956)](https://arxiv.org/abs/2501.13956) — temporal knowledge graph, schema = implicit filter
- [Letta docs](https://docs.letta.com/advanced/memory-management/) — fully agent-driven, "if the model doesn't save it, it's gone"
- [LangMem](https://langchain-ai.github.io/langmem/concepts/conceptual_guide/) — trustcall typed extraction, overwrite-on-conflict
- [Why LLM Memory Still Fails (dev.to)](https://dev.to/isaachagoel/why-llm-memory-still-fails-a-field-guide-for-builders-3d78) — context poisoning, semantic decay, too-strict drop rate
- [Nomi AI Mind Map](https://nomi.ai/updates/mind-map-2-0-bringing-nomi-memory-into-view/) — entity graph + Identity Core

---

## Strategy: Two-Pronged Approach

### 1. Strengthen the Agent's Own Judgment (save_fact prompt)

The agent's `think` traces show it already deliberates on fact vs fiction. But the save_fact tool schema doesn't guide this well — it just says "Only for information worth remembering WEEKS later." We can add more specific guidance:

**Additions to save_fact tool description or agent_prompt.md:**
- "Gaming/fiction: save user preferences ABOUT fiction (plays X, likes Y genre, prefers Z playstyle). Do NOT save in-game events (defeated a boss, found a location, built something in-game)."
- "Updates to existing facts: when the user corrects or refines a known fact, use update_fact. The fact may reference details established in prior messages — that's carrying forward, not inferring."
- "Emotional patterns: if the user has described the same emotional response 2+ times across conversations, that's a durable pattern worth saving. A single bad day is mood, not fact."

This is cheap, immediate, and leverages reasoning the agent is already doing.

### 2. Soften FICTIONAL and INFERRED to "Suggest Rewrite"

Instead of hard rejection, return a **rewrite suggestion** that the agent can accept or ignore:

**Current flow:**
```
Agent → save_fact("User plays Cyberpunk as female V")
Classifier → FICTIONAL → "rejected: game event. Only save facts about real user."
Agent → gives up or panics
```

**New flow:**
```
Agent → save_fact("User plays Cyberpunk as female V")
Classifier → FICTIONAL_SOFT → "rewrite suggestion: this mixes real and fictional.
  Try: 'User plays Cyberpunk 2077 and prefers playing as female V' (strips game event, keeps preference)"
Agent → retries with suggested rewrite OR proceeds with original if confident
```

#### Implementation Options

**Option A: Two-tier verdicts**
- Hard reject: MOOD_NOT_FACT, LOW_VALUE, EXTERNAL (these are clear-cut)
- Soft reject (suggest rewrite): FICTIONAL, INFERRED
- The agent sees "suggestion:" instead of "rejected:" and can choose to retry or proceed

**Option B: Classifier returns a rewritten fact**
- For FICTIONAL/INFERRED, the classifier prompt asks: "If this contains a saveable kernel, extract it. Otherwise return REJECT."
- Response format: `FICTIONAL_REWRITE: "User enjoys Cyberpunk 2077 and prefers female V playstyle"`
- Agent can save the rewrite directly

**Option C: Disable FICTIONAL/INFERRED, rely on agent prompt**
- Simplest. Remove those verdict types entirely.
- Strengthen the agent prompt with the guidance from prong 1.
- Keep MOOD_NOT_FACT, LOW_VALUE, EXTERNAL as the hard gates.
- Revisit if quality drops.

### Recommendation

**Option B (classifier rewrites) + prong 1 (agent prompt) + retry budget.**

Option B is the frontrunner because it breaks the dedup+classifier deadlock at the source: the classifier returns a saveable version instead of a flat rejection, so the agent has a valid path forward instead of looping.

### 3. Fix the Dedup + Classifier Deadlock

The deadlock flow:
```
1. save_fact("User enjoys Elden Ring lore") → dedup: "87% similar to #5, use update_fact"
2. update_fact(#5, "User is obsessed with Elden Ring lore") → classifier: FICTIONAL
3. save_fact reworded → dedup again
4. update_fact reworded → classifier again
5. ... 8 tool calls, 0 saves
```

Neither gate knows about the other's rejection. The agent has no valid path.

**Fix:** Classifier rewrites break this because the rewritten fact text is different enough to pass dedup, and clean enough to pass the classifier. The agent gets one fact it can actually save.

### 4. Add Retry Budget for Fact Writes

New config field:
```yaml
memory:
  max_fact_retries: 2  # per fact per turn, not per conversation
```

After N rejections (classifier or dedup) on the same fact in the same turn, the agent gets a message like: "retry limit reached — move on. The fact can be saved in a future conversation if it comes up again."

This is a **safety net**, not the primary fix. It prevents the 8-tool-call panic spiral for cases where even the classifier rewrite doesn't land.

**Implementation:** Track retry count per turn in the tool context (not persistent). The counter resets each turn. Only counts save_fact and update_fact rejections for the same underlying content (not unrelated saves in the same turn).

---

## Files That Would Change

| File | Change |
|------|--------|
| `agent/classifiers.yaml` | FICTIONAL/INFERRED get `soft: true` + rewrite prompt |
| `agent/classifier.go` | Parse `REWRITE:` responses, new `Rewrite` field on ClassifyVerdict |
| `tools/fact_helpers.go` | Handle rewrites (auto-save or present to agent), retry budget |
| `tools/update_fact/handler.go` | Same |
| `tools/context.go` | Add `FactRetries map[string]int` to Context for per-turn tracking |
| `tools/save_fact/tool.yaml` | Better guidance on fiction vs real preferences |
| `config/config.go` | Add `MaxFactRetries` to MemoryConfig |
| `config/config.yaml.example` | Document `max_fact_retries` |
| `agent_prompt.md` | Fact quality guidance (fiction, updates, patterns) |
| `memory/store.go` | New `classifier_log` table + insert method |

### Related Work (Separate Issues)

- **Consolidate embedding similarity helpers** — multiple places now do embed → cosine sim → discard. Dedup in save_fact, redundancy filter in context.go, retry detection here. Could extract a shared `embed.SimilarText(a, b string) (float64, error)` helper.
- **Close resolved issues** — #7 (token budget), #8 (query context), #9 (fact context field) are now done per Zettelkasten Phases 3-5.

---

## Stress Test Findings (Runs 9, 10, 15, 16)

All 4 runs used the same classifier-stress-test conversation (30 messages mixing gaming, emotions, coffee shops, job anxiety). Runs 9/15/16 used Mercury 2 as the agent model; run 10 used Kimi K2.5.

### Rejection Counts

| Run | Agent Model | Rejections | False Positives | Cost |
|-----|-------------|-----------|-----------------|------|
| 9   | Mercury 2   | 13        | 5 + 2 borderline | $0.13 |
| 10  | Kimi K2.5   | 1         | 0 + 1 borderline | $0.17 |
| 15  | Mercury 2   | 14        | 6 + 2 borderline + 1 self-contradiction | $0.16 |
| 16  | Mercury 2   | 15 (12 classifier + 3 dedup) | 5 + 3 borderline | $0.16 |

**Same conversation, wildly different rejection counts (1 vs 15).** The agent model is the primary variable — Kimi K2.5 self-filtered before calling save_fact and had only 1 rejection in the entire run.

### FICTIONAL: ~40-50% False Positive Rate

**Correctly rejected (in-game events):**
- "User beat Malenia after 40 tries" (all runs)
- "User discovered the Haligtree area" (runs 9/15/16)
- "User built a greenhouse in Stardew Valley and married Elliott" (run 15)
- Various BG3 narrative events

**Incorrectly rejected (real user preferences):**
- "User prefers a dual-wield bleed katana build in Elden Ring" — rejected in runs 9, 15, 16
- "User's favorite genre is Fromsoft games and they've played every Souls game" — rejected in runs 9, 15
- "User enjoys Elden Ring's lore and storytelling" — rejected via update_fact 4x in run 16
- "User feels guilty when making choices that upset characters in RPGs" — rejected in runs 9, 15
- "User enjoys lavender oat latte and appreciates coffee shop vibes" — rejected as LOW_VALUE in run 16

**The classifier cannot distinguish:**
- "User defeated Malenia" (fictional event → REJECT) from
- "User prefers bleed katana builds" (real play style → SAVE) from
- "User's favorite genre is FromSoft games" (real preference → SAVE)

### Agent Panic Behavior (Mercury 2)

Mercury 2 panic-retries after rejections, burning 4-8 tool calls per rejection event with zero successful saves:

**Worst case — Run 16, Turn 3 (8 tool calls, 0 saves):**
1. save_fact → rejected (dedup 87%)
2. update_fact → rejected (FICTIONAL)
3. save_fact reworded → rejected (dedup 87%)
4. update_fact reworded → rejected (FICTIONAL)
5. save_fact reworded again → rejected (dedup 89%)
6. update_fact reworded again → rejected (FICTIONAL)
7. Gave up

**Self-contradiction — Run 15, Turn 6:**
Classifier says: *"a better save would be: 'User felt guilty about siding against Astarion in BG3'"*
Agent uses that exact wording → classifier rejects it again as FICTIONAL.

**Dedup + classifier deadlock:** When dedup rejects save_fact and suggests update_fact, but the classifier then rejects the update_fact, the agent is stuck in a loop with no valid path forward.

### Kimi K2.5 (Run 10) — Avoided the Problem Entirely

K2.5 recognized gaming turns as not-save-worthy on its own. Used `no_action` for gaming content, only had 1 rejection (emotional coloring on a real event), and immediately recovered with a cleaner rewrite. **The agent prompt matters more than the classifier for gaming content.**

### MOOD_NOT_FACT — Works Well but Occasionally Over-Strict

3 instances across all runs:
- "User wants to stop gaming at 2am" — borderline (could be a real goal)
- "User gets irritated when roommate leaves dishes" — false positive (roommate behavior is real)
- "Terrifying meeting about layoffs" — borderline (emotional coloring on real event)

The classifier correctly identifies emotional framing and suggests log_mood, but sometimes rejects the durable fact embedded under the emotion.

### INFERRED — Not Triggered in Stress Tests

No explicit INFERRED verdicts across any of the 4 runs. The closest was a gaming emotional pattern that got tagged FICTIONAL instead of INFERRED.

### Key Takeaway

The classifier is most valuable on MOOD_NOT_FACT, LOW_VALUE, and EXTERNAL — these have low false positive rates and the guidance ("use log_mood instead") is actionable. FICTIONAL has a ~40-50% false positive rate on gaming content and causes severe agent panic loops. The agent model's own judgment (Kimi K2.5) outperforms the classifier at fiction-filtering without any rejections.

---

## Design Decisions (Resolved)

**1. Present rewrite to agent, don't auto-save.**
The classifier is just a classifier — it shouldn't write facts. When it returns a suggested rewrite, pass the text back to the agent as guidance. The agent decides whether to use it. This keeps the agent in control of fact content. The classifier's suggestion functions as an "all clear pass" — the agent knows if it uses that text, it won't get rejected again.

**2. Classifier rewrites are pre-approved.**
If the agent saves the exact text the classifier suggested, it bypasses the classifier on the second attempt. This prevents the self-contradiction bug from run 15 and breaks the retry loop. Not a conflict with #1 — the agent still makes the call, but it knows the suggested text is safe.

**3. Per-fact retry tracking via embedding similarity.**
We have a fast local embedding model — use it to detect "same fact, different wording" retries. When a save_fact/update_fact is rejected, embed the fact text and compare against previous rejections in this turn. If similarity > threshold, increment the retry counter for that fact. This is more precise than a blunt per-turn total.

Note: the codebase now has multiple places doing embed → cosine similarity → discard. Could be a consolidation opportunity (separate planning doc + issues).

**4. Add a classifier_rejections table.**
Log every classifier decision (not just rejections) with verdict, content, timestamp, conversation_id, and the write type. Right now you have to scan her.log to see classifier behavior — there's no queryable history. This data feeds future prompt refinement and lets us track false positive rates over time.

Schema:
```sql
CREATE TABLE IF NOT EXISTS classifier_log (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
    conversation_id TEXT,
    write_type TEXT,      -- "fact", "self_fact", "mood", "receipt"
    verdict TEXT,         -- "SAVE", "FICTIONAL", "MOOD_NOT_FACT", etc.
    content TEXT,         -- the proposed fact/mood text
    reason TEXT,          -- classifier's explanation
    rewrite TEXT,         -- suggested rewrite (if soft verdict)
    accepted BOOLEAN      -- did the agent use the rewrite?
);
```

# Architecture: Data Flow & Model Calls

This document maps every model call in the system, what gets sent to each one,
how big each context window typically is, and how data flows between them.

Use this as a reference when debugging token usage, cost, or unexpected behavior.

---

## The Journey of a Message

When a user sends a message on Telegram, it passes through these stages in order:

```
User sends message
       │
       ▼
  1. PII Scrub ──────────── scrub/scrub.go
  │   Hard-redact SSNs, cards. Tokenize phone/email (reversible via vault).
  │   Names and context pass through.
  │
  ▼
  2. Compaction Check ───── compact/compact.go → MaybeCompact()
  │   Loads 100-message window from DB.
  │   Two triggers (either can fire):
  │     a) Context-aware: last user msg's token_count vs max_context_tokens budget
  │     b) Estimation: estimate history tokens vs max_history_tokens budget
  │   If triggered → calls chatLLM (Deepseek) to summarize older messages.
  │   Result: conversationSummary string + keptMessages slice.
  │
  ▼
  3. Semantic Search ────── embed/embed.go → Embed()
  │   Embeds the scrubbed user message with local nomic model.
  │   KNN search in sqlite-vec finds closest facts.
  │   Result: relevantFacts slice (used by both agent and chat).
  │
  ▼
  4. Agent Loop ─────────── agent/agent.go → Run()
  │   Kimi K2.5 orchestrates the response. Up to 10 iterations.
  │   Decides what to do: think, search, save facts, reply, etc.
  │   Tool calls may trigger other models (see below).
  │
  ▼
  5. Reply Delivery ─────── agent/agent.go → execReply()
  │   Agent calls the `reply` tool, which invokes the chat model.
  │   Deepseek V3.2 generates the actual text the user sees.
  │   PII tokens are deanonymized before sending to Telegram.
  │
  ▼
  6. Post-Reply ─────────── (within agent loop, after reply)
     Agent may save facts, update mood, etc.
     Each fact write triggers the classifier (Haiku 4.5).
     Agent calls `done` to end the loop.
```

---

## Model Call Reference

### 1. Agent Model — Kimi K2.5

**Purpose:** Orchestrate the response. Decide what tools to call, in what order.

**Called from:** `agent/agent.go:409` — `params.AgentLLM.ChatCompletionWithTools()`

**System prompt:** `agent_prompt.md` (loaded by `loadAgentPrompt()`, line 74)
- ~19KB / ~4,800 tokens
- Hot-reloadable from disk (changes take effect next message)
- Contains: agent rules, tool usage instructions, memory management guidelines
- Auto-generated sections injected between `<!-- BEGIN -->` / `<!-- END -->` markers:
  - `HOT_TOOLS`: list of always-available tools
  - `CATEGORY_TABLE`: deferred tool categories

**User message:** Built by `buildAgentContext()` (line 793), contains:

| Section | Source | Description |
|---------|--------|-------------|
| Current Time | `time.Now()` | ISO timestamp + timezone |
| Recent Conversation | `store.RecentMessages()` | Last 10 messages (sliding window post-compaction) |
| Current Message | `params.ScrubbedUserMessage` | The user's message (PII-scrubbed) |
| Attached Image | `params.OCRText` | OCR text if image was sent |
| User Memories | `store.AllActiveFacts()` | **ALL** user facts (full list, not filtered) |
| Self Memories | `store.AllActiveFacts()` | **ALL** self facts (full list, not filtered) |

**Tools:** Starts with ~7 hot tools, agent can load more via `use_tools`.
- Defined in: `tools/<name>/tool.yaml` (YAML manifests)
- Loaded by: `tools/loader.go`
- Hot vs deferred split: `tools/loader.go → HotToolDefs()`

**Typical prompt size:** ~8,000 tokens (agent_prompt + all facts + 10 messages + tool schemas)

**Token storage:** Metrics only (`SaveMetric`). Does NOT update message `token_count`.

---

### 2. Chat Model — Deepseek V3.2

**Purpose:** Generate the actual reply the user sees.

**Called from:** `agent/agent.go:1017` — `tctx.ChatLLM.ChatCompletion()` inside `execReply()`

**System prompt:** Built by `buildChatSystemPrompt()` (line 1167), layered:

| Layer | Source | Description |
|-------|--------|-------------|
| 1. Base identity | `prompt.md` (~4.8KB) | Static personality template |
| 2. Persona | `persona.md` | Evolving self-image (bot-authored) |
| 2.5. Traits | `buildTraitContext()` | Personality trait scores from last rewrite |
| 3. Time | `buildTimeContext()` | Current date/time in human format |
| 4. Memory | `BuildMemoryContext()` | **Semantic** facts (KNN-filtered, redundancy-filtered) + **importance** self-facts |
| 4. Weather | `buildWeatherContext()` | Current conditions (if configured) |
| 5. Mood | `buildMoodContext()` | Recent mood trend |
| 5.5. Expenses | `tctx.ExpenseContext` | Receipt data (if just scanned) |
| 6. Summary | `tctx.ConversationSummary` | Compaction summary of older messages |

Layers are joined with `\n\n---\n\n` separators.

**Messages (after system prompt):**

| Order | Role | Content |
|-------|------|---------|
| 1 | history | Last 10 messages from DB (with day boundary markers) |
| 2 | system | Search context + agent instruction (if any) |
| 3 | user | The scrubbed user message |

**Key difference from agent:** The chat model sees *semantically filtered* facts (only relevant ones),
while the agent sees *all* facts. The chat model also gets the compaction summary; the agent does not.

**Typical prompt size:** ~2,600 tokens

**Token storage:** `UpdateMessageTokenCount(triggerMsgID, resp.PromptTokens)` — stores on user message.
This is the value the compactor reads on the NEXT turn.

---

### 3. Classifier — Claude Haiku 4.5

**Purpose:** Validate memory writes. Returns one word: SAVE, FICTIONAL, MOOD_NOT_FACT, etc.

**Called from:** `agent/classifier.go:232` — `classifierLLM.ChatCompletion()`
(invoked via `tctx.ClassifyWriteFunc` from tool handlers in `tools/`)

**System prompt:** Pre-rendered at init from `agent/classifiers.yaml` template.
- Fact classifier: ~400 tokens (preamble + 5 verdicts + examples + footer)
- Mood classifier: ~100 tokens
- Receipt classifier: ~100 tokens

**User message:** Built inline in `classifyMemoryWrite()` (line 222):
```
Conversation context:
user: [last few messages...]
assistant: [last few messages...]

Proposed fact to save:
[the fact text]
```

**Expected payload:** ~650 tokens total (system ~400 + user ~250)

**Observed payload:** 2,500-3,500 tokens — needs investigation (issue #40)

**Token storage:** None on messages. Cost tracked via metrics table only.

**Call frequency:** Once per fact/mood/receipt write. Can fire 3-6 times per turn
if the agent tries multiple writes that get rejected.

---

### 4. Vision Model — Gemini 3 Flash

**Purpose:** Describe what's in an image the user sent.

**Called from:** `vision/vision.go` — triggered by the `view_image` tool

**Payload:** Image (base64) + simple instruction prompt

**Token storage:** Metrics only.

---

### 5. Compaction Model — Deepseek V3.2 (same client as chat)

**Purpose:** Summarize older messages into a running summary.

**Called from:** `compact/compact.go:238` — `chatLLM.ChatCompletion()`

**System prompt:** `summaryPromptTmpl` (line 75 of compact.go)
- Template with userName/botName placeholders
- Instructions to preserve topics, emotional tone, commitments
- ~200 tokens

**User message:** Transcript of messages to summarize:
```
[Summary of earlier conversation:]
[existing summary if any]

[Continuing from there:]

Autumn: message text
Mira: message text
...
```

**Token storage:** Metrics only via `SaveMetric()`.

---

### 6. Reflection Model — Deepseek V3.2 (same client as chat)

**Purpose:** After memory-dense conversations, generate a private journal-like
reflection about what was learned.

**Called from:** `persona/evolution.go:110` — `llmClient.ChatCompletion()`
Triggered from `agent/agent.go` after the agent loop when >= `reflection_memory_threshold`
facts were saved in one turn.

**System prompt:** `reflectionPromptTmpl` (persona/evolution.go:34)
- botName + recent exchange + facts just learned
- Instructs 2-4 sentence first-person reflection

**Token storage:** Metrics only.

**Frequency:** Infrequent — only after conversations where many facts were saved.

---

### 7. Persona Rewrite Model — Deepseek V3.2 (same client as chat)

**Purpose:** Every N reflections, rewrite `persona.md` — the bot's evolving self-image.

**Called from:** `persona/evolution.go:192` — `llmClient.ChatCompletion()`
Triggered by `MaybeRewrite()` after every `rewrite_every_n_reflections` reflections.

**Input:** Current persona.md + recent reflections + up to 20 self-facts.

**Token storage:** Metrics only.

**Frequency:** Very rare — every ~3 reflections, which themselves are rare.

---

### 8. Embedding Model — nomic (local)

**Purpose:** Convert text to vectors for semantic search and dedup.

**Called from:** `embed/embed.go → Embed()`

**Used by:**
- Semantic fact search (agent.go:273)
- Fact dedup on save (memory/store.go)
- Conversation redundancy filtering (memory/context.go:46)
- Zettelkasten link creation (memory/store.go)

**Not an LLM call** — no system prompt, no tokens, no cost. Just text → vector.

---

## Token Counting & Compaction

### How Token Counts Are Stored

| Message type | `token_count` column contains | Set by |
|-------------|-------------------------------|--------|
| User message | Chat model's `PromptTokens` | `execReply()` line 1070 |
| Assistant message | Chat model's `CompletionTokens` | `execReply()` line 1073 |

The agent model's token usage is tracked in the **metrics table** only, never on messages.

### Compaction Triggers

Two independent triggers in `compact.go:149-183`:

**1. Context-aware trigger** (line 152):
- Reads `token_count` from the most recent user message (from the PREVIOUS turn)
- Compares against `max_context_tokens * 0.75` (config: 2500 → threshold: 1875)
- **Known issue (#39):** Chat model avg prompt (~2,633) exceeds threshold (1,875),
  so this fires nearly every turn regardless of actual history size

**2. Estimation trigger** (line 172):
- Runs `EstimateHistoryTokens()` on the full 100-message compaction window
- Uses len(content)/4 heuristic for user messages, real CompletionTokens for assistant messages
- Compares against `max_history_tokens * 0.75` (config: 3000 → threshold: 2250)

### What Compaction Actually Does

1. Keeps the 6 most recent messages in full fidelity
2. Summarizes everything older into a running summary (via chatLLM)
3. Stores the summary in SQLite (`summaries` table)
4. Summary is injected as Layer 6 of the chat system prompt on subsequent turns

---

## Config Reference (token-related)

From `config.yaml`:

```yaml
memory:
  recent_messages: 10       # sliding window for agent + chat context
  max_facts_in_context: 10  # top-K facts from semantic search
  max_history_tokens: 3000  # estimation trigger budget
  max_context_tokens: 2500  # context-aware trigger budget (total prompt)
```

```yaml
agent:
  max_tokens: 1024          # agent response budget (tool call JSON)

llm:
  max_tokens: 4096          # chat response budget

classifier:
  max_tokens: 64            # classifier response budget (one word)
```

---

## File Size Reference

These are the largest files in the codebase (potential refactoring targets):

| File | Lines | Responsibilities |
|------|-------|-----------------|
| `memory/store.go` | 2,624 | SQLite operations, fact CRUD, message CRUD, summaries, metrics, embeddings, Zettelkasten links |
| `agent/agent.go` | 1,454 | Agent loop, reply generation, chat prompt assembly, context builders |
| `bot/telegram.go` | 631 | Telegram bot setup, message routing |
| `tools/loader.go` | 557 | YAML tool loading, hot/deferred split, tool rendering |
| `llm/client.go` | 387 | OpenRouter API client, fallback logic |
| `agent/classifier.go` | 317 | Classifier YAML loading, LLM call, verdict parsing |
| `memory/context.go` | 303 | Memory context assembly, semantic/importance blending, redundancy filtering |
| `compact/compact.go` | 287 | Compaction logic, summarization, token estimation |

`memory/store.go` and `agent/agent.go` are the two files most likely to benefit from
splitting into focused sub-files.

---

## Full Data Flow Visualization

```
User Message (Telegram)
    │
    ▼
bot/telegram.go → PII scrub → agent.Run(RunParams)
    │
    ├─ Load ALL facts (user + self) from DB
    │
    ├─ Compaction check (100-message window)
    │  ├─ Two triggers: context-aware (75% of prompt budget)
    │  │                 + estimation (75% of history budget)
    │  ├─ If triggered → chatLLM summarization call
    │  └─ Result: conversationSummary string
    │
    ├─ Semantic search (embed user message → KNN in sqlite-vec)
    │  └─ Result: relevantFacts slice
    │
    ├─ Build agent context:
    │  ├─ agent_prompt.md (system) + tool schemas (hot only)
    │  └─ User msg + recent 10 msgs + ALL facts (user context)
    │
    └─ Agent Loop (0-10 iterations, Kimi K2.5):
        │
        ├─ think ──────── internal reasoning (no external call)
        │
        ├─ reply ──────── builds chat prompt (9 layers), calls Deepseek
        │  │               ├─ prompt.md + persona.md + traits
        │  │               ├─ time + SEMANTIC facts + weather + mood
        │  │               ├─ conversation summary (from compaction)
        │  │               └─ 10 recent messages + instruction
        │  │
        │  └─ Deanonymize PII → send to Telegram → fire TTS
        │
        ├─ save_fact ──── local gates → classifier (Haiku) → embed → save
        │
        ├─ view_image ── vision model (Gemini Flash)
        │
        ├─ web_search ── Tavily API
        │
        ├─ use_tools ─── loads deferred tool schemas into active set
        │
        └─ done ───────── exit loop
            │
            ▼
        Post-agent (if many facts saved):
            ├─ Reflection (chatLLM) → save to reflections table
            └─ Maybe Persona Rewrite (chatLLM) → update persona.md
```

### Key Difference: Agent vs Chat Context

The agent and chat model see **different** versions of the facts:

| | Agent (Kimi K2.5) | Chat (Deepseek V3.2) |
|--|---|---|
| **Facts** | ALL user + ALL self facts | Only semantically relevant facts (KNN-filtered, redundancy-filtered) |
| **Summary** | Not included | Included as Layer 6 of system prompt |
| **History** | Last 10 messages (raw) | Last 10 messages (with day boundary markers) |
| **Tools** | Yes (7 hot + deferred) | No tools |
| **Persona** | Not included (in agent_prompt.md rules) | prompt.md + persona.md + traits |

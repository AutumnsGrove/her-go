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
  2. Chat Compaction ────── compact/compact.go → MaybeCompact()
  │   Loads message window from DB.
  │   Trigger: estimated tokens vs max_history_tokens budget (75% threshold).
  │   If triggered → calls chatLLM (Kimi K2) to summarize older messages.
  │   Summary stored with stream="chat" in summaries table.
  │   Result: conversationSummary string + keptMessages slice.
  │
  ▼
  3. Agent Compaction ───── compact/compact.go → MaybeCompactAgent()
  │   Loads tool calls from agent_turns table.
  │   Checks estimated tokens vs driver_context_budget (75% threshold).
  │   If triggered → calls chatLLM to summarize older actions.
  │   Smart filtering: verbose tool results (web_search, search_books,
  │   recall_memories) are truncated; action outcomes preserved.
  │   Summary stored with stream="agent" in summaries table.
  │   Result: agentActionSummary string + recentAgentActions slice.
  │
  ▼
  4. Semantic Search ────── embed/embed.go → Embed()
  │   Embeds the scrubbed user message with local nomic model.
  │   KNN search in sqlite-vec finds closest facts.
  │   Result: relevantFacts slice (used by both agent and chat).
  │
  ▼
  5. Driver Loop ────────── agent/agent.go → Run()
  │   Qwen3 235B orchestrates the response. Up to 15 iterations.
  │   Decides what to do: think, search, reply, etc.
  │   Tool calls stored in agent_turns table for future action history.
  │   Tool calls may trigger other models (see below).
  │
  ▼
  6. Reply Delivery ─────── agent/agent.go → execReply()
  │   Agent calls the `reply` tool, which invokes the chat model.
  │   Kimi K2 generates the actual text the user sees.
  │   PII tokens are deanonymized before sending to Telegram.
  │
  ▼
  7. Post-Turn ──────────── (background goroutines, parallel)
     Memory agent reviews turn → save/update/remove facts.
     Mood agent infers valence + labels → auto-log/propose/drop.
     Each write gated by classifier (Gemini Flash Lite).
     User never waits — reply already sent.
```

---

## Prompt Assembly: Layer Registry

Both the agent and chat model prompts are assembled from **layers** — small,
self-contained files that each produce one section of the system prompt. Layers
register themselves via `init()` in `agent/layers/` and are sorted by Order.

The same registry is used by runtime (`layers.BuildAll`) and the CLI
(`her shape`) — impossible for them to drift out of sync.

### Driver Layers (StreamAgent)

| Order | Layer | File | Description |
|-------|-------|------|-------------|
| 10 | Driver Prompt | `agent_prompt.go` | Overhead: reports `driver_agent_prompt.md` token size |
| 50 | Tool Schemas | `agent_tools.go` | Overhead: reports hot tool schema token size |
| 100 | Time | `agent_time.go` | ISO timestamp + timezone |
| 150 | Action History | `agent_action_history.go` | Driver's own tool call history (summary + recent actions) |
| 200 | Recent Conversation | `agent_history.go` | Last 6 messages with day boundary markers |
| 300 | Current Message | `agent_message.go` | The scrubbed user message |
| 350 | Image Context | `agent_image.go` | OCR text + image description (if image sent) |
| 400 | User Memories | `agent_user_facts.go` | Semantically relevant user facts (KNN-filtered) |
| 500 | Self Memories | `agent_self_facts.go` | Semantically relevant self facts (KNN-filtered) |
| 900 | Footer | `agent_footer.go` | Instruction footer |

### Chat Layers (StreamChat)

| Order | Layer | File | Description |
|-------|-------|------|-------------|
| 100 | Base Identity | `chat_prompt.go` | `prompt.md` — static personality template |
| 200 | Persona | `chat_persona.go` | `persona.md` — evolving self-image (bot-authored) |
| 300 | Time | `chat_time.go` | Current date/time in human format |
| 400 | Memory | `chat_memory.go` | Semantic facts (KNN-filtered, redundancy-filtered) |
| 450 | Weather | `chat_weather.go` | Current conditions (if configured) |
| 500 | Mood | `chat_mood.go` | Recent mood entries + trend |
| 600 | Summary | `chat_summary.go` | Chat compaction summary of older conversation |

---

## Model Call Reference

### 1. Driver Agent — Qwen3 235B

**Purpose:** Orchestrate the response. Decide what tools to call, in what order.

**Called from:** `agent/agent.go` — `params.DriverLLM.ChatCompletionWithTools()`

**System prompt:** `driver_agent_prompt.md` (loaded by `loadAgentPrompt()`)
- Hot-reloadable from disk (changes take effect next message)
- Contains: agent rules, tool usage instructions, orchestration guidelines
- Auto-generated sections injected between `<!-- BEGIN -->` / `<!-- END -->` markers:
  - `HOT_TOOLS`: list of always-available tools
  - `CATEGORY_TABLE`: deferred tool categories

**User message:** Built by `layers.BuildAll(StreamAgent, ctx)`, contains:

| Layer | Source | Description |
|-------|--------|-------------|
| Time | `time.Now()` | ISO timestamp + timezone |
| Action History | `store.RecentAgentActions()` + compaction | Summary of past actions + recent tool calls in full |
| Recent Conversation | `store.RecentMessages()` | Last 6 messages (sliding window post-compaction) |
| Current Message | `params.ScrubbedUserMessage` | The user's message (PII-scrubbed) |
| Image Context | `params.OCRText` | OCR text if image was sent |
| User Memories | Semantic search results | KNN-filtered user facts + recall_memories hint |
| Self Memories | Semantic search results | KNN-filtered self facts + recall_memories hint |

**Tools:** Starts with 8 hot tools, agent can load more via `use_tools`.
- Hot tools: done, reply, think, recall_memories, get_time, send_task, list_calendars, view_image
- Deferred categories: search (web_search, web_read, search_books), context (get_weather, set_location, nearby_search), calendar (calendar_create, calendar_list, etc.)
- Defined in: `tools/<name>/tool.yaml` (YAML manifests)
- Loaded by: `tools/loader.go`

**Token storage:** Metrics only (`SaveMetric`). Does NOT update message `token_count`.

---

### 2. Chat Model — Kimi K2

**Purpose:** Generate the actual reply the user sees.

**Called from:** `agent/agent.go` — `tctx.ChatLLM.ChatCompletion()` inside `execReply()`

**System prompt:** Built by `layers.BuildAll(StreamChat, ctx)`, layered:

| Layer | Order | Source | Description |
|-------|-------|--------|-------------|
| Base identity | 100 | `prompt.md` (~4.8KB) | Static personality template |
| Persona | 200 | `persona.md` | Evolving self-image (bot-authored) |
| Traits | 250 | DB trait scores | Personality trait scores from last rewrite |
| Time | 300 | `time.Now()` | Current date/time in human format |
| Memory | 400 | Semantic search | KNN-filtered facts, redundancy-filtered against recent messages |
| Weather | 450 | Weather client | Current conditions (if configured) |
| Mood | 500 | DB mood entries | Recent mood trend |
| Expenses | 550 | Receipt context | Receipt data (if just scanned) |
| Summary | 600 | Compaction summary | Summary of older conversation (stream="chat") |

Layers are joined with `\n\n---\n\n` separators.

**Messages (after system prompt):**

| Order | Role | Content |
|-------|------|---------|
| 1 | history | Last 10 messages from DB (with day boundary markers) |
| 2 | system | Search context + agent instruction (if any) |
| 3 | user | The scrubbed user message |

**Typical prompt size:** ~2,600 tokens (with chat_context_budget of 8000)

**Token storage:** `UpdateMessageTokenCount(triggerMsgID, historyTokens)` in execReply — stores history-only tokens (total prompt minus scaffolding estimate) on the user message. This is the value the chat compactor reads on the NEXT turn to decide if history is approaching the budget.

---

### 3. Classifier — Gemini 3.1 Flash Lite

**Purpose:** Validate memory writes. Returns one word: SAVE, FICTIONAL, MOOD_NOT_FACT, etc.

**Called from:** `classifier/classifier.go` — `classifierLLM.ChatCompletion()`
(invoked via `tctx.ClassifyWriteFunc` from tool handlers in `tools/`)

**System prompt:** Pre-rendered at init from `agent/classifiers.yaml` template.
- Fact classifier: ~400 tokens (preamble + 5 verdicts + examples + footer)
- Mood classifier: ~100 tokens
- Receipt classifier: ~100 tokens

**User message:** Built inline in `classifyMemoryWrite()`:
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

### 5. Chat Compaction Model — Kimi K2 (same client as chat)

**Purpose:** Summarize older conversation messages into a running summary.

**Called from:** `compact/compact.go` — `chatLLM.ChatCompletion()` inside `MaybeCompact()`

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

**Summary storage:** `summaries` table with `stream='chat'`

**Token storage:** Metrics only via `SaveMetric()`.

---

### 6. Agent Compaction Model — Kimi K2 (same client as chat)

**Purpose:** Summarize the agent's older tool call history into a running summary.

**Called from:** `compact/compact.go` — `chatLLM.ChatCompletion()` inside `MaybeCompactAgent()`

**System prompt:** `agentSummaryPromptTmpl` (line 298 of compact.go)
- Focused on actions taken, tool calls, decisions, outcomes
- Drops verbose search results, preserves fact operations
- ~250 tokens

**User message:** Formatted action transcript:
```
[Summary of earlier agent actions:]
[existing summary if any]

[Actions since then:]

→ save_fact({"fact": "User works as a software engineer"})
  Result: Saved as fact #42

→ web_search({"query": "Go testing patterns"})
  Result: Found 5 results... (truncated)
...
```

**Smart filtering:** Verbose tools (web_search, book_search, find_skill,
recall_memories, search_history, query_expenses) have their results truncated
to ~200 chars. Action tools (save_fact, update_fact, reply, create_reminder)
keep full results.

**Summary storage:** `summaries` table with `stream='agent'`

**Token storage:** Metrics only via `SaveMetric()`.

---

### 7. Reflection Model — Kimi K2 (same client as chat)

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

### 8. Persona Rewrite Model — Kimi K2 (same client as chat)

**Purpose:** Every N reflections, rewrite `persona.md` — the bot's evolving self-image.

**Called from:** `persona/evolution.go:192` — `llmClient.ChatCompletion()`
Triggered by `MaybeRewrite()` after every `rewrite_every_n_reflections` reflections.

**Input:** Current persona.md + recent reflections + up to 20 self-facts.

**Token storage:** Metrics only.

**Frequency:** Very rare — every ~3 reflections, which themselves are rare.

---

### 9. Embedding Model — Nomic Embed Text (local)

**Purpose:** Convert text to vectors for semantic search and dedup.

**Called from:** `embed/embed.go → Embed()`

**Used by:**
- Semantic fact search (agent.go, recall_memories tool)
- Fact dedup on save (memory agent)
- Mood dedup (mood agent)
- Memory linking (Zettelkasten-style connections)

**Not an LLM call** — no system prompt, no tokens, no cost. Just text → vector.

**Server:** Ollama recommended (`ollama pull nomic-embed-text`), LM Studio also supported.

---

## Dual Compaction System

The system maintains **two independent compaction streams**, each with its own
budget, trigger, summary prompt, and DB storage.

### Chat Compaction (conversations)

| Aspect | Detail |
|--------|--------|
| **What it summarizes** | Conversation messages (user + assistant) |
| **Focus** | Conversational flow, emotional tone, commitments, arc |
| **Budget config** | `max_history_tokens` (default: 8000) |
| **Trigger** | Estimation-based (75% of budget) |
| **Keeps in full** | 6 most recent messages (configured via `recent_messages`) |
| **DB storage** | `summaries` table, `stream='chat'` |
| **Injected into** | Chat model (`chat_summary.go` layer) |

### Agent Compaction (tool call history)

| Aspect | Detail |
|--------|--------|
| **What it summarizes** | Agent tool calls and results from `agent_turns` table |
| **Focus** | Actions taken, decisions made, outcomes, fact operations |
| **Budget config** | `driver_context_budget` (default: 16000) |
| **Trigger** | Estimation-based (75% of budget) |
| **Keeps in full** | 10 most recent actions |
| **Smart filtering** | Verbose tool results (search, books) truncated to ~200 chars |
| **DB storage** | `summaries` table, `stream='agent'` |
| **Injected into** | Driver model (`agent_action_history.go` layer) |

### Why Two Streams?

The chat summary captures *what was discussed*: "They talked about her new job,
she seemed excited, they agreed to revisit the topic tomorrow."

The agent summary captures *what was done*: "Saved fact #42 about her job.
Searched web for salary data. Set a reminder for tomorrow. Updated fact #15
to correct her timezone."

Without the agent summary, the agent has no memory of its own actions once they
scroll out of the 10-message window. This means it can't:
- See facts it previously saved and correct them
- Avoid re-doing work it already did
- Build on past decisions

This is the **defense in depth** complement to semantic fact search — the agent
can see "I saved fact #42 about X" in its action history AND find fact #42 via
recall_memories. Either path leads to the same information.

---

## Token Counting & Storage

### How Token Counts Are Stored

| Message type | `token_count` column contains | Set by |
|-------------|-------------------------------|--------|
| User message | Chat model's `PromptTokens` | `execReply()` line 1052 |
| Assistant message | Chat model's `CompletionTokens` | `execReply()` line 1055 |

The agent model's token usage is tracked in the **metrics table** only, never on messages.

---

## Config Reference (token-related)

From `config.yaml`:

```yaml
memory:
  recent_messages: 6              # sliding window for agent + chat context
  max_facts_in_context: 10        # top-K facts from semantic search
  max_history_tokens: 8000        # chat compaction budget (triggers at 75%)
  driver_context_budget: 16000    # driver action history budget (triggers at 75%)
```

```yaml
driver:
  max_tokens: 4096          # driver response budget (tool call JSON)

chat:
  max_tokens: 4096          # chat response budget

classifier:
  max_tokens: 64            # classifier response budget (one word)
```

---

## Key Differences: Driver vs Chat Context

| | Driver (Qwen3 235B) | Chat (Kimi K2) |
|--|---|---|
| **Facts** | Semantically relevant only (KNN-filtered) + recall_memories tool hint | Semantically relevant only (KNN-filtered, redundancy-filtered) |
| **Compaction summary** | Agent action summary (what it DID) | Chat conversation summary (what was DISCUSSED) |
| **Action history** | Full tool call log from previous turns | Not included |
| **History** | Last 6 messages (with day boundary markers) | Last 6 messages (with day boundary markers) |
| **Tools** | Yes (8 hot + deferred via use_tools) | No tools |
| **Persona** | Not included (personality rules in driver_agent_prompt.md) | prompt.md + persona.md |
| **Prompt assembly** | `layers.BuildAll(StreamAgent, ctx)` | `layers.BuildAll(StreamChat, ctx)` |

---

## Full Data Flow Visualization

```
User Message (Telegram)
    │
    ▼
bot/telegram.go → PII scrub → agent.Run(RunParams)
    │
    ├─ Chat Compaction (sliding window)
    │  ├─ Trigger: 75% of max_history_tokens budget
    │  ├─ If triggered → chatLLM summarization call
    │  └─ Result: conversationSummary string (stream="chat")
    │
    ├─ Agent Compaction (action window)
    │  ├─ Trigger: 75% of driver_context_budget
    │  ├─ Smart filtering: truncates verbose tool outputs
    │  ├─ If triggered → chatLLM summarization call
    │  └─ Result: agentActionSummary string (stream="agent")
    │
    ├─ Semantic search (embed user message → KNN in sqlite-vec)
    │  └─ Result: relevantFacts slice
    │
    ├─ Build agent context (layers.BuildAll StreamAgent):
    │  ├─ driver_agent_prompt.md (system) + tool schemas (hot only)
    │  ├─ Action history (summary + recent tool calls)
    │  ├─ Recent 6 msgs + current message
    │  └─ Semantic facts (user + self) + recall hint
    │
    └─ Driver Loop (up to 15 iterations, Qwen3 235B):
        │
        ├─ think ──────── internal reasoning (surfaces in traces)
        │
        ├─ reply ──────── builds chat prompt, calls Kimi K2
        │  │               ├─ prompt.md + persona.md
        │  │               ├─ time + SEMANTIC facts + weather + mood
        │  │               ├─ conversation summary (stream="chat")
        │  │               └─ 6 recent messages + instruction
        │  │
        │  └─ Deanonymize PII → send to Telegram → fire TTS
        │
        ├─ view_image ── vision model (Gemini Flash)
        │
        ├─ web_search ── Tavily API (via use_tools → search category)
        │
        ├─ use_tools ─── loads deferred tool schemas into active set
        │
        └─ done ───────── exit loop
            │
            ▼
        Post-turn (background goroutines, parallel):
            │
            ├─ Memory Agent (Kimi K2):
            │  ├─ save_memory → classifier (Gemini Flash Lite) → embed → save
            │  ├─ update_memory, remove_memory
            │  └─ done
            │
            └─ Mood Agent (Kimi K2):
               ├─ infer valence (1-7) + labels + associations
               ├─ classifier check + KNN dedup
               └─ auto-log / propose / drop based on confidence
```

---

## Project Structure

### Core Packages

| Package | Description |
|---------|-------------|
| `agent/` | Driver agent + memory agent, reply generation, thinking traces |
| `bot/` | Telegram handler, commands, mood wizard |
| `calendar/` | Apple Calendar EventKit bridge |
| `classifier/` | Memory + mood write classifiers (Gemini Flash Lite) |
| `cmd/` | Cobra CLI commands (run, sim, shape, logs, tunnel, sync) |
| `compact/` | Dual compaction system (chat + agent streams) |
| `config/` | YAML + env var loading |
| `d1/` | Cloudflare D1 HTTP client for cross-machine sync |
| `embed/` | Local embedding model client (Ollama/LM Studio) |
| `integrate/` | External integrations (Nominatim geocoding, Foursquare places) |
| `layers/` | Prompt layer registry — one file per layer |
| `llm/` | OpenRouter client with fallbacks |
| `logger/` | Shared structured logger (charmbracelet/log) |
| `memory/` | SQLite store + SyncedStore decorator for D1 mirroring |
| `mood/` | Mood agent, vocab loader, daily rollup, PNG graphs |
| `persona/` | Dreaming system, persona evolution, trait tracking |
| `retry/` | Unified retry package with configurable backoff |
| `scheduler/` | Extension-based cron system (registry, runner, retry) |
| `scrub/` | Tiered PII detection + deanonymization |
| `search/` | Tavily web search, Open Library book search |
| `tools/` | Tool YAML manifests + handlers (per-tool directories) |
| `trace/` | Trace inbox (Stream registry + Board) |
| `turn/` | Turn phase tracking (driver → memory → mood) |
| `vision/` | Image understanding via Gemini Flash |
| `voice/` | Parakeet STT + Piper TTS clients |
| `weather/` | Open-Meteo weather integration |
| `worker/` | Cloudflare Worker for webhook routing |

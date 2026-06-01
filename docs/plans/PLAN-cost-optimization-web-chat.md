# Cost Optimization + Local Web Chat

## Context

her-go's per-turn cost ($0.005-$0.03) is higher than ideal, and some turns are slower than they need to be because every single turn runs the full driver agent + all background agents. Additionally, the only way to chat currently is Telegram (prod) or Gradio (dev). A WebSocket adapter + local SvelteKit chat app would give a better development/usage experience with real-time streaming.

**Scope:** No auth, no multi-tenant, no hosting. These optimizations benefit the single-user local setup.

**Broader context:** This is the first step toward potential productization — a hosted web product where users sign up and chat with their own instance of the bot. See `PLAN-productization.md` for the full roadmap.

---

## Step 1A: Fast-Path Classifier ✅

**What:** A Gemini Flash Lite classifier call before the driver agent. Returns PASS (full driver pipeline) or SKIP (direct to chat model). Simple conversational turns skip the entire driver loop.

**Integration point:** Inside `bot.runAgent()`, after UI scaffolding (typing, placeholder, callbacks) but before `agent.Run()`. All adapters benefit.

**Files:**
- `bot/fast_path.go` — `shouldFastPath()`, `classifyRoute()`, `runFastPath()` methods on Bot
- `bot/fast_path_prompt.md` — embedded classifier prompt (Data Primacy compliant)
- `bot/run_agent.go` — fast-path check before agent.Run()
- `config/config.go` — `FastPath bool` on DriverConfig

**The skip path:**
1. Call classifier LLM (Gemini Flash Lite) with the scrubbed message + 3 recent messages for context
2. If SKIP: auto-recall memories via semantic search (embed + KNN)
3. Assemble chat context via `layers.BuildAll(StreamChat, layerCtx)`
4. Load recent messages and latest stored summary
5. Call chat model (with streaming if adapter supports it)
6. Save assistant message, deliver via Frontend
7. Count toward the background batcher threshold (Step 1B)

**Always PASS:** image messages, first message in a conversation, classifier failures (fail-open), messages sharing personal facts or longer than a couple sentences.

**Measured savings:** ~33% per-turn cost reduction on simple messages ($0.0016 vs $0.0024).

---

## Step 1B: Background Agent Batching ✅

**What:** Accumulate N turns (default 3) before running memory/mood/introspection agents. Currently they run after every single turn.

**Files:**
- `bot/batcher.go` — `BackgroundBatcher` struct (counter + inactivity timer)
- `bot/run_agent.go` — batcher integration for both full-pipeline and fast-path turns
- `bot/telegram.go` — batcher initialization in `New()` and `NewDev()`
- `config/config.go` — `BatchThreshold int` on MemoryAgentConfig

**Design:** Simple counter with 45s inactivity timer. On non-batch turns, memory agent suppressed via `MemoryAgentLLM = nil`. The memory agent already reads recent messages from the store, so it sees all intermediate turns when it finally runs.

**Estimated savings:** ~60% reduction on background agent costs.

---

## Step 2: WebSocket Gateway Adapter ✅

**What:** New `gateway/websocket.go` implementing the Adapter interface. Real-time chat with token streaming.

**Protocol (JSON over WebSocket):**
```
Inbound:  {"type":"message", "text":"...", "request_id":"uuid"}
Outbound: {"type":"stream_token", "token":"Hi"}
          {"type":"stream_end"}
          {"type":"reply", "text":"full reply text"}
          {"type":"status", "text":"recalling memories..."}
          {"type":"typing", "active":true}
          {"type":"trace", "event_type":"...", "data":{...}}
```

**Files:**
- `gateway/websocket.go` — full adapter (~400 lines)
- `gateway/adapter.go` — `Streamer` optional interface for token streaming
- `gateway/pipeline.go` — `gatewayFrontend.MakeStreamCallback()` implements `StreamProvider`
- `gateway/gateway.go` — "websocket" case in adapter factory
- `go.mod` — `github.com/gorilla/websocket` dependency

**Security:** Binds to `127.0.0.1` only, 64KB read limit, max 10 connections, ping/pong keepalive.

---

## Step 3: Local SvelteKit Chat App (TODO)

**What:** A SvelteKit app at `web/` that connects to the local WebSocket adapter. Clean chat UI with streaming.

**Structure:**
```
web/
  src/
    routes/
      +page.svelte         — chat view (the whole app is one page)
      +layout.svelte       — shell
    lib/
      ws.ts                — WebSocket client class
      stores.ts            — Svelte stores for messages, connection, typing
    app.css
  package.json
  svelte.config.js
```

**Features:**
- Message list with user/assistant bubbles
- Streaming token rendering (tokens append with blinking cursor, finalize to rendered markdown)
- Status indicator ("thinking...", "recalling memories...")
- Typing indicator
- Input bar fixed at bottom
- Auto-scroll to latest message
- Responsive (works on mobile browsers too)
- Optional: trace panel (expandable, shows agent thinking like the Telegram /traces command)

**No auth flow** — connects directly to `ws://localhost:7778/ws`

---

## Build Order

```
1A (fast-path classifier) ─── ✅ done
2  (WebSocket adapter)    ─── ✅ done
1B (background batching)  ─── ✅ done
3  (SvelteKit app)        ─── TODO
```

## Cost Impact Summary

| Optimization | Savings | Blended $/turn |
|---|---|---|
| Baseline (today) | — | ~$0.01 |
| + Fast-path (30-40% skip rate) | -30% | ~$0.007 |
| + Background batching (every 3) | -20% | ~$0.005 |
| **Combined** | **~50%** | **~$0.005** |

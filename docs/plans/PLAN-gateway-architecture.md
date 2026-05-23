# Gateway Architecture Plan

## Context

The `bot/` package is currently a 5,200-line monolith that owns transport (Telegram), agent orchestration, command handling, mood pipelines, voice, traces, and state management. A `Frontend` interface (14 methods) already abstracts transport, and `ProcessMessage()` is transport-neutral — but everything lives in `bot/`, making it impossible to cleanly add new interfaces (Gradio, Discord, etc.) without touching Telegram code.

This plan introduces a `gateway/` package that sits **above** `bot/`, owns all transport adapters, and reduces `bot/` to a pure agent pipeline. The goal: `her run` reads config, starts the gateway, gateway spins up adapters, each adapter feeds messages into `bot/` for processing. Adding a new interface = implementing one adapter, zero changes to core logic.

**Branch strategy:** Merge `feat/vps-deployment` first (it has good isolated work: procmgr, D1 sync, audit fixes), then create `feat/gateway` from main.

---

## Architecture Overview

```
┌─────────────────────────────────────────────────────┐
│                    cmd/run.go                       │
│  Load config → Create gateway → gateway.Run()       │
└──────────────────────┬──────────────────────────────┘
                       │
┌──────────────────────▼──────────────────────────────┐
│                  gateway.Gateway                     │
│                                                      │
│  Owns: adapters[], command registry, event stream    │
│  For each adapter config:                            │
│    1. Open Store (db path from adapter config)       │
│    2. Create Pipeline (bot/ agent orchestration)     │
│    3. Start Adapter (telegram/gradio/tui)            │
│    4. Route: Adapter.Receive() → Pipeline.Process()  │
│             Pipeline result → Adapter.Send()         │
└──┬──────────┬──────────┬────────────────────────────┘
   │          │          │
┌──▼──┐  ┌───▼───┐  ┌───▼───┐
│ TG  │  │Gradio │  │ TUI   │
│Adapt│  │ Adapt │  │Observ │
└─────┘  └───────┘  └───────┘
```

---

## Phase 1: Gateway Foundation + Gradio Adapter

This is the first PR. Get local Gradio chat working through the gateway, with traces in a side panel. Telegram untouched — still works the old way via `bot/`.

### 1.1 Define Gateway Types

**New file: `gateway/types.go`**

```go
// InboundMsg is the standard message entering the system from any adapter.
type InboundMsg struct {
    Text           string
    Audio          []byte   // nil if text-only
    ImageBase64    string   // nil if no image
    ImageMIME      string
    ConversationID string   // adapter-assigned, unique per adapter instance
    AdapterName    string   // "telegram", "gradio", etc.
    Timestamp      time.Time
}

// OutboundMsg is the standard response leaving the system to any adapter.
type OutboundMsg struct {
    Text    string
    Audio   []byte   // nil if text-only (TTS handled externally)
    IsError bool
}

// TraceEvent is published on the gateway's trace channel for any subscriber.
type TraceEvent struct {
    Phase   string    // "think", "tool", "reply", "memory", "mood"
    Agent   string    // "driver", "memory", "mood", "introspection", "dream"
    Content string
    TurnID  int64
    Time    time.Time
}

// Command is a platform-agnostic command invocation.
type Command struct {
    Name string   // "traces", "mood", "clear", "help", etc.
    Args string   // everything after the command name
}

// CapSet declares what an adapter supports.
type CapSet struct {
    Edit     bool // edit previously sent messages in-place
    Stream   bool // token-by-token streaming
    Paginate bool // multi-page messages with navigation
    Typing   bool // typing indicators
    Audio    bool // voice message send/receive
    Confirm  bool // interactive yes/no confirmations
}
```

### 1.2 Define Adapter Interface

**New file: `gateway/adapter.go`**

```go
// Adapter is the contract for any transport (Telegram, Gradio, TUI, etc.).
type Adapter interface {
    // Name returns a human-readable identifier ("telegram", "gradio").
    Name() string

    // Capabilities declares what this adapter supports.
    Capabilities() CapSet

    // Start begins listening. Blocks until ctx is cancelled.
    Start(ctx context.Context) error

    // Stop gracefully shuts down the adapter.
    Stop() error

    // Receive returns a channel of inbound messages.
    // The gateway reads from this to feed the pipeline.
    Receive() <-chan InboundMsg

    // Send delivers an outbound message to the user.
    Send(msg OutboundMsg) error

    // SendStatus updates the user on what the agent is doing (e.g., "thinking...").
    // No-op if the adapter doesn't support in-place edits.
    SendStatus(text string) error

    // StartTyping begins a typing indicator. Returns a cancel func.
    // No-op (returns func(){}) if not supported.
    StartTyping() func()

    // OnTraceEvent is called when a trace event fires.
    // Each adapter renders traces however it wants (or ignores them).
    OnTraceEvent(evt TraceEvent)

    // OnCommand registers a handler for a gateway-level command.
    // The adapter translates its native command format into Command structs.
    RegisterCommands(cmds []CommandDef)
}

// CommandDef defines a gateway-level command.
type CommandDef struct {
    Name        string
    Description string
    Handler     func(ctx context.Context, args string) (string, error)
}
```

### 1.3 Define Adapter Config

**New section in `config.yaml`:**

```yaml
gateway:
  adapters:
    - name: telegram
      type: telegram
      enabled: true
      db: her.db
      memory: true
      # telegram-specific
      token: ${TELEGRAM_BOT_TOKEN}
      mode: poll

    - name: gradio-dev
      type: gradio
      enabled: true
      db: her-dev.db
      memory: false
      # gradio-specific
      port: 7860
      traces: true   # show trace panel
```

**New config types in `config/config.go`:**

```go
type GatewayConfig struct {
    Adapters []AdapterConfig `yaml:"adapters"`
}

type AdapterConfig struct {
    Name    string         `yaml:"name"`
    Type    string         `yaml:"type"`    // "telegram", "gradio", "tui"
    Enabled bool           `yaml:"enabled"`
    DB      string         `yaml:"db"`      // database path
    Memory  *bool          `yaml:"memory"`  // nil = use default (true)
    Extra   map[string]any `yaml:"extra"`   // adapter-specific config (parsed by adapter)
    // Common adapter-specific fields promoted for convenience:
    Token   string         `yaml:"token"`   // telegram
    Port    int            `yaml:"port"`    // gradio
    Mode    string         `yaml:"mode"`    // telegram: poll/webhook
    Traces  bool           `yaml:"traces"`  // show traces
}
```

### 1.4 Gateway Orchestrator

**New file: `gateway/gateway.go`**

```go
type Gateway struct {
    cfg      *config.Config
    adapters []Adapter
    commands []CommandDef
    traceCh  chan TraceEvent  // fan-out to all adapters
    bus      *tui.Bus
}

func New(cfg *config.Config, bus *tui.Bus) *Gateway

// Run starts all enabled adapters and blocks until ctx is cancelled.
// For each adapter:
//   1. Open/reuse Store based on adapter's db path
//   2. Create a Pipeline (wraps bot/ agent orchestration)
//   3. Start adapter in a goroutine
//   4. Goroutine: read from adapter.Receive(), call pipeline.Process(), send result via adapter.Send()
func (g *Gateway) Run(ctx context.Context) error

// RegisterCommand adds a gateway-level command available to all adapters.
func (g *Gateway) RegisterCommand(def CommandDef)

// Stop shuts down all adapters gracefully.
func (g *Gateway) Stop() error
```

**Key detail — DB sharing:** The gateway maintains a `map[string]memory.Store` keyed by db path. Two adapters pointing to the same `her.db` get the same Store instance. Different paths get different stores. This is how shared vs. isolated memory works — zero special logic, just pointer equality.

### 1.5 Pipeline (Bot Refactor Target)

**New file: `gateway/pipeline.go`** (thin wrapper around bot/ for now)

In Phase 1, this is a minimal shim that translates between gateway types and the existing bot code. The full bot/ refactor happens in Phase 2.

```go
type Pipeline struct {
    bot   *bot.Bot  // existing Bot, used in compatibility mode
    store memory.Store
    cfg   *config.Config
    bus   *tui.Bus
}

func NewPipeline(store memory.Store, cfg *config.Config, bus *tui.Bus) (*Pipeline, error)

// Process takes an InboundMsg and returns an OutboundMsg.
// Internally calls bot.ProcessMessage() with a GatewayFrontend adapter.
func (p *Pipeline) Process(ctx context.Context, msg InboundMsg) (OutboundMsg, error)
```

### 1.6 Gradio Adapter

**New file: `gateway/gradio/adapter.go`**

HTTP server that Gradio connects to. Two endpoints:
- `POST /api/chat` — receives user message, returns reply
- `GET /api/traces` — SSE stream of trace events for the side panel

The Gradio Python script (`scripts/dev_chat.py`) gets a second panel wired to the SSE trace stream.

```go
type Adapter struct {
    cfg     config.AdapterConfig
    port    int
    msgCh   chan gateway.InboundMsg
    traceCh chan gateway.TraceEvent  // buffered, adapter subscribes
    server  *http.Server
}

func New(cfg config.AdapterConfig) *Adapter
func (a *Adapter) Name() string           { return "gradio" }
func (a *Adapter) Capabilities() CapSet   { return CapSet{Stream: true} }
func (a *Adapter) Start(ctx) error        // starts HTTP server, blocks
func (a *Adapter) Stop() error
func (a *Adapter) Receive() <-chan InboundMsg
func (a *Adapter) Send(msg OutboundMsg) error
func (a *Adapter) SendStatus(text string) error  // push via SSE
func (a *Adapter) StartTyping() func()           // push "typing" SSE event
func (a *Adapter) OnTraceEvent(evt TraceEvent)    // push to SSE stream
func (a *Adapter) RegisterCommands(cmds []CommandDef)  // wire as HTTP endpoints
```

### 1.7 TUI as Observer

The TUI stays exactly as-is — it reads from `tui.Bus` which already carries all the events it needs. It's NOT an adapter. It's wired up in `cmd/run.go` the same way it is today: subscribe to bus, render.

The only change: gateway also publishes `TraceEvent`s to the bus (or adapters subscribe directly to the bus).

### 1.8 CLI Restructure

**`her run`** becomes the single entry point:

```
her run                      # start with config.yaml (all enabled adapters)
her run --adapter=gradio     # override: only start gradio adapter
her run --db=test.db         # override: all adapters use this db
her run --no-tui             # disable TUI (plain log mode)

her service install          # install as system service (procmgr)
her service start
her service stop
her service status

her sync push/pull           # D1 operations (unchanged)
```

The `her dev` command is **removed** — replaced by `her run --adapter=gradio` or just having gradio enabled in config.

---

## Phase 2: Telegram Adapter Migration

Move Telegram handling out of `bot/` into `gateway/telegram/`. This is the big refactor.

### 2.1 Telegram Adapter

**New: `gateway/telegram/adapter.go`**

Implements the `Adapter` interface. Wraps `telebot.v4`. Owns:
- Bot token validation and connection
- Handler registration (translates Telegram commands → gateway Commands)
- Message receiving (OnText, OnPhoto, OnVoice, OnLocation → InboundMsg)
- Message sending (OutboundMsg → tele.Send/Edit)
- Typing indicators (4s refresh loop)
- Inline buttons (for confirmations, pagination)
- Streaming (400ms buffer + cursor)
- Voice handling (delegates to voice service for STT/TTS)

### 2.2 Bot Package Cleanup

After Telegram adapter migration, `bot/` becomes:
- `bot/pipeline.go` — agent orchestration (current `runAgent` + `baseRunParams`)
- `bot/commands.go` — command logic (clear, stats, facts, forget — transport-neutral)
- `bot/mood.go` — mood pipeline (background agent)
- `bot/introspection.go` — introspection agent (background)

**Removed from bot/:**
- `telegram.go` (Bot struct, New(), Start(), handler registration) → `gateway/telegram/`
- `frontend.go`, `frontend_telegram.go`, `frontend_dev.go` → replaced by Adapter interface
- `handlers_*.go` (Telegram-specific routing) → `gateway/telegram/`
- `callbacks.go` (inline buttons) → `gateway/telegram/`
- `webhook.go` → `gateway/telegram/`
- `paginate.go` → `gateway/telegram/` (Telegram-specific UI)

### 2.3 Command Migration

Gateway-level commands (work everywhere):
- `/help` — render help text
- `/clear` — rotate conversation ID
- `/stats` — show usage metrics
- `/facts` — list memories
- `/forget` — deactivate memory
- `/traces` — toggle trace visibility
- `/mood` — show/set mood
- `/dream` — trigger dream cycle
- `/status` — show system health

Telegram-only (stay in telegram adapter):
- Inline button callbacks (confirm/deny, pagination)
- `/update` (git pull + restart — only makes sense for managed service)
- Voice message handling
- Photo/location handlers (though these become InboundMsg fields)

---

## Phase 3: TUI Observer Mode

The TUI already works as an observer via `tui.Bus`. This phase is minimal:
- Ensure gateway publishes all relevant events to the bus
- Add adapter status lines to TUI header (which adapters are running, message counts per adapter)
- Optional: add trace rendering to TUI sections (already done via ToolCallEvent routing)

---

## Phase 4: VPS Deployment

With gateway in place:
- `her run` on a VPS starts only enabled adapters (Telegram in config)
- `procmgr` handles service management (already done)
- D1 sync handles cross-machine state (already done)
- No macOS-specific code needed if calendar/voice disabled in config

---

## Critical Files to Modify

### New Files
| File | Purpose |
|------|---------|
| `gateway/types.go` | InboundMsg, OutboundMsg, TraceEvent, Command, CapSet |
| `gateway/adapter.go` | Adapter interface, CommandDef |
| `gateway/gateway.go` | Gateway orchestrator |
| `gateway/pipeline.go` | Thin wrapper around bot/ for Phase 1 |
| `gateway/gradio/adapter.go` | Gradio HTTP adapter |
| `gateway/gradio/handler.go` | HTTP handlers (/api/chat, /api/traces SSE) |

### Modified Files
| File | Change |
|------|--------|
| `config/config.go` | Add GatewayConfig, AdapterConfig structs |
| `config.yaml.example` | Add gateway section with adapter examples |
| `cmd/run.go` | Refactor to create Gateway, call gateway.Run() |
| `cmd/root.go` | Restructure: remove `dev`, add `service` subcommand group |
| `scripts/dev_chat.py` | Add trace panel (SSE subscriber) |

### Untouched in Phase 1
| File | Why |
|------|-----|
| `bot/*.go` | Phase 1 wraps bot/ via Pipeline shim; refactor in Phase 2 |
| `agent/*.go` | Already transport-neutral, no changes needed |
| `memory/*.go` | Already interface-based, no changes |
| `tools/*.go` | Already transport-neutral |
| `tui/*.go` | Already works as observer via Bus |
| `trace/*.go` | Reused as-is; gateway publishes TraceEvents alongside |

---

## Verification Plan

### Phase 1 Smoke Test
1. `go build -o her && ./her run --adapter=gradio`
2. Open Gradio UI at localhost:7860
3. Send a message → get reply
4. Check trace panel shows agent phases (think, tool, reply)
5. Send `/clear` → conversation resets
6. Send `/facts` → shows memories (if memory enabled in adapter config)
7. Verify TUI still shows events when running with `--adapter=gradio`

### Phase 1 Isolation Test
1. Configure two adapters in config: gradio (db: test.db, memory: false) + gradio-2 (db: her.db, memory: true)
2. Chat on gradio → no memories saved
3. Chat on gradio-2 → memories saved
4. Verify separate conversation threads

### Regression Test
1. `./her run` with only telegram adapter enabled → existing behavior unchanged
2. All existing Telegram commands work
3. TUI renders correctly
4. Traces toggle works via /traces

---

## Design Decisions Log

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Adapter discovery | Multi-attach, config-driven | Run Telegram + Gradio simultaneously, each with own settings |
| Conversation isolation | Per-adapter config (shared DB = shared memory, different DB = isolated) | Maximum flexibility for dev/test/prod |
| Package boundary | Gateway above bot | Gateway orchestrates; bot becomes pure pipeline |
| TUI role | Observer only | Read-only dashboard, no chat. Simplifies adapter count |
| Rich features | Capability flags (CapSet) | Adapters declare support; gateway checks before calling |
| Commands | Mixed: core gateway-level + platform-specific in adapters | Common commands work everywhere; platform chrome stays local |
| Traces | Event stream (TraceEvent channel) | Each adapter subscribes and renders its own way |
| CLI shape | Single `her run` + config overrides, `her service` for mgmt | Simpler mental model, config is source of truth |
| Voice | Separate service | Gateway is text-only; adapters connect to voice service independently |
| Phase 1 target | Gateway + Gradio first | Develop/test locally without touching production Telegram |
| Branch strategy | Merge VPS, refactor in place | Keep procmgr/D1/audit work, start gateway fresh |

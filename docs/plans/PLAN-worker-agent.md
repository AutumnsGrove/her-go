# Worker Agent System

## Context

Inspired by exploring OpenClaw, Hermes Agent, and Picobot on the VPS, we identified the highest-value capability gap: **Mira can't do background work that produces artifacts**. The #1 real-world use case across all agent frameworks is proactive scheduled briefings — not coding, not shell access. A morning digest delivered in Mira's voice, with a rich-format report link.

Rather than bolting on shell/exec tools or importing a third-party framework, we're building a **general-purpose worker agent** — a standalone LLM loop that receives tasks, does work (search, file operations), and produces file artifacts. The scheduler triggers it on a cron, or the driver delegates to it via `send_task`. When done, it hands results back through the event bus so Mira comments on the report in her voice.

This is also the foundation for future coding agent work — the same loop, different task type prompt, different tools enabled.

---

## Architecture

```
Trigger (scheduler cron OR driver send_task)
  │
  ▼
workeragent.RunWorker(input, params)
  │
  ├── Load per-task-type prompt from workeragent/tasks/<type>/prompt.md
  ├── Select LLM client from tier map (low/medium/high)
  ├── Build tool set from YAML registry (agent: [worker])
  │
  ├── Tool-calling loop (up to 20 iterations)
  │   ├── web_search, web_read   (research)
  │   ├── write_file, read_file, patch_file, list_files  (artifacts)
  │   ├── think                  (reasoning)
  │   └── done                   (completion signal + summary)
  │
  ├── Publish to Telegraph (if configured)
  │
  └── Emit EventWorkerComplete → event bus
                                    │
                                    ▼
                          Driver agent wakes up
                          Mira comments on the report
                          Sends Telegraph link to user
```

---

## Package Layout

```
workeragent/
  worker.go                   # RunWorker(), WorkerInput, WorkerParams, WorkerResult
  registry.go                 # Task type discovery — scans tasks/ dirs at init
  briefing_handler.go         # scheduler.Handler for cron-fired briefings
  tasks/
    briefing/
      prompt.md               # System prompt for briefing work
      meta.yaml               # model_tier: low
      task.yaml               # cron: "0 7 * * *", retry config
    research/
      prompt.md               # System prompt for ad-hoc research
      meta.yaml               # model_tier: medium

telegraph/
  client.go                   # CreateAccount(), CreatePage()
  nodes.go                    # MarkdownToNodes() converter

tools/write_file/             # New tool — agent: [worker]
tools/read_file/              # New tool — agent: [worker]
tools/patch_file/             # New tool — agent: [worker]
tools/list_files/             # New tool — agent: [worker]

reports/                      # Output directory (gitignored)
```

---

## Key Types

### WorkerInput / WorkerParams / WorkerResult (`workeragent/worker.go`)

```go
type WorkerInput struct {
    TaskType string            // "briefing", "research", "coding"
    Payload  map[string]string // from scheduler payload or send_task note
}

type WorkerParams struct {
    LLM            *llm.Client
    TavilyClient   *search.TavilyClient
    Store          memory.Store
    Cfg            *config.Config
    TelegraphToken string
    ReportsDir     string
}

type WorkerResult struct {
    ReportPath   string  // local file: reports/2026-06-10-tech-digest.md
    TelegraphURL string  // https://telegra.ph/Tech-Digest-06-10
    Title        string  // from first heading
    Summary      string  // first 2-3 sentences for driver handoff
}
```

### Task Type Registry (`workeragent/registry.go`)

**Directory-driven, zero Go code to add a new task type.** At startup, scan `workeragent/tasks/*/` for `prompt.md` + `meta.yaml`. Each meta.yaml:

```yaml
name: briefing
model_tier: low
```

Adding a new task type = create a directory with prompt.md and meta.yaml. No Go changes.

### Config (`config/config.go`)

New `WorkerAgentConfig` struct added to `Config`:

```go
type WorkerAgentConfig struct {
    Tiers          map[string]TierConfig `yaml:"tiers"`
    TelegraphToken string                `yaml:"telegraph_token"`
    ReportsDir     string                `yaml:"reports_dir"`     // default "reports"
}

type TierConfig struct {
    Model       string  `yaml:"model"`
    Temperature float64 `yaml:"temperature"`
    MaxTokens   int     `yaml:"max_tokens"`
}
```

config.yaml example:
```yaml
worker_agent:
  telegraph_token: "${TELEGRAPH_TOKEN}"
  reports_dir: "reports"
  tiers:
    low:
      model: "qwen/qwen3-235b-a22b-2507"
      temperature: 0.7
      max_tokens: 8192
    medium:
      model: "deepseek/deepseek-v4-flash"
      temperature: 0.7
      max_tokens: 16384
    high:
      model: "moonshotai/kimi-k2-0905"
      provider: "groq"
      temperature: 0.7
      max_tokens: 32768
```

---

## Tool Set

### Existing tools — add `worker` to agent field

| Tool | Current agents | Change |
|---|---|---|
| `web_search` | `[main]` | `[main, worker]` |
| `web_read` | `[main]` | `[main, worker]` |
| `think` | `[main, introspection, dream]` | `[main, introspection, dream, worker]` |
| `done` | `[main, memory, introspection, dream]` | `[main, memory, introspection, dream, worker]` |

### New file tools — `agent: [worker]`

| Tool | Description |
|---|---|
| `write_file` | Create/overwrite a file in reports/. Path is relative. |
| `read_file` | Read a file from reports/. |
| `patch_file` | Find-and-replace edit on a file in reports/. |
| `list_files` | List files in reports/. |

All four enforce path scoping: `filepath.Clean(filepath.Join(reportsDir, path))` must have `reportsDir` prefix. Prevents traversal.

New field on `tools.Context`:
```go
ReportsDir     string                    // absolute path to reports/
WorkerCallback func(taskType, note string) // fires worker in background goroutine
```

---

## Scheduler Integration

`workeragent/briefing_handler.go` implements `scheduler.Handler`:
- `Kind()` → `"worker_briefing"`
- `ConfigPath()` → `"workeragent/tasks/briefing/task.yaml"`
- `Execute()` → calls `RunWorker()`, then emits `EventWorkerComplete`

Registers at init: `scheduler.Register(briefingHandler{})`

`scheduler.Deps` gets additional fields:
```go
WorkerLLMs   map[string]*llm.Client   // tier → client
TavilyClient *search.TavilyClient
Cfg          *config.Config
RootDir      string
AgentEventCh chan<- agent.AgentEvent
```

Wired in `cmd/run.go` alongside existing scheduler Deps construction.

---

## Driver Delegation (send_task extension)

`tools/send_task/tool.yaml` adds `target` parameter:
```yaml
parameters:
  properties:
    target:
      type: string
      enum: [memory, worker]
      description: "Which agent to delegate to (default: memory)"
    task_type:
      type: string
      description: "Type of task"
    note:
      type: string
      description: "Instructions for the target agent"
```

Handler routes by target:
- `memory` → existing inbox pattern (unchanged)
- `worker` → calls `ctx.WorkerCallback(taskType, note)` → goroutine

---

## Event Bus Handoff

New event type in `agent/event.go`:
```go
EventWorkerComplete AgentEventType = iota // added after EventInboxReady
```

New fields on `AgentEvent`:
```go
ReportURL  string // Telegraph URL
ReportPath string // local file path
```

Handling in `bot/telegram.go` `handleAgentEvent()`:
```go
case agent.EventWorkerComplete:
    prompt = fmt.Sprintf(
        "[system] Your worker agent finished a %s report.\n\n"+
        "Summary: %s\n\nPublished at: %s\n\n"+
        "Share this naturally — comment on highlights, include the link.",
        evt.TaskName, evt.Summary, evt.ReportURL,
    )
    conversationID = "worker-report"
```

This triggers a full driver → chat model pipeline. Mira comments in her voice with a link to the report.

---

## Telegraph Client (`telegraph/`)

Small, focused package. Two files:

**client.go** — HTTP client for Telegraph API:
- `CreateAccount(shortName, authorName)` → access token (one-time setup)
- `CreatePage(title, markdownContent)` → Telegraph URL
- Token stored in config.yaml

**nodes.go** — Markdown → Telegraph DOM node converter:
- Uses `yuin/goldmark` to parse markdown AST
- Walks AST, emits Telegraph-compatible nodes (h3, h4, p, b, em, code, pre, ul, ol, li, a, blockquote)
- Tables → pre blocks (Telegraph limitation)
- ~150 lines

Telegraph is optional — if `telegraph_token` is empty, reports are local-only and the driver gets the summary + file path.

---

## Implementation Phases

### Phase 1: File tools + reports directory
New tools: `write_file`, `read_file`, `patch_file`, `list_files` with `agent: [worker]`. Add `ReportsDir` to `tools.Context`. Create `reports/` dir, gitignore report files.

**Files:** `tools/{write,read,patch,list}_file/{handler.go,tool.yaml}`, `tools/context.go`, `reports/.gitkeep`, `.gitignore`

**Done when:** Tools registered, callable from tool registry, path scoping tested.

### Phase 2: Worker agent loop + task type registry
Create `workeragent/` package: agent loop, task registry, briefing prompt/meta. Add `WorkerAgentConfig` to config. Wire LLM clients in `cmd/run.go`. Add `worker` to agent fields in existing tool YAMLs.

**Files:** `workeragent/{worker.go,registry.go}`, `workeragent/tasks/briefing/{prompt.md,meta.yaml}`, `config/config.go`, `config.yaml.example`, `cmd/run.go`, tool YAMLs

**Done when:** `workeragent.RunWorker()` callable in isolation, produces a markdown report file.

### Phase 3: Telegraph client
Create `telegraph/` package. Markdown-to-node converter + API client. Tests for node conversion.

**Files:** `telegraph/{client.go,nodes.go,nodes_test.go}`

**Done when:** `telegraph.Client.CreatePage()` returns a working URL.

### Phase 4: Scheduler integration
Briefing handler, task.yaml with cron, extend `scheduler.Deps`, wire in `cmd/run.go`.

**Files:** `workeragent/{briefing_handler.go,tasks/briefing/task.yaml}`, `scheduler/types.go`, `cmd/run.go`

**Done when:** Briefing fires at cron time, produces report + Telegraph page.

### Phase 5: Event bus handoff
Add `EventWorkerComplete` to event types. Handle in `handleAgentEvent()`. Emit from briefing handler.

**Files:** `agent/event.go`, `bot/telegram.go`, `workeragent/briefing_handler.go`

**Done when:** Mira sends a message with commentary + Telegraph link after briefing completes.

### Phase 6: Driver delegation
Extend `send_task` with `target: worker`. Add `WorkerCallback` to `tools.Context`. Wire callback. Add research task type.

**Files:** `tools/send_task/{tool.yaml,handler.go}`, `tools/context.go`, `bot/run_agent.go`, `workeragent/tasks/research/{prompt.md,meta.yaml}`

**Done when:** Driver can delegate research tasks that produce reports.

---

## Verification

1. **Phase 2 — unit test:** Call `RunWorker()` with a mock LLM that returns search + write_file tool calls. Assert report file exists in reports/.
2. **Phase 3 — integration test:** Call `CreatePage()` with sample markdown. Verify returned URL is accessible.
3. **Phase 4 — manual test:** Set cron to `* * * * *` (every minute), verify briefing fires and report appears.
4. **Phase 5 — manual test:** After briefing, verify Mira sends a Telegram message with Telegraph link.
5. **Phase 6 — sim test:** Run a sim where user says "research Go testing libraries" → driver calls send_task(target=worker) → report produced → Mira comments.

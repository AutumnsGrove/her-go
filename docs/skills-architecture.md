# Skills Harness Architecture

> Design document for the skills system in her-go. This document captures architectural decisions
> made during planning and serves as the implementation blueprint.
>
> Status: **PARTIALLY IMPLEMENTED** — core system working (skillkit, loader, registry, agent tools, 3 skills migrated). Advanced infra (proxy, trust, sidecar DBs, coding agent, event bus) not yet started.

---

## Table of Contents

1. [Philosophy](#1-philosophy)
2. [Skill Format](#2-skill-format)
3. [Trust Model](#3-trust-model)
4. [Skill Discovery](#4-skill-discovery)
5. [Skill Execution](#5-skill-execution)
6. [Network Proxy](#6-network-proxy)
7. [Sidecar Databases](#7-sidecar-databases)
8. [Coding Agent Delegation](#8-coding-agent-delegation)
9. [Event Bus](#9-event-bus)
10. [Skillkit Libraries](#10-skillkit-libraries)
11. [Dependency Management](#11-dependency-management)
12. [Migration Plan](#12-migration-plan)
13. [Security Considerations](#13-security-considerations)
14. [Open Questions](#14-open-questions)

---

## 1. Philosophy

### Tools vs Skills

The core distinction: **tools are internal, skills are external.**

- **Tools** are Mira's internal state — thinking, replying, remembering, mood logging. They are
  compiled into the binary, have full access to the database and LLM clients, and are never
  sandboxed. These are her mind.
- **Skills** are how Mira interacts with the outside world — web search, scraping, transit,
  weather, email, calendars. They run as separate binaries in a sandbox with a permission model.
  These are her hands.

This separation creates a clean trust boundary. Tools are first-party code that we control
completely. Skills are extensible, modifiable (even by Mira herself), and therefore require
sandboxing and permission enforcement.

### Context Engineering

The entire skills system is designed around one principle: **minimize what touches the context
window.** Every design decision flows from this:

- Skills are discovered via semantic search, not a static table injected every turn
- Only the skill being used gets its instructions loaded into context
- Skill scripts do the heavy lifting (parsing, filtering, formatting) so the model receives
  clean, structured, token-efficient results
- Sidecar databases persist skill results invisibly — no tokens spent on persistence logic
- The coding agent works asynchronously outside Mira's context window entirely

### Multi-Model Orchestra

Her-go is not one model doing everything. It is one orchestrator (Trinity) with multiple
specialized models and agents at its disposal:

| Model/Agent | Role | Runs |
|---|---|---|
| Trinity | Agent orchestration, tool/skill calling | Every turn |
| Deepseek | Conversational response generation | Per reply |
| Gemini Flash | Image understanding | On image receipt |
| Piper/Parakeet | TTS/STT | On voice messages |
| Coding Agent | Skill editing, creation, debugging | Rarely, async |

Each does what it is best at. No overlap.

## 2. Skill Format

### Directory Structure

Each skill is a self-contained directory under `skills/`:

```
skills/
├── web_search/
│   ├── skill.md              # metadata (YAML frontmatter) + instructions (markdown body)
│   ├── main.go               # source code (Go skill)
│   ├── bin/                   # compiled binary output
│   │   └── web_search
│   ├── web_search.db          # sidecar SQLite (harness-managed, skill never touches)
│   └── refs/                  # reference files, examples, schemas
│       └── output_example.json
│
├── recipe_scraper/
│   ├── skill.md
│   ├── main.py               # source code (Python skill)
│   ├── pyproject.toml         # pinned dependencies
│   ├── uv.lock               # lockfile (never auto-updated)
│   ├── .venv/                 # uv-managed virtualenv (skill-local)
│   ├── recipe_scraper.db
│   └── refs/
│       └── schema.json
│
└── skillkit/                  # shared libraries (not a skill itself)
    ├── go/
    │   ├── args.go
    │   ├── output.go
    │   └── http.go
    └── python/
        └── skillkit.py
```

### skill.md Format

YAML frontmatter for machine-readable metadata, markdown body for agent instructions.
The body is only loaded into context AFTER the agent decides to use the skill.

```yaml
---
name: web_search
description: "Search the web for current information using the Tavily API"
version: "1.0.0"
language: go                    # go | python
author: autumn                  # who wrote it

# Trust verification
hash: "sha256:a1b2c3d4..."      # SHA256 of the source file(s), computed by Autumn

# Parameters the skill accepts (agent sees this when skill is loaded)
params:
  - name: query
    type: string
    required: true
    description: "The search query"
  - name: limit
    type: int
    required: false
    default: 5
    description: "Maximum number of results to return"

# Permissions (enforced by the sandbox)
permissions:
  network: true
  domains:                      # allowlisted domains (enforced by proxy for 3rd/4th party)
    - api.tavily.com
  fs:
    - refs/                     # read-only access to reference files
  env:                          # environment variables the skill needs
    - TAVILY_API_KEY
  timeout: 30s                  # max execution time

# Requirements (skill hidden if not met)
requires:
  env: [TAVILY_API_KEY]         # env vars that must be set
  bins: []                      # binaries that must be on PATH
  os: [linux, darwin]           # supported platforms
---

## Instructions

Search the web for current, factual information. Use when the user asks about
recent events, needs up-to-date data, or asks questions beyond your training data.

Return results as JSON with the following structure:
{see refs/output_example.json}
```

### Load-Time Gating

Skills declare requirements in the frontmatter. If requirements are not met, the skill
does not appear in search results at all. This prevents the agent from trying to use
a skill that cannot run.

Checked at startup and on skill directory changes:
- `requires.env` — all listed environment variables must be set
- `requires.bins` — all listed binaries must be on PATH
- `requires.os` — current OS must be in the list

## 3. Trust Model

### Four Trust Tiers

Trust flows one direction: demotion is automatic, promotion is manual.

#### 1st Party — Tools (compiled into binary)

- `think`, `reply`, `done`, `save_fact`, `update_fact`, `no_action`
- `save_self_fact`, `update_persona`, `recall_memories`, `remove_fact`
- `log_mood`, `get_current_time`, `set_location`
- `find_skill`, `run_skill`, `delegate_coding`, `search_history`
- Full database, LLM, and system access. No sandbox. These ARE the harness.
- Author: Autumn. Never modified by the agent.

#### 2nd Party — Vetted Skills

- Built and tested by Autumn (possibly with Mira's help during development).
- Source hash in `skill.md` matches computed hash of source files on disk.
- Full declared permissions honored. Direct network access (no proxy).
- Timeout: up to 30s. Full sidecar DB read/write.
- Example: `web_search`, `weather`, `book_search`

#### 3rd Party — Agent-Modified Skills

- Was 2nd party, but Mira edited the source via the coding agent.
- Source hash in `skill.md` no longer matches computed hash on disk.
- Automatic demotion — no manual step needed to detect.
- Same declared permissions (cannot escalate), but:
  - All network traffic routed through the proxy (transparent to the skill)
  - Sidecar DB access: read-only
  - Timeout: capped at 10s
  - Domain allowlist enforced by proxy
- Stays 3rd party until Autumn reviews changes and re-computes hash.

#### 4th Party — Agent-Created Skills

- Mira created this from scratch via the coding agent. Never vetted.
- No known-good hash exists in `skill.md`.
- Maximum restriction:
  - All network traffic routed through proxy
  - Domain allowlist enforced (must be declared in skill.md)
  - Timeout: 5s
  - No sidecar DB access
  - Rate limited more aggressively
- Promoted to 2nd party only after Autumn reviews and adds hash.

### Hash Verification

Trust is determined by comparing the SHA256 hash stored in `skill.md` against the
computed hash of the actual source file(s) on disk.

```
Stored hash matches disk    → 2nd party (vetted)
Stored hash differs         → 3rd party (modified)
No hash in skill.md         → 4th party (agent-created)
```

When Autumn reviews and approves a skill (whether modified or newly created), she
recomputes the hash and updates `skill.md`. This is the only way to promote trust.

### Trust Demotion Flow

```
2nd party ──(Mira edits source)──► 3rd party ──(Autumn reviews)──► 2nd party

4th party ──(Autumn reviews)──► 2nd party

Never auto-promotes. Always manual.
```

## 4. Skill Discovery

### KNN Semantic Search

Skills are NOT presented as a static table in the agent prompt. Instead, the agent
searches for skills by intent using the same embedding + KNN infrastructure already
used for fact recall (`recall_memories`).

**Why:** A static table grows linearly with skill count. At 100 skills, that is ~2,400
tokens injected every single turn. At 1,000 skills, the system breaks. Semantic search
keeps the per-turn cost at ~50 tokens regardless of how many skills exist.

### Embedding Pipeline

1. On startup (and on skill directory changes), the harness reads each `skill.md`
2. Extracts `name` + `description` from frontmatter
3. Embeds the description using the existing embedding client
4. Stores embeddings (could live in a dedicated `skills.db` or in `her.db`)

### The `find_skill` Tool

A first-party tool (compiled into the binary). The agent calls it with a natural
language query describing what it needs:

```
find_skill("get the bus schedule to downtown")
```

The harness:
1. Embeds the query
2. KNN search against skill description embeddings
3. Returns top-N matches with similarity scores and metadata

```json
{
  "results": [
    {"name": "transit", "description": "Get public transit directions and schedules", "score": 0.92, "trust": "2nd-party"},
    {"name": "scrape", "description": "Extract structured data from a webpage", "score": 0.41, "trust": "2nd-party"}
  ]
}
```

The agent then decides which skill to use (or none). If it picks one, the harness
loads the full `skill.md` body into context so the agent has the instructions and
parameter schema.

### What the Agent Prompt Contains

Instead of a tool table, the agent prompt contains a brief instruction:

```markdown
You have skills available for interacting with the outside world.
Use find_skill(query) to search for a skill by describing what you need.
Use run_skill(name, args) to execute a skill.
Use search_history(skill, query) to check if a skill has cached results.
Use delegate_coding(instruction) to create or fix a skill.
```

~50 tokens. Constant regardless of skill count.

### Skill Creation via Search Miss

If the agent searches for a skill and nothing matches (all scores below threshold),
this is a signal that the capability doesn't exist yet. The agent can then use
`delegate_coding` to create a new skill for the task. This new skill would be
4th-party (agent-created, maximum restriction) until Autumn reviews it.

## 5. Skill Execution

### Execution Flow

When the agent calls `run_skill(name, args)`:

1. **Load skill metadata** from `skill.md` frontmatter
2. **Determine trust level** via hash verification
3. **Check binary freshness** (Go skills: compare `main.go` mtime vs `bin/` mtime)
   - Stale → compile first (`go build`), then run
   - Fresh → use existing binary
4. **Build sandbox constraints** from trust level + declared permissions
5. **Execute** the skill binary in the sandbox
6. **Capture output** (stdout = result, stderr = error log, exit code)
7. **Post-process**:
   - Parse stdout as JSON (with markdown fallback)
   - Auto-write inputs, outputs, and timestamp to sidecar `<skill_name>.db`
   - Return structured result to the agent

### Go Skill Execution

```
echo '{"query":"bus schedule","limit":5}' | ./skills/transit/bin/transit
```

The harness pipes JSON to stdin. The skill binary reads it, does its work, writes
structured JSON to stdout. The harness captures stdout as the result.

If the binary is stale (source newer than binary), the harness compiles first:
```
cd skills/transit && go build -o bin/transit main.go
```

### Python Skill Execution

```
echo '{"url":"https://example.com"}' | uv run --frozen python main.py
```

Uses the skill-local `.venv` managed by uv. `--frozen` ensures no dependency updates
happen at runtime.

### Argument Passing

Skills support both stdin JSON and CLI flags. The skillkit library handles both
transparently:

1. Check if stdin has data (piped JSON) → parse it
2. Otherwise → parse CLI flags
3. Populate the args struct either way

The harness always uses stdin JSON (cleaner, no shell escaping). CLI flags exist so
skills can also be tested manually from the command line.

### Sandbox Constraints by Trust Level

| Constraint | 2nd Party | 3rd Party | 4th Party |
|---|---|---|---|
| Network | Direct | Proxied | Proxied |
| Domain filtering | None | Allowlist enforced | Allowlist enforced |
| Timeout | 30s | 10s | 5s |
| Sidecar DB | Read/Write | Read-only | None |
| Rate limiting | Standard | Standard | Aggressive |
| Env vars | Declared set | Declared set | None |
| File system | refs/ + <skill_name>.db | refs/ (read-only) | refs/ (read-only) |

### Parallel Execution

When Trinity calls multiple `run_skill` tools in the **same LLM iteration** (same
response), the harness runs them concurrently via goroutines. Skills called in
different iterations run sequentially.

Same-iteration is the independence signal — no explicit flag needed. If the model
emitted both calls in one response, it wasn't waiting for one result to inform the
other.

```
Iteration 3: Trinity returns two tool calls:
  run_skill("weather", {"location": "Portland"})
  run_skill("transit", {"from": "home", "to": "downtown"})

  → harness spawns both in goroutines
  → collects results via WaitGroup
  → returns both results to agent in iteration 4
```

### Skill Source Versioning

Before the coding agent edits a skill's source file, the harness creates a timestamped
snapshot:

```
skills/transit/
├── main.go                             # current version
├── main.go.2026-03-27T14-30-00.bak     # previous
├── main.go.2026-03-25T09-15-00.bak     # older
└── ...
```

**Cleanup policy:** keep the greater of 5 snapshots or 7 days of history. Whichever
rule preserves more files wins. This covers both rapid-edit scenarios (many edits in
one day → keep at least 5) and slow-edit scenarios (one edit per week → keep 7 days).

Cleanup runs lazily — checked each time a new snapshot is created.

### Error Handling

- Non-zero exit code → harness returns error message to agent
- Timeout exceeded → process killed, agent receives timeout error
- Invalid JSON output → harness returns raw stdout as string with warning
- Compilation failure → harness returns compiler error to agent

## 6. Network Proxy

### Purpose

3rd and 4th party skills route all network traffic through a transparent proxy.
The skill doesn't know it's being proxied — the harness sets `HTTP_PROXY`/`HTTPS_PROXY`
environment variables when spawning the process, which Go's `net/http` and Python's
`requests` both respect automatically.

2nd party skills get direct network access (no proxy).

### Implementation

A goroutine inside the her binary, using `elazarl/goproxy` as the proxy engine.

**Startup:**
1. Create `goproxy` instance with handler chains
2. Listen on `127.0.0.1:0` (random available port)
3. Store the assigned port for subprocess spawning

**When spawning untrusted skills:**
```go
cmd.Env = append(skillEnv,
    "HTTP_PROXY=http://127.0.0.1:<port>",
    "HTTPS_PROXY=http://127.0.0.1:<port>",
    "http_proxy=http://127.0.0.1:<port>",
    "https_proxy=http://127.0.0.1:<port>",
    "NO_PROXY=",
    "no_proxy=",
)
```

Both uppercase and lowercase variants are set (some tools only check one form).
`NO_PROXY` is explicitly emptied to prevent bypass.

### Proxy Capabilities

**Request filtering (OnRequest):**
- Check domain against skill's declared `permissions.domains` allowlist
- Log: method, URL, headers
- Per-skill rate limiting via `golang.org/x/time/rate`
- Strip sensitive headers

**HTTPS handling (HandleConnect):**
- Domain-level filtering only (no MITM)
- The proxy sees the domain from the CONNECT request but not the payload
- No CA certificate management needed, no TLS termination overhead
- Skills' TLS verification works normally

**Response handling (OnResponse):**
- Log: status code, response size
- Optional payload size limits (prevent OOM from huge responses)

### SSRF Prevention

The critical security layer. Uses `net.Dialer.Control` to check the resolved IP
address AFTER DNS resolution but BEFORE the TCP connection is established. This
prevents DNS rebinding attacks (where an attacker makes a domain resolve to a
private IP after the initial check).

**Library:** `code.dny.dev/ssrf` — auto-syncs with IANA Special Purpose Registries.

**Blocked ranges:**
- `127.0.0.0/8` — loopback
- `10.0.0.0/8` — private
- `172.16.0.0/12` — private
- `192.168.0.0/16` — private
- `169.254.0.0/16` — link-local (cloud metadata endpoint)
- `0.0.0.0/8` — "this" network
- `::1/128` — IPv6 loopback
- `fc00::/7` — IPv6 unique local
- `fe80::/10` — IPv6 link-local

Wired into goproxy's transport:
```go
proxy.Tr = &http.Transport{
    DialContext: (&net.Dialer{
        Timeout: 10 * time.Second,
        Control: ssrf.New().Safe,
    }).DialContext,
}
```

### Limitations

The env-var proxy approach is best-effort, not a hard security boundary. A malicious
Go binary could construct its own `http.Transport` and bypass the proxy entirely.

Mitigation options for future hardening:
- Linux network namespaces (`unshare(CLONE_NEWNET)`) for 4th-party skills
- `seccomp-bpf` to block raw socket syscalls
- iptables rules within the namespace to force proxy usage

For the initial implementation, the proxy covers the common case (both Go stdlib and
Python requests/httpx respect the env vars). Hardening can be layered on later.

### Dependencies

- `github.com/elazarl/goproxy` — forward proxy engine (10+ years mature, 6600+ stars)
- `code.dny.dev/ssrf` — SSRF prevention via net.Dialer.Control
- `golang.org/x/time/rate` — per-skill rate limiting

## 7. Sidecar Databases

### Design

Each skill has its own `<skill_name>.db` — a SQLite database inside the skill directory,
named after the skill (e.g., `skills/web_search/web_search.db`). This is the skill's
full operational memory: execution history, cached results, and embeddings for semantic
search. The harness manages all writes. Mira never touches these databases directly
and does not need to know they exist.

This is the same pattern as TTS: the pipeline runs invisibly in the background.

### Schema

```sql
-- Execution history and cached results
CREATE TABLE runs (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    args        TEXT NOT NULL,       -- JSON input args
    result      TEXT NOT NULL,       -- JSON output (stdout)
    exit_code   INTEGER NOT NULL,
    duration_ms INTEGER NOT NULL,
    timestamp   DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Embeddings for semantic search over cached results
CREATE TABLE embeddings (
    run_id      INTEGER PRIMARY KEY REFERENCES runs(id),
    embedding   BLOB NOT NULL        -- vector for KNN search via sqlite-vec
);
```

Two tables, one DB. `runs` is the execution log, `embeddings` enables semantic
search via `search_history`. The harness embeds a concatenation of the input args
and a summary of the result after each run, storing the vector in `embeddings`.

### Reading Cached Results

The `search_history` tool (first-party, compiled into binary) lets the agent search
across sidecar databases for past results:

```
search_history("web_search", "piper tts")
```

The harness:
1. Opens `skills/web_search/web_search.db`
2. Embeds the query and performs KNN search against embedded run results
3. Returns matching results with freshness metadata

```json
{
  "results": [
    {
      "args": {"query": "piper tts voice models"},
      "result": {"items": [...]},
      "age": "2 days ago",
      "timestamp": "2026-03-25T14:30:00Z"
    }
  ]
}
```

The agent decides whether to reuse the cached result or re-run the skill fresh.

### Access by Trust Level

- **2nd party:** harness reads and writes to <skill_name>.db
- **3rd party:** harness reads only (no writes from modified skills)
- **4th party:** no sidecar DB access at all

### Why Not the Central her.db?

Separation of concerns. Skill state is transient, operational data (cached search
results, scraping history). It is NOT part of Mira's core memory (facts, persona,
metrics, messages). Keeping it separate means:

- Skills are fully portable — copy the directory, get everything
- Deleting a skill cleanly removes all its state
- No schema conflicts or migration headaches with her.db
- Sidecar DBs can be wiped without affecting core functionality

## 8. Coding Agent Delegation

### Why Delegate?

Trinity is an orchestrator. Deepseek is a conversationalist. Neither is a coding model.
Asking Trinity to rewrite Go source code in the agent loop wastes tokens and produces
bad results. Instead, we delegate coding tasks to a purpose-built coding agent.

### Architecture

The `delegate_coding` tool is first-party (compiled into binary). When called, it spawns
an **asynchronous goroutine** that launches a coding agent as a non-interactive subprocess.

**The agent loop is never blocked.** Mira continues chatting while the coding agent works.

### Coding Agent

**Claude Code CLI** in non-interactive mode (`claude --non-interactive`).

Selected for: strong Go capabilities, built-in MCP server support (context7, deepwiki),
and familiarity with the toolchain. Configurable in `config.yaml` if we want to swap
to an alternative (e.g., Crush) later.

The coding agent gets:
- Scoped file access (only the skill directory)
- MCP servers: context7 (library docs), deepwiki (project context)
- Build tools: `go build`, `go vet`, `go test`, `uv sync --frozen`
- Clear success criteria (defined by the delegation call)
- A timeout (configurable, default 5 minutes)

### Delegation Flow

```
Trinity agent loop:
  1. Agent calls delegate_coding({
       skill: "transit",
       instruction: "The transit skill returns malformed JSON.
         The 'time' field is missing from departures. Fix it.",
       success_criteria: "go build && go vet"
     })
  2. Harness spawns goroutine → launches coding agent
  3. Returns immediately: "Task accepted, I'll notify you when done"
  4. Agent continues: reply("I noticed the transit skill is broken.
     I've sent it off to be fixed, I'll let you know!")
  5. Agent calls done()

... Autumn and Mira keep chatting ...

Background goroutine:
  1. Coding agent reads skills/transit/main.go
  2. Identifies the bug, fixes the struct
  3. Runs go build → success
  4. Runs go vet → clean
  5. Returns summary: "Fixed missing time field in Departure struct"

Completion:
  1. Goroutine fires CodingComplete event to event bus
  2. Event triggers a new agent loop run (no user message needed)
  3. Trinity sees: "Background task completed: transit skill fixed"
  4. Trinity re-runs the skill, replies with the result
```

### Skill Creation

The same mechanism handles creating new skills from scratch:

```
delegate_coding({
  action: "create_skill",
  name: "recipe_scraper",
  description: "Scrape recipes from URLs, return structured JSON
    with title, ingredients, steps, and prep time",
  reference_skills: ["scrape", "web_search"],
  success_criteria: "go build && go vet"
})
```

The coding agent:
1. Reads reference skills for patterns and skillkit usage
2. Fetches docs via MCP servers if needed
3. Creates `skill.md` + `main.go` (or `main.py`)
4. Compiles and verifies
5. Returns success → harness embeds the new skill description
6. Skill is immediately available via `find_skill` (as 4th party)

### Trust Implications

- Skills edited by the coding agent: hash drifts → auto-demoted to 3rd party
- Skills created by the coding agent: no hash → 4th party
- The coding agent itself runs with scoped permissions (skill directory only)

### Post-Completion Steps

After the coding agent finishes successfully:
- **Go skills:** binary already compiled by the agent as part of success criteria
- **Python skills:** harness runs `uv sync --frozen` to install/verify dependencies
- **Both:** harness re-embeds the skill description if `skill.md` changed

## 9. Event Bus

### Purpose

The agent loop currently has one entry point: a user message from Telegram. The event
bus adds additional entry points so that background tasks (coding agent completion,
scheduler fires, skill failures) can trigger agent runs without a user message.

This is not a new concept for her-go — the scheduler already triggers `run_prompt` tasks
without user messages. The event bus formalizes and generalizes this pattern.

### Event Sources

| Source | Event Type | What Happens |
|---|---|---|
| Telegram message | `UserMessage` | Normal agent loop (existing flow) |
| Coding agent done | `CodingComplete` | Agent sees result, may re-run skill |
| Scheduler fired | `SchedulerFired` | Existing pattern (run_prompt etc) |
| Skill execution failed | `SkillFailed` | Agent can react (retry, delegate fix) |

### Event Structure

```go
type AgentEvent struct {
    Type            EventType
    // For CodingComplete:
    Skill           string
    Result          string
    Success         bool
    OriginalRequest string  // what the user originally asked for
    // For UserMessage:
    Message         string
    ChatID          int64
    // Common:
    Timestamp       time.Time
}
```

### Agent Loop Entry

The agent loop receives events from a channel. It constructs the appropriate context
depending on the event type:

- **UserMessage:** normal flow — user message becomes the input
- **CodingComplete:** a system event is injected into context:
  `"Background task completed: transit skill fixed and recompiled successfully."`
  The agent sees this, can mention it to the user, and can re-run the skill.
- **SchedulerFired:** existing behavior, already implemented

### User Experience

```
User:   "What's the bus schedule to downtown?"
Mira:   "The transit skill isn't working right now — I've sent it off
         to be fixed. I'll let you know when it's ready!"

         ... 2 minutes of normal chatting ...

Mira:   "Hey! The transit skill is fixed. The next 42 leaves at
         3:15 PM, gets you downtown by 3:40."
```

No extra message needed from the user. The event bus triggers the agent loop,
the agent re-runs the skill, and replies with the result.

## 10. Skillkit Libraries

### Purpose

Shared libraries that every skill imports. Provides a consistent contract for argument
parsing, output formatting, and HTTP requests. Prevents each skill from reinventing
the wheel and ensures compatibility with the harness.

### Go Skillkit

Lives at `skills/skillkit/go/`. Skills import it as a local module.

**args.go — Argument Parsing:**
```go
// Supports both stdin JSON and CLI flags transparently.
// The harness pipes JSON to stdin; CLI flags exist for manual testing.

type Args struct {
    Query string `json:"query" flag:"query" desc:"Search query"`
    Limit int    `json:"limit" flag:"limit" desc:"Max results" default:"5"`
}

func main() {
    var args Args
    skillkit.ParseArgs(&args)  // tries stdin JSON first, falls back to CLI flags
    // ... do work ...
    skillkit.Output(result)    // writes structured JSON to stdout
}
```

**output.go — Structured Output:**
```go
// Writes JSON to stdout. The harness captures this as the skill result.
func Output(v any)               // JSON-encode to stdout
func Error(msg string)           // JSON error to stdout with error field
func Logf(format string, ...)    // Writes to stderr (harness captures for logging)
```

**http.go — Proxy-Aware HTTP Client:**
```go
// Returns an http.Client that respects HTTP_PROXY env vars.
// Skills don't need to know about the proxy — they just use this client.
func HTTPClient() *http.Client
```

### Python Skillkit

Lives at `skills/skillkit/python/skillkit.py`. Skills import it via relative path
or as a local package.

```python
import skillkit

# Argument parsing (stdin JSON or argparse fallback)
args = skillkit.parse_args({
    "query": {"type": str, "required": True, "desc": "Search query"},
    "limit": {"type": int, "default": 5, "desc": "Max results"},
})

# ... do work ...

# Structured output
skillkit.output({"items": results})

# Error output
skillkit.error("API returned 429")

# Logging (to stderr, captured by harness)
skillkit.log("Fetching page 2...")
```

### Why Shared Libraries?

1. **Consistency** — every skill has the same input/output contract
2. **Less skill code** — the boilerplate is handled
3. **Proxy transparency** — skills don't need to know about the proxy
4. **Easier skill creation** — the coding agent uses skillkit as a template
5. **Future extensibility** — add features to all skills at once (e.g., metrics,
   structured logging, retry logic)

## 11. Dependency Management

### Go Skills

Go skills are compiled to static binaries. Dependencies are managed via `go.mod` in
the skill directory (or via the skillkit shared module). No runtime dependency
resolution needed — everything is baked into the binary at compile time.

**Compilation:** `go build -o bin/<skill_name> main.go`

The harness checks binary freshness by comparing source mtime vs binary mtime.
Stale binaries are recompiled before execution. The coding agent also compiles as
part of its success criteria (`go build && go vet`).

### Python Skills

Python dependencies are managed by **uv** with strict version pinning.

**Why uv, not pip/venv:**
- Proper isolation per skill (each gets its own `.venv`)
- No cross-environment contamination
- Deterministic installs via lockfile
- Much faster than pip

**Lockfile discipline:**
```
uv sync --frozen
```
`--frozen` refuses to update the lockfile. If `pyproject.toml` says `requests==2.31.0`,
that's what gets installed. Period. No auto-update to newer versions.

### Why Strict Pinning?

The LiteLLM supply chain attack (March 24, 2026) demonstrated exactly what happens
with loose version pins. Attackers compromised the Trivy security scanner, used it to
exfiltrate PyPI credentials from LiteLLM's CI/CD, and published malicious versions
(1.82.7 and 1.82.8) that stole SSH keys, API keys, and credentials from every machine
that installed them. The malicious versions were live for ~3 hours. LiteLLM is downloaded
~3.4 million times per day.

Root cause: unpinned dependencies in CI/CD. Exactly what would happen if we ran
`uv sync` with loose version specs or auto-updated to latest.

**Our policy:**
- All Python skill dependencies pinned to exact versions in `pyproject.toml`
- `uv.lock` committed alongside the skill
- `uv sync --frozen` at runtime — never resolves, never updates
- Dependency changes only via the coding agent, which updates `pyproject.toml`,
  runs `uv lock`, then `uv sync` — all verified before returning success

### When Syncing Happens

1. **On coding agent completion** — after the agent finishes editing/creating a
   Python skill, the harness runs `uv sync --frozen` automatically
2. **On first run** — if no `.venv` exists, `uv sync --frozen` creates it
3. **On startup** — as a fallback safety net, verify existing venvs match lockfiles

## 12. Migration Plan

### What Moves from Tools to Skills

Current tools that interact with the outside world migrate to skills:

| Current Tool | New Skill | Priority |
|---|---|---|
| `web_search` | `skills/web_search/` | ~~High~~ **DONE** |
| `web_read` | `skills/web_read/` | ~~High~~ **DONE** |
| `book_search` | `skills/book_search/` | ~~Medium~~ **DONE** |
| `view_image` | stays as tool | N/A — needs vision LLM client + base64 image data from agent context |
| `get_current_time` | stays as tool | N/A — internal state |
| `set_location` | stays as tool | N/A — internal state |
| `log_mood` | stays as tool | N/A — internal state |
| Scheduling tools | stays as tools | N/A — tightly coupled to harness |
| Weather (currently in reply pipeline) | stays as tool | N/A — tightly coupled to config location + reply pipeline |

### What Stays as Tools

Everything internal to Mira's state:
- `think`, `reply`, `done` — agent loop mechanics
- `save_fact`, `update_fact`, `remove_fact` — core memory
- `save_self_fact`, `update_persona` — self-knowledge
- `recall_memories` — memory retrieval
- `log_mood`, `get_current_time`, `set_location` — internal context
- `view_image` — needs vision LLM client + base64 image data from agent context
- Weather — tightly coupled to config location + injected in reply pipeline
- `find_skill`, `run_skill`, `delegate_coding`, `search_history` — skills harness
- Scheduling tools — tightly coupled to harness DB and delivery system

### New Tools Added

| Tool | Category | Purpose |
|---|---|---|
| `find_skill` | Skills (hot) | KNN search over skill descriptions |
| `run_skill` | Skills (hot) | Execute a skill in sandbox |
| `delegate_coding` | Skills (hot) | Async coding agent delegation |
| `search_history` | Skills (deferred) | Search sidecar DBs for cached results |

### Implementation Order

1. ~~**Skillkit libraries** — Go and Python shared libs~~ **DONE** (2026-03-27, `skills/skillkit/go/` + `skills/skillkit/python/`)
2. ~~**Skill format and loader** — parse `skill.md`, build registry, embed descriptions~~ **DONE** (`skills/loader/`)
3. ~~**`find_skill` tool** — KNN search over skill embeddings~~ **DONE** (`agent/skills.go`)
4. ~~**`run_skill` tool** — sandbox execution (without proxy initially)~~ **DONE** (`skills/loader/runner.go`)
5. ~~**Migrate `web_search`** — first real skill, proves the architecture end-to-end~~ **DONE** (`skills/web_search/`)
6. ~~**Migrate `web_read` and `book_search`**~~ **DONE** (`skills/web_read/`, `skills/book_search/`)
7. ~~**Startup wiring** — registry init, Bot integration, all RunParams callsites~~ **DONE** (`cmd/run.go`, `cmd/sim.go`, `bot/telegram.go`)
8. **Network proxy** — `elazarl/goproxy` goroutine with SSRF prevention
9. **Trust model** — hash verification, permission enforcement by tier
10. **Sidecar databases** — harness-managed persistence, `search_history` tool
11. **`delegate_coding` tool** — async coding agent with event bus integration
12. **Event bus** — generalized event-driven agent entry points
13. **Skill creation flow** — 4th-party skills via coding agent delegation

## 13. Security Considerations

### Lessons from OpenClaw

OpenClaw is the closest existing project to what we're building. Its skill system is
elegant but its security model is a cautionary tale:

- **CVE-2026-25253 (CVSS 8.8):** Visiting a malicious webpage while OpenClaw ran allowed
  full agent takeover including shell access and credential exfiltration.
- **ClawHavoc:** 800+ malicious skills planted in their registry (~20% at the time).
  Skills distributed infostealers, keyloggers, and backdoors.
- **Cisco audit:** 26% of 31,000 analyzed skills contained at least one vulnerability.
- **Root cause:** sandboxing opt-in (not default), no code signing, skills can instruct
  the agent to run arbitrary shell commands, no capability-based permissions.

### How Our Design Addresses These

| OpenClaw Flaw | Our Mitigation |
|---|---|
| Sandboxing opt-in | Sandboxing default-on, always enforced |
| No code signing | SHA256 hash verification, trust tiers |
| Skills = shell instructions to model | Skills = compiled binaries, not prompt text |
| No capability-based permissions | Explicit permission declarations in skill.md |
| No supply chain verification | No public marketplace; 2nd-party = vetted by Autumn |
| 135K exposed instances | Single-user, runs on local machine only |
| Exec approvals are best-effort | Skills cannot exec arbitrary commands — they run as isolated processes |

### Attack Surface Analysis

**Skill binary itself:**
- Runs in sandbox with scoped permissions
- Network proxied for untrusted skills
- Timeout enforced (process killed)
- File system scoped to skill directory

**DNS rebinding / SSRF:**
- `net.Dialer.Control` checks resolved IP at connect time
- Private/reserved IP ranges blocked (`code.dny.dev/ssrf`)
- Redirect destinations re-validated through same dialer

**Supply chain (Python dependencies):**
- Strict version pinning in `pyproject.toml`
- `uv sync --frozen` — never auto-updates
- Dependency changes only via coding agent (auditable)

**Proxy bypass:**
- Env-var proxy is best-effort (a malicious binary could bypass)
- Future hardening: network namespaces for 4th-party skills
- Acceptable risk for 3rd-party (was once vetted)

**Coding agent producing malicious code:**
- 4th-party skills get maximum restriction
- No sidecar DB access, minimal timeout, proxied network
- Promotion to 2nd-party requires manual Autumn review

**Data exfiltration via allowed domains:**
- A skill allowed to talk to `api.tavily.com` could theoretically encode
  data in query parameters. This is a residual risk.
- Mitigation: proxy logs all requests for audit. Anomaly detection is a
  future hardening step.

### Principle: Defense in Depth

No single layer is a complete security boundary. The design stacks:
1. Trust tiers (who wrote it)
2. Permission declarations (what it can do)
3. Sandbox enforcement (timeout, filesystem scope)
4. Network proxy (where it can connect)
5. SSRF prevention (what IPs it can reach)
6. Audit logging (what it actually did)
7. Manual review gate (promotion requires Autumn)

## 14. Open Questions

### Resolved During Planning

These were discussed and decided:

- **Script language?** → Both Go and Python. Go-first (precompiled binaries for speed),
  Python for library-heavy tasks. Go skills compile to binaries; Python skills run via uv.
- **Skill state storage?** → Sidecar SQLite per skill, not central her.db. Harness manages
  all writes; Mira can read via `search_history` but never writes directly.
- **Trust model?** → Four tiers (1st-4th party), hash-based verification, manual promotion only.
- **Skill discovery?** → KNN semantic search over embedded skill descriptions. Not a static table.
- **Agent prompt changes?** → Dynamic skills manifest injected into context (not modifying
  agent_prompt.md). But with KNN search, even the manifest is replaced by a ~50 token
  instruction to use `find_skill`.
- **Network for untrusted skills?** → Transparent proxy, not blocked. Skills still work but
  traffic is logged, domain-filtered, and SSRF-protected.
- **Skill editing?** → Dedicated coding agent (Claude Code / Crush), async, event-driven
  completion. Mira does not edit code directly in the agent loop.
- **Dependency management?** → uv with strict pinning. No auto-updates. Sync on coding
  agent completion and first run.
- **Compilation?** → Dynamic, on-demand via coding agent. Startup check as fallback.
  Harness also checks freshness before each skill execution.

### Also Resolved (Q&A Session)

- **Multi-skill chaining?** → Yes. Trinity can call `find_skill` and `run_skill` multiple
  times in the same agent loop. Enables combined responses (weather + transit in one reply).

- **Parallel skill execution?** → Yes, when independent. If Trinity calls two `run_skill`
  tools in the same LLM iteration (same response), the harness runs them concurrently via
  goroutines. Different iterations are sequential. No explicit flag needed — same-iteration
  is the independence signal.

- **Skill versioning?** → Timestamped snapshots. Before each edit, the harness copies the
  source file to `main.go.<timestamp>.bak`. Provides a full audit trail of runtime changes
  independent of git (since 3rd/4th-party edits happen at runtime, not via commits).
  Cleanup policy: keep the greater of 5 snapshots or 7 days of history.

- **Skill deletion/cleanup?** → Autumn only. Mira can suggest deprecation but never removes
  a skill directory. Safest approach — no accidental loss of vetted skills.

- **search_history search method?** → Embedding + KNN. Sidecar DB contents are embedded for
  semantic search. Better intent matching ("weather last week" finds the right cached result)
  is worth the compute cost, especially given it avoids unnecessary external API calls.

- **Skill hot-reload?** → Check on execution. Before running a skill, the harness checks if
  the source changed since last compile. No background file watcher, no fsnotify dependency.
  Simpler and sufficient for our use case.

- **Coding agent selection?** → Claude Code CLI (`claude --non-interactive`). Strong Go
  capabilities, MCP server support built in, already familiar tooling.

### Still Open

- **4th-party skill creation details:** The full flow for Mira creating a brand new skill
  from scratch needs its own design session. How does she gather context (context7, deepwiki)?
  What templates does the coding agent use? How does she specify the permission requirements
  for a skill that doesn't exist yet?

- **Parallel execution error handling:** If two skills run in parallel and one fails, does
  the harness still return the successful result? Or does it wait and return both? Likely
  return both (success + error) and let Trinity decide.

- **Snapshot cleanup implementation:** Goroutine on a timer? Or lazy cleanup (check on each
  new snapshot)? Lazy is simpler but could leave stale files if a skill isn't edited often.

- **Embedding storage for skill history:** ~~Where do the sidecar content embeddings live?~~
  **Resolved:** In the skill's `<name>.db` alongside the `runs` table. Separate `embeddings`
  table. Keeps skills fully portable (copy the directory, get everything).

---

## References

- [OpenClaw Skills Documentation](https://docs.openclaw.ai/tools/skills)
- [OpenClaw Security Architecture](https://docs.openclaw.ai/gateway/security)
- [ClawHavoc: 341 Malicious Skills Report](https://clawtrust.ai/blog/openclaw-security-341-malicious-skills-and-what-we-do-about-it)
- [Cisco: Personal AI Agents Are a Security Nightmare](https://blogs.cisco.com/ai/personal-ai-agents-like-openclaw-are-a-security-nightmare)
- [LiteLLM Supply Chain Attack](https://docs.litellm.ai/blog/security-update-march-2026)
- [elazarl/goproxy](https://github.com/elazarl/goproxy)
- [SSRF Prevention in Go](https://www.agwa.name/blog/post/preventing_server_side_request_forgery_in_golang)
- [code.dny.dev/ssrf](https://pkg.go.dev/code.dny.dev/ssrf)
- [safedialer](https://github.com/mccutchen/safedialer)
- [Go and Proxy Servers (Eli Bendersky)](https://eli.thegreenplace.net/2022/go-and-proxy-servers-part-2-https-proxies/)
- [YouTube: Skills and Code Sandboxes](https://youtu.be/IjiaCOt7bP8) — inspiration for this architecture

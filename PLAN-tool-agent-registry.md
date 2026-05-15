# Tool Agent Registry: Agent-Driven Tool Loading

**Status:** Planning
**Date:** 2026-05-15
**Scope:** tools/loader.go, tools/*/tool.yaml, agent/, persona/
**Branch:** `feat/tool-agent-registry`

### Progress

- [ ] **Phase 1: Parse `agent` field in loader** — Add to toolManifest, build agentTools index, standardize YAML to arrays
- [ ] **Phase 2: ToolDefsForAgent()** — New function returns all tools for a given agent name, with hot filtering
- [ ] **Phase 3: Agent-aware hot tools** — HotToolDefs takes agent name, scopes hot tools per agent
- [ ] **Phase 4: Agent-aware use_tools** — Category loading respects agent field
- [ ] **Phase 5: Migrate all agents** — Driver, memory, introspection, dream all use ToolDefsForAgent
- [ ] **Phase 6: Remove LookupToolDefs** — Delete the function, verify no callers remain
- [ ] **Phase 7: Testing** — Unit tests for agent-based loading, sim validation

---

## Problem Statement

The `agent:` field in `tools/<name>/tool.yaml` is decorative — the loader (`tools/loader.go`) doesn't parse it. Each agent manually assembles its tools via `tools.LookupToolDefs([]string{...})` with hardcoded lists. This violates Data Primacy: the tool→agent mapping is defined in YAML but duplicated in Go code.

When a new tool is added, you have to:
1. Create the YAML with `agent: [memory]`
2. Also add the tool name to the hardcoded list in `memory_agent.go`

If you forget step 2, the tool silently doesn't load. If you change step 1 without step 2, YAML and code disagree. One source of truth should drive everything.

## Design

### Core Principle

> **If a tool.yaml says `agent: [memory, introspection]`, that tool is available to those agents. No code changes needed.**

### Phase 1: Parse `agent` Field

**File:** `tools/loader.go`

Add `Agent` to `toolManifest`:
```go
type toolManifest struct {
    Name        string         `yaml:"name"`
    Agent       agentList      `yaml:"agent"`  // NEW — which agents can use this tool
    Description string         `yaml:"description"`
    Hint        string         `yaml:"hint,omitempty"`
    Hot         bool           `yaml:"hot"`
    Category    string         `yaml:"category,omitempty"`
    Parameters  parametersDef  `yaml:"parameters"`
    Trace       *traceSpec     `yaml:"trace,omitempty"`
}
```

**Custom `agentList` type** — handles the YAML format standardization:
```go
type agentList []string

func (a *agentList) UnmarshalYAML(node *yaml.Node) error {
    // Always expects a YAML sequence: [main, memory, introspection]
    var list []string
    if err := node.Decode(&list); err != nil {
        return fmt.Errorf("agent field must be a YAML array (e.g., [main, memory]): %w", err)
    }
    *a = list
    return nil
}
```

**Build agent→tools index at init:**
```go
// agentTools maps agent name → list of tool names belonging to that agent.
var agentTools = map[string][]string{}

// Inside the init loop, after parsing each manifest:
for _, agentName := range manifest.Agent {
    agentTools[agentName] = append(agentTools[agentName], manifest.Name)
}
```

**Standardize all tool.yaml files** to use YAML arrays:
- `agent: main` → `agent: [main]`
- `agent: main, memory` → `agent: [main, memory]`
- `agent: [main, memory, introspection]` → already correct

### Phase 2: ToolDefsForAgent()

**File:** `tools/loader.go`

```go
// ToolDefsForAgent returns all tool definitions for the named agent.
// This is the primary tool loading path — agents call this instead of
// assembling hardcoded lists via LookupToolDefs.
func ToolDefsForAgent(agentName string, cfg *config.Config) []llm.ToolDef {
    names := agentTools[agentName]
    result := make([]llm.ToolDef, 0, len(names))
    for _, name := range names {
        if t, ok := toolDefs[name]; ok {
            result = append(result, ExpandToolIdentity(t, cfg))
        }
    }
    return result
}
```

### Phase 3: Agent-Aware Hot Tools

**File:** `tools/loader.go`

Track which tools are hot per agent:
```go
// agentHotTools maps agent name → hot tool names for that agent.
var agentHotTools = map[string][]string{}

// Built during init:
if manifest.Hot {
    for _, agentName := range manifest.Agent {
        agentHotTools[agentName] = append(agentHotTools[agentName], manifest.Name)
    }
}
```

Update `HotToolDefs` signature:
```go
// HotToolDefs returns the hot (always-loaded) tools for the named agent.
func HotToolDefs(agentName string, cfg *config.Config) []llm.ToolDef {
    names := agentHotTools[agentName]
    result := make([]llm.ToolDef, 0, len(names)+1)
    for _, name := range names {
        if t, ok := toolDefs[name]; ok {
            result = append(result, ExpandToolIdentity(t, cfg))
        }
    }
    // Only add use_tools meta-tool for the driver agent.
    if agentName == "main" {
        result = append(result, UseToolsDef())
    }
    return result
}
```

**Callers to update:**
- `agent/agent.go` — `HotToolDefs(cfg)` → `HotToolDefs("main", cfg)`
- `RenderHotToolsList()` — also needs agent name, or defaults to "main"

### Phase 4: Agent-Aware use_tools

**File:** `tools/use_tools/handler.go`

When the driver calls `use_tools("search")`, filter by both category AND agent:
```go
// Current: loads all tools in the category
members := tools.CategoryMembers(category)

// New: loads only tools in the category that belong to this agent
members := tools.CategoryMembersForAgent(category, "main")
```

Add helper:
```go
func CategoryMembersForAgent(category, agentName string) []string {
    agentSet := make(map[string]bool)
    for _, name := range agentTools[agentName] {
        agentSet[name] = true
    }
    var result []string
    for _, name := range categories[category] {
        if agentSet[name] {
            result = append(result, name)
        }
    }
    return result
}
```

The agent name needs to be on `tools.Context` so the handler can read it:
```go
// tools/context.go
AgentName string // "main", "memory", "introspection", "dream"
```

### Phase 5: Migrate All Agents

**Driver agent (`agent/agent.go`):**
- Hot tools: `HotToolDefs("main", cfg)` (already uses HotToolDefs, just add agent name)
- Set `tctx.AgentName = "main"`

**Memory agent (`agent/memory_agent.go`):**
- Replace `LookupToolDefs([]string{"list_cards", "recall_memories", ...}, cfg)` with `ToolDefsForAgent("memory", cfg)`
- Set `tctx.AgentName = "memory"`

**Introspection agent (`agent/introspection_agent.go`):**
- Replace `LookupToolDefs([]string{"think", "list_cards", ...}, cfg)` with `ToolDefsForAgent("introspection", cfg)`
- Set `tctx.AgentName = "introspection"`

**Dream agent (`persona/memory_dreamer.go`):**
- Replace its `LookupToolDefs` call with `ToolDefsForAgent("dream", cfg)`
- Set `tctx.AgentName = "dream"`
- Update tool.yaml files for dream tools to include `dream` in their agent arrays

### Phase 6: Remove LookupToolDefs

- Delete the function from `tools/loader.go`
- Verify no callers remain (`grep -r LookupToolDefs`)
- Run tests

### Phase 7: Testing

- Unit test: `TestToolDefsForAgent` — verify correct tools returned per agent
- Unit test: `TestHotToolDefsPerAgent` — verify hot scoping works
- Unit test: `TestCategoryMembersForAgent` — verify use_tools filtering
- Existing sim suites should still pass (tool loading is transparent to LLMs)

---

## Tool → Agent Mapping (Current State)

After this refactor, each tool.yaml declares its agents. Here's the planned mapping:

| Tool | main | memory | introspection | dream |
|------|------|--------|---------------|-------|
| think | ✅ | | ✅ | |
| done | ✅ | ✅ | ✅ | ✅ |
| reply | ✅ | | | |
| recall_memories | ✅ | ✅ | ✅ | |
| list_cards | | ✅ | ✅ | ✅ |
| save_memory | | ✅ | | |
| save_self_memory | | ✅ | ✅ | |
| update_memory | | ✅ | ✅ | ✅ |
| remove_memory | | ✅ | | ✅ |
| split_memory | | ✅ | | |
| create_card | | ✅ | | |
| notify_agent | | ✅ | | |
| merge_memories | | | | ✅ |
| skip | | | ✅ | |
| view_image | ✅ | | | |
| web_search | ✅ | | | |
| web_read | ✅ | | | |
| search_books | ✅ | | | |
| get_weather | ✅ | | | |
| get_time | ✅ | | | |
| set_location | ✅ | | | |
| nearby_search | ✅ | | | |
| send_task | ✅ | ✅ | | |
| calendar_* | ✅ | | | |
| shift_hours | ✅ | | | |
| update_persona | | | | ✅ |
| read_card | | | | ✅ |
| update_card | | | | ✅ |

---

## Verification

1. `go build ./...` — compiles after each phase
2. `go test ./...` — all tests pass
3. Run existing sim suites (card-lifecycle, introspection-test) — behavior unchanged
4. `grep -r LookupToolDefs` returns zero results after Phase 6
5. Adding a new tool to an agent requires ONLY editing tool.yaml — no Go code changes

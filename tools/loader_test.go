package tools

import (
	"sort"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestYAMLLoader(t *testing.T) {
	// These are loaded at init time from embedded YAML files.
	// If this test runs, init() already succeeded.

	// Check that all expected tools loaded from YAML.
	// Update this count when tools are added or removed.
	const expectedToolCount = 32
	if len(toolDefs) != expectedToolCount {
		t.Errorf("expected %d tools, got %d", expectedToolCount, len(toolDefs))
	}

	// Spot-check the core tool set (post-Phase-1 scope).
	expectedTools := []string{"think", "done", "reply", "save_memory", "view_image", "web_search", "web_read"}
	for _, name := range expectedTools {
		def, ok := LookupDef(name)
		if !ok {
			t.Errorf("tool %q not found in registry", name)
			continue
		}
		if def.Function.Name != name {
			t.Errorf("tool %q has wrong name: got %q", name, def.Function.Name)
		}
		if def.Function.Description == "" {
			t.Errorf("tool %q has empty description", name)
		}
		if def.Type != "function" {
			t.Errorf("tool %q has type %q, want \"function\"", name, def.Type)
		}
		t.Logf("  loaded: %s (desc=%d chars)", name, len(def.Function.Description))
	}

	// Check hot tools via agent-scoped index.
	mainHot := agentHotTools["main"]
	t.Logf("  main hot tools: %v", mainHot)

	hotSet := map[string]bool{}
	for _, h := range mainHot {
		hotSet[h] = true
	}
	if !hotSet["think"] {
		t.Error("think should be hot for main")
	}
	if !hotSet["done"] {
		t.Error("done should be hot for main")
	}
	// Deferred tools (loaded on demand) must NOT be hot.
	if hotSet["web_search"] {
		t.Error("web_search should NOT be hot (it's a deferred search tool)")
	}
	if !hotSet["recall_memories"] {
		t.Error("recall_memories should be hot for main")
	}

	// Check categories.
	cats := Categories()
	t.Logf("  categories: %v", cats)

	// search category must exist and contain web_search and web_read.
	searchMembers, ok := cats["search"]
	if !ok {
		t.Error("search category not found")
	} else {
		searchSet := map[string]bool{}
		for _, m := range searchMembers {
			searchSet[m] = true
		}
		if !searchSet["web_search"] {
			t.Error("web_search should be in search category")
		}
		if !searchSet["web_read"] {
			t.Error("web_read should be in search category")
		}
	}

	// memory category was removed in Phase 8 — memory tools are now
	// exclusively used by Kimi (memory agent), not loaded via use_tools.
	if _, ok := cats["memory"]; ok {
		t.Error("memory category should not exist (removed — memory tools are Kimi-only)")
	}

	// Check think's parameters have the thought property.
	thinkDef, _ := LookupDef("think")
	params, ok := thinkDef.Function.Parameters.(map[string]interface{})
	if !ok {
		t.Fatal("think parameters is not a map")
	}
	props, ok := params["properties"].(map[string]interface{})
	if !ok {
		t.Fatal("think properties is not a map")
	}
	if _, ok := props["thought"]; !ok {
		t.Error("think is missing 'thought' property")
	}
	required, ok := params["required"].([]string)
	if !ok {
		t.Fatal("think required is not a []string")
	}
	if len(required) != 1 || required[0] != "thought" {
		t.Errorf("think required = %v, want [thought]", required)
	}
}

func TestAgentToolsIndex(t *testing.T) {
	// The agentTools index should contain entries for all 4 agents.
	for _, agent := range []string{"main", "memory", "introspection", "dream"} {
		tools := agentTools[agent]
		if len(tools) == 0 {
			t.Errorf("agent %q has no tools in agentTools index", agent)
		}
		t.Logf("  %s: %d tools %v", agent, len(tools), tools)

		// Should be sorted (deterministic output).
		if !sort.StringsAreSorted(tools) {
			t.Errorf("agent %q tools not sorted", agent)
		}
	}
}

func TestAgentToolsMapping(t *testing.T) {
	// Spot-check key tool→agent assignments from the mapping table.
	tests := []struct {
		tool   string
		agents []string
	}{
		{"think", []string{"main", "introspection"}},
		{"done", []string{"main", "memory", "introspection", "dream"}},
		{"reply", []string{"main"}},
		{"skip", []string{"introspection"}},
		{"save_memory", []string{"memory"}},
		{"merge_memories", []string{"dream"}},
		{"update_persona", []string{"dream"}},
		{"read_card", []string{"memory", "introspection", "dream"}},
		{"recall_memories", []string{"main", "memory", "introspection"}},
	}

	for _, tt := range tests {
		t.Run(tt.tool, func(t *testing.T) {
			for _, agent := range tt.agents {
				found := false
				for _, name := range agentTools[agent] {
					if name == tt.tool {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("tool %q should belong to agent %q but doesn't", tt.tool, agent)
				}
			}
		})
	}
}

func TestToolDefsForAgent_UnknownAgent(t *testing.T) {
	// Unknown agent should return empty slice, not panic.
	defs := ToolDefsForAgent("nonexistent", nil)
	if len(defs) != 0 {
		t.Errorf("expected 0 defs for unknown agent, got %d", len(defs))
	}
}

func TestHotToolDefsPerAgent(t *testing.T) {
	// Main agent should have hot tools.
	mainHot := agentHotTools["main"]
	if len(mainHot) == 0 {
		t.Fatal("main agent has no hot tools")
	}

	// think should be hot for main.
	hasThink := false
	for _, name := range mainHot {
		if name == "think" {
			hasThink = true
		}
	}
	if !hasThink {
		t.Error("think should be hot for main agent")
	}

	// skip should NOT be hot for any agent (hot: false in YAML).
	for agent, hots := range agentHotTools {
		for _, name := range hots {
			if name == "skip" {
				t.Errorf("skip should not be hot for agent %q", agent)
			}
		}
	}

	// Memory agent should have no hot tools that don't belong to it.
	for _, name := range agentHotTools["memory"] {
		found := false
		for _, memTool := range agentTools["memory"] {
			if memTool == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("memory hot tool %q is not in memory's tool set", name)
		}
	}
}

func TestAgentListUnmarshal(t *testing.T) {
	// Array syntax should work.
	t.Run("array", func(t *testing.T) {
		var a agentList
		node := &yaml.Node{}
		if err := yaml.Unmarshal([]byte("[main, memory]"), node); err != nil {
			t.Fatal(err)
		}
		if err := a.UnmarshalYAML(node.Content[0]); err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
		if len(a) != 2 || a[0] != "main" || a[1] != "memory" {
			t.Errorf("expected [main memory], got %v", a)
		}
	})

	// Bare string should error.
	t.Run("bare_string", func(t *testing.T) {
		var a agentList
		node := &yaml.Node{}
		if err := yaml.Unmarshal([]byte("main"), node); err != nil {
			t.Fatal(err)
		}
		if err := a.UnmarshalYAML(node.Content[0]); err == nil {
			t.Error("expected error for bare string, got nil")
		}
	})
}

func TestCategoryMembersForAgent(t *testing.T) {
	// Search category tools should all belong to main.
	members := CategoryMembersForAgent("search", "main")
	if len(members) == 0 {
		t.Fatal("expected search tools for main agent")
	}
	for _, name := range members {
		if _, ok := toolDefs[name]; !ok {
			t.Errorf("CategoryMembersForAgent returned unknown tool %q", name)
		}
	}

	// Memory agent should get zero search tools (no search tools have agent: memory).
	memMembers := CategoryMembersForAgent("search", "memory")
	if len(memMembers) != 0 {
		t.Errorf("memory agent should not have search tools, got %v", memMembers)
	}

	// Unknown category returns empty.
	empty := CategoryMembersForAgent("nonexistent", "main")
	if len(empty) != 0 {
		t.Errorf("expected empty for unknown category, got %v", empty)
	}
}

package tools

import (
	"testing"
)

func TestYAMLLoader(t *testing.T) {
	// These are loaded at init time from embedded YAML files.
	// If this test runs, init() already succeeded.

	// Check that all expected tools loaded from YAML.
	// Update this count when tools are added or removed.
	const expectedToolCount = 26
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

	// Check hot tools.
	hots := HotToolNames()
	t.Logf("  hot tools: %v", hots)

	hotSet := map[string]bool{}
	for _, h := range hots {
		hotSet[h] = true
	}
	if !hotSet["think"] {
		t.Error("think should be hot")
	}
	if !hotSet["done"] {
		t.Error("done should be hot")
	}
	// Deferred tools (loaded on demand) must NOT be hot.
	if hotSet["web_search"] {
		t.Error("web_search should NOT be hot (it's a deferred search tool)")
	}
	if !hotSet["recall_memories"] {
		t.Error("recall_memories should be hot")
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

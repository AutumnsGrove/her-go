package tools

import (
	"testing"
)

func TestYAMLLoader(t *testing.T) {
	// These are loaded at init time from embedded YAML files.
	// If this test runs, init() already succeeded.

	// Check that all 26 tools loaded from YAML.
	if len(toolDefs) != 26 {
		t.Errorf("expected 26 tools, got %d", len(toolDefs))
	}

	// Spot-check a few specific tools.
	expectedTools := []string{"think", "done", "no_action", "get_current_time", "reply", "save_fact", "scan_receipt", "view_image"}
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

	// think, done, no_action should be hot; get_current_time should not.
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
	if hotSet["get_current_time"] {
		t.Error("get_current_time should NOT be hot")
	}

	// Check categories.
	cats := Categories()
	t.Logf("  categories: %v", cats)
	if members, ok := cats["context"]; ok {
		found := false
		for _, m := range members {
			if m == "get_current_time" {
				found = true
			}
		}
		if !found {
			t.Error("get_current_time should be in context category")
		}
	} else {
		t.Error("context category not found")
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

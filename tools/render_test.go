package tools

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Render function tests
// ---------------------------------------------------------------------------

func TestRenderHotToolsList(t *testing.T) {
	result := RenderHotToolsList()

	// Every hot tool should appear as a bolded markdown bullet.
	for _, name := range hotTools {
		target := "- **" + name + "** — "
		if !strings.Contains(result, target) {
			t.Errorf("hot tool %q missing from rendered list", name)
		}
	}

	// Output should have the same number of lines as hot tools.
	lines := strings.Split(result, "\n")
	if len(lines) != len(hotTools) {
		t.Errorf("expected %d lines, got %d", len(hotTools), len(lines))
	}

	// Lines should be sorted (hotTools is sorted at init).
	for i := 1; i < len(lines); i++ {
		if lines[i] < lines[i-1] {
			t.Errorf("lines not sorted: %q comes after %q", lines[i], lines[i-1])
		}
	}
}

func TestRenderCategoryTable(t *testing.T) {
	result := RenderCategoryTable()
	lines := strings.Split(result, "\n")

	// First two lines should be the table header.
	if len(lines) < 3 {
		t.Fatalf("expected at least 3 lines (header + separator + 1 row), got %d", len(lines))
	}
	if !strings.Contains(lines[0], "Category") {
		t.Error("first line should be the header row")
	}
	if !strings.HasPrefix(lines[1], "|---") {
		t.Error("second line should be the separator row")
	}

	// Every category should appear as a bolded cell.
	for name, members := range categories {
		if !strings.Contains(result, "| **"+name+"**") {
			t.Errorf("category %q missing from rendered table", name)
		}
		// Every tool in the category should appear in its row.
		for _, tool := range members {
			if !strings.Contains(result, tool) {
				t.Errorf("tool %q (category %q) missing from rendered table", tool, name)
			}
		}
	}

	// Number of data rows should equal number of categories.
	dataRows := len(lines) - 2 // subtract header + separator
	if dataRows != len(categories) {
		t.Errorf("expected %d data rows, got %d", len(categories), dataRows)
	}
}

// ---------------------------------------------------------------------------
// Drift guard tests — catch missing hints
// ---------------------------------------------------------------------------

func TestHotToolHintsComplete(t *testing.T) {
	// Every hot tool should have a hint (from YAML or fallback).
	// This catches the case where someone adds a new hot tool YAML
	// but forgets the hint field.
	for _, name := range hotTools {
		hint := hotToolHints[name]
		if hint == "" {
			// Check if there's a description to fall back on.
			def, ok := toolDefs[name]
			if !ok || def.Function.Description == "" {
				t.Errorf("hot tool %q has no hint and no description fallback", name)
			} else {
				t.Logf("hot tool %q: no explicit hint, falling back to description", name)
			}
		}
	}
}

func TestCategoryHintsComplete(t *testing.T) {
	// Every category from tool YAMLs should have a hint in categories.yaml.
	for name := range categories {
		if _, ok := categoryHints[name]; !ok {
			t.Errorf("category %q has no hint in categories.yaml", name)
		}
	}

	// Every hint in categories.yaml should correspond to an actual category.
	for name := range categoryHints {
		if _, ok := categories[name]; !ok {
			t.Errorf("categories.yaml has hint for %q but no tools use that category", name)
		}
	}
}

// ---------------------------------------------------------------------------
// Helper tests
// ---------------------------------------------------------------------------

func TestFirstSentence(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Hello world. More text.", "Hello world."},
		{"Single sentence", "Single sentence"},
		{"Line one\nLine two", "Line one"},
		{"Ends with period.", "Ends with period."},
		{"", ""},
	}
	for _, tt := range tests {
		got := firstSentence(tt.input)
		if got != tt.want {
			t.Errorf("firstSentence(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// TestReplaceBetweenMarkers lives in agent/prompt_test.go since
// replaceBetweenMarkers is in the agent package.

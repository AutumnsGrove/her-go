package tools

// Unit tests for the memory pipeline quality gates.
//
// These tests call the gate logic directly by driving ExecSaveMemory with a
// minimal tools.Context (nil EmbedClient, nil ClassifierLLM). That means
// only gates 1-2 (style, length) fire — the dedup and classifier gates are
// skipped when embedding/classifier clients are absent. That's exactly what
// we want for isolated unit tests.
//
// In Go, test files live in the same package as the code they test (no
// separate test package needed). The _test.go suffix tells the compiler
// these files are test-only — never included in a normal build.

import (
	"strings"
	"testing"

	"her/config"
)

// minimalCtx returns a tools.Context with only what the style/length gates
// need. EmbedClient and ClassifierLLM are nil so dedup and classifier skip.
func minimalCtx() *Context {
	return &Context{
		Cfg: &config.Config{
			Identity: config.IdentityConfig{Her: "Mira", User: "Autumn"},
		},
	}
}

// TestStyleGate verifies that memories containing blocked patterns are rejected.
func TestStyleGate(t *testing.T) {
	t.Run("em_dash_blocked", func(t *testing.T) {
		ctx := minimalCtx()
		// Trailing em-dash — sentence hangs with "—" at the end.
		// Mid-sentence em-dashes are fine; only trailing ones are blocked.
		result := ExecSaveMemory(`{"memory":"User loves hiking —","category":"preference","tags":"outdoors, hiking"}`, "user", ctx)
		if !strings.HasPrefix(result, "rejected:") {
			t.Errorf("expected rejection for trailing em-dash memory, got: %s", result)
		}
	})

	t.Run("ai_slop_blocked", func(t *testing.T) {
		ctx := minimalCtx()
		result := ExecSaveMemory(`{"memory":"User wants to leverage her Go skills for backend projects","category":"work","tags":"go, backend"}`, "user", ctx)
		if !strings.HasPrefix(result, "rejected:") {
			t.Errorf("expected rejection for 'leverage' memory, got: %s", result)
		}
	})

	t.Run("clean_memory_passes", func(t *testing.T) {
		ctx := minimalCtx()
		result := ExecSaveMemory(`{"memory":"User prefers stealth builds in FromSoft games","category":"preference","tags":"games, elden ring, stealth"}`, "user", ctx)
		// Without embed/classifier, save should succeed (returns "saved user memory ID=...")
		// or fail only due to nil store — not due to a style/length gate rejection.
		if strings.HasPrefix(result, "rejected:") {
			t.Errorf("expected clean memory to pass style gate, got: %s", result)
		}
	})
}

// TestLengthGate verifies that memories exceeding maxMemoryLength are rejected.
func TestLengthGate(t *testing.T) {
	t.Run("over_limit_rejected", func(t *testing.T) {
		ctx := minimalCtx()
		// Build a memory that's exactly maxMemoryLength+1 characters.
		longMem := strings.Repeat("x", maxMemoryLength+1)
		argsJSON := `{"memory":"` + longMem + `","category":"other","tags":"test"}`
		result := ExecSaveMemory(argsJSON, "user", ctx)
		if !strings.HasPrefix(result, "rejected:") {
			t.Errorf("expected rejection for %d-char memory, got: %s", maxMemoryLength+1, result)
		}
		if !strings.Contains(result, "characters") {
			t.Errorf("rejection message should mention character count, got: %s", result)
		}
	})

	t.Run("at_limit_passes_style_gate", func(t *testing.T) {
		ctx := minimalCtx()
		// Exactly maxMemoryLength characters — clean content, should pass style+length.
		// Uses only simple alphanumeric content to avoid triggering style gate.
		exactMem := "User studies Go programming and finds " + strings.Repeat("the language clean", 1)
		// Pad to exactly maxMemoryLength with safe characters.
		for len(exactMem) < maxMemoryLength {
			exactMem += "a"
		}
		exactMem = exactMem[:maxMemoryLength] // trim to exact limit
		argsJSON := `{"memory":"` + exactMem + `","category":"other","tags":"test"}`
		result := ExecSaveMemory(argsJSON, "user", ctx)
		// Should NOT be rejected by style or length gate.
		if strings.Contains(result, "characters (max") {
			t.Errorf("memory at exactly maxMemoryLength should not be rejected by length gate, got: %s", result)
		}
	})
}

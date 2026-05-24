package gateway

// Tests for commands.go — specifically the shape and completeness of the
// CommandDef slice produced by buildCommandsFromBot.
//
// buildCommandsFromBot takes a *bot.Bot but only closes over it — it does
// not dereference the pointer during the slice construction itself. That
// means we can safely pass nil and inspect the returned names and
// descriptions without pulling in a real database or bot instance.
//
// We do NOT invoke any Handler here. Handler tests belong in bot/ where
// the full pipeline context is available.

import (
	"testing"

	"her/bot"
)

// expectedGatewayCommands is the canonical list of command names that
// buildCommandsFromBot must produce. If a command is added or removed in
// commands.go, this test will catch the drift.
var expectedGatewayCommands = []string{
	"help",
	"stats",
	"facts",
	"forget",
	"traces",
	"status",
	"reflect",
	"reflections",
	"persona",
	"dream",
	"dreamlog",
	"lasttrace",
	"rollup",
}

func TestBuildCommandsFromBot_ReturnsExpectedNames(t *testing.T) {
	// Passing a nil *bot.Bot is safe here: buildCommandsFromBot only
	// constructs closures — it never calls through the pointer at build time.
	defs := buildCommandsFromBot((*bot.Bot)(nil))

	if len(defs) != len(expectedGatewayCommands) {
		t.Errorf("command count: got %d, want %d", len(defs), len(expectedGatewayCommands))
	}

	// Index by name for O(1) lookup.
	got := make(map[string]CommandDef, len(defs))
	for _, d := range defs {
		got[d.Name] = d
	}

	for _, name := range expectedGatewayCommands {
		d, ok := got[name]
		if !ok {
			t.Errorf("missing command %q", name)
			continue
		}
		if d.Description == "" {
			t.Errorf("command %q has empty Description", name)
		}
		if d.Handler == nil {
			t.Errorf("command %q has nil Handler", name)
		}
	}
}

func TestBuildCommandsFromBot_NoDuplicateNames(t *testing.T) {
	defs := buildCommandsFromBot((*bot.Bot)(nil))

	seen := make(map[string]int)
	for _, d := range defs {
		seen[d.Name]++
	}
	for name, count := range seen {
		if count > 1 {
			t.Errorf("command %q appears %d times, want 1", name, count)
		}
	}
}

func TestBuildCommandsFromBot_NoAdapterSpecificCommands(t *testing.T) {
	// These commands must NOT appear in the gateway-level list — each
	// adapter handles them directly (e.g. /clear resets adapter state,
	// /compact needs the conversation ID from the adapter).
	adapterOnly := []string{"clear", "compact"}

	defs := buildCommandsFromBot((*bot.Bot)(nil))
	byName := make(map[string]struct{}, len(defs))
	for _, d := range defs {
		byName[d.Name] = struct{}{}
	}

	for _, name := range adapterOnly {
		if _, ok := byName[name]; ok {
			t.Errorf("command %q is adapter-specific and must not appear in buildCommandsFromBot output", name)
		}
	}
}

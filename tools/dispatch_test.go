package tools

import (
	"strings"
	"testing"
)

// TestExecuteUnknownTool verifies that calling an unregistered tool
// returns a clear error string rather than panicking. This is the
// log-and-continue resilience behavior: bad tool calls are survivable.
func TestExecuteUnknownTool(t *testing.T) {
	result := Execute("nonexistent_tool_xyz", `{}`, nil)
	if !strings.Contains(result, "unknown tool") {
		t.Errorf("expected 'unknown tool' error, got: %q", result)
	}
	// Must NOT panic — this test passing is the proof.
}

// TestExecuteMalformedJSON verifies that truncated/invalid JSON arguments
// return a clear error rather than crashing the handler. Models hit token
// limits mid-generation and produce broken JSON; the agent loop needs to
// survive this and feed the error back to the model.
func TestExecuteMalformedJSON(t *testing.T) {
	// Register a dummy handler so we can test the JSON validation path.
	// We're testing Execute's validation layer, not the handler itself.
	Register("_test_tool_malformed", func(argsJSON string, ctx *Context) string {
		return "handler called: " + argsJSON
	})
	defer func() {
		// Clean up the test handler. In production init() runs once —
		// but tests reuse the same process, so we tidy up.
		delete(toolHandlers, "_test_tool_malformed")
	}()

	// Truncated JSON (as if max_tokens cut it off mid-generation).
	result := Execute("_test_tool_malformed", `{"fact":"likes coffee`, nil)
	if !strings.Contains(result, "malformed JSON") {
		t.Errorf("expected malformed JSON error, got: %q", result)
	}

	// Valid JSON should reach the handler normally.
	result = Execute("_test_tool_malformed", `{"fact":"likes coffee"}`, nil)
	if !strings.Contains(result, "handler called") {
		t.Errorf("expected handler to be called with valid JSON, got: %q", result)
	}
}

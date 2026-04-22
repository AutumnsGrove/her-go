// Package think — tests for the think tool handler.
//
// The think tool is intentionally forgiving: every input, valid or not,
// returns "tool call complete". Tests here verify that contract holds and
// that no input can cause a panic or a different return value.
package think

import (
	"testing"

	"her/tools"
)

// TestHandle_ValidThought verifies the happy path: a well-formed JSON object
// with a non-empty thought string returns the expected completion sentinel.
// If this breaks, the agent loop will misread the tool result and may stall
// or loop incorrectly.
func TestHandle_ValidThought(t *testing.T) {
	result := Handle(`{"thought": "I should check the user's calendar before replying"}`, nil)

	const want = "tool call complete"
	if result != want {
		t.Errorf("Handle with valid thought = %q, want %q", result, want)
	}
}

// TestHandle_EmptyThought verifies that an empty thought string is accepted
// without error. The model may legitimately emit an empty think call when
// it has nothing new to deliberate on.
func TestHandle_EmptyThought(t *testing.T) {
	result := Handle(`{"thought": ""}`, nil)

	const want = "tool call complete"
	if result != want {
		t.Errorf("Handle with empty thought = %q, want %q", result, want)
	}
}

// TestHandle_MalformedJSON verifies the forgiving contract: even a completely
// broken JSON payload must not panic and must still return "tool call complete".
// Think is unique in that it has no meaningful error path — there is nothing
// to fail. This mirrors the comment in handler.go about not wanting the agent
// to interpret any other return value as a user message.
func TestHandle_MalformedJSON(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"truncated", `{"thought":`},
		{"empty_string", ``},
		{"not_json", `i am not json`},
		{"wrong_type", `{"thought": 42}`},
		{"null", `null`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := Handle(tc.input, nil)

			const want = "tool call complete"
			if result != want {
				t.Errorf("Handle(%q) = %q, want %q", tc.input, result, want)
			}
		})
	}
}

// TestHandle_ContextIgnored verifies that passing a non-nil context does not
// change the return value. Think never reads the context — this test guards
// against an accidental nil-deref if the signature ever starts reading ctx.
func TestHandle_ContextIgnored(t *testing.T) {
	ctx := &tools.Context{}

	result := Handle(`{"thought": "double-checking with a real context"}`, ctx)

	const want = "tool call complete"
	if result != want {
		t.Errorf("Handle with real context = %q, want %q", result, want)
	}
}

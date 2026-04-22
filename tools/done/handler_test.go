// Package done — tests for the done tool handler.
//
// The done tool has a single responsibility: set ctx.DoneCalled = true and
// return the turn-complete sentinel string. The agent loop in agent/agent.go
// checks DoneCalled after every tool call to decide whether to continue
// iterating — if this flag is not set, the agent will run forever until the
// iteration cap kicks in.
package done

import (
	"testing"

	"her/tools"
)

// TestHandle_SetsDoneCalled verifies the core invariant: after Handle returns,
// ctx.DoneCalled must be true. If this breaks, the agent loop will never exit
// cleanly and will burn through its iteration budget on every turn.
func TestHandle_SetsDoneCalled(t *testing.T) {
	ctx := &tools.Context{DoneCalled: false}

	Handle("", ctx)

	if !ctx.DoneCalled {
		t.Error("ctx.DoneCalled = false after Handle; agent loop will not terminate")
	}
}

// TestHandle_ReturnValue verifies the exact return string the agent model
// sees as the tool result. The model uses this to understand the turn is
// over — changing it without updating the agent prompt risks confusing the
// model into calling done again or emitting a spurious reply.
func TestHandle_ReturnValue(t *testing.T) {
	ctx := &tools.Context{}

	result := Handle("", ctx)

	const want = "tool call complete, turn complete"
	if result != want {
		t.Errorf("Handle() = %q, want %q", result, want)
	}
}

// TestHandle_EmptyArgs verifies that an empty args string (the normal case —
// done takes no parameters) does not panic or return an error. The model
// frequently passes an empty object or empty string here.
func TestHandle_EmptyArgs(t *testing.T) {
	cases := []struct {
		name string
		args string
	}{
		{"empty_string", ""},
		{"empty_object", "{}"},
		{"null", "null"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := &tools.Context{}

			result := Handle(tc.args, ctx)

			const want = "tool call complete, turn complete"
			if result != want {
				t.Errorf("Handle(%q) = %q, want %q", tc.args, result, want)
			}
			if !ctx.DoneCalled {
				t.Errorf("Handle(%q) did not set DoneCalled", tc.args)
			}
		})
	}
}

// TestHandle_Idempotent verifies that calling done twice on the same context
// does not panic and leaves DoneCalled true after both calls. The agent loop
// might call done in edge cases where the model emits it redundantly —
// idempotency prevents a double-done from crashing the turn.
func TestHandle_Idempotent(t *testing.T) {
	ctx := &tools.Context{}

	first := Handle("", ctx)
	second := Handle("", ctx)

	const want = "tool call complete, turn complete"
	if first != want {
		t.Errorf("first Handle() = %q, want %q", first, want)
	}
	if second != want {
		t.Errorf("second Handle() = %q, want %q", second, want)
	}
	if !ctx.DoneCalled {
		t.Error("ctx.DoneCalled = false after two Handle calls")
	}
}

// TestHandle_AlreadyDone verifies that a context that already has DoneCalled
// set (e.g. from a previous call) is handled safely. DoneCalled starts true
// and should remain true — Handle must not reset it to false.
func TestHandle_AlreadyDone(t *testing.T) {
	ctx := &tools.Context{DoneCalled: true}

	Handle("", ctx)

	if !ctx.DoneCalled {
		t.Error("ctx.DoneCalled was reset to false by Handle; it should only ever be set, never cleared")
	}
}

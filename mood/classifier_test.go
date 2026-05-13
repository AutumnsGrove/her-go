package mood

// TestClassifyReal_ContextTimeout verifies that classifyReal fails open
// when the context is already cancelled before the LLM call resolves.
//
// "Fail open" means: when the classifier is unavailable or times out,
// we allow the write to proceed rather than blocking it. A missed
// classifier pass is less harmful than silently dropping real mood data.

import (
	"context"
	"testing"

	"her/llm"
)

// TestClassifyReal_ContextTimeout passes a pre-cancelled context to
// classifyReal and asserts it returns (true, "") immediately —
// the fail-open outcome. This tests the select branch:
//
//	case <-ctx.Done():
//	    return true, ""
//
// The goroutine may still be running the LLM call (or blocked trying
// to), but the function must not block waiting for it.
func TestClassifyReal_ContextTimeout(t *testing.T) {
	// Cancel the context before we even call classifyReal. The select
	// inside classifyReal must pick ctx.Done() over the ch channel
	// because ctx.Done() is already closed.
	//
	// In Go, a cancelled context's Done() channel is already closed,
	// so any select that includes <-ctx.Done() will take that branch
	// immediately — the same way Python's asyncio.Event fires instantly
	// for any awaiter if it's already set.
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel

	// A real (but pointed-at-nothing) client. classifyReal will launch
	// a goroutine that tries to call it, but the ctx.Done() branch
	// fires before that goroutine can write to the channel — so the
	// HTTP call is irrelevant to this test's assertion.
	//
	// We use a dummy URL; the goroutine may error trying to dial it,
	// but that's fine — classifyReal recovers panics and the ctx.Done()
	// branch exits before the goroutine result arrives.
	client := llm.NewClient("http://127.0.0.1:0", "test-key", "mood-classifier", 0, 16)

	inf := &Inference{
		Labels: []string{"Stressed"},
		Note:   "user sounds overwhelmed",
	}
	turns := []Turn{{
		Role:            "user",
		ScrubbedContent: "I'm absolutely exhausted today",
	}}

	ok, reason := classifyReal(ctx, client, inf, turns)

	// Fail-open: cancelled context must yield (true, "").
	if !ok {
		t.Errorf("classifyReal with cancelled context: ok = false, want true (fail-open)")
	}
	if reason != "" {
		t.Errorf("classifyReal with cancelled context: reason = %q, want \"\" (fail-open)", reason)
	}
}

// TestClassifyReal_NilClient verifies the nil-guard at the top of
// classifyReal: a nil client must always return (true, "") without
// panicking. This covers the "classifier not configured" production
// path where classifierLLM is nil.
func TestClassifyReal_NilClient(t *testing.T) {
	inf := &Inference{
		Labels: []string{"Sad"},
		Note:   "test note",
	}
	turns := []Turn{{Role: "user", ScrubbedContent: "feeling down"}}

	ok, reason := classifyReal(context.Background(), nil, inf, turns)

	if !ok {
		t.Errorf("nil client: ok = false, want true")
	}
	if reason != "" {
		t.Errorf("nil client: reason = %q, want \"\"", reason)
	}
}

// TestClassifyReal_NilInference verifies the nil-guard for a nil
// Inference — same fail-open contract.
func TestClassifyReal_NilInference(t *testing.T) {
	client := llm.NewClient("http://127.0.0.1:0", "test-key", "mood-classifier", 0, 16)

	ok, reason := classifyReal(context.Background(), client, nil, nil)

	if !ok {
		t.Errorf("nil inference: ok = false, want true")
	}
	if reason != "" {
		t.Errorf("nil inference: reason = %q, want \"\"", reason)
	}
}

// TestClassifyReal_VerdictParsing verifies that the classifier correctly
// accepts REAL and rejects FICTION/NOT_SELF verdicts from the LLM.
// This uses the same scriptedServer helper from agent_test.go — both
// files are in package mood, so the type is shared.
func TestClassifyReal_VerdictParsing(t *testing.T) {
	cases := []struct {
		name    string
		verdict string
		wantOK  bool
	}{
		{"REAL", "REAL", true},
		{"REAL with trailing period", "REAL.", true},
		{"FICTION", "FICTION", false},
		{"NOT_SELF", "NOT_SELF", false},
		{"REJECT", "REJECT", false},
		// Unexpected verdict → fail open
		{"unknown verdict", "MAYBE", true},
	}

	inf := &Inference{Labels: []string{"Sad"}, Note: "test"}
	turns := []Turn{{Role: "user", ScrubbedContent: "I feel really sad"}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newScriptedServer(t, tc.verdict)
			client := llm.NewClient(server.URL(), "test-key", "mood-classifier", 0, 16)

			ok, _ := classifyReal(context.Background(), client, inf, turns)
			if ok != tc.wantOK {
				t.Errorf("verdict %q: ok = %v, want %v", tc.verdict, ok, tc.wantOK)
			}
		})
	}
}

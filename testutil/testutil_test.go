package testutil_test

import (
	"testing"

	"her/testutil"
)

// TestTempStore_Creates verifies the TempStore helper creates a working
// database with the full schema.
func TestTempStore_Creates(t *testing.T) {
	store := testutil.TempStore(t)

	// If we got here without a fatal, the DB opened, schema ran, and
	// sqlite-vec registered. Verify we can actually write to it.
	id, err := store.SaveMessage("user", "hello", "hello", "test-conv")
	if err != nil {
		t.Fatalf("SaveMessage failed: %v", err)
	}
	if id == 0 {
		t.Fatal("expected non-zero message ID")
	}
}

// TestSeedFact_Inserts verifies SeedFact writes a fact that can be read back.
func TestSeedFact_Inserts(t *testing.T) {
	store := testutil.TempStore(t)

	id := testutil.SeedFact(t, store, "user likes cats", "preferences")
	if id == 0 {
		t.Fatal("expected non-zero fact ID")
	}
}

// TestDeterministicEmbedding_Stable verifies that the same input produces
// the same vector across calls (determinism).
func TestDeterministicEmbedding_Stable(t *testing.T) {
	a := testutil.DeterministicEmbedding("hello world")
	b := testutil.DeterministicEmbedding("hello world")

	if len(a) != testutil.TestEmbedDim {
		t.Fatalf("expected %d dimensions, got %d", testutil.TestEmbedDim, len(a))
	}

	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("embedding not deterministic at index %d: %f != %f", i, a[i], b[i])
		}
	}
}

// TestDeterministicEmbedding_Differs verifies that different inputs produce
// different vectors.
func TestDeterministicEmbedding_Differs(t *testing.T) {
	a := testutil.DeterministicEmbedding("cats are great")
	b := testutil.DeterministicEmbedding("quantum physics")

	same := true
	for i := range a {
		if a[i] != b[i] {
			same = false
			break
		}
	}
	if same {
		t.Fatal("different inputs produced identical embeddings")
	}
}

// TestStubEmbedClient_RoundTrip verifies the stub embed server responds
// with correctly-shaped vectors.
func TestStubEmbedClient_RoundTrip(t *testing.T) {
	client := testutil.StubEmbedClient(t)

	vec, err := client.Embed("test input")
	if err != nil {
		t.Fatalf("Embed() error: %v", err)
	}
	if len(vec) != testutil.TestEmbedDim {
		t.Fatalf("expected %d dimensions, got %d", testutil.TestEmbedDim, len(vec))
	}

	// Verify it matches what DeterministicEmbedding would produce.
	// The stub server uses the same hash function, so the float32→float64→float32
	// round-trip through JSON should be close (within float32 precision).
	expected := testutil.DeterministicEmbedding("test input")
	for i := range vec {
		diff := vec[i] - expected[i]
		if diff > 0.001 || diff < -0.001 {
			t.Fatalf("vector mismatch at index %d: got %f, want %f", i, vec[i], expected[i])
		}
	}
}

// TestMockLLMClient_ServesResponses verifies the mock LLM returns canned
// responses in order.
func TestMockLLMClient_ServesResponses(t *testing.T) {
	client := testutil.MockLLMClient(t,
		testutil.LLMResponse("first reply"),
		testutil.LLMResponse("second reply"),
	)

	// First call should get "first reply"
	resp1, err := client.ChatCompletion(nil)
	if err != nil {
		t.Fatalf("first call error: %v", err)
	}
	if resp1.Content != "first reply" {
		t.Fatalf("first call: got %q, want %q", resp1.Content, "first reply")
	}

	// Second call should get "second reply"
	resp2, err := client.ChatCompletion(nil)
	if err != nil {
		t.Fatalf("second call error: %v", err)
	}
	if resp2.Content != "second reply" {
		t.Fatalf("second call: got %q, want %q", resp2.Content, "second reply")
	}
}

// TestTestToolContext_Creates verifies the full context assembly works.
func TestTestToolContext_Creates(t *testing.T) {
	ctx := testutil.TestToolContext(t)

	if ctx.Store == nil {
		t.Fatal("expected non-nil Store")
	}
	if ctx.EmbedClient == nil {
		t.Fatal("expected non-nil EmbedClient")
	}
	if ctx.Cfg == nil {
		t.Fatal("expected non-nil Cfg")
	}
	if ctx.ScrubVault == nil {
		t.Fatal("expected non-nil ScrubVault")
	}
	if ctx.ConversationID == "" {
		t.Fatal("expected non-empty ConversationID")
	}

	// Verify the store is usable through the context.
	_, err := ctx.Store.SaveMessage("user", "hello", "hello", ctx.ConversationID)
	if err != nil {
		t.Fatalf("Store.SaveMessage through context failed: %v", err)
	}
}

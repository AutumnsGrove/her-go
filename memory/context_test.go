package memory_test

import (
	"strings"
	"testing"

	"her/memory"
	"her/testutil"
)

// ---------------------------------------------------------------------------
// BuildMemoryContext
// ---------------------------------------------------------------------------

// TestBuildContext_NoFacts verifies that an empty DB produces no context
// string (rather than crashing or producing a template with empty sections).
func TestBuildContext_NoFacts(t *testing.T) {
	store := testutil.TempStore(t)

	text, injected, err := memory.BuildMemoryContext(store, 10, nil, "Autumn", 0)
	if err != nil {
		t.Fatalf("BuildMemoryContext: %v", err)
	}
	if text != "" {
		t.Errorf("expected empty context, got %q", text)
	}
	if len(injected) != 0 {
		t.Errorf("expected 0 injected facts, got %d", len(injected))
	}
}

// TestBuildContext_SemanticFacts verifies that passing pre-searched relevant
// facts causes them to appear in the context string.
func TestBuildContext_SemanticFacts(t *testing.T) {
	store := testutil.TempStore(t)

	// Seed a user fact so blendFacts can find it.
	emb := testutil.DeterministicEmbedding("user loves hiking")
	id, _ := store.SaveFact("user loves hiking", "hobbies", "user", 0, 7, emb, emb, "", "")

	// Simulate what the agent does: pre-search for relevant facts and pass them in.
	relevantFacts := []memory.Fact{
		{ID: id, Fact: "user loves hiking", Category: "hobbies", Subject: "user", Importance: 7, Distance: 0.1},
	}

	text, injected, err := memory.BuildMemoryContext(store, 10, relevantFacts, "Autumn", 0)
	if err != nil {
		t.Fatalf("BuildMemoryContext: %v", err)
	}

	if !strings.Contains(text, "user loves hiking") {
		t.Error("expected context to contain the seeded fact")
	}
	if len(injected) == 0 {
		t.Error("expected at least 1 injected fact")
	}
	if injected[0].Source != "semantic" {
		t.Errorf("expected source %q, got %q", "semantic", injected[0].Source)
	}
}

// TestBuildContext_SelfFacts_BackfillByImportance verifies that self-facts
// appear in context even without semantic relevance (importance backfill).
func TestBuildContext_SelfFacts_BackfillByImportance(t *testing.T) {
	store := testutil.TempStore(t)

	// Save a self-fact (bot's own knowledge).
	store.SaveFact("I tend to use too many exclamation marks", "voice", "self", 0, 8, nil, nil, "", "")

	// Pass nil relevantFacts — no semantic search happened.
	// Self-facts should still appear via importance backfill.
	text, injected, err := memory.BuildMemoryContext(store, 10, nil, "Autumn", 0)
	if err != nil {
		t.Fatalf("BuildMemoryContext: %v", err)
	}

	if !strings.Contains(text, "exclamation marks") {
		t.Error("expected self-fact to appear via importance backfill")
	}

	// Should be tagged as "importance" source.
	foundImportance := false
	for _, inj := range injected {
		if inj.Source == "importance" {
			foundImportance = true
			break
		}
	}
	if !foundImportance {
		t.Error("expected at least one importance-sourced fact")
	}
}

// TestBuildContext_UserFacts_NoBackfill verifies that user-facts do NOT
// backfill by importance — only semantic results are included.
func TestBuildContext_UserFacts_NoBackfill(t *testing.T) {
	store := testutil.TempStore(t)

	// Save a user fact, but pass no relevant facts (empty semantic search).
	store.SaveFact("user has a dog", "pets", "user", 0, 9, nil, nil, "", "")

	text, _, err := memory.BuildMemoryContext(store, 10, []memory.Fact{}, "Autumn", 0)
	if err != nil {
		t.Fatalf("BuildMemoryContext: %v", err)
	}

	// User-facts should NOT appear without semantic relevance.
	if strings.Contains(text, "user has a dog") {
		t.Error("user-fact should not appear without semantic relevance (no backfill)")
	}
}

// TestBuildContext_MaxDistanceFilter verifies that semantically distant
// facts are filtered out even if they're the nearest KNN results.
func TestBuildContext_MaxDistanceFilter(t *testing.T) {
	store := testutil.TempStore(t)

	emb := testutil.DeterministicEmbedding("some random text")
	id, _ := store.SaveFact("irrelevant fact", "general", "user", 0, 5, emb, emb, "", "")

	// Pass the fact with a high distance (far from query).
	relevantFacts := []memory.Fact{
		{ID: id, Fact: "irrelevant fact", Category: "general", Subject: "user", Importance: 5, Distance: 0.8},
	}

	// Set maxSemanticDist to 0.5 — the fact at distance 0.8 should be filtered.
	text, _, err := memory.BuildMemoryContext(store, 10, relevantFacts, "Autumn", 0.5)
	if err != nil {
		t.Fatalf("BuildMemoryContext: %v", err)
	}

	if strings.Contains(text, "irrelevant fact") {
		t.Error("fact beyond max distance should be filtered out")
	}
}

// TestBuildContext_FactCategorization verifies that facts are grouped by
// category in the output.
func TestBuildContext_FactCategorization(t *testing.T) {
	store := testutil.TempStore(t)

	emb1 := testutil.DeterministicEmbedding("user likes rock climbing")
	emb2 := testutil.DeterministicEmbedding("user allergic to shellfish")
	id1, _ := store.SaveFact("user likes rock climbing", "hobbies", "user", 0, 7, emb1, emb1, "", "")
	id2, _ := store.SaveFact("user allergic to shellfish", "health", "user", 0, 8, emb2, emb2, "", "")

	relevantFacts := []memory.Fact{
		{ID: id1, Fact: "user likes rock climbing", Category: "hobbies", Subject: "user", Importance: 7, Distance: 0.05},
		{ID: id2, Fact: "user allergic to shellfish", Category: "health", Subject: "user", Importance: 8, Distance: 0.1},
	}

	text, _, err := memory.BuildMemoryContext(store, 10, relevantFacts, "Autumn", 0)
	if err != nil {
		t.Fatalf("BuildMemoryContext: %v", err)
	}

	// Both categories should appear as bold headers.
	if !strings.Contains(text, "**Hobbies:**") {
		t.Error("expected Hobbies category header")
	}
	if !strings.Contains(text, "**Health:**") {
		t.Error("expected Health category header")
	}
}

// ---------------------------------------------------------------------------
// FilterRedundantFacts
// ---------------------------------------------------------------------------

// TestFilterRedundantFacts_NilEmbed verifies the no-op case: when there's
// no embed client, all facts pass through unchanged.
func TestFilterRedundantFacts_NilEmbed(t *testing.T) {
	facts := []memory.Fact{
		{ID: 1, Fact: "test fact"},
	}

	result := memory.FilterRedundantFacts(facts, nil, nil)
	if len(result) != 1 {
		t.Errorf("expected 1 fact with nil embed client, got %d", len(result))
	}
}

// TestFilterRedundantFacts_NoMessages verifies that with no recent messages,
// nothing is filtered.
func TestFilterRedundantFacts_NoMessages(t *testing.T) {
	embedClient := testutil.StubEmbedClient(t)
	facts := []memory.Fact{
		{ID: 1, Fact: "test fact"},
	}

	result := memory.FilterRedundantFacts(facts, []memory.Message{}, embedClient)
	if len(result) != 1 {
		t.Errorf("expected 1 fact with no messages, got %d", len(result))
	}
}

// TestFilterRedundantFacts_FiltersRedundant verifies that a fact nearly
// identical to a recent message gets filtered out.
func TestFilterRedundantFacts_FiltersRedundant(t *testing.T) {
	embedClient := testutil.StubEmbedClient(t)

	// The fact and the message say the same thing.
	sameText := "user really loves hiking in the mountains on weekends"
	factEmb := testutil.DeterministicEmbedding(sameText)

	facts := []memory.Fact{
		{ID: 1, Fact: sameText, EmbeddingText: factEmb},
	}
	messages := []memory.Message{
		{ContentScrubbed: sameText},
	}

	result := memory.FilterRedundantFacts(facts, messages, embedClient)

	// Identical text → identical embedding → similarity 1.0 → filtered.
	if len(result) != 0 {
		t.Errorf("expected redundant fact to be filtered, got %d facts", len(result))
	}
}

// TestFilterRedundantFacts_KeepsUnrelated verifies that unrelated facts
// are not filtered out.
func TestFilterRedundantFacts_KeepsUnrelated(t *testing.T) {
	embedClient := testutil.StubEmbedClient(t)

	// Use a cached embedding that is DIFFERENT from what the embed client
	// would produce for the message text. This guarantees the fact and
	// message have low similarity regardless of our hash function's
	// distribution.
	//
	// Note: our DeterministicEmbedding hash is compact (4 dims), so
	// unrelated strings can still collide. By manually setting the fact's
	// embedding to a known orthogonal vector, we avoid false positives.
	factEmb := []float32{1, 0, 0, 0}
	facts := []memory.Fact{
		{ID: 1, Fact: "user has a pet iguana", EmbeddingText: factEmb},
	}
	// The stub embed client will hash this message text into a vector
	// that is NOT [1,0,0,0], so cosine similarity will be low.
	messages := []memory.Message{
		{ContentScrubbed: "zzzzzzzzzzzzzzzzzzzzzzz completely different topic altogether"},
	}

	result := memory.FilterRedundantFacts(facts, messages, embedClient)

	if len(result) != 1 {
		t.Errorf("expected unrelated fact to be kept, got %d facts", len(result))
	}
}

// TestFilterRedundantFacts_SkipsShortMessages verifies that very short
// messages (< 20 chars) are not used for redundancy checks.
func TestFilterRedundantFacts_SkipsShortMessages(t *testing.T) {
	embedClient := testutil.StubEmbedClient(t)

	factEmb := testutil.DeterministicEmbedding("ok")
	facts := []memory.Fact{
		{ID: 1, Fact: "ok", EmbeddingText: factEmb},
	}
	messages := []memory.Message{
		{ContentScrubbed: "ok"}, // too short to embed
	}

	result := memory.FilterRedundantFacts(facts, messages, embedClient)

	// Short message is skipped → no comparison → fact kept.
	if len(result) != 1 {
		t.Errorf("expected fact kept (short message skipped), got %d facts", len(result))
	}
}

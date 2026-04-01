// Package testutil provides shared test helpers for her-go.
//
// The idea: individual test files should focus on BEHAVIOR, not setup
// boilerplate. These helpers handle the plumbing — creating temp databases,
// stubbing external services, assembling tool contexts — so each test
// starts with a one-liner like TempStore(t) and gets straight to asserting.
//
// Every helper takes *testing.T and uses t.Cleanup() for automatic teardown.
// This is Go's equivalent of pytest fixtures — no manual cleanup needed.
package testutil

import (
	"math"
	"path/filepath"
	"testing"

	"her/memory"
)

// TestEmbedDim is a small dimension for test embeddings.
// Real embeddings are 768-dimensional, but for tests we only need a few
// floats to verify KNN search and cosine similarity work correctly.
// Smaller = faster tests, less memory, same correctness guarantees.
const TestEmbedDim = 4

// TempStore creates a temporary SQLite database with the full her-go schema
// and returns a ready-to-use *memory.Store. The database lives in t.TempDir()
// so it's automatically cleaned up when the test finishes.
//
// This is the foundation for most integration tests — real SQL, real
// sqlite-vec vectors, no mocks. The only thing we DON'T get is real
// embedding vectors (those come from StubEmbedClient).
//
// Usage:
//
//	func TestSomething(t *testing.T) {
//	    store := testutil.TempStore(t)
//	    // store is ready — all tables exist, WAL mode enabled
//	}
func TempStore(t *testing.T) *memory.Store {
	t.Helper()

	// t.TempDir() gives us a unique directory per test that Go's test
	// runner cleans up automatically. Each test gets its own isolated DB.
	dbPath := filepath.Join(t.TempDir(), "test.db")

	store, err := memory.NewStore(dbPath, TestEmbedDim)
	if err != nil {
		t.Fatalf("TempStore: failed to create store: %v", err)
	}

	// t.Cleanup registers a function that runs when the test finishes
	// (pass or fail). It's like Python's addCleanup() or a finally block.
	// LIFO order — if multiple cleanups are registered, they run in
	// reverse order (same as defer).
	t.Cleanup(func() {
		store.Close()
	})

	return store
}

// SeedFact is a shortcut for inserting a single fact with sensible defaults.
// It generates a deterministic embedding from the fact text so the fact is
// searchable via KNN. Returns the fact's database ID.
//
// store.SaveFact has a wide signature (9 params) because it mirrors the
// full SQL schema. This helper fills in the boring parts so tests can
// focus on the text content.
func SeedFact(t *testing.T, store *memory.Store, text, category string) int64 {
	t.Helper()

	emb := DeterministicEmbedding(text)
	id, err := store.SaveFact(
		text,      // fact
		category,  // category
		"user",    // subject
		0,         // sourceMessageID (no parent message)
		5,         // importance (middle of 1-10 range)
		emb,       // embedding (for vec_facts KNN index)
		emb,       // embeddingText (for dedup checks)
		"",        // tags
		"",        // context
	)
	if err != nil {
		t.Fatalf("SeedFact: failed to save %q: %v", text, err)
	}
	return id
}

// DeterministicEmbedding produces a reproducible TestEmbedDim-length vector
// from any string. The values are derived from a simple hash — not
// semantically meaningful, but stable and unique enough that KNN search
// returns sensible results in tests (similar strings → similar hashes).
//
// This is the same approach used by the stub embed server, so vectors
// from SeedFact and from StubEmbedClient are comparable.
func DeterministicEmbedding(text string) []float32 {
	vec := make([]float32, TestEmbedDim)
	// FNV-style rolling hash: cheap, deterministic, decent distribution.
	// We rotate through the vector dimensions so each position gets a
	// different value.
	h := uint32(2166136261) // FNV offset basis
	for i, b := range []byte(text) {
		h ^= uint32(b)
		h *= 16777619 // FNV prime
		// Deposit into the current dimension
		vec[i%TestEmbedDim] += float32(h%1000) / 1000.0
	}
	// Normalize to unit length so cosine similarity works properly.
	// Without normalization, longer strings would have larger magnitudes
	// and similarity scores would be skewed by length, not content.
	var norm float32
	for _, v := range vec {
		norm += v * v
	}
	if norm > 0 {
		norm = float32(1.0 / float64(sqrt32(norm)))
		for i := range vec {
			vec[i] *= norm
		}
	}
	return vec
}

// sqrt32 is a float32 square root. Go's math.Sqrt takes float64,
// so this avoids casts at every call site.
func sqrt32(x float32) float32 {
	return float32(math.Sqrt(float64(x)))
}

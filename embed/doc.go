// Package embed provides embedding vector generation and similarity comparison
// utilities for semantic deduplication and search.
//
// This package is the single source of truth for:
//   - Embedding vector generation (via OpenAI-compatible APIs)
//   - Cosine similarity computation
//   - Similarity threshold constants
//   - Pattern helpers for finding best matches
//
// # Embedding Generation
//
// The Client talks to any OpenAI-compatible embedding API (LM Studio, Ollama,
// OpenRouter, OpenAI) and returns float32 vectors. These vectors are stored in
// SQLite using the sqlite-vec extension for fast KNN queries.
//
//	client := embed.NewClient("http://localhost:1234/v1", "nomic-embed-text-v1.5", "", 768)
//	vec, err := client.Embed("User has a dog named Max")
//	// vec is []float32 with 768 dimensions
//
// # Similarity Comparison
//
// CosineSimilarity computes the angle between two vectors. Returns 1.0 for
// identical vectors, 0.0 for orthogonal (unrelated), and values in between
// for varying degrees of similarity.
//
//	sim := embed.CosineSimilarity(vecA, vecB)
//	if sim >= 0.85 {
//	    fmt.Println("Too similar - likely a duplicate")
//	}
//
// # Similarity Patterns
//
// The package provides two patterns for similarity-based operations:
//
// ## Pattern 1: Find Best Match
//
// FindBestMatch scans a collection of candidate vectors and returns the best
// match that exceeds a threshold. Use this when you have multiple candidates
// and need to find which one is most similar.
//
//	// Build candidate map (ID → vector)
//	candidates := make(map[int64][]float32)
//	for _, memory := range memories {
//	    candidates[memory.ID] = memory.Embedding
//	}
//
//	// Find best match with optional early exit
//	id, sim, matched := embed.FindBestMatch(queryVec, candidates, 0.85, false)
//	if matched {
//	    fmt.Printf("Duplicate found: memory #%d with %.0f%% similarity\n", id, sim*100)
//	}
//
// The earlyExit parameter controls the performance vs accuracy tradeoff:
//   - earlyExit=false: scans all candidates, returns true best (use for logging/debugging)
//   - earlyExit=true: returns immediately when threshold exceeded (use for performance)
//
// ## Pattern 2: Fire-and-Forget Comparison
//
// SimilarText embeds two strings and returns their similarity in one call.
// Use this for one-off comparisons where you don't need to store vectors.
//
//	sim, err := client.SimilarText("User has a dog", "User owns a dog")
//	if err != nil {
//	    return err
//	}
//	if sim > 0.85 {
//	    fmt.Println("These mean the same thing")
//	}
//
// # Threshold Constants
//
// The package defines similarity thresholds for different use cases. These
// constants document the semantic meaning of different similarity ranges and
// provide a single source of truth for threshold values.
//
// ## Binary Deduplication Thresholds
//
// Use these when you have a simple "duplicate or not" decision:
//
//	embed.DefaultSimilarityThreshold           = 0.85  // Memory dedup (tag/text)
//	embed.ContextMemorySimilarityThreshold     = 0.70  // Same-day context memories
//	embed.ConversationRedundancyThreshold      = 0.60  // Filter memories echoing chat
//
// ## Multi-Tier Deduplication Thresholds
//
// Use these when you need "drop vs update vs new" logic:
//
//	embed.HighSimilarityThreshold    = 0.80  // Nearly identical → DROP
//	embed.MediumSimilarityThreshold  = 0.55  // Same entity, different details → UPDATE
//
// Example tier pattern (used by mood agent):
//
//	if similarity >= embed.HighSimilarityThreshold {
//	    // Drop as duplicate - nearly identical content
//	    return ActionDrop
//	} else if similarity >= embed.MediumSimilarityThreshold && /* domain checks */ {
//	    // Update existing entry - same entity, refine with new details
//	    return ActionUpdate
//	} else {
//	    // Create new entry - clearly different content
//	    return ActionNew
//	}
//
// # Design Principles
//
// This package follows her-go's "code translates data, never defines it" principle:
//
//   - Threshold values are defined as named constants, not magic numbers in logic
//   - Each constant has documentation explaining its purpose and use case
//   - Constants can be overridden via config.yaml where needed
//   - Helper functions are pure (no side effects, no hidden state)
//
// # Performance Considerations
//
// ## Embedding Generation
//
// Each Embed() call makes an HTTP request to the embedding API. For bulk
// operations, consider:
//   - Caching: Store embeddings in the database for reuse
//   - Batching: Group multiple embedding requests if the API supports it
//   - Fallback: Use cached embeddings when available, compute on-the-fly only when needed
//
// ## Similarity Computation
//
// CosineSimilarity is O(n) where n is the vector dimension (typically 768).
// For large-scale similarity search (thousands of candidates):
//   - Use sqlite-vec's KNN queries instead of in-memory comparison
//   - Set earlyExit=true on FindBestMatch for performance-sensitive paths
//   - Pre-filter candidates before similarity comparison when possible
//
// # Testing
//
// The package includes comprehensive test coverage in similarity_test.go:
//   - Edge cases (empty vectors, mismatched dimensions)
//   - Boundary conditions (threshold exact matches)
//   - Error propagation (embedding API failures)
//   - Performance scenarios (early exit vs full scan)
//
// Run tests with:
//
//	go test ./embed -v
//	go test ./embed -race  # Check for data races
//
// # Example: Memory Deduplication
//
// This example shows the full pattern for deduplicating a new memory against
// existing memories:
//
//	func checkDuplicate(newText string, existingMemories []Memory, client *embed.Client) (bool, int64, float64) {
//	    // Generate embedding for new text
//	    newVec, err := client.Embed(newText)
//	    if err != nil {
//	        return false, 0, 0
//	    }
//
//	    // Build candidate map (with backfill for missing embeddings)
//	    candidates := make(map[int64][]float32)
//	    for _, m := range existingMemories {
//	        vec := m.Embedding
//	        if len(vec) == 0 {
//	            // Backfill missing embedding
//	            vec, err = client.Embed(m.Content)
//	            if err != nil {
//	                continue
//	            }
//	            // Store for future use
//	            store.UpdateEmbedding(m.ID, vec)
//	        }
//	        candidates[m.ID] = vec
//	    }
//
//	    // Find best match
//	    id, sim, matched := embed.FindBestMatch(newVec, candidates, embed.DefaultSimilarityThreshold, false)
//	    return matched, id, sim
//	}
package embed

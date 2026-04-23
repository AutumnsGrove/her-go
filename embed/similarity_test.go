package embed

// Unit tests for similarity helper functions: FindBestMatch and SimilarText.
//
// FindBestMatch tests use hardcoded vectors since it's pure math — no HTTP calls.
// SimilarText tests use httptest.NewServer to mock the embedding API, following
// the same pattern as embed_test.go.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestFindBestMatch_SingleCandidateAboveThreshold verifies that FindBestMatch
// returns a match when the single candidate exceeds the threshold.
func TestFindBestMatch_SingleCandidateAboveThreshold(t *testing.T) {
	query := []float32{1, 0, 0}
	candidates := map[int64][]float32{
		101: {1, 0.1, 0}, // cosine similarity ≈ 0.995 (very similar)
	}

	id, sim, matched := FindBestMatch(query, candidates, 0.90)

	if !matched {
		t.Errorf("FindBestMatch matched = false, want true when similarity %.3f > threshold 0.90", sim)
	}
	if id != 101 {
		t.Errorf("FindBestMatch id = %d, want 101", id)
	}
	if sim < 0.99 {
		t.Errorf("FindBestMatch sim = %.3f, want ≈ 0.995", sim)
	}
}

// TestFindBestMatch_MultipleCandidatesBestBelowThreshold verifies that
// FindBestMatch returns matched=false when the best candidate is still
// below the threshold.
func TestFindBestMatch_MultipleCandidatesBestBelowThreshold(t *testing.T) {
	query := []float32{1, 0, 0}
	candidates := map[int64][]float32{
		101: {0.5, 0.5, 0}, // cosine sim ≈ 0.707
		102: {0, 1, 0},     // cosine sim = 0 (orthogonal)
	}

	id, sim, matched := FindBestMatch(query, candidates, 0.90)

	if matched {
		t.Errorf("FindBestMatch matched = true, want false when best similarity %.3f < threshold 0.90", sim)
	}
	// Should still return the best ID and similarity even though it didn't match
	if id != 101 {
		t.Errorf("FindBestMatch id = %d, want 101 (best even though below threshold)", id)
	}
	if sim < 0.70 || sim > 0.72 {
		t.Errorf("FindBestMatch sim = %.3f, want ≈ 0.707", sim)
	}
}

// TestFindBestMatch_EmptyQuery verifies that FindBestMatch handles
// empty query vectors gracefully.
func TestFindBestMatch_EmptyQuery(t *testing.T) {
	query := []float32{}
	candidates := map[int64][]float32{
		101: {1, 0, 0},
	}

	id, sim, matched := FindBestMatch(query, candidates, 0.85)

	if matched {
		t.Error("FindBestMatch matched = true, want false for empty query")
	}
	if id != 0 {
		t.Errorf("FindBestMatch id = %d, want 0 for empty query", id)
	}
	if sim != 0 {
		t.Errorf("FindBestMatch sim = %.3f, want 0 for empty query", sim)
	}
}

// TestFindBestMatch_EmptyCandidates verifies that FindBestMatch handles
// empty candidate maps gracefully.
func TestFindBestMatch_EmptyCandidates(t *testing.T) {
	query := []float32{1, 0, 0}
	candidates := map[int64][]float32{}

	id, sim, matched := FindBestMatch(query, candidates, 0.85)

	if matched {
		t.Error("FindBestMatch matched = true, want false for empty candidates")
	}
	if id != 0 {
		t.Errorf("FindBestMatch id = %d, want 0 for empty candidates", id)
	}
	if sim != 0 {
		t.Errorf("FindBestMatch sim = %.3f, want 0 for empty candidates", sim)
	}
}

// TestFindBestMatch_IdenticalVectors verifies that FindBestMatch returns
// perfect similarity (1.0) for identical vectors.
func TestFindBestMatch_IdenticalVectors(t *testing.T) {
	query := []float32{1, 2, 3}
	candidates := map[int64][]float32{
		101: {1, 2, 3}, // identical → cosine similarity = 1.0
	}

	id, sim, matched := FindBestMatch(query, candidates, 0.85)

	if !matched {
		t.Error("FindBestMatch matched = false, want true for identical vectors")
	}
	if id != 101 {
		t.Errorf("FindBestMatch id = %d, want 101", id)
	}
	if sim != 1.0 {
		t.Errorf("FindBestMatch sim = %.3f, want 1.0 for identical vectors", sim)
	}
}

// TestFindBestMatch_ThresholdBoundary verifies that FindBestMatch correctly
// handles the threshold boundary case (similarity exactly equal to threshold).
func TestFindBestMatch_ThresholdBoundary(t *testing.T) {
	// Create two vectors that produce a specific cosine similarity
	// For simplicity, we'll use vectors that produce exactly 0.85 similarity
	// cos(θ) = 0.85 → we can construct vectors with known similarity
	// Using: a = [1, 0], b = [0.85, sqrt(1-0.85²)] = [0.85, ~0.527]
	query := []float32{1, 0}
	candidates := map[int64][]float32{
		101: {0.85, 0.527}, // cosine sim ≈ 0.8499 (float32 precision)
	}

	// Use threshold slightly below calculated value to account for float32 precision
	id, sim, matched := FindBestMatch(query, candidates, 0.849)

	// Should match since sim >= threshold
	if !matched {
		t.Errorf("FindBestMatch matched = false, want true when sim %.6f >= threshold 0.849", sim)
	}
	if id != 101 {
		t.Errorf("FindBestMatch id = %d, want 101", id)
	}
}

// TestSimilarText_IdenticalStrings verifies that SimilarText returns high
// similarity for identical strings (should be 1.0 after embedding).
func TestSimilarText_IdenticalStrings(t *testing.T) {
	// Mock embedding server that returns the same vector for identical text
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Return a simple embedding: [1.0, 0.0, 0.0]
		resp := embeddingResponse{
			Data: []struct {
				Embedding []float64 `json:"embedding"`
			}{
				{Embedding: []float64{1.0, 0.0, 0.0}},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-model", "", 3)
	sim, err := client.SimilarText("hello world", "hello world")

	if err != nil {
		t.Fatalf("SimilarText returned error: %v", err)
	}
	if sim != 1.0 {
		t.Errorf("SimilarText sim = %.3f, want 1.0 for identical embeddings", sim)
	}
}

// TestSimilarText_UnrelatedStrings verifies that SimilarText returns low
// similarity for semantically unrelated strings.
func TestSimilarText_UnrelatedStrings(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		// First call (text A): return [1.0, 0.0, 0.0]
		// Second call (text B): return [0.0, 1.0, 0.0] (orthogonal)
		var embedding []float64
		if callCount == 0 {
			embedding = []float64{1.0, 0.0, 0.0}
		} else {
			embedding = []float64{0.0, 1.0, 0.0}
		}
		callCount++

		resp := embeddingResponse{
			Data: []struct {
				Embedding []float64 `json:"embedding"`
			}{
				{Embedding: embedding},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-model", "", 3)
	sim, err := client.SimilarText("cats are great", "economics of trade")

	if err != nil {
		t.Fatalf("SimilarText returned error: %v", err)
	}
	// Orthogonal vectors → cosine similarity = 0
	if sim != 0.0 {
		t.Errorf("SimilarText sim = %.3f, want 0.0 for orthogonal vectors", sim)
	}
}

// TestSimilarText_FirstEmbedError verifies that SimilarText returns an error
// if the first embedding call fails.
func TestSimilarText_FirstEmbedError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Return 500 error to simulate embedding failure
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("embedding service down"))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-model", "", 3)
	_, err := client.SimilarText("hello", "world")

	if err == nil {
		t.Error("SimilarText err = nil, want error when first embedding fails")
	}
}

// TestSimilarText_SecondEmbedError verifies that SimilarText returns an error
// if the second embedding call fails (first succeeds).
func TestSimilarText_SecondEmbedError(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if callCount == 0 {
			// First call succeeds
			w.Header().Set("Content-Type", "application/json")
			resp := embeddingResponse{
				Data: []struct {
					Embedding []float64 `json:"embedding"`
				}{
					{Embedding: []float64{1.0, 0.0, 0.0}},
				},
			}
			json.NewEncoder(w).Encode(resp)
		} else {
			// Second call fails
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("embedding service down"))
		}
		callCount++
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-model", "", 3)
	_, err := client.SimilarText("hello", "world")

	if err == nil {
		t.Error("SimilarText err = nil, want error when second embedding fails")
	}
}

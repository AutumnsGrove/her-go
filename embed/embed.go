// Package embed provides an embedding client for computing text similarity.
// It talks to any OpenAI-compatible embedding API (LM Studio, Ollama, OpenAI, etc.)
// and provides cosine similarity for comparing vectors.
//
// This is used for memory deduplication — before saving a new fact, we embed it
// and compare against existing facts to catch semantic duplicates like
// "User has a dog named Max" vs "User owns a dog called Max".
package embed

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"time"
)

// Dimension is the vector size the embedding model produces.
// nomic-embed-text-v1.5 → 768, OpenAI text-embedding-3-small → 1536, etc.
// Set this based on your model; it's used to size the sqlite-vec virtual table.
const DefaultDimension = 768

// Threshold constants for different similarity use cases.
// These define the cosine similarity cutoffs (0.0-1.0, where 1.0 = identical)
// for various deduplication and filtering operations.
const (
	// DefaultSimilarityThreshold is the standard duplicate detection threshold.
	// Used for tag-based and text-based memory deduplication when no category-
	// specific override applies. Configurable via config.yaml embed.similarity_threshold.
	DefaultSimilarityThreshold = 0.85

	// ContextMemorySimilarityThreshold is a lower threshold for "context"
	// category memories (same-day situational snapshots). Catches duplicates
	// like "at Bolivar feeling low" vs "at Bolivar doing grounding exercise"
	// that tag-based comparison misses.
	ContextMemorySimilarityThreshold = 0.70

	// ConversationRedundancyThreshold controls how similar a memory must be
	// to a recent message before it's filtered out as redundant. Lower than
	// memory-vs-memory dedup because we're comparing structured memories
	// against freeform conversation text.
	ConversationRedundancyThreshold = 0.60
)

// Client wraps an HTTP client configured to talk to an embedding API.
// It's similar in shape to llm.Client — base URL + model name.
//
// In Python you might use the openai library directly:
//
//	client.embeddings.create(model="...", input="hello")
//
// In Go, we make the HTTP call ourselves — it's just a POST with JSON.
type Client struct {
	baseURL    string
	model      string
	apiKey     string // optional — needed for remote APIs (OpenRouter, OpenAI), empty for local (LM Studio, Ollama)
	Dimension  int    // vector dimension (e.g. 768 for nomic-embed-text-v1.5)
	httpClient *http.Client
}

// NewClient creates an embedding client pointed at the given server.
// For LM Studio: baseURL = "http://localhost:1234/v1", apiKey = ""
// For Ollama: baseURL = "http://localhost:11434/v1", apiKey = ""
// For OpenRouter: baseURL = "https://openrouter.ai/api/v1", apiKey = "sk-or-..."
// dimension is the vector size your model produces (768 for nomic, 4096 for qwen3-embedding).
// Pass 0 to use DefaultDimension.
func NewClient(baseURL, model, apiKey string, dimension int) *Client {
	if dimension <= 0 {
		dimension = DefaultDimension
	}
	return &Client{
		baseURL:   baseURL,
		model:     model,
		apiKey:    apiKey,
		Dimension: dimension,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// embeddingRequest is the JSON body sent to the /embeddings endpoint.
type embeddingRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

// embeddingResponse is the JSON response from the /embeddings endpoint.
// The actual vector lives in Data[0].Embedding.
// Note: the API returns float64 JSON numbers, but we convert to float32
// immediately — embedding vectors don't need 64-bit precision, and
// sqlite-vec (our vector index) works with float32 natively.
type embeddingResponse struct {
	Data []struct {
		Embedding []float64 `json:"embedding"`
	} `json:"data"`
}

// GetDimension returns the vector dimension this client produces.
// Used by the sidecar DB to create correctly-sized vec0 virtual tables.
func (c *Client) GetDimension() int {
	return c.Dimension
}

// IsAvailable checks whether the embedding server is reachable and responding.
// Uses a short 2-second timeout so startup checks don't block — the main
// httpClient has a 30-second timeout which is fine for real embed calls but
// too slow for a health probe.
func (c *Client) IsAvailable() bool {
	// Build a minimal embed request — same shape as a real call but with
	// a tiny throw-away input. We don't use the response, just the status.
	body := []byte(`{"model":"` + c.model + `","input":"health"}`)
	req, err := http.NewRequest("POST", c.baseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	// Short-lived client — don't reuse c.httpClient since its 30s timeout
	// would make a failed health check take a full half-minute to report.
	probe := &http.Client{Timeout: 2 * time.Second}
	resp, err := probe.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// Embed returns the embedding vector for a single text string as float32.
// The vector length depends on the model — nomic-embed-text-v1.5
// returns 768-dimensional vectors. We use float32 because that's what
// sqlite-vec expects, and it halves storage with no meaningful precision loss.
func (c *Client) Embed(text string) ([]float32, error) {
	reqBody := embeddingRequest{
		Model: c.model,
		Input: text,
	}

	jsonBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	// Build the request manually instead of using httpClient.Post so we
	// can attach auth headers. Same pattern as llm.Client — local servers
	// like LM Studio ignore the header, remote APIs like OpenRouter require it.
	req, err := http.NewRequest("POST", c.baseURL+"/embeddings", bytes.NewReader(jsonBytes))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embedding request failed: %w", err)
	}
	// defer resp.Body.Close() is Go's version of Python's "with" statement.
	// The HTTP response body is an open stream — if you don't close it,
	// the underlying TCP connection leaks and can't be reused.
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embedding API error (status %d): %s", resp.StatusCode, string(body))
	}

	var embResp embeddingResponse
	if err := json.Unmarshal(body, &embResp); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}

	if len(embResp.Data) == 0 || len(embResp.Data[0].Embedding) == 0 {
		return nil, fmt.Errorf("empty embedding returned")
	}

	// Convert float64 (JSON default) → float32 (sqlite-vec native format).
	// This is like numpy's .astype(np.float32) — a narrowing conversion
	// that's safe for embedding vectors since models don't produce
	// more than ~7 digits of precision anyway.
	f64 := embResp.Data[0].Embedding
	vec := make([]float32, len(f64))
	for i, v := range f64 {
		vec[i] = float32(v)
	}

	return vec, nil
}

// SimilarText embeds two strings and computes their cosine similarity in one shot.
// This is a convenience wrapper for one-off comparisons where vectors won't be
// stored ("fire-and-forget" pattern).
//
// Use this when:
//   - You have two strings to compare and don't need to cache the vectors
//   - You're doing a one-time similarity check (e.g., retry budget tracking)
//
// Don't use this when:
//   - Vectors are already computed (use CosineSimilarity directly)
//   - Vectors need to be persisted (call Embed, save to DB, then compare)
//   - You're comparing one vector against many (build candidate map, use FindBestMatch)
//
// Returns similarity (0.0-1.0) and any embedding error from either text.
//
// Example:
//
//	sim, err := embedClient.SimilarText("User has a dog", "User owns a dog")
//	if err != nil {
//	    return err
//	}
//	if sim > 0.85 {
//	    fmt.Println("These are semantically similar")
//	}
func (c *Client) SimilarText(a, b string) (float64, error) {
	vecA, err := c.Embed(a)
	if err != nil {
		return 0, fmt.Errorf("embedding text A: %w", err)
	}

	vecB, err := c.Embed(b)
	if err != nil {
		return 0, fmt.Errorf("embedding text B: %w", err)
	}

	return CosineSimilarity(vecA, vecB), nil
}

// CosineSimilarity computes the cosine similarity between two float32 vectors.
// Returns a value between -1.0 and 1.0, where:
//   - 1.0 means identical direction (semantically identical)
//   - 0.0 means orthogonal (unrelated)
//   - -1.0 means opposite (semantically opposite)
//
// For memory deduplication, we typically consider > 0.85 as "too similar."
// For semantic search, we use sqlite-vec's built-in cosine distance instead,
// but this is still useful for quick in-memory comparisons (e.g., dedup checks
// when the embedding is already loaded).
//
// The math: cos(θ) = (A · B) / (|A| × |B|)
// In Python you'd use numpy: np.dot(a, b) / (np.linalg.norm(a) * np.linalg.norm(b))
// In Go, we do the same thing with a loop. We accumulate in float64 for
// numerical stability even though the inputs are float32.
func CosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}

	var dot, normA, normB float64
	for i := range a {
		ai, bi := float64(a[i]), float64(b[i])
		dot += ai * bi
		normA += ai * ai
		normB += bi * bi
	}

	denominator := math.Sqrt(normA) * math.Sqrt(normB)
	if denominator == 0 {
		return 0
	}

	return dot / denominator
}

// FindBestMatch scans a collection of candidate vectors and returns the best
// match that exceeds the threshold. This consolidates the "loop over candidates,
// track best similarity, check threshold" pattern used throughout memory dedup
// and conversation filtering.
//
// The caller is responsible for providing pre-computed or backfilled vectors.
// This function only handles the comparison logic — it doesn't load embeddings
// from the database or compute missing ones.
//
// Parameters:
//   - query: the vector to compare against all candidates
//   - candidates: map of ID → vector for all items to check
//   - threshold: minimum similarity score to consider a match (0.0-1.0)
//
// Returns:
//   - bestID: the ID of the highest-scoring candidate (0 if no match)
//   - bestSim: the cosine similarity score of the best match (0.0-1.0)
//   - matched: true if bestSim >= threshold, false otherwise
//
// Example usage (fact deduplication):
//
//	tagCandidates := make(map[int64][]float32)
//	for _, existing := range memories {
//	    tagCandidates[existing.ID] = existing.Embedding
//	}
//	id, sim, matched := embed.FindBestMatch(newTagVec, tagCandidates, 0.85)
//	if matched {
//	    fmt.Printf("Duplicate found: memory #%d with similarity %.3f\n", id, sim)
//	}
func FindBestMatch(query []float32, candidates map[int64][]float32, threshold float64) (bestID int64, bestSim float64, matched bool) {
	if len(query) == 0 || len(candidates) == 0 {
		return 0, 0, false
	}

	var maxSim float64
	var maxID int64

	for id, vec := range candidates {
		sim := CosineSimilarity(query, vec)
		if sim > maxSim {
			maxSim = sim
			maxID = id
		}
	}

	return maxID, maxSim, maxSim >= threshold
}

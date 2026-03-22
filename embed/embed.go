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

// Client wraps an HTTP client configured to talk to an embedding API.
// It's similar in shape to llm.Client — base URL + model name.
//
// In Python you might use the openai library directly:
//   client.embeddings.create(model="...", input="hello")
// In Go, we make the HTTP call ourselves — it's just a POST with JSON.
type Client struct {
	baseURL    string
	model      string
	httpClient *http.Client
}

// NewClient creates an embedding client pointed at the given server.
// For LM Studio: baseURL = "http://localhost:1234/v1"
// For Ollama: baseURL = "http://localhost:11434/v1"
func NewClient(baseURL, model string) *Client {
	return &Client{
		baseURL: baseURL,
		model:   model,
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
type embeddingResponse struct {
	Data []struct {
		Embedding []float64 `json:"embedding"`
	} `json:"data"`
}

// Embed returns the embedding vector for a single text string.
// The vector length depends on the model — nomic-embed-text-v1.5
// returns 768-dimensional vectors.
func (c *Client) Embed(text string) ([]float64, error) {
	reqBody := embeddingRequest{
		Model: c.model,
		Input: text,
	}

	jsonBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	resp, err := c.httpClient.Post(
		c.baseURL+"/embeddings",
		"application/json",
		bytes.NewReader(jsonBytes),
	)
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

	return embResp.Data[0].Embedding, nil
}

// CosineSimilarity computes the cosine similarity between two vectors.
// Returns a value between -1.0 and 1.0, where:
//   - 1.0 means identical direction (semantically identical)
//   - 0.0 means orthogonal (unrelated)
//   - -1.0 means opposite (semantically opposite)
//
// For memory deduplication, we typically consider > 0.85 as "too similar."
//
// The math: cos(θ) = (A · B) / (|A| × |B|)
// In Python you'd use numpy: np.dot(a, b) / (np.linalg.norm(a) * np.linalg.norm(b))
// In Go, we do the same thing with a loop.
func CosineSimilarity(a, b []float64) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}

	var dot, normA, normB float64
	for i := range a {
		dot += a[i] * b[i]
		normA += a[i] * a[i]
		normB += b[i] * b[i]
	}

	denominator := math.Sqrt(normA) * math.Sqrt(normB)
	if denominator == 0 {
		return 0
	}

	return dot / denominator
}

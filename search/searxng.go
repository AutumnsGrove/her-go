package search

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// SearXNGClient wraps a self-hosted SearXNG instance for web search.
//
// SearXNG is a privacy-respecting meta-search engine that aggregates results
// from multiple search engines (Google, DuckDuckGo, Brave, etc.). Unlike
// Tavily, it doesn't provide an AI-generated answer, but it's free, has no
// rate limits, and runs locally.
//
// Returns results in the same SearchResponse format as Tavily for drop-in
// compatibility.
type SearXNGClient struct {
	baseURL string
	http    *http.Client
}

// NewSearXNGClient creates a client for a SearXNG instance.
// baseURL should be the root URL (e.g., http://localhost:8888).
func NewSearXNGClient(baseURL string) *SearXNGClient {
	return &SearXNGClient{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		http:    &http.Client{Timeout: 15 * time.Second},
	}
}

// searxngResponse is the raw JSON response from SearXNG.
// We only parse the fields we need and convert to our standard SearchResponse.
type searxngResponse struct {
	Query   string            `json:"query"`
	Results []searxngResult   `json:"results"`
}

// searxngResult is a single search result from SearXNG.
type searxngResult struct {
	Title     string  `json:"title"`
	URL       string  `json:"url"`
	Content   string  `json:"content"`
	Score     float64 `json:"score"`     // SearXNG uses an integer-like score; we'll normalize it
	Thumbnail string  `json:"thumbnail"` // thumbnail image URL (may be empty)
}

// Search performs a web search via SearXNG.
// Returns up to maxResults results with relevance-ranked snippets.
//
// Unlike Tavily, SearXNG doesn't provide an AI-generated answer summary,
// so the Answer field in SearchResponse will be empty.
func (c *SearXNGClient) Search(query string, maxResults int) (*SearchResponse, error) {
	if maxResults <= 0 {
		maxResults = 5
	}

	// SearXNG uses query params: /search?format=json&q=<query>
	u := fmt.Sprintf("%s/search?format=json&q=%s", c.baseURL, url.QueryEscape(query))

	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("search request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("searxng error (status %d): %s", resp.StatusCode, string(body))
	}

	var searxngResp searxngResponse
	if err := json.Unmarshal(body, &searxngResp); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}

	// Convert to our standard SearchResponse format
	results := make([]SearchResult, 0, len(searxngResp.Results))
	for i, r := range searxngResp.Results {
		if i >= maxResults {
			break
		}

		// SearXNG scores are typically 0-10ish, normalize to 0-1 like Tavily
		normalizedScore := r.Score / 10.0
		if normalizedScore > 1.0 {
			normalizedScore = 1.0
		}

		results = append(results, SearchResult{
			Title:     r.Title,
			URL:       r.URL,
			Content:   r.Content,
			Score:     normalizedScore,
			Thumbnail: r.Thumbnail,
		})
	}

	return &SearchResponse{
		Query:   query,
		Answer:  "", // SearXNG doesn't provide AI-generated answers
		Results: results,
	}, nil
}

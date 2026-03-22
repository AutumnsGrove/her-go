package search

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// TavilyClient wraps the Tavily REST API for web search and URL extraction.
//
// Tavily is an AI-focused search API that returns pre-ranked, relevant
// snippets. Think of it like Google but optimized for feeding results
// into LLMs — the snippets are clean and concise.
//
// Free tier: 1,000 credits/month (1 credit = 1 basic search).
type TavilyClient struct {
	apiKey  string
	baseURL string
	http    *http.Client
}

// NewTavilyClient creates a client for the Tavily API.
func NewTavilyClient(apiKey, baseURL string) *TavilyClient {
	if baseURL == "" {
		baseURL = "https://api.tavily.com"
	}
	return &TavilyClient{
		apiKey:  apiKey,
		baseURL: baseURL,
		http:    &http.Client{Timeout: 15 * time.Second},
	}
}

// -- Search --

// searchRequest is the JSON body for POST /search.
type searchRequest struct {
	Query           string `json:"query"`
	SearchDepth     string `json:"search_depth,omitempty"`
	MaxResults      int    `json:"max_results,omitempty"`
	IncludeAnswer   bool   `json:"include_answer,omitempty"`
}

// SearchResponse is the parsed response from Tavily search.
type SearchResponse struct {
	Query        string         `json:"query"`
	Answer       string         `json:"answer"`
	Results      []SearchResult `json:"results"`
	ResponseTime float64        `json:"response_time"`
}

// SearchResult is a single search result from Tavily.
type SearchResult struct {
	Title   string  `json:"title"`
	URL     string  `json:"url"`
	Content string  `json:"content"` // NLP-extracted snippet
	Score   float64 `json:"score"`   // relevance 0-1
}

// Search performs a web search via Tavily.
// Returns up to maxResults results with relevance-ranked snippets.
func (c *TavilyClient) Search(query string, maxResults int) (*SearchResponse, error) {
	if maxResults <= 0 {
		maxResults = 5
	}

	reqBody := searchRequest{
		Query:         query,
		SearchDepth:   "basic", // 1 credit per search
		MaxResults:    maxResults,
		IncludeAnswer: true, // get a synthesized answer
	}

	jsonBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	req, err := http.NewRequest("POST", c.baseURL+"/search", bytes.NewReader(jsonBytes))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

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
		return nil, fmt.Errorf("tavily error (status %d): %s", resp.StatusCode, string(body))
	}

	var searchResp SearchResponse
	if err := json.Unmarshal(body, &searchResp); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}

	return &searchResp, nil
}

// -- Extract --

// extractRequest is the JSON body for POST /extract.
type extractRequest struct {
	URLs []string `json:"urls"`
}

// ExtractResponse is the parsed response from Tavily extract.
type ExtractResponse struct {
	Results []ExtractResult `json:"results"`
	Failed  []struct {
		URL   string `json:"url"`
		Error string `json:"error"`
	} `json:"failed_results"`
	ResponseTime float64 `json:"response_time"`
}

// ExtractResult is extracted content from a single URL.
type ExtractResult struct {
	URL        string `json:"url"`
	RawContent string `json:"raw_content"`
}

// Extract fetches and extracts clean text content from one or more URLs.
// Useful when Mira needs to read a specific page the user linked.
func (c *TavilyClient) Extract(urls []string) (*ExtractResponse, error) {
	reqBody := extractRequest{URLs: urls}

	jsonBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	req, err := http.NewRequest("POST", c.baseURL+"/extract", bytes.NewReader(jsonBytes))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("extract request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tavily error (status %d): %s", resp.StatusCode, string(body))
	}

	var extractResp ExtractResponse
	if err := json.Unmarshal(body, &extractResp); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}

	return &extractResp, nil
}

// -- Formatting helpers --

// FormatSearchResults turns search results into a readable string for
// injection into the LLM context.
func FormatSearchResults(resp *SearchResponse) string {
	var b strings.Builder

	if resp.Answer != "" {
		fmt.Fprintf(&b, "**Summary:** %s\n\n", resp.Answer)
	}

	b.WriteString("**Sources:**\n")
	for i, r := range resp.Results {
		fmt.Fprintf(&b, "%d. [%s](%s)\n   %s\n", i+1, r.Title, r.URL, r.Content)
	}

	return b.String()
}

// FormatExtractResults turns extracted content into a readable string.
// Truncates very long content to keep prompt sizes reasonable.
func FormatExtractResults(resp *ExtractResponse) string {
	var b strings.Builder

	for _, r := range resp.Results {
		fmt.Fprintf(&b, "**Content from %s:**\n", r.URL)
		content := r.RawContent
		if len(content) > 3000 {
			content = content[:3000] + "\n...(truncated)"
		}
		b.WriteString(content)
		b.WriteString("\n\n")
	}

	for _, f := range resp.Failed {
		fmt.Fprintf(&b, "**Failed to extract %s:** %s\n", f.URL, f.Error)
	}

	return b.String()
}

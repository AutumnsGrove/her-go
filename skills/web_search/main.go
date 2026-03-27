// web_search is a skill that searches the web using the Tavily API.
//
// It runs as a standalone binary: the harness pipes JSON to stdin,
// the skill calls Tavily, and writes formatted results to stdout.
//
// Usage (via harness):
//
//	echo '{"query":"weather in portland"}' | ./bin/web_search
//
// Usage (manual testing):
//
//	go run main.go --query "weather in portland" --limit 3
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"skillkit"
)

// Args defines the parameters this skill accepts.
// The struct tags configure both stdin JSON parsing and CLI flag mode.
type Args struct {
	Query string `json:"query" flag:"query" desc:"Search query" `
	Limit int    `json:"limit" flag:"limit" desc:"Max results" default:"5"`
}

// SearchResponse mirrors the Tavily API response structure.
// We only define the fields we care about — Go's JSON decoder
// silently ignores extra fields (unlike Python's strict mode).
type SearchResponse struct {
	Query   string         `json:"query"`
	Answer  string         `json:"answer"`
	Results []SearchResult `json:"results"`
}

// SearchResult is a single search hit from Tavily.
type SearchResult struct {
	Title   string  `json:"title"`
	URL     string  `json:"url"`
	Content string  `json:"content"` // NLP-extracted snippet
	Score   float64 `json:"score"`   // relevance 0-1
}

// Output is the structured result we return to the harness.
type Output struct {
	Query     string         `json:"query"`
	Answer    string         `json:"answer,omitempty"`
	Results   []SearchResult `json:"results"`
	Formatted string         `json:"formatted"` // human-readable summary
}

func main() {
	var args Args
	skillkit.ParseArgs(&args)

	if args.Query == "" {
		skillkit.Error("query is required")
	}
	if args.Limit <= 0 {
		args.Limit = 5
	}

	apiKey := os.Getenv("TAVILY_API_KEY")
	if apiKey == "" {
		skillkit.Error("TAVILY_API_KEY not set")
	}

	skillkit.Logf("searching for: %s (limit %d)", args.Query, args.Limit)

	resp, err := tavilySearch(apiKey, args.Query, args.Limit)
	if err != nil {
		skillkit.Error(fmt.Sprintf("search failed: %s", err))
	}

	formatted := formatResults(resp)

	skillkit.Output(Output{
		Query:     args.Query,
		Answer:    resp.Answer,
		Results:   resp.Results,
		Formatted: formatted,
	})
}

// tavilySearch calls the Tavily search API. This is essentially the same
// logic as search/tavily.go but self-contained — the skill doesn't import
// any project packages except skillkit.
func tavilySearch(apiKey, query string, maxResults int) (*SearchResponse, error) {
	reqBody, err := json.Marshal(map[string]any{
		"query":          query,
		"search_depth":   "basic",
		"max_results":    maxResults,
		"include_answer": true,
	})
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	// Use skillkit.HTTPClient() for proxy support. In production,
	// untrusted skills get routed through the network proxy via
	// HTTP_PROXY env var. This skill is 2nd-party (trusted) so it
	// connects directly, but using the client keeps the pattern consistent.
	client := skillkit.HTTPClient()

	req, err := http.NewRequest("POST", "https://api.tavily.com/search", bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
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

// formatResults builds a human-readable summary of search results.
// This matches the format the agent expects from the old web_search tool.
func formatResults(resp *SearchResponse) string {
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

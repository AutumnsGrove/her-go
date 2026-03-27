// web_read is a skill that extracts clean text content from a URL
// using the Tavily Extract API.
//
// Usage (via harness):
//
//	echo '{"url":"https://example.com"}' | ./bin/web_read
//
// Usage (manual testing):
//
//	go run main.go --url "https://example.com"
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
type Args struct {
	URL string `json:"url" flag:"url" desc:"URL to read"`
}

// ExtractResponse mirrors the Tavily Extract API response.
type ExtractResponse struct {
	Results []ExtractResult `json:"results"`
	Failed  []struct {
		URL   string `json:"url"`
		Error string `json:"error"`
	} `json:"failed_results"`
}

// ExtractResult is extracted content from a single URL.
type ExtractResult struct {
	URL        string `json:"url"`
	RawContent string `json:"raw_content"`
}

// Output is the structured result we return to the harness.
type Output struct {
	URL       string `json:"url"`
	Content   string `json:"content"`
	Formatted string `json:"formatted"`
	Error     string `json:"error,omitempty"`
}

func main() {
	var args Args
	skillkit.ParseArgs(&args)

	if args.URL == "" {
		skillkit.Error("url is required")
	}

	apiKey := os.Getenv("TAVILY_API_KEY")
	if apiKey == "" {
		skillkit.Error("TAVILY_API_KEY not set")
	}

	skillkit.Logf("reading: %s", args.URL)

	resp, err := tavilyExtract(apiKey, args.URL)
	if err != nil {
		skillkit.Error(fmt.Sprintf("extract failed: %s", err))
	}

	formatted := formatResults(resp, args.URL)

	// Return the first result's content, or an error if extraction failed.
	if len(resp.Results) > 0 {
		skillkit.Output(Output{
			URL:       args.URL,
			Content:   resp.Results[0].RawContent,
			Formatted: formatted,
		})
	} else if len(resp.Failed) > 0 {
		skillkit.Output(Output{
			URL:   args.URL,
			Error: resp.Failed[0].Error,
		})
	} else {
		skillkit.Error("no content extracted")
	}
}

// tavilyExtract calls the Tavily Extract API to read a URL.
func tavilyExtract(apiKey, url string) (*ExtractResponse, error) {
	reqBody, err := json.Marshal(map[string]any{
		"urls": []string{url},
	})
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	client := skillkit.HTTPClient()

	req, err := http.NewRequest("POST", "https://api.tavily.com/extract", bytes.NewReader(reqBody))
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

	var extractResp ExtractResponse
	if err := json.Unmarshal(body, &extractResp); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}

	return &extractResp, nil
}

// formatResults builds a human-readable summary of extracted content.
func formatResults(resp *ExtractResponse, url string) string {
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

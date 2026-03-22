// Package llm provides a client for OpenAI-compatible chat completion APIs.
// It's designed for OpenRouter but works with any endpoint that implements
// the same interface (OpenAI, local models via Ollama, etc.).
package llm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client talks to an OpenAI-compatible chat completions API.
type Client struct {
	baseURL     string
	apiKey      string
	model       string
	temperature float64
	maxTokens   int
	httpClient  *http.Client
}

// ChatMessage represents a single message in the conversation.
// Role is "system", "user", or "assistant".
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatResponse holds the LLM's reply plus token usage data for metrics.
type ChatResponse struct {
	Content          string
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	CostUSD          float64 // actual cost from OpenRouter (0 if not provided)
	Model            string  // the model that actually responded (may differ from requested)
}

// chatRequest is the JSON body we send to the API.
// These struct fields are lowercase (unexported) — they're internal to this
// package. The json tags control the actual JSON field names.
type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []ChatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
	MaxTokens   int           `json:"max_tokens"`
}

// chatAPIResponse mirrors the JSON structure returned by the API.
// We only define the fields we actually need — Go's JSON decoder
// silently ignores fields not present in the struct (unlike Python's
// strict mode in Pydantic, for example).
type chatAPIResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int     `json:"prompt_tokens"`
		CompletionTokens int     `json:"completion_tokens"`
		TotalTokens      int     `json:"total_tokens"`
		Cost             float64 `json:"cost"` // OpenRouter extension: actual cost in USD
	} `json:"usage"`
	Model string `json:"model"`
}

// NewClient creates a configured LLM client.
func NewClient(baseURL, apiKey, model string, temperature float64, maxTokens int) *Client {
	return &Client{
		baseURL:     baseURL,
		apiKey:      apiKey,
		model:       model,
		temperature: temperature,
		maxTokens:   maxTokens,
		httpClient: &http.Client{
			Timeout: 60 * time.Second, // LLM calls can be slow
		},
	}
}

// ChatCompletion sends a conversation to the LLM and returns its response.
// The messages slice should include the system prompt, conversation history,
// and the current user message — fully assembled, ready to send.
func (c *Client) ChatCompletion(messages []ChatMessage) (*ChatResponse, error) {
	// Build the request body.
	reqBody := chatRequest{
		Model:       c.model,
		Messages:    messages,
		Temperature: c.temperature,
		MaxTokens:   c.maxTokens,
	}

	// json.Marshal converts a Go struct to JSON bytes.
	// Like Python's json.dumps() but works directly with struct tags.
	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	// Build the HTTP request. In Python you'd use requests.post() — in Go,
	// you construct a Request object, set headers, then execute it with a Client.
	// More verbose, but you get full control over every aspect of the request.
	req, err := http.NewRequest("POST", c.baseURL+"/chat/completions", bytes.NewReader(jsonData))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	// OpenRouter-specific headers for attribution/tracking
	req.Header.Set("HTTP-Referer", "https://github.com/AutumnsGrove/her-go")
	req.Header.Set("X-Title", "her-go")

	// Execute the request. This is where the actual HTTP call happens.
	start := time.Now()
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling LLM API: %w", err)
	}
	// ALWAYS close the response body when done. Forgetting this leaks
	// connections — one of the most common Go mistakes. The defer ensures
	// it happens even if we return early due to an error below.
	defer resp.Body.Close()
	latency := time.Since(start)

	// Read the full response body into memory.
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	// Check for HTTP errors.
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("LLM API returned %d: %s", resp.StatusCode, string(body))
	}

	// Parse the JSON response.
	var apiResp chatAPIResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, fmt.Errorf("parsing response JSON: %w", err)
	}

	// Extract the assistant's message.
	if len(apiResp.Choices) == 0 {
		return nil, fmt.Errorf("LLM returned no choices")
	}

	_ = latency // we'll use this for metrics logging in the caller

	return &ChatResponse{
		Content:          apiResp.Choices[0].Message.Content,
		PromptTokens:     apiResp.Usage.PromptTokens,
		CompletionTokens: apiResp.Usage.CompletionTokens,
		TotalTokens:      apiResp.Usage.TotalTokens,
		CostUSD:          apiResp.Usage.Cost,
		Model:            apiResp.Model,
	}, nil
}

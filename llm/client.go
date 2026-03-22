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
// Role is "system", "user", "assistant", or "tool".
// ToolCalls is populated when the model wants to call tools.
// ToolCallID is set when Role is "tool" (the result of a tool call).
//
// For multi-modal messages (text + images), set ContentParts instead of
// Content. The custom MarshalJSON handles serializing the right format:
//   - ContentParts set → "content" becomes an array of typed parts
//   - ContentParts nil  → "content" stays a plain string
//
// This matches the OpenAI vision API format, which accepts either shape.
type ChatMessage struct {
	Role         string        `json:"role"`
	Content      string        `json:"content"`
	ContentParts []ContentPart `json:"-"` // excluded from default marshal; handled by MarshalJSON
	ToolCalls    []ToolCall    `json:"tool_calls,omitempty"`
	ToolCallID   string        `json:"tool_call_id,omitempty"`
}

// ContentPart is one piece of a multi-modal message. Each part is either
// text or an image URL (which can be a data: URI with base64 content).
// This maps to the OpenAI "content parts" format used for vision models.
type ContentPart struct {
	Type     string    `json:"type"`               // "text" or "image_url"
	Text     string    `json:"text,omitempty"`      // set when Type is "text"
	ImageURL *ImageURL `json:"image_url,omitempty"` // set when Type is "image_url"
}

// ImageURL holds the URL for an image content part. The URL can be a
// regular https:// link or a data URI like "data:image/jpeg;base64,...".
// Detail controls image processing quality: "low" (faster/cheaper),
// "high" (more detail), or "auto" (model decides).
type ImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

// MarshalJSON implements custom JSON marshaling for ChatMessage.
//
// This is how Go lets you control JSON output — if a type has a
// MarshalJSON method, encoding/json calls it instead of using its
// default reflection-based approach. Think of it like Python's
// __json__ or a custom JSONEncoder.
//
// The trick: we can't call json.Marshal(msg) inside here because
// that would call MarshalJSON again → infinite recursion. The fix
// is a type alias. "type Alias ChatMessage" creates a new type with
// the same fields but WITHOUT the MarshalJSON method. So marshaling
// the alias uses the default behavior. Weird pattern, but it's the
// standard Go way to do this.
func (m ChatMessage) MarshalJSON() ([]byte, error) {
	// Type alias to avoid infinite recursion — Alias has the same
	// fields as ChatMessage but doesn't inherit MarshalJSON.
	type Alias ChatMessage

	if len(m.ContentParts) > 0 {
		// Multi-modal: marshal content as an array of parts.
		// We build an anonymous struct that embeds the alias (for all
		// the other fields) but overrides Content with the parts array.
		return json.Marshal(struct {
			Alias
			Content []ContentPart `json:"content"`
		}{
			Alias:   Alias(m),
			Content: m.ContentParts,
		})
	}

	// Plain text: marshal normally (content as a string).
	return json.Marshal(struct {
		Alias
	}{
		Alias: Alias(m),
	})
}

// ToolCall represents a function call requested by the model.
// The model returns an ID, the function name, and a JSON string of arguments.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"` // always "function" for now
	Function FunctionCall `json:"function"`
}

// FunctionCall holds the function name and its arguments as a JSON string.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON-encoded arguments
}

// ToolDef defines a tool the model can call. This maps to the OpenAI
// "tools" parameter format. In Python's OpenAI SDK you'd pass these as
// dicts — in Go we use structs that marshal to the same JSON shape.
type ToolDef struct {
	Type     string         `json:"type"` // always "function"
	Function ToolFunctionDef `json:"function"`
}

// ToolFunctionDef describes a function: its name, what it does, and
// what parameters it accepts (as a JSON Schema object).
type ToolFunctionDef struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Parameters  interface{} `json:"parameters"` // JSON Schema object
}

// ChatResponse holds the LLM's reply plus token usage data for metrics.
type ChatResponse struct {
	Content          string
	ToolCalls        []ToolCall // populated if the model wants to call tools
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	CostUSD          float64 // actual cost from OpenRouter (0 if not provided)
	Model            string  // the model that actually responded (may differ from requested)
}

// chatRequest is the JSON body we send to the API.
type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []ChatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
	MaxTokens   int           `json:"max_tokens"`
	Tools       []ToolDef     `json:"tools,omitempty"`
}

// chatAPIResponse mirrors the JSON structure returned by the API.
type chatAPIResponse struct {
	Choices []struct {
		Message struct {
			Content   string     `json:"content"`
			ToolCalls []ToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int     `json:"prompt_tokens"`
		CompletionTokens int     `json:"completion_tokens"`
		TotalTokens      int     `json:"total_tokens"`
		Cost             float64 `json:"cost"`
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
			Timeout: 60 * time.Second,
		},
	}
}

// ChatCompletion sends a conversation to the LLM and returns its response.
// For regular conversation — no tools.
func (c *Client) ChatCompletion(messages []ChatMessage) (*ChatResponse, error) {
	return c.chatCompletion(messages, nil)
}

// ChatCompletionWithTools sends a conversation with tool definitions.
// The model may respond with tool_calls instead of (or in addition to)
// regular content. The caller is responsible for executing the tools
// and sending results back in a follow-up call.
func (c *Client) ChatCompletionWithTools(messages []ChatMessage, tools []ToolDef) (*ChatResponse, error) {
	return c.chatCompletion(messages, tools)
}

// chatCompletion is the shared implementation for both regular and
// tool-calling completions.
func (c *Client) chatCompletion(messages []ChatMessage, tools []ToolDef) (*ChatResponse, error) {
	reqBody := chatRequest{
		Model:       c.model,
		Messages:    messages,
		Temperature: c.temperature,
		MaxTokens:   c.maxTokens,
		Tools:       tools,
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	req, err := http.NewRequest("POST", c.baseURL+"/chat/completions", bytes.NewReader(jsonData))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("HTTP-Referer", "https://github.com/AutumnsGrove/her-go")
	req.Header.Set("X-Title", "her-go")

	start := time.Now()
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling LLM API: %w", err)
	}
	defer resp.Body.Close()
	_ = time.Since(start)

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("LLM API returned %d: %s", resp.StatusCode, string(body))
	}

	var apiResp chatAPIResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, fmt.Errorf("parsing response JSON: %w", err)
	}

	if len(apiResp.Choices) == 0 {
		return nil, fmt.Errorf("LLM returned no choices")
	}

	return &ChatResponse{
		Content:          apiResp.Choices[0].Message.Content,
		ToolCalls:        apiResp.Choices[0].Message.ToolCalls,
		PromptTokens:     apiResp.Usage.PromptTokens,
		CompletionTokens: apiResp.Usage.CompletionTokens,
		TotalTokens:      apiResp.Usage.TotalTokens,
		CostUSD:          apiResp.Usage.Cost,
		Model:            apiResp.Model,
	}, nil
}

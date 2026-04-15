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
	"strings"
	"time"

	"her/logger"
)

// Package-level logger with [llm] prefix — same pattern as every other
// package in the project. Uses charmbracelet/log under the hood.
var log = logger.WithPrefix("llm")

// debugMode enables full API request/response logging. When true, the client
// logs the entire messages array and response details to the file logger.
// This is verbose but essential for debugging prompt issues.
var debugMode bool

// SetDebugMode enables or disables full API request/response logging.
// Called from cmd/run.go based on the config.Debug setting.
func SetDebugMode(enabled bool) {
	debugMode = enabled
}

// Client talks to an OpenAI-compatible chat completions API.
// If a fallback model is configured (via WithFallback), the client
// automatically retries with the fallback on retriable errors —
// rate limits (429), server errors (500-503), timeouts, and empty responses.
type Client struct {
	baseURL     string
	apiKey      string
	model       string
	temperature float64
	maxTokens   int
	httpClient  *http.Client

	// Fallback model — used when the primary fails with a retriable error.
	// If fallbackModel is empty, no fallback is attempted.
	fallbackModel       string
	fallbackTemperature float64
	fallbackMaxTokens   int
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
	FinishReason     string     // "stop", "tool_calls", "length", etc. — drives the agent loop
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	CostUSD          float64 // actual cost from OpenRouter (0 if not provided)
	Model            string  // the model that actually responded (may differ from requested)
	UsedFallback     bool    // true if the primary model failed and the fallback model was used
}

// chatRequest is the JSON body we send to the API.
type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []ChatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
	MaxTokens   int           `json:"max_tokens"`
	Tools       []ToolDef     `json:"tools,omitempty"`
	// ToolChoice controls whether the model must call tools.
	// "auto" (default) = model decides, "required" = must call at least one,
	// "none" = text only. Uses interface{} because the OpenAI spec also
	// allows an object like {"type":"function","function":{"name":"X"}}
	// to force a specific tool.
	ToolChoice interface{} `json:"tool_choice,omitempty"`

	// ParallelToolCalls controls whether the model can batch multiple tool
	// calls into a single response. We set this to false so tool calls run
	// sequentially — each call sees the result of the previous one before
	// deciding what to do next. Without this, think() + reply() could fire
	// in the same batch, making the thinking step pointless.
	ParallelToolCalls *bool `json:"parallel_tool_calls,omitempty"`
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

// WithFallback configures an alternative model to try when the primary
// fails with a retriable error. Returns the same *Client for chaining.
//
// This is the "builder pattern" — like Python's fluent APIs where you
// chain method calls: client = LLMClient(...).with_fallback(...).
// In Go, the convention is to return the receiver pointer so the caller
// can chain or ignore the return value.
func (c *Client) WithFallback(model string, temperature float64, maxTokens int) *Client {
	c.fallbackModel = model
	c.fallbackTemperature = temperature
	c.fallbackMaxTokens = maxTokens
	return c
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
//
// toolChoice is optional — pass nil for "auto" (model decides), or a
// string like "required" to force tool use. The agent uses "required"
// so it always calls tools (think/reply/done) instead of outputting
// text directly.
func (c *Client) ChatCompletionWithTools(messages []ChatMessage, tools []ToolDef, toolChoice ...interface{}) (*ChatResponse, error) {
	var choice interface{}
	if len(toolChoice) > 0 {
		choice = toolChoice[0]
	}
	return c.chatCompletion(messages, tools, choice)
}

// chatCompletion is the shared implementation for both regular and
// tool-calling completions. toolChoice is optional — nil means the API
// default ("auto"), "required" forces at least one tool call.
//
// If a fallback model is configured and the primary fails with a
// retriable error, the request is automatically retried with the
// fallback model. The caller doesn't need to know — it just works.
func (c *Client) chatCompletion(messages []ChatMessage, tools []ToolDef, toolChoice ...interface{}) (*ChatResponse, error) {
	var tc interface{}
	if len(toolChoice) > 0 {
		tc = toolChoice[0]
	}

	// Try the primary model first.
	resp, err := c.doRequest(c.model, c.temperature, c.maxTokens, messages, tools, tc)
	if err != nil && c.fallbackModel != "" && isRetriable(err) {
		// Primary failed with a retriable error — try the fallback.
		log.Warn("primary model failed, trying fallback",
			"primary", c.model,
			"fallback", c.fallbackModel,
			"err", err,
		)
		resp, err = c.doRequest(c.fallbackModel, c.fallbackTemperature, c.fallbackMaxTokens, messages, tools, tc)
		if err == nil {
			resp.UsedFallback = true
		}
		return resp, err
	}

	return resp, err
}

// isRetriable checks whether an error from doRequest is worth retrying
// with a fallback model. The key question: "would the fallback model
// hit the same error?" If yes, don't retry. If no, try fallback.
//
// We retry on:
//   - HTTP 400 (model not found — the model may have been removed from OpenRouter)
//   - HTTP 429 (rate limited)
//   - HTTP 500, 502, 503 (server errors)
//   - Timeouts / connection errors
//   - Empty responses (no choices)
//
// We do NOT retry on:
//   - 401/403 (auth issues — fallback shares the same API key, would fail too)
//   - JSON marshal errors (our bug, not the model's)
func isRetriable(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()

	// Timeout or connection errors (from net/http)
	if strings.Contains(msg, "deadline exceeded") ||
		strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "connection reset") {
		return true
	}

	// HTTP status-based errors (from our formatted error strings).
	// 400 is included because OpenRouter returns 400 when a model is
	// not found / no longer hosted — a different model would succeed.
	if strings.Contains(msg, "LLM API returned 400") ||
		strings.Contains(msg, "LLM API returned 429") ||
		strings.Contains(msg, "LLM API returned 500") ||
		strings.Contains(msg, "LLM API returned 502") ||
		strings.Contains(msg, "LLM API returned 503") {
		return true
	}

	// Empty response
	if strings.Contains(msg, "LLM returned no choices") {
		return true
	}

	return false
}

// doRequest sends a single chat completion request to the API with the
// given model settings. This is the low-level HTTP call — no retry logic.
// Both primary and fallback calls go through here.
func (c *Client) doRequest(model string, temperature float64, maxTokens int, messages []ChatMessage, tools []ToolDef, toolChoice interface{}) (*ChatResponse, error) {
	reqBody := chatRequest{
		Model:       model,
		Messages:    messages,
		Temperature: temperature,
		MaxTokens:   maxTokens,
		Tools:       tools,
	}
	if toolChoice != nil {
		reqBody.ToolChoice = toolChoice
	}
	// Disable parallel tool calls so the model returns one tool call per
	// response. This ensures each call sees the result of the previous one
	// (e.g., think→reply→done runs sequentially, not batched).
	if len(tools) > 0 {
		f := false
		reqBody.ParallelToolCalls = &f
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	// Debug mode: log the full API request body. This is essential for
	// debugging prompt issues but very verbose — only enable when needed.
	if debugMode {
		prettyJSON, _ := json.MarshalIndent(reqBody, "", "  ")
		log.Debug("API_REQUEST",
			"model", model,
			"messages", len(messages),
			"tools", len(tools),
			"body", string(prettyJSON),
		)
	}

	req, err := http.NewRequest("POST", c.baseURL+"/chat/completions", bytes.NewReader(jsonData))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("HTTP-Referer", "https://github.com/AutumnsGrove/her-go")
	req.Header.Set("X-Title", "her-go")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling LLM API: %w", err)
	}
	defer resp.Body.Close()

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

	// Debug mode: log response metadata. We don't log the full content
	// (it's already visible in traces), just the usage stats.
	if debugMode {
		log.Debug("API_RESPONSE",
			"model", apiResp.Model,
			"finish_reason", apiResp.Choices[0].FinishReason,
			"prompt_tokens", apiResp.Usage.PromptTokens,
			"completion_tokens", apiResp.Usage.CompletionTokens,
			"cost", apiResp.Usage.Cost,
			"tool_calls", len(apiResp.Choices[0].Message.ToolCalls),
		)
	}

	return &ChatResponse{
		Content:          apiResp.Choices[0].Message.Content,
		ToolCalls:        apiResp.Choices[0].Message.ToolCalls,
		FinishReason:     apiResp.Choices[0].FinishReason,
		PromptTokens:     apiResp.Usage.PromptTokens,
		CompletionTokens: apiResp.Usage.CompletionTokens,
		TotalTokens:      apiResp.Usage.TotalTokens,
		CostUSD:          apiResp.Usage.Cost,
		Model:            apiResp.Model,
	}, nil
}

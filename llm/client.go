// Package llm provides a client for OpenAI-compatible chat completion APIs.
// It's designed for OpenRouter but works with any endpoint that implements
// the same interface (OpenAI, local models via Ollama, etc.).
package llm

import (
	"bufio"
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

	// Provider routing — OpenRouter-specific. Controls which providers
	// serve this model (e.g., pin a memory-agent model to Groq for speed).
	provider *ProviderRouting

	// Reasoning control — OpenRouter-specific. For hybrid models that support
	// both reasoning and non-reasoning modes (Qwen3.6, DeepSeek V3.2), this
	// toggles the mode. nil = API default, pointer to false = disable reasoning.
	reasoning *ReasoningControl
}

// ReasoningControl maps to OpenRouter's `reasoning` parameter object.
// Currently we only expose `enabled`, but the API supports other fields
// (max_tokens, effort, exclude) if we need them later.
type ReasoningControl struct {
	Enabled *bool `json:"enabled,omitempty"` // nil = default, false = disable, true = enable
}

// ProviderRouting controls OpenRouter's provider selection for a model.
// Order tries providers in sequence; Only hard-restricts; Sort overrides
// the default price-priority routing with "latency", "throughput", or "price".
type ProviderRouting struct {
	Order          []string `json:"order,omitempty"`           // try these providers first, in order
	Only           []string `json:"only,omitempty"`            // restrict to ONLY these providers
	Ignore         []string `json:"ignore,omitempty"`          // exclude these providers
	AllowFallbacks *bool    `json:"allow_fallbacks,omitempty"` // false = no fallback beyond your list
	Sort           string   `json:"sort,omitempty"`            // "latency", "throughput", or "price"
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

	// Provider controls OpenRouter's provider routing — which infrastructure
	// serves the model. Pin to fast providers (Groq) or exclude slow ones.
	Provider *ProviderRouting `json:"provider,omitempty"`

	// Reasoning controls reasoning behavior for hybrid models (OpenRouter-specific).
	// Pure reasoning models ignore this, pure instruct models don't need it.
	Reasoning *ReasoningControl `json:"reasoning,omitempty"`

	Stream bool `json:"stream,omitempty"`
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

// sseChunk mirrors one streaming delta event from the OpenAI SSE format.
// Each chunk arrives as a "data: {...}" line in the SSE stream.
type sseChunk struct {
	Choices []struct {
		Delta struct {
			Content   string             `json:"content"`
			ToolCalls []sseToolCallDelta `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int     `json:"prompt_tokens"`
		CompletionTokens int     `json:"completion_tokens"`
		TotalTokens      int     `json:"total_tokens"`
		Cost             float64 `json:"cost"`
	} `json:"usage"`
	Model string `json:"model"`
}

// sseToolCallDelta is one fragment of a streaming tool call. Arguments
// arrive in pieces across many chunks; index identifies which tool call
// this fragment belongs to. ID and Name only appear in the first chunk
// for that index.
type sseToolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// partialToolCall accumulates streaming fragments for a single tool call
// until the stream is complete or aborted.
type partialToolCall struct {
	id        string
	callType  string
	name      string
	arguments strings.Builder
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

// WithTimeout overrides the default 60s HTTP timeout. Returns the same
// *Client for chaining. Use this for models that need more time (e.g.,
// the memory agent processes long transcripts and may need 120s+).
func (c *Client) WithTimeout(d time.Duration) *Client {
	c.httpClient.Timeout = d
	return c
}

// WithProvider configures OpenRouter provider routing. Controls which
// infrastructure serves requests for this model — pin to fast providers
// like Groq, or exclude unreliable ones.
func (c *Client) WithProvider(p *ProviderRouting) *Client {
	c.provider = p
	return c
}

// WithReasoning configures reasoning control for hybrid models.
// Pass nil to use API default, &ReasoningControl{Enabled: &false} to disable.
func (c *Client) WithReasoning(r *ReasoningControl) *Client {
	c.reasoning = r
	return c
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

// ChatCompletionStreaming sends a conversation to the chat model and streams
// tokens back via onChunk as they arrive. Returns the complete ChatResponse
// once streaming finishes. If streaming fails with a retriable error and a
// fallback model is configured, falls back to a non-streaming call and
// delivers the full content as a single onChunk call.
//
// Used by the reply tool to show a live typing effect in Telegram while
// still capturing the full response for style gating, length guards, PII
// deanonymization, TTS, and DB persistence.
func (c *Client) ChatCompletionStreaming(messages []ChatMessage, onChunk func(string)) (*ChatResponse, error) {
	resp, err := c.doStreamingChat(c.model, c.temperature, c.maxTokens, messages, onChunk)
	if err != nil && c.fallbackModel != "" && isRetriable(err) {
		log.Warn("streaming chat failed, falling back to non-streaming",
			"primary", c.model,
			"fallback", c.fallbackModel,
			"err", err,
		)
		resp, err = c.doRequest(c.fallbackModel, c.fallbackTemperature, c.fallbackMaxTokens, messages, nil, nil)
		if err == nil {
			resp.UsedFallback = true
			onChunk(resp.Content)
		}
	}
	return resp, err
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

	// Use streaming for tool-calling completions. The SSE stream lets us
	// abort the moment a second tool call (index >= 1) appears — enforcing
	// sequential execution even when the model ignores parallel_tool_calls:false.
	// Plain chat completions (no tools) use the non-streaming path.
	var doReq func(string, float64, int, []ChatMessage, []ToolDef, interface{}) (*ChatResponse, error)
	if len(tools) > 0 {
		doReq = c.doStreamRequest
	} else {
		doReq = c.doRequest
	}

	resp, err := doReq(c.model, c.temperature, c.maxTokens, messages, tools, tc)
	if err != nil && c.fallbackModel != "" && isRetriable(err) {
		log.Warn("primary model failed, trying fallback",
			"primary", c.model,
			"fallback", c.fallbackModel,
			"err", err,
		)
		resp, err = doReq(c.fallbackModel, c.fallbackTemperature, c.fallbackMaxTokens, messages, tools, tc)
		if err == nil {
			resp.UsedFallback = true
		}
		return resp, err
	}

	// Defense-in-depth: if the model returned multiple tool calls despite
	// our streaming abort, only keep the first one. The agent loop expects
	// sequential execution — batched calls would skip the think result.
	if resp != nil && len(resp.ToolCalls) > 1 {
		log.Warn("model returned parallel tool calls — truncating to first",
			"total", len(resp.ToolCalls),
			"kept", resp.ToolCalls[0].Function.Name,
		)
		resp.ToolCalls = resp.ToolCalls[:1]
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

	// Truncated tool call arguments from streaming — JSON was cut off before
	// closing. The fallback (non-streaming) model may handle it better.
	if strings.Contains(msg, "truncated tool call arguments") {
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
	// Include provider routing if configured (OpenRouter-specific).
	if c.provider != nil {
		reqBody.Provider = c.provider
	}
	// Include reasoning control if configured (OpenRouter-specific).
	if c.reasoning != nil {
		reqBody.Reasoning = c.reasoning
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

// doStreamRequest sends a streaming chat completion request and returns
// when the stream ends or is aborted. For tool calls, it aborts immediately
// when a second tool call (index >= 1) starts — this enforces sequential
// tool execution at our layer, preventing the model from batching think+reply
// into one response even when parallel_tool_calls:false is ignored upstream.
func (c *Client) doStreamRequest(model string, temperature float64, maxTokens int, messages []ChatMessage, tools []ToolDef, toolChoice interface{}) (*ChatResponse, error) {
	reqBody := chatRequest{
		Model:       model,
		Messages:    messages,
		Temperature: temperature,
		MaxTokens:   maxTokens,
		Tools:       tools,
		Stream:      true,
	}
	if toolChoice != nil {
		reqBody.ToolChoice = toolChoice
	}
	if len(tools) > 0 {
		f := false
		reqBody.ParallelToolCalls = &f
	}
	if c.provider != nil {
		reqBody.Provider = c.provider
	}
	if c.reasoning != nil {
		reqBody.Reasoning = c.reasoning
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	if debugMode {
		prettyJSON, _ := json.MarshalIndent(reqBody, "", "  ")
		log.Debug("API_REQUEST (stream)",
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
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling LLM API (stream): %w", err)
	}
	// resp.Body is closed by the defer below (normal path) or explicitly
	// inside the loop (abort path). The defer is a safety net for both.
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("LLM API returned %d: %s", resp.StatusCode, string(body))
	}

	// bufio.Scanner reads line-by-line. 1MB buffer handles large JSON chunks
	// (e.g., long think arguments can arrive as one SSE line).
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	partials := make(map[int]*partialToolCall)
	var contentBuilder strings.Builder
	var finishReason string
	var promptTokens, completionTokens, totalTokens int
	var costUSD float64
	var respModel string

	for scanner.Scan() {
		line := scanner.Text()

		if line == "data: [DONE]" {
			break
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		var chunk sseChunk
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err != nil {
			// Malformed chunk — skip. OpenRouter sometimes sends keep-alive
			// pings or comment lines that aren't valid JSON.
			continue
		}

		if chunk.Model != "" {
			respModel = chunk.Model
		}
		if chunk.Usage != nil {
			promptTokens = chunk.Usage.PromptTokens
			completionTokens = chunk.Usage.CompletionTokens
			totalTokens = chunk.Usage.TotalTokens
			costUSD = chunk.Usage.Cost
		}
		if len(chunk.Choices) == 0 {
			continue
		}

		delta := chunk.Choices[0].Delta
		if chunk.Choices[0].FinishReason != "" {
			finishReason = chunk.Choices[0].FinishReason
		}
		if delta.Content != "" {
			contentBuilder.WriteString(delta.Content)
		}

		for _, tc := range delta.ToolCalls {
			if tc.Index >= 1 {
				// Second tool call detected — model is batching.
				// Close the body to abort the connection, then jump
				// to response assembly with only call index 0.
				resp.Body.Close()
				goto buildResponse
			}
			p, ok := partials[tc.Index]
			if !ok {
				p = &partialToolCall{}
				partials[tc.Index] = p
			}
			if tc.ID != "" {
				p.id = tc.ID
			}
			if tc.Type != "" {
				p.callType = tc.Type
			}
			if tc.Function.Name != "" {
				p.name = tc.Function.Name
			}
			p.arguments.WriteString(tc.Function.Arguments)
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading SSE stream: %w", err)
	}

buildResponse:
	var toolCalls []ToolCall
	if p, ok := partials[0]; ok {
		args := p.arguments.String()
		if args != "" && !json.Valid([]byte(args)) {
			return nil, fmt.Errorf("truncated tool call arguments: %.100s", args)
		}
		callType := p.callType
		if callType == "" {
			callType = "function"
		}
		toolCalls = []ToolCall{{
			ID:   p.id,
			Type: callType,
			Function: FunctionCall{
				Name:      p.name,
				Arguments: args,
			},
		}}
	}

	if finishReason == "" && len(toolCalls) > 0 {
		finishReason = "tool_calls"
	}

	if debugMode {
		log.Debug("API_RESPONSE (stream)",
			"model", respModel,
			"finish_reason", finishReason,
			"prompt_tokens", promptTokens,
			"completion_tokens", completionTokens,
			"cost", costUSD,
			"tool_calls", len(toolCalls),
		)
	}

	return &ChatResponse{
		Content:          contentBuilder.String(),
		ToolCalls:        toolCalls,
		FinishReason:     finishReason,
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		TotalTokens:      totalTokens,
		CostUSD:          costUSD,
		Model:            respModel,
	}, nil
}

// doStreamingChat sends a streaming chat completion (no tools) and calls
// onChunk for each token as it arrives. Returns the complete ChatResponse
// once the stream ends. Used by ChatCompletionStreaming to deliver tokens
// to Telegram in real time while still capturing the full response for
// downstream processing (style gate, length guard, deanonymization, TTS).
func (c *Client) doStreamingChat(model string, temperature float64, maxTokens int, messages []ChatMessage, onChunk func(string)) (*ChatResponse, error) {
	reqBody := chatRequest{
		Model:       model,
		Messages:    messages,
		Temperature: temperature,
		MaxTokens:   maxTokens,
		Stream:      true,
	}
	if c.provider != nil {
		reqBody.Provider = c.provider
	}
	if c.reasoning != nil {
		reqBody.Reasoning = c.reasoning
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
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling LLM API (stream): %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("LLM API returned %d: %s", resp.StatusCode, string(body))
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	var contentBuilder strings.Builder
	var finishReason string
	var promptTokens, completionTokens, totalTokens int
	var costUSD float64
	var respModel string

	for scanner.Scan() {
		line := scanner.Text()
		if line == "data: [DONE]" {
			break
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var chunk sseChunk
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err != nil {
			continue
		}
		if chunk.Model != "" {
			respModel = chunk.Model
		}
		if chunk.Usage != nil {
			promptTokens = chunk.Usage.PromptTokens
			completionTokens = chunk.Usage.CompletionTokens
			totalTokens = chunk.Usage.TotalTokens
			costUSD = chunk.Usage.Cost
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		if chunk.Choices[0].FinishReason != "" {
			finishReason = chunk.Choices[0].FinishReason
		}
		token := chunk.Choices[0].Delta.Content
		if token != "" {
			contentBuilder.WriteString(token)
			onChunk(token)
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading SSE stream: %w", err)
	}

	return &ChatResponse{
		Content:          contentBuilder.String(),
		FinishReason:     finishReason,
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		TotalTokens:      totalTokens,
		CostUSD:          costUSD,
		Model:            respModel,
	}, nil
}

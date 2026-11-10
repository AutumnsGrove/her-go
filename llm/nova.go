// Nova-backed transport for llm.Client.
//
// Seam 1 of the Nova migration (docs/plans/PLAN-nova-migration.md §3, step 5).
// `llm.Client` is the project-owned entry point for every LLM call — 28 call
// sites outside the agent loop use it (reply streaming, classifier, mood,
// vision, compact, fast path). Rather than rewrite those, the client keeps its
// public API and swaps its insides for Nova's provider when
// `llm.backend: nova` is set. That is the strangler-fig shape: one file's
// internals change, callers do not notice.
//
// The old hand-written HTTP client stays in client.go and remains the default
// until the loop has also been ported and soaked. Both backends answer the same
// three methods:
//
//	ChatCompletion            no tools, non-streaming
//	ChatCompletionStreaming   no tools, streaming with an onChunk callback
//	ChatCompletionWithTools   tools, streaming
//
// so parity can be tested by running the same table against both.
package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/AutumnsGrove/nova/message"
	"github.com/AutumnsGrove/nova/provider"
	"github.com/AutumnsGrove/nova/provider/openrouter"
)

// backend selects the transport implementation. Package-level because the flag
// describes one process-wide configuration choice (from config.yaml's `llm:`
// section), not a per-client one — every client in a run must speak the same
// dialect or logs and metrics become ambiguous.
//
// It is read without a lock: cmd sets it once at startup, before any client is
// used. That is the same contract as SetDebugMode above.
var backend = BackendLegacy

// Backend names, as written in config.yaml.
const (
	BackendLegacy = "legacy"
	BackendNova   = "nova"
)

// SetBackend chooses the transport. Unknown or empty values keep the legacy
// implementation, so a typo in config cannot silently disable the LLM.
func SetBackend(name string) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case BackendNova:
		backend = BackendNova
	default:
		backend = BackendLegacy
	}
}

// ActiveBackend reports which transport is in use (for logs and /status).
func ActiveBackend() string { return backend }

// novaProvider lazily builds the Nova provider for this client's endpoint.
//
// One provider per client, built on first use: that keeps the 28 call sites
// unchanged and costs nothing when the legacy backend is active. sync.Once
// makes the lazy init safe if two goroutines race on the first call — the Go
// equivalent of Python's threading.Lock around a lazy singleton, except the
// runtime guarantees the function body runs exactly once.
func (c *Client) novaProvider() provider.Provider {
	c.novaOnce.Do(func() {
		httpClient := c.httpClient
		if httpClient == nil {
			httpClient = &http.Client{Timeout: 60 * time.Second}
		}
		c.novaProviderInstance = openrouter.New(openrouter.Config{
			APIKey:  c.apiKey,
			BaseURL: c.baseURL,
			Referer: "https://github.com/AutumnsGrove/her-go",
			Title:   "her-go",
			// Reuse the client's http.Client so WithTimeout keeps working.
			HTTPClient: httpClient,
			// The legacy client always serialised `"content": ""` on an
			// assistant message that only carries tool calls (ChatMessage.Content
			// has no omitempty). Sending the same bytes matters: the provider's
			// prompt cache is keyed on them, and a key change is the costliest
			// silent regression in this migration (delta D7).
			EmptyToolCallContent: true,
		})
	})
	return c.novaProviderInstance
}

// novaComplete is the single implementation behind all three public methods on
// the Nova backend. Nova always streams, so "non-streaming" is expressed by
// passing a nil onChunk and ignoring the deltas.
//
// timeout mirrors the legacy split: tool-calling turns get three minutes
// (they were streamed with a 3-minute context deadline before), plain
// completions get the client's configured HTTP timeout.
func (c *Client) novaComplete(model string, temperature float64, maxTokens int, messages []ChatMessage, tools []ToolDef, toolChoice any, onChunk func(string)) (*ChatResponse, error) {
	timeout := c.httpClient.Timeout
	if len(tools) > 0 {
		timeout = 3 * time.Minute
	}
	ctx := context.Background()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	req, err := c.novaRequest(model, temperature, maxTokens, messages, tools, toolChoice)
	if err != nil {
		return nil, err
	}

	resp, err := c.novaProvider().Stream(ctx, req, func(ev provider.StreamEvent) {
		if ev.Type == provider.StreamTextDelta && onChunk != nil {
			onChunk(ev.Text)
		}
	})
	if err != nil {
		// Nova returns the partial response alongside a mid-stream failure. A
		// partial answer is useful to the caller only if there is text; otherwise
		// report the error and let the fallback logic try another model.
		if resp == nil || resp.Message.Content == "" {
			return nil, err
		}
		return c.novaResponse(resp), err
	}
	return c.novaResponse(resp), nil
}

// novaRequest converts her-go's request shape into Nova's. Every conversion
// here is deliberate: the goal is byte-identical wire bodies.
func (c *Client) novaRequest(model string, temperature float64, maxTokens int, messages []ChatMessage, tools []ToolDef, toolChoice any) (provider.Request, error) {
	msgs, err := toNovaMessages(messages)
	if err != nil {
		return provider.Request{}, err
	}

	req := provider.Request{
		Model:       model,
		Messages:    msgs,
		ToolChoice:  toolChoice,
		Temperature: &temperature,
		MaxTokens:   maxTokens,
		// her-go has always forbidden parallel tool calls so that think→reply→done
		// run in order. Nova's loop defaults to parallel; this preserves the old
		// behaviour at the transport as well.
		ParallelToolCalls: boolPtr(false),
		// An explicit "no marker" keeps the request free of the automatic
		// cache_control field Nova would otherwise add for a catalogued model.
		// OpenRouter caches implicitly; a marker it does not expect risks
		// invalidating the prompt cache we are trying to protect (delta D7).
		PromptCache: boolPtr(false),
	}

	if len(tools) > 0 {
		converted, err := toNovaTools(tools)
		if err != nil {
			return provider.Request{}, err
		}
		req.Tools = converted
	}

	// OpenRouter-specific knobs travel as provider options, which Nova merges
	// into the top level of the wire body. That is how provider.order,
	// session_id and reasoning reach OpenRouter without Nova knowing them.
	opts := map[string]any{}
	if c.provider != nil {
		// Routing.Options renders exactly one entry, {"provider": {...}}.
		if rendered := (openrouter.Routing{
			Order:          c.provider.Order,
			Only:           c.provider.Only,
			Ignore:         c.provider.Ignore,
			AllowFallbacks: c.provider.AllowFallbacks,
			Sort:           c.provider.Sort,
		}).Options(); rendered != nil {
			opts["provider"] = rendered["provider"]
		}
	}
	if c.reasoning != nil && c.reasoning.Enabled != nil {
		opts["reasoning"] = map[string]any{"enabled": *c.reasoning.Enabled}
	}
	if c.sessionID != "" {
		opts["session_id"] = c.sessionID
	}
	if len(opts) > 0 {
		req.ProviderOptions = opts
	}

	return req, nil
}

// novaResponse maps Nova's finalized response onto her-go's ChatResponse.
func (c *Client) novaResponse(resp *provider.Response) *ChatResponse {
	// Nova's OpenAI-compatible dialect does not surface the upstream provider
	// name (OpenRouter reports it per chunk as `provider`, which Nova does not
	// parse). Record the provider we configured instead of an empty string: ""
	// would drop the call from the sim report's provider table entirely. The
	// finer-grained attribution is a recorded gap — see docs/plans/NOVA-GAPS.md.
	providerName := resp.Provider
	if providerName == "" {
		providerName = "openrouter"
	}
	out := &ChatResponse{
		Content:          resp.Message.Content,
		Reasoning:        resp.Message.Reasoning,
		FinishReason:     resp.FinishReason,
		PromptTokens:     resp.Usage.PromptTokens,
		CompletionTokens: resp.Usage.CompletionTokens,
		TotalTokens:      resp.Usage.TotalTokens,
		CostUSD:          resp.Usage.CostUSD,
		Model:            resp.Model,
		CacheReadTokens:  resp.Usage.CachedTokens,
		CacheWriteTokens: resp.Usage.CacheWriteTokens,
		Provider:         providerName,
	}
	if len(resp.Message.ToolCalls) > 0 {
		out.ToolCalls = make([]ToolCall, 0, len(resp.Message.ToolCalls))
		for _, tc := range resp.Message.ToolCalls {
			out.ToolCalls = append(out.ToolCalls, ToolCall{
				ID:   tc.ID,
				Type: tc.Type,
				Function: FunctionCall{
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				},
			})
		}
	}
	return out
}

// toNovaMessages converts the transcript. Text stays in Content; image parts
// move to Parts, because that is where Nova keeps media (its Content is always
// the text, which keeps a text-only transcript byte-stable).
func toNovaMessages(msgs []ChatMessage) ([]message.Message, error) {
	out := make([]message.Message, 0, len(msgs))
	for _, m := range msgs {
		nm := message.Message{
			Role:       message.Role(m.Role),
			Content:    m.Content,
			ToolCallID: m.ToolCallID,
		}
		if len(m.ToolCalls) > 0 {
			nm.ToolCalls = make([]message.ToolCall, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				callType := tc.Type
				if callType == "" {
					callType = "function"
				}
				nm.ToolCalls = append(nm.ToolCalls, message.ToolCall{
					ID:   tc.ID,
					Type: callType,
					Function: message.FunctionCall{
						Name:      tc.Function.Name,
						Arguments: tc.Function.Arguments,
					},
				})
			}
		}
		for _, part := range m.ContentParts {
			if part.Type != "image_url" || part.ImageURL == nil {
				continue
			}
			url := part.ImageURL.URL
			if mime, data, ok := splitDataURI(url); ok {
				nm.Parts = append(nm.Parts, message.ImagePart(mime, data))
			} else {
				nm.Parts = append(nm.Parts, message.ContentPart{Type: message.PartImage, URL: url})
			}
		}
		out = append(out, nm)
	}
	return out, nil
}

// splitDataURI splits "data:image/png;base64,AAAA" into its media type and the
// base64 payload. Nova prefers Data over URL for images: a data URI as a URL
// would make the provider fetch a "URL" that is not one.
func splitDataURI(uri string) (mime, data string, ok bool) {
	if !strings.HasPrefix(uri, "data:") {
		return "", "", false
	}
	rest := strings.TrimPrefix(uri, "data:")
	comma := strings.Index(rest, ",")
	if comma < 0 {
		return "", "", false
	}
	meta, payload := rest[:comma], rest[comma+1:]
	mime = strings.TrimSuffix(meta, ";base64")
	if mime == "" {
		mime = "image/jpeg"
	}
	return mime, payload, true
}

// toNovaTools converts tool definitions. her-go carries Parameters as an
// `interface{}` (it comes straight from YAML), Nova wants a map; anything else
// is a bug in the manifest loader and is reported rather than silently sent as
// an empty schema (which would let the model call a tool with no arguments).
func toNovaTools(tools []ToolDef) ([]provider.ToolDef, error) {
	out := make([]provider.ToolDef, 0, len(tools))
	for _, t := range tools {
		params, ok := t.Function.Parameters.(map[string]any)
		if !ok {
			if t.Function.Parameters == nil {
				params = map[string]any{"type": "object", "properties": map[string]any{}}
			} else {
				return nil, fmt.Errorf("tool %q: parameters are %T, want map[string]any", t.Function.Name, t.Function.Parameters)
			}
		}
		out = append(out, provider.ToolDef{
			Type: "function",
			Function: provider.ToolFunction{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				Parameters:  params,
			},
		})
	}
	return out, nil
}

// isRetriableNova is the Nova-backend counterpart of isRetriable: same policy
// (a different model might succeed), expressed against Nova's typed error
// instead of a formatted string.
//
// Nova classifies HTTP statuses itself (provider.Classify) and sets Retryable,
// so the important cases come for free. 400 is added explicitly because
// OpenRouter returns it when a model has been retired — her-go's long-standing
// behaviour is to try the fallback model in that case, and it is right.
func isRetriableNova(err error) bool {
	if err == nil {
		return false
	}
	var perr *provider.Error
	if errors.As(err, &perr) {
		if perr.Retryable {
			return true
		}
		if perr.Status == http.StatusBadRequest {
			return true
		}
		return false
	}
	// Context deadlines and transport errors: same rules as the legacy client.
	msg := err.Error()
	return strings.Contains(msg, "deadline exceeded") ||
		strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "connection reset")
}

// boolPtr is a tiny helper for the optional booleans Nova's Request takes.
// Go has no address-of literal for a bool, and every optional bool needs one.
func boolPtr(b bool) *bool { return &b }

// novaChatCompletion is the Nova equivalent of chatCompletion: primary model,
// then the fallback model on a retriable failure, with the same
// truncate-to-one-tool-call defence the legacy path applies.
func (c *Client) novaChatCompletion(messages []ChatMessage, tools []ToolDef, toolChoice any, onChunk func(string)) (*ChatResponse, error) {
	resp, err := c.novaComplete(c.model, c.temperature, c.maxTokens, messages, tools, toolChoice, onChunk)
	if err != nil && c.fallbackModel != "" && isRetriableNova(err) {
		log.Warn("nova: primary model failed, trying fallback",
			"primary", c.model,
			"fallback", c.fallbackModel,
			"err", err,
		)
		fallbackResp, fallbackErr := c.novaComplete(c.fallbackModel, c.fallbackTemperature, c.fallbackMaxTokens, messages, tools, toolChoice, onChunk)
		if fallbackErr == nil {
			fallbackResp.UsedFallback = true
		}
		return fallbackResp, fallbackErr
	}

	// Nova's collector assembles every tool call the model asked for. her-go's
	// loop executes them one at a time and its transcripts assume the same, so
	// keep only the first — the same defence-in-depth the legacy path applies
	// after aborting the stream on a second call.
	if resp != nil && len(resp.ToolCalls) > 1 {
		log.Warn("nova: model returned parallel tool calls — truncating to first",
			"total", len(resp.ToolCalls),
			"kept", resp.ToolCalls[0].Function.Name,
		)
		resp.ToolCalls = resp.ToolCalls[:1]
	}
	return resp, err
}

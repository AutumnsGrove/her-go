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
// The old hand-written HTTP client stays in client.go as an opt-in escape hatch
// (`llm.backend: legacy`); Nova is the default. Both backends answer the same
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
var backend = BackendNova

// Backend names, as written in config.yaml.
const (
	BackendLegacy = "legacy"
	BackendNova   = "nova"
)

// SetBackend chooses the transport. Nova is the default: an empty value, an
// absent `llm:` section in config.yaml, or a typo all select it, so a config
// written before the migration runs on the same path the sims proved. Only the
// explicit name "legacy" selects the hand-written client, which stays as an
// escape hatch until it is deleted.
func SetBackend(name string) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case BackendLegacy:
		backend = BackendLegacy
	case "", BackendNova:
		backend = BackendNova
	default:
		log.Warn("unknown llm.backend, using nova", "value", name)
		backend = BackendNova
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
		timeout = streamTimeout
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

	c.applyProviderOptions(&req)
	return req, nil
}

// applyProviderOptions merges her-go's OpenRouter knobs into a Nova request.
// Nova merges ProviderOptions into the top level of the wire body, which is how
// provider.order, session_id and reasoning reach OpenRouter without Nova knowing
// about any of them.
//
// Shared by both entry points (the direct calls and the provider interface the
// agent loop uses) so pinning and sticky routing cannot apply on one path and
// silently not the other.
func (c *Client) applyProviderOptions(req *provider.Request) {
	opts := map[string]any{}
	for k, v := range req.ProviderOptions {
		opts[k] = v
	}
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
	// Remember whether any text already reached the caller. A fallback replays
	// the whole answer from the start, so after a mid-stream failure it would
	// show the user the beginning of the reply twice. In that case keep the
	// partial text and report the error instead of falling back.
	streamed := false
	var forward func(string)
	if onChunk != nil {
		forward = func(chunk string) {
			if chunk != "" {
				streamed = true
			}
			onChunk(chunk)
		}
	}

	resp, err := c.novaComplete(c.model, c.temperature, c.maxTokens, messages, tools, toolChoice, forward)
	if err != nil && c.fallbackModel != "" && !streamed && isRetriableNova(err) {
		log.Warn("nova: primary model failed, trying fallback",
			"primary", c.model,
			"fallback", c.fallbackModel,
			"err", err,
		)
		var fbErr error
		resp, fbErr = c.novaComplete(c.fallbackModel, c.fallbackTemperature, c.fallbackMaxTokens, messages, tools, toolChoice, forward)
		if fbErr == nil {
			resp.UsedFallback = true
		}
		err = fbErr
	}

	// Nova's collector assembles every tool call the model asked for. her-go's
	// loop executes them one at a time and its transcripts assume the same, so
	// keep only the first. This applies to the fallback's answer too.
	if resp != nil && len(resp.ToolCalls) > 1 {
		log.Warn("nova: model returned parallel tool calls — truncating to first",
			"total", len(resp.ToolCalls),
			"kept", resp.ToolCalls[0].Function.Name,
		)
		resp.ToolCalls = resp.ToolCalls[:1]
	}
	return resp, err
}

// ---------------------------------------------------------------------------
// Export surface for the agent-loop seam (agent_engine/nova.go)
// ---------------------------------------------------------------------------

// ResponseInfo is the per-call metadata her-go records in its metrics table.
//
// It exists so the loop can save one metric row per provider call without
// importing Nova's provider types: Nova's loop only reports usage summed over a
// run, but her-go's cost dashboards break spend down per call, per model and per
// provider, and flag fallbacks.
type ResponseInfo struct {
	Model            string
	Provider         string
	FinishReason     string
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	CacheReadTokens  int
	CacheWriteTokens int
	CostUSD          float64
	UsedFallback     bool
}

// Model returns the primary model name this client was built for.
func (c *Client) Model() string { return c.model }

// MaxTokens returns the completion budget this client was built with.
func (c *Client) MaxTokens() int { return c.maxTokens }

// TemperaturePtr returns the temperature as the pointer Nova's request wants.
// A pointer, because "unset" and "0.0" mean different things on the wire.
func (c *Client) TemperaturePtr() *float64 {
	t := c.temperature
	return &t
}

// Name implements provider.Provider. A *llm.Client is usable anywhere Nova wants
// a provider, which is how the agent loop reaches the real transport (with
// her-go's retry, fallback and metrics semantics) instead of Nova's.
func (c *Client) Name() string { return "her-go" }

// Stream implements provider.Provider on top of the Nova transport, adding
// her-go's fallback to a second model on a retriable failure.
//
// Nova's own retry policy is disabled for these runs (agent.Config.Retry.Max =
// -1): retrying belongs to the retry capability, and a fallback already re-tried.
func (c *Client) Stream(ctx context.Context, req provider.Request, onEvent func(provider.StreamEvent)) (*provider.Response, error) {
	resp, _, err := c.stream(ctx, req, onEvent)
	return resp, err
}

// WithResponseHook returns a provider that behaves like c but reports every
// completed provider call to hook, which is how the agent loop records one
// metric row per call (Nova's loop only sums usage per run).
//
// The hook lives on the returned value, not on the client. A *Client is shared:
// the memory, mood and introspection agents each keep one for the life of the
// process and run in goroutines, so two runs can overlap on the same client.
// Installing the hook by assigning to a client field made those runs overwrite
// each other (and tripped the race detector). A per-run wrapper has no shared
// state — in Python terms, it is the difference between a module-level global
// and a value passed into the call.
func (c *Client) WithResponseHook(hook func(ResponseInfo)) provider.Provider {
	return hookedProvider{client: c, hook: hook}
}

// hookedProvider is the per-run view of a Client created by WithResponseHook.
type hookedProvider struct {
	client *Client
	hook   func(ResponseInfo)
}

func (p hookedProvider) Name() string { return p.client.Name() }

func (p hookedProvider) Stream(ctx context.Context, req provider.Request, onEvent func(provider.StreamEvent)) (*provider.Response, error) {
	resp, usedFallback, err := p.client.stream(ctx, req, onEvent)
	if resp != nil && p.hook != nil {
		p.hook(ResponseInfo{
			Model:            resp.Model,
			Provider:         resp.Provider,
			FinishReason:     resp.FinishReason,
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			TotalTokens:      resp.Usage.TotalTokens,
			CacheReadTokens:  resp.Usage.CachedTokens,
			CacheWriteTokens: resp.Usage.CacheWriteTokens,
			CostUSD:          resp.Usage.CostUSD,
			UsedFallback:     usedFallback,
		})
	}
	return resp, err
}

// stream is the shared implementation behind Stream and the hooked provider.
// It also reports whether the fallback model produced the response.
func (c *Client) stream(ctx context.Context, req provider.Request, onEvent func(provider.StreamEvent)) (*provider.Response, bool, error) {
	// her-go runs tool calls one at a time in one-assistant-message order, and a
	// second call in the same message could see a stale picture of the world. The
	// hand-written client enforced that by aborting the SSE stream the moment a
	// second call appeared; here the stream is read to the end and only the first
	// call is surfaced. Both the events and the response are filtered, so the
	// loop cannot see or run the extra call.
	var toolCallEvents int
	guarded := func(ev provider.StreamEvent) {
		if onEvent == nil {
			return
		}
		if ev.Type == provider.StreamToolCall {
			toolCallEvents++
			if toolCallEvents > 1 {
				return
			}
		}
		onEvent(ev)
	}

	c.applyProviderOptions(&req)
	// An explicit "no marker" keeps the request free of the automatic
	// cache_control field Nova would add for a catalogued model. OpenRouter
	// caches implicitly; a marker it does not expect risks invalidating the
	// prompt cache we are trying to protect (delta D7).
	noCache := false
	req.PromptCache = &noCache

	// Each attempt gets its own ceiling (the legacy client applied it per call
	// too), so a primary that burns the whole budget cannot starve the fallback.
	// The agent loop passes a background context with no deadline of its own, and
	// the http.Client timeout alone is a per-client number.
	attempt := func(r provider.Request) (*provider.Response, error) {
		actx := ctx
		if _, hasDeadline := ctx.Deadline(); !hasDeadline {
			var cancel context.CancelFunc
			actx, cancel = context.WithTimeout(ctx, streamTimeout)
			defer cancel()
		}
		return c.novaProvider().Stream(actx, r, guarded)
	}

	resp, err := attempt(req)
	usedFallback := false

	if err != nil && c.fallbackModel != "" && isRetriableNova(err) {
		log.Warn("nova: primary model failed, trying fallback",
			"primary", req.Model,
			"fallback", c.fallbackModel,
			"err", err,
		)
		fbReq := req
		fbReq.Model = c.fallbackModel
		fbReq.ModelID = ""
		if c.fallbackMaxTokens > 0 {
			fbReq.MaxTokens = c.fallbackMaxTokens
		}
		fbTemp := c.fallbackTemperature
		fbReq.Temperature = &fbTemp

		toolCallEvents = 0
		fbResp, fbErr := attempt(fbReq)
		if fbErr == nil {
			resp, err = fbResp, nil
			usedFallback = true
		} else {
			err = fbErr
		}
	}

	if resp != nil && len(resp.Message.ToolCalls) > 1 {
		log.Warn("nova: model returned parallel tool calls — keeping the first",
			"total", len(resp.Message.ToolCalls),
			"kept", resp.Message.ToolCalls[0].Function.Name,
		)
		resp.Message.ToolCalls = resp.Message.ToolCalls[:1]
	}
	return resp, usedFallback, err
}

// NovaMessages converts her-go's transcript to Nova's. Exported for the loop
// seam, which needs the same conversion the transport uses (one implementation,
// two callers).
func NovaMessages(msgs []ChatMessage) ([]message.Message, error) { return toNovaMessages(msgs) }

// NovaToolDefs converts her-go tool definitions to Nova's wire shape. Exported
// for the loop seam's deferred-tool refresh; identical to what the transport
// sends, so a refresh cannot change the schema by accident.
func NovaToolDefs(defs []ToolDef) ([]provider.ToolDef, error) { return toNovaTools(defs) }

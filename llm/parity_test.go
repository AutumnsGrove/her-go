package llm

// Parity tests for the two transports (llm.backend: legacy | nova).
//
// The point of these tests is the migration contract: the same table of
// behaviours runs against both implementations, so "the Nova backend behaves
// like the old client" is checked by the test suite rather than asserted in a
// PR description. Each subtest starts by selecting its backend.
//
// Both implementations talk HTTP, so every case uses a real httptest server.
// The helper servers speak both dialects: a request with "stream": true gets
// SSE (Nova always streams), one without gets a plain JSON response (the legacy
// non-streaming path). That is what lets one test cover both.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// forEachBackend runs the same subtest against both transports and restores the
// package default afterwards. t.Cleanup is Go's defer for tests: it runs even
// if the subtest fails with t.Fatal, which matters because `backend` is global.
func forEachBackend(t *testing.T, run func(t *testing.T)) {
	t.Helper()
	for _, name := range []string{BackendLegacy, BackendNova} {
		t.Run(name, func(t *testing.T) {
			SetBackend(name)
			t.Cleanup(func() { SetBackend(BackendLegacy) })
			run(t)
		})
	}
}

// dualServer answers both dialects depending on the request's "stream" field and
// records the decoded body of the first request it sees.
type dualServer struct {
	*httptest.Server
	body     map[string]any
	requests int
}

func newDualServer(t *testing.T, sse func(w http.ResponseWriter), jsonBody any) *dualServer {
	t.Helper()
	ds := &dualServer{}
	ds.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if ds.body == nil {
			ds.body = body
		}
		ds.requests++
		if stream, _ := body["stream"].(bool); stream {
			w.Header().Set("Content-Type", "text/event-stream")
			sse(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jsonBody)
	}))
	t.Cleanup(ds.Close)
	return ds
}

// usageStream writes a stream that carries one tool call and full usage,
// including the cache detail OpenRouter reports.
func usageStream(w http.ResponseWriter) {
	writeSSE(w, toolCallChunk(0, "call_1", "done", `{}`))
	body := map[string]any{
		"choices": []map[string]any{
			{"delta": map[string]any{}, "finish_reason": "tool_calls"},
		},
		"usage": map[string]any{
			"prompt_tokens": 100, "completion_tokens": 20, "total_tokens": 120, "cost": 0.00042,
			"prompt_tokens_details": map[string]any{"cached_tokens": 64, "cache_write_tokens": 8},
		},
		"model": "test-model",
	}
	b, _ := json.Marshal(body)
	writeSSE(w, string(b))
	writeSSEDone(w)
}

func TestParity_UsageAndCacheMapping(t *testing.T) {
	forEachBackend(t, func(t *testing.T) {
		srv := newDualServer(t, usageStream, map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"content": "", "tool_calls": []map[string]any{
					{"id": "call_1", "type": "function", "function": map[string]any{"name": "done", "arguments": "{}"}},
				}}, "finish_reason": "tool_calls"},
			},
			"usage": map[string]any{
				"prompt_tokens": 100, "completion_tokens": 20, "total_tokens": 120, "cost": 0.00042,
				"prompt_tokens_details": map[string]any{"cached_tokens": 64, "cache_write_tokens": 8},
			},
			"model": "test-model",
		})

		client := newTestClient(srv.URL)
		resp, err := client.ChatCompletionWithTools(nil, []ToolDef{{Type: "function", Function: ToolFunctionDef{Name: "done"}}})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp.PromptTokens != 100 || resp.CompletionTokens != 20 || resp.TotalTokens != 120 {
			t.Errorf("tokens = %d/%d/%d, want 100/20/120", resp.PromptTokens, resp.CompletionTokens, resp.TotalTokens)
		}
		if resp.CostUSD != 0.00042 {
			t.Errorf("cost = %v, want 0.00042", resp.CostUSD)
		}
		if resp.CacheReadTokens != 64 || resp.CacheWriteTokens != 8 {
			t.Errorf("cache = %d/%d, want 64/8", resp.CacheReadTokens, resp.CacheWriteTokens)
		}
		if resp.Model != "test-model" {
			t.Errorf("model = %q, want test-model", resp.Model)
		}
		if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Function.Name != "done" {
			t.Errorf("tool calls = %+v, want one 'done'", resp.ToolCalls)
		}
	})
}

func TestParity_RequestBodyShape(t *testing.T) {
	forEachBackend(t, func(t *testing.T) {
		srv := newDualServer(t, usageStream, map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": ""}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
			"model":   "test-model",
		})

		client := newTestClient(srv.URL)
		client.WithProvider(&ProviderRouting{Order: []string{"xiaomi/fp8"}})
		client.WithSessionID("session-123")
		disabled := false
		client.WithReasoning(&ReasoningControl{Enabled: &disabled})

		_, err := client.ChatCompletionWithTools(nil,
			[]ToolDef{{Type: "function", Function: ToolFunctionDef{Name: "done"}}},
			"required")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		body := srv.body
		if body["model"] != "test-model" {
			t.Errorf("model = %v", body["model"])
		}
		if body["tool_choice"] != "required" {
			t.Errorf("tool_choice = %v, want required", body["tool_choice"])
		}
		if v, ok := body["parallel_tool_calls"]; !ok || v != false {
			t.Errorf("parallel_tool_calls = %v (present=%v), want false", v, ok)
		}
		if v, _ := body["temperature"].(float64); v != 0.5 {
			t.Errorf("temperature = %v, want 0.5", body["temperature"])
		}
		if v, _ := body["max_tokens"].(float64); v != 512 {
			t.Errorf("max_tokens = %v, want 512", body["max_tokens"])
		}
		session, _ := body["session_id"].(string)
		if session != "session-123" {
			t.Errorf("session_id = %v, want session-123", body["session_id"])
		}
		prov, ok := body["provider"].(map[string]any)
		if !ok {
			t.Fatalf("provider block missing: %v", body["provider"])
		}
		order, _ := prov["order"].([]any)
		if len(order) != 1 || order[0] != "xiaomi/fp8" {
			t.Errorf("provider.order = %v", prov["order"])
		}
		reasoning, ok := body["reasoning"].(map[string]any)
		if !ok {
			t.Fatalf("reasoning block missing: %v", body["reasoning"])
		}
		if reasoning["enabled"] != false {
			t.Errorf("reasoning.enabled = %v, want false", reasoning["enabled"])
		}
		if tools, ok := body["tools"].([]any); !ok || len(tools) != 1 {
			t.Errorf("tools = %v, want one entry", body["tools"])
		}
	})
}

func TestParity_EmptyContentOnToolCallMessages(t *testing.T) {
	forEachBackend(t, func(t *testing.T) {
		srv := newDualServer(t, usageStream, map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": ""}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
			"model":   "test-model",
		})

		client := newTestClient(srv.URL)
		msgs := []ChatMessage{
			{Role: "user", Content: "hi"},
			{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Type: "function", Function: FunctionCall{Name: "think", Arguments: "{}"}}}},
		}
		if _, err := client.ChatCompletionWithTools(msgs, []ToolDef{{Type: "function", Function: ToolFunctionDef{Name: "think"}}}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		raw, err := json.Marshal(srv.body["messages"])
		if err != nil {
			t.Fatalf("re-marshalling messages: %v", err)
		}
		// The provider's prompt cache is keyed on these bytes. An assistant
		// message that only carries tool calls must send "content":"" — the
		// legacy client always did, and Nova must not silently drop the field
		// (delta D7).
		var decoded []map[string]any
		_ = json.Unmarshal(raw, &decoded)
		if len(decoded) != 2 {
			t.Fatalf("messages = %s", raw)
		}
		assistant := decoded[1]
		content, present := assistant["content"]
		if !present {
			t.Fatalf("assistant tool-call message has no content field: %s", raw)
		}
		if content != "" {
			t.Errorf("assistant content = %v, want empty string", content)
		}
	})
}

func TestParity_BatchedToolCallsKeepOnlyFirst(t *testing.T) {
	forEachBackend(t, func(t *testing.T) {
		batched := func(w http.ResponseWriter) {
			writeSSE(w, toolCallChunk(0, "call_think", "think", `{"thought":"hmm"}`))
			writeSSE(w, toolCallChunk(1, "call_reply", "reply", `{"instruction":"hi"}`))
			writeSSE(w, finishChunk("tool_calls"))
			writeSSEDone(w)
		}
		srv := newDualServer(t, batched, nil)

		client := newTestClient(srv.URL)
		resp, err := client.ChatCompletionWithTools(nil, []ToolDef{
			{Type: "function", Function: ToolFunctionDef{Name: "think"}},
			{Type: "function", Function: ToolFunctionDef{Name: "reply"}},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(resp.ToolCalls) != 1 {
			t.Fatalf("tool calls = %d, want 1", len(resp.ToolCalls))
		}
		if resp.ToolCalls[0].Function.Name != "think" {
			t.Errorf("kept %q, want the first call 'think'", resp.ToolCalls[0].Function.Name)
		}
	})
}

func TestParity_StreamingCallback(t *testing.T) {
	forEachBackend(t, func(t *testing.T) {
		stream := func(w http.ResponseWriter) {
			for _, tok := range []string{"Hello", ", ", "world"} {
				writeSSE(w, contentChunk(tok))
			}
			writeSSE(w, finishChunk("stop"))
			writeSSEDone(w)
		}
		srv := newDualServer(t, stream, map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": "Hello, world"}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
			"model":   "test-model",
		})

		client := newTestClient(srv.URL)
		var got []string
		resp, err := client.ChatCompletionStreaming([]ChatMessage{{Role: "user", Content: "hi"}}, func(tok string) {
			got = append(got, tok)
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp.Content != "Hello, world" {
			t.Errorf("content = %q", resp.Content)
		}
		if strings.Join(got, "") != "Hello, world" {
			t.Errorf("streamed chunks = %v, want the full text in order", got)
		}
	})
}

func TestParity_FallbackOnRetriableError(t *testing.T) {
	forEachBackend(t, func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["model"] == "primary-model" {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
				return
			}
			if stream, _ := body["stream"].(bool); stream {
				w.Header().Set("Content-Type", "text/event-stream")
				writeSSE(w, contentChunk("fallback answered"))
				writeSSE(w, finishChunk("stop"))
				writeSSEDone(w)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{"message": map[string]any{"content": "fallback answered"}, "finish_reason": "stop"}},
				"usage":   map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
				"model":   "fallback-model",
			})
		}))
		defer srv.Close()

		client := NewClient(srv.URL, "key", "primary-model", 0.5, 512)
		client.WithFallback("fallback-model", 0.1, 256)

		resp, err := client.ChatCompletion([]ChatMessage{{Role: "user", Content: "hi"}})
		if err != nil {
			t.Fatalf("expected the fallback to answer, got %v", err)
		}
		if !resp.UsedFallback {
			t.Error("UsedFallback = false, want true")
		}
		if !strings.Contains(resp.Content, "fallback answered") {
			t.Errorf("content = %q", resp.Content)
		}
	})
}

func TestParity_NoFallbackOnAuthError(t *testing.T) {
	forEachBackend(t, func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
		}))
		defer srv.Close()

		client := NewClient(srv.URL, "key", "primary-model", 0.5, 512)
		client.WithFallback("fallback-model", 0.1, 256)

		_, err := client.ChatCompletion([]ChatMessage{{Role: "user", Content: "hi"}})
		if err == nil {
			t.Fatal("expected an error on 401, got nil")
		}
	})
}

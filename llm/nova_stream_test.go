package llm

// Tests for the provider.Provider surface (*llm.Client). The agent-loop seam
// (agent_engine/nova.go) calls Stream directly, bypassing the three public chat
// methods — so the request decorations that live on the client (provider pinning,
// sticky session id, reasoning toggle) must be applied on this path too. These
// tests pin that: a drift here would silently un-pin the driver model.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/AutumnsGrove/nova/message"
	"github.com/AutumnsGrove/nova/provider"
)

func TestClientStream_AppliesProviderOptions(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w, contentChunk("hi"))
		writeSSE(w, finishChunk("stop"))
		writeSSEDone(w)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "key", "test-model", 0.4, 256)
	client.WithProvider(&ProviderRouting{Order: []string{"xiaomi/fp8"}})
	client.WithSessionID("sess-1")
	disabled := false
	client.WithReasoning(&ReasoningControl{Enabled: &disabled})

	resp, err := client.Stream(context.Background(), provider.Request{
		Model:    "test-model",
		Messages: []message.Message{message.User("hi")},
	}, nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if resp.Message.Content != "hi" {
		t.Errorf("content = %q", resp.Message.Content)
	}

	prov, ok := body["provider"].(map[string]any)
	if !ok {
		t.Fatalf("provider block missing from the wire body")
	}
	order, _ := prov["order"].([]any)
	if len(order) != 1 || order[0] != "xiaomi/fp8" {
		t.Errorf("provider.order = %v", prov["order"])
	}
	if body["session_id"] != "sess-1" {
		t.Errorf("session_id = %v, want sess-1", body["session_id"])
	}
	if reas, ok := body["reasoning"].(map[string]any); !ok || reas["enabled"] != false {
		t.Errorf("reasoning = %v, want enabled:false", body["reasoning"])
	}
	if _, exists := body["cache_control"]; exists {
		t.Error("cache_control marker must not be sent to OpenRouter (prompt-cache bytes)")
	}
}

func TestClientStream_ReportsResponseInfoAndFallback(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		var reqBody map[string]any
		_ = json.NewDecoder(r.Body).Decode(&reqBody)
		if reqBody["model"] == "primary" {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"busy"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w, contentChunk("fallback"))
		writeSSE(w, finishChunk("stop"))
		writeSSEDone(w)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "key", "primary", 0.4, 256)
	client.WithFallback("backup", 0.1, 128)

	var infos []ResponseInfo
	hooked := client.WithResponseHook(func(info ResponseInfo) { infos = append(infos, info) })

	if _, err := hooked.Stream(context.Background(), provider.Request{
		Model:    "primary",
		Messages: []message.Message{message.User("hi")},
	}, nil); err != nil {
		t.Fatalf("Stream: %v", err)
	}

	if len(infos) != 1 {
		t.Fatalf("ResponseHook fired %d times, want 1 (only the successful call)", len(infos))
	}
	if !infos[0].UsedFallback {
		t.Error("UsedFallback = false, want true after the primary failed")
	}
	if infos[0].PromptTokens != 10 || infos[0].CostUSD != 0.0001 {
		t.Errorf("usage not mapped: %+v", infos[0])
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2 (primary then fallback)", attempts)
	}
}

// useNovaBackend switches the process-wide transport for one test. The backend
// is package state (see SetBackend), so the test must put it back.
func useNovaBackend(t *testing.T) {
	t.Helper()
	previous := ActiveBackend()
	SetBackend(BackendNova)
	t.Cleanup(func() { SetBackend(previous) })
}

// A fallback answer that carries two tool calls must be cut to the first, like
// the primary's answer is. her-go runs tool calls one at a time, so a second
// call would run against a stale picture of the world.
func TestNovaChatCompletion_FallbackAnswerKeepsOnlyFirstToolCall(t *testing.T) {
	useNovaBackend(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var reqBody map[string]any
		_ = json.NewDecoder(r.Body).Decode(&reqBody)
		if reqBody["model"] == "primary" {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"busy"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w, toolCallChunk(0, "call-a", "alpha", `{}`))
		writeSSE(w, toolCallChunk(1, "call-b", "beta", `{}`))
		writeSSE(w, finishChunk("tool_calls"))
		writeSSEDone(w)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "key", "primary", 0.4, 256)
	client.WithFallback("backup", 0.1, 128)

	tools := []ToolDef{{Type: "function", Function: ToolFunctionDef{
		Name: "alpha", Parameters: map[string]any{"type": "object"},
	}}}
	resp, err := client.ChatCompletionWithTools([]ChatMessage{{Role: "user", Content: "hi"}}, tools)
	if err != nil {
		t.Fatalf("ChatCompletionWithTools: %v", err)
	}
	if !resp.UsedFallback {
		t.Fatal("expected the fallback model to answer")
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Function.Name != "alpha" {
		t.Errorf("tool calls = %+v, want only the first (alpha)", resp.ToolCalls)
	}
}

// If the primary dies after the user already saw part of the reply, a fallback
// would replay the answer from the start. Keep the partial text and surface the
// error instead.
func TestNovaChatCompletion_NoFallbackAfterTextWasStreamed(t *testing.T) {
	useNovaBackend(t)

	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w, contentChunk("The beginning of a"))
		w.(http.Flusher).Flush()
		// Drop the connection mid-stream: no finish frame, no [DONE].
		conn, _, _ := w.(http.Hijacker).Hijack()
		_ = conn.Close()
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "key", "primary", 0.4, 256)
	client.WithFallback("backup", 0.1, 128)

	var got string
	_, err := client.ChatCompletionStreaming([]ChatMessage{{Role: "user", Content: "hi"}}, func(chunk string) { got += chunk })
	if err == nil {
		t.Fatal("expected the mid-stream failure to be reported")
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1: a fallback would replay the streamed text", attempts)
	}
	if got != "The beginning of a" {
		t.Errorf("streamed text = %q", got)
	}
}

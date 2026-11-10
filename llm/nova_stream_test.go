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
	client.ResponseHook = func(info ResponseInfo) { infos = append(infos, info) }

	if _, err := client.Stream(context.Background(), provider.Request{
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

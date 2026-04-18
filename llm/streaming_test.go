package llm

// Tests for the SSE streaming paths: doStreamRequest (agent tool calls) and
// doStreamingChat (chat model text streaming).
//
// Each test spins up a real httptest.Server that speaks the OpenAI SSE format
// and controls exactly what the stream delivers. This lets us verify the abort
// mechanism deterministically — we can inject the exact "model batched two tool
// calls" scenario without relying on a live model to misbehave on demand.
//
// The key behaviors under test:
//   1. Normal single tool call streams cleanly.
//   2. A second tool call (index >= 1) triggers abort — only call 0 comes back.
//   3. Truncated JSON (stream cut before closing brace) returns a retriable error.
//   4. Chat streaming delivers tokens via onChunk and returns the full content.
//   5. ChatCompletionWithTools sends stream:true in the HTTP request body.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// writeSSE writes one SSE data frame followed by a blank line.
// The double newline is the SSE event boundary — the scanner's line-by-line
// read handles it correctly since blank lines are simply skipped.
func writeSSE(w http.ResponseWriter, data string) {
	fmt.Fprintf(w, "data: %s\n\n", data)
}

func writeSSEDone(w http.ResponseWriter) {
	fmt.Fprintf(w, "data: [DONE]\n\n")
}

// toolCallChunk builds one SSE chunk for a tool call delta.
// index identifies which parallel tool call slot this belongs to.
// id/name are only sent in the first chunk for that slot (mirrors real API).
func toolCallChunk(index int, id, name, argsFragment string) string {
	tc := map[string]any{
		"index": index,
		"function": map[string]any{
			"arguments": argsFragment,
		},
	}
	if id != "" {
		tc["id"] = id
		tc["type"] = "function"
	}
	if name != "" {
		tc["function"].(map[string]any)["name"] = name
	}
	body := map[string]any{
		"choices": []map[string]any{
			{
				"delta": map[string]any{
					"tool_calls": []map[string]any{tc},
				},
				"finish_reason": nil,
			},
		},
	}
	b, _ := json.Marshal(body)
	return string(b)
}

// finishChunk builds the final SSE chunk with finish_reason and usage.
func finishChunk(reason string) string {
	body := map[string]any{
		"choices": []map[string]any{
			{"delta": map[string]any{}, "finish_reason": reason},
		},
		"usage": map[string]any{
			"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15, "cost": 0.0001,
		},
		"model": "test-model",
	}
	b, _ := json.Marshal(body)
	return string(b)
}

// contentChunk builds a SSE chunk with a text delta (for chat streaming).
func contentChunk(token string) string {
	body := map[string]any{
		"choices": []map[string]any{
			{"delta": map[string]any{"content": token}, "finish_reason": nil},
		},
	}
	b, _ := json.Marshal(body)
	return string(b)
}

// newTestClient creates an llm.Client pointing at the given test server URL.
func newTestClient(url string) *Client {
	return NewClient(url, "test-key", "test-model", 0.5, 512)
}

// --- Tests ---

// TestDoStreamRequest_SingleToolCall verifies the happy path: one tool call
// arrives cleanly across multiple argument fragments, is assembled correctly,
// and the returned ChatResponse has exactly one ToolCall with valid JSON args.
func TestDoStreamRequest_SingleToolCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// First chunk: announce the tool call (name + id, empty args).
		writeSSE(w, toolCallChunk(0, "call_abc", "think", ""))
		// Next chunks: arguments arrive in fragments, as a real model would send.
		writeSSE(w, toolCallChunk(0, "", "", `{"thought`))
		writeSSE(w, toolCallChunk(0, "", "", `":"planning`))
		writeSSE(w, toolCallChunk(0, "", "", `"}`))
		writeSSE(w, finishChunk("tool_calls"))
		writeSSEDone(w)
	}))
	defer srv.Close()

	client := newTestClient(srv.URL)
	tools := []ToolDef{{Type: "function", Function: ToolFunctionDef{Name: "think"}}}

	resp, err := client.doStreamRequest("test-model", 0.5, 512, nil, tools, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("want 1 tool call, got %d", len(resp.ToolCalls))
	}
	tc := resp.ToolCalls[0]
	if tc.Function.Name != "think" {
		t.Errorf("tool name = %q, want %q", tc.Function.Name, "think")
	}
	if tc.ID != "call_abc" {
		t.Errorf("tool ID = %q, want %q", tc.ID, "call_abc")
	}
	if !json.Valid([]byte(tc.Function.Arguments)) {
		t.Errorf("arguments are not valid JSON: %s", tc.Function.Arguments)
	}
	if tc.Function.Arguments != `{"thought":"planning"}` {
		t.Errorf("arguments = %q, want %q", tc.Function.Arguments, `{"thought":"planning"}`)
	}
}

// TestDoStreamRequest_AbortsOnBatchedToolCalls is the core regression test.
// The mock server sends two tool calls in the same stream — index 0 (think)
// followed immediately by index 1 (reply). This is the exact batching behavior
// that Qwen3 exhibited and caused malformed JSON truncation errors.
//
// Expected: the client aborts the stream on index 1 and returns only call 0.
// The reply call should never be executed by the agent.
func TestDoStreamRequest_AbortsOnBatchedToolCalls(t *testing.T) {
	var streamAborted bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Tool call 0: think (complete, valid JSON).
		writeSSE(w, toolCallChunk(0, "call_think", "think", ""))
		writeSSE(w, toolCallChunk(0, "", "", `{"thought":"I should respond warmly"}`))
		// Tool call 1: reply — model is batching. This should trigger abort.
		writeSSE(w, toolCallChunk(1, "call_reply", "reply", ""))
		writeSSE(w, toolCallChunk(1, "", "", `{"instruction":"Greet`))
		writeSSE(w, toolCallChunk(1, "", "", ` the user warmly"}`))
		writeSSE(w, finishChunk("tool_calls"))
		writeSSEDone(w)
		// If the client aborted correctly, most of these frames were never read.
		streamAborted = true
	}))
	defer srv.Close()

	client := newTestClient(srv.URL)
	tools := []ToolDef{
		{Type: "function", Function: ToolFunctionDef{Name: "think"}},
		{Type: "function", Function: ToolFunctionDef{Name: "reply"}},
	}

	resp, err := client.doStreamRequest("test-model", 0.5, 512, nil, tools, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Critical: exactly one tool call should come back — only think, not reply.
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("want 1 tool call after abort, got %d: %v", len(resp.ToolCalls), resp.ToolCalls)
	}
	if resp.ToolCalls[0].Function.Name != "think" {
		t.Errorf("expected first call to be 'think', got %q", resp.ToolCalls[0].Function.Name)
	}

	// The reply tool call must NOT be present.
	for _, tc := range resp.ToolCalls {
		if tc.Function.Name == "reply" {
			t.Errorf("'reply' tool call should have been aborted, but it's in the response")
		}
	}

	_ = streamAborted // server wrote all frames; client may have read some before abort
}

// TestDoStreamRequest_TruncatedJSON verifies that a stream which ends before
// the tool call's JSON arguments are closed returns an error tagged with
// "truncated tool call arguments" — which isRetriable recognises for fallback.
//
// This simulates the original bug: model hits max_tokens mid-JSON.
func TestDoStreamRequest_TruncatedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w, toolCallChunk(0, "call_abc", "reply", ""))
		// Truncated — JSON never closes.
		writeSSE(w, toolCallChunk(0, "", "", `{"instruction":"Tell the user about`))
		writeSSEDone(w)
	}))
	defer srv.Close()

	client := newTestClient(srv.URL)
	tools := []ToolDef{{Type: "function", Function: ToolFunctionDef{Name: "reply"}}}

	_, err := client.doStreamRequest("test-model", 0.5, 512, nil, tools, nil)
	if err == nil {
		t.Fatal("expected an error for truncated JSON, got nil")
	}
	if !strings.Contains(err.Error(), "truncated tool call arguments") {
		t.Errorf("error = %q, want it to contain 'truncated tool call arguments'", err.Error())
	}
	// Confirm isRetriable returns true so the fallback path fires.
	if !isRetriable(err) {
		t.Errorf("isRetriable(%q) = false, want true — fallback must fire on truncation", err.Error())
	}
}

// TestDoStreamingChat_TokensDelivered verifies that doStreamingChat calls
// onChunk for every content token and assembles the full content in the
// returned ChatResponse.
func TestDoStreamingChat_TokensDelivered(t *testing.T) {
	tokens := []string{"Hello", ", ", "how", " are", " you", "?"}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, tok := range tokens {
			writeSSE(w, contentChunk(tok))
		}
		writeSSE(w, finishChunk("stop"))
		writeSSEDone(w)
	}))
	defer srv.Close()

	client := newTestClient(srv.URL)

	var received []string
	resp, err := client.doStreamingChat("test-model", 0.5, 512, nil, func(tok string) {
		received = append(received, tok)
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Every token must have been delivered via onChunk.
	if len(received) != len(tokens) {
		t.Errorf("onChunk called %d times, want %d", len(received), len(tokens))
	}
	for i, tok := range tokens {
		if i < len(received) && received[i] != tok {
			t.Errorf("received[%d] = %q, want %q", i, received[i], tok)
		}
	}

	// Full content must equal the concatenation of all tokens.
	want := strings.Join(tokens, "")
	if resp.Content != want {
		t.Errorf("Content = %q, want %q", resp.Content, want)
	}
}

// TestChatCompletionWithTools_SendsStreamTrue verifies that ChatCompletionWithTools
// sends "stream":true in the request body when tools are provided. This confirms
// that the agent model always uses the streaming path — the core of the fix.
func TestChatCompletionWithTools_SendsStreamTrue(t *testing.T) {
	var requestBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Capture the request body to inspect it.
		if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		// Respond with a minimal valid SSE stream so the client doesn't error.
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w, toolCallChunk(0, "call_done", "done", "{}"))
		writeSSE(w, finishChunk("tool_calls"))
		writeSSEDone(w)
	}))
	defer srv.Close()

	client := newTestClient(srv.URL)
	tools := []ToolDef{{Type: "function", Function: ToolFunctionDef{Name: "done"}}}

	_, _ = client.ChatCompletionWithTools(nil, tools)

	if requestBody == nil {
		t.Fatal("no request body captured — server may not have been called")
	}
	streamVal, ok := requestBody["stream"]
	if !ok {
		t.Fatal("request body missing 'stream' field — doStreamRequest not used")
	}
	if streamVal != true {
		t.Errorf("stream = %v, want true", streamVal)
	}
}

// TestChatCompletion_NoStreamField verifies that plain ChatCompletion (no tools)
// does NOT send stream:true — it still uses the non-streaming path.
func TestChatCompletion_NoStreamField(t *testing.T) {
	var requestBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&requestBody)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"content": "hello", "role": "assistant"}, "finish_reason": "stop"},
			},
			"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8},
			"model": "test-model",
		})
	}))
	defer srv.Close()

	client := newTestClient(srv.URL)
	_, _ = client.ChatCompletion(nil)

	if v, ok := requestBody["stream"]; ok && v == true {
		t.Errorf("plain ChatCompletion sent stream:true — should use non-streaming path")
	}
}

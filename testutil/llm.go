package testutil

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"her/llm"
)

// MockLLMServer creates an httptest server that serves canned ChatResponse
// objects in order. Each call to /chat/completions returns the next response
// from the queue. If the queue is exhausted, it returns an error.
//
// This is the one place where we mock an LLM — it's an external HTTP
// boundary, not our own code. The mock exercises the full HTTP path
// (JSON serialization, headers, status codes) so it catches integration
// bugs that a pure function mock would miss.
//
// The server is automatically shut down when the test finishes.
//
// Usage:
//
//	srv := testutil.MockLLMServer(t,
//	    llm.ChatResponse{Content: "Hello!"},
//	    llm.ChatResponse{ToolCalls: []llm.ToolCall{...}},
//	)
//	client := llm.NewClient(srv.URL, "test-key", "test-model", 0.0, 100)
//	resp, err := client.ChatCompletion(messages)
//	// resp.Content == "Hello!"
func MockLLMServer(t *testing.T, responses ...llm.ChatResponse) *httptest.Server {
	t.Helper()

	// mu + index track which response to serve next. We need a mutex
	// because httptest servers handle requests concurrently (each request
	// gets its own goroutine), and Go's race detector will flag unsync'd
	// access to the index.
	var mu sync.Mutex
	index := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		if index >= len(responses) {
			http.Error(w, `{"error": "mock LLM: no more canned responses"}`, http.StatusInternalServerError)
			return
		}

		resp := responses[index]
		index++

		// Build a response that matches the OpenAI chat completions API
		// format — this is what llm.Client.doRequest() expects to parse.
		// The structure: { choices: [{message: {content, tool_calls}, finish_reason}], usage: {...}, model: "..." }
		finishReason := "stop"
		if len(resp.ToolCalls) > 0 {
			finishReason = "tool_calls"
		}
		if resp.FinishReason != "" {
			finishReason = resp.FinishReason
		}

		model := resp.Model
		if model == "" {
			model = "test-model"
		}

		apiResp := map[string]interface{}{
			"choices": []map[string]interface{}{
				{
					"message": map[string]interface{}{
						"content":    resp.Content,
						"tool_calls": resp.ToolCalls,
					},
					"finish_reason": finishReason,
				},
			},
			"usage": map[string]interface{}{
				"prompt_tokens":     resp.PromptTokens,
				"completion_tokens": resp.CompletionTokens,
				"total_tokens":      resp.TotalTokens,
				"cost":              resp.CostUSD,
			},
			"model": model,
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(apiResp)
	}))

	t.Cleanup(func() {
		srv.Close()
	})

	return srv
}

// MockLLMClient is a convenience that creates both a MockLLMServer and an
// llm.Client pointed at it. For tests that don't need direct access to
// the server.
func MockLLMClient(t *testing.T, responses ...llm.ChatResponse) *llm.Client {
	t.Helper()

	srv := MockLLMServer(t, responses...)
	return llm.NewClient(srv.URL, "test-key", "test-model", 0.0, 100)
}

// LLMResponse is a shorthand for building a simple text ChatResponse.
// Saves typing in tests that just need the model to say something.
//
//	client := testutil.MockLLMClient(t, testutil.LLMResponse("hello"))
func LLMResponse(content string) llm.ChatResponse {
	return llm.ChatResponse{Content: content}
}

// LLMToolCall builds a ChatResponse where the model calls a single tool.
// arguments should be a JSON string (e.g., `{"text": "hi"}`).
//
//	client := testutil.MockLLMClient(t, testutil.LLMToolCall("reply", `{"text":"hi"}`))
func LLMToolCall(toolName, arguments string) llm.ChatResponse {
	return llm.ChatResponse{
		ToolCalls: []llm.ToolCall{
			{
				ID:   "call_test_" + toolName,
				Type: "function",
				Function: llm.FunctionCall{
					Name:      toolName,
					Arguments: arguments,
				},
			},
		},
	}
}

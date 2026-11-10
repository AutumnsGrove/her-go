// Package llmtest holds helpers for tests that stand in for the LLM provider.
//
// It plays the same role for package llm that net/http/httptest plays for
// net/http: the production package stays free of test code, and every test
// that mocks a provider speaks the same wire format.
//
// Why it exists: the default transport is Nova, which always streams (a caller
// that does not want chunks simply discards them). A mock that answers with one
// plain JSON body therefore no longer parses. Serving the SSE shape from one
// place means the day the wire format changes again, one file changes.
package llmtest

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// WriteTextCompletion writes a complete streamed text answer: one content
// delta, a finish frame carrying token usage, and the [DONE] sentinel.
func WriteTextCompletion(w http.ResponseWriter, content string) {
	w.Header().Set("Content-Type", "text/event-stream")
	delta, _ := json.Marshal(map[string]any{
		"model": "test-model",
		"choices": []map[string]any{
			{"delta": map[string]any{"content": content}, "finish_reason": nil},
		},
	})
	finish, _ := json.Marshal(map[string]any{
		"model": "test-model",
		"choices": []map[string]any{
			{"delta": map[string]any{}, "finish_reason": "stop"},
		},
		"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
	})
	fmt.Fprintf(w, "data: %s\n\n", delta)
	fmt.Fprintf(w, "data: %s\n\n", finish)
	fmt.Fprint(w, "data: [DONE]\n\n")
}

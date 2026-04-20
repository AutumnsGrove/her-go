package agent

// Integration test for RunMemoryAgent.
//
// This test spins up a real HTTP server using net/http/httptest — the same
// package Go's standard library uses to test HTTP handlers. The server speaks
// the OpenAI chat completions JSON format that llm.Client expects, so we're
// testing the full stack from LLM response parsing down to SQLite writes,
// without touching any external API.
//
// Two key design choices here:
//
//  1. ClassifierLLM is nil — classifyMemoryWrite short-circuits to SAVE when
//     the classifier is not configured. This keeps the test focused on the
//     agent loop itself, not the classifier.
//
//  2. memory.NewStore(":memory:", 0) gives us a fully functional SQLite DB
//     with zero teardown — SQLite destroys the in-memory DB when the
//     connection closes. No temp files, no cleanup code needed.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"her/config"
	"her/llm"
	"her/memory"
)

// writeMockSSEToolCall writes an SSE streaming response for a single tool call.
// The agent client now uses doStreamRequest (SSE) for all tool-calling completions,
// so mock servers must respond in SSE format.
//
// Two data frames are sent:
//  1. The tool call delta (index 0, ID, name, full arguments in one chunk)
//  2. The finish frame (finish_reason + usage)
//
// Followed by "data: [DONE]" to signal stream end.
func writeMockSSEToolCall(w http.ResponseWriter, toolName, toolArgs string) {
	w.Header().Set("Content-Type", "text/event-stream")
	// Frame 1: tool call delta with full arguments in one chunk.
	fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_test\",\"type\":\"function\",\"function\":{\"name\":%q,\"arguments\":%s}}]},\"finish_reason\":null}]}\n\n",
		toolName, jsonQuote(toolArgs))
	// Frame 2: finish reason + usage.
	fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15,\"cost\":0.0001},\"model\":\"test-model\"}\n\n")
	fmt.Fprintf(w, "data: [DONE]\n\n")
}

// jsonQuote wraps a raw JSON string in JSON string quotes so it can be
// embedded as a string value inside another JSON object. The arguments
// field in the SSE delta must be a JSON-encoded string, not a raw object.
func jsonQuote(s string) string {
	// Use %q for Go string quoting — it produces valid JSON string syntax
	// for all inputs we use in tests (no special unicode, just ASCII JSON).
	return fmt.Sprintf("%q", s)
}

// TestRunMemoryAgent_SavesMemoryAndCallsDone is the main integration test.
// It verifies that RunMemoryAgent:
//   - calls save_memory when the LLM requests it
//   - the memory lands in the DB (visible via AllActiveMemories)
//   - the loop terminates on done
func TestRunMemoryAgent_SavesMemoryAndCallsDone(t *testing.T) {
	// callCount lets the handler serve different responses per request.
	// atomic.Int32 is used here instead of a mutex because the increment
	// and read happen in the same goroutine (the test server), but
	// atomic is the idiomatic Go choice for simple counters.
	var callCount atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := callCount.Add(1)
		switch n {
		case 1:
			// First request: ask the agent to save a memory.
			writeMockSSEToolCall(w, "save_memory",
				`{"memory":"User prefers stealth builds in FromSoft games","category":"preference","tags":"games, stealth"}`)
		default:
			// Second request (and any beyond): call done to end the loop.
			writeMockSSEToolCall(w, "done", `{}`)
		}
	}))
	defer srv.Close()

	store, err := memory.NewStore(":memory:", 0)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	cfg := &config.Config{
		Identity: config.IdentityConfig{Her: "Mira", User: "Autumn"},
	}

	input := MemoryAgentInput{
		UserMessage:    "I love using stealth builds whenever FromSoft gives me the option",
		ReplyText:      "That's very on-brand for you!",
		TriggerMsgID:   1,
		ConversationID: "test-conv",
	}

	params := MemoryAgentParams{
		LLM:   llm.NewClient(srv.URL, "test-key", "test-model", 0.3, 4096),
		Store: store,
		Cfg:   cfg,
		// ClassifierLLM nil → classifier skips, all writes pass through as SAVE.
		// EmbedClient nil → dedup check skips (no vectors to compare).
	}

	RunMemoryAgent(input, params)

	// Verify the memory was written to SQLite.
	memories, err := store.AllActiveMemories()
	if err != nil {
		t.Fatalf("AllActiveMemories: %v", err)
	}
	if len(memories) == 0 {
		t.Fatal("expected at least one memory to be saved, got none")
	}

	found := false
	for _, m := range memories {
		if m.Content == "User prefers stealth builds in FromSoft games" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("memory not found in DB; saved memories: %v", memories)
	}

	// Verify the loop made at least 2 LLM calls: one for save_memory, one for done.
	if got := int(callCount.Load()); got < 2 {
		t.Errorf("expected ≥2 LLM calls (save_memory + done), got %d", got)
	}
}

// TestRunMemoryAgent_NilLLM verifies the nil guard — calling RunMemoryAgent
// with no LLM configured should return immediately without panicking.
func TestRunMemoryAgent_NilLLM(t *testing.T) {
	store, _ := memory.NewStore(":memory:", 0)
	RunMemoryAgent(
		MemoryAgentInput{UserMessage: "test"},
		MemoryAgentParams{
			LLM:   nil,
			Store: store,
			Cfg:   &config.Config{Identity: config.IdentityConfig{Her: "Mira", User: "Autumn"}},
		},
	)
	// No assertion needed — reaching here without panic is the test.
}

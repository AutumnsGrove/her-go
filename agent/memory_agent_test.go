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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"her/config"
	"her/llm"
	"her/memory"
)

// mockToolCallResponse builds the JSON body the mock server sends back.
// It matches the chatAPIResponse struct in llm/client.go:
//
//	choices[0].message.tool_calls → the tool the model wants to call
//	usage → token counts (required so SaveMetric doesn't get zeros)
//	model  → string (returned in ChatResponse.Model)
func mockToolCallResponse(toolName, toolArgs string) map[string]any {
	return map[string]any{
		"choices": []map[string]any{
			{
				"message": map[string]any{
					"content": "",
					"tool_calls": []map[string]any{
						{
							"id":   "call_test",
							"type": "function",
							"function": map[string]any{
								"name":      toolName,
								"arguments": toolArgs,
							},
						},
					},
				},
				"finish_reason": "tool_calls",
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     10,
			"completion_tokens": 5,
			"total_tokens":      15,
			"cost":              0.0001,
		},
		"model": "test-model",
	}
}

// TestRunMemoryAgent_SavesFactAndCallsDone is the main integration test.
// It verifies that RunMemoryAgent:
//   - calls save_fact when the LLM requests it
//   - the fact lands in the DB (visible via AllActiveFacts)
//   - the loop terminates on done
func TestRunMemoryAgent_SavesFactAndCallsDone(t *testing.T) {
	// callCount lets the handler serve different responses per request.
	// atomic.Int32 is used here instead of a mutex because the increment
	// and read happen in the same goroutine (the test server), but
	// atomic is the idiomatic Go choice for simple counters.
	var callCount atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := callCount.Add(1)

		var resp map[string]any
		switch n {
		case 1:
			// First request: ask the agent to save a fact.
			resp = mockToolCallResponse(
				"save_fact",
				`{"fact":"User prefers stealth builds in FromSoft games","category":"preference","tags":"games, stealth"}`,
			)
		default:
			// Second request (and any beyond): call done to end the loop.
			resp = mockToolCallResponse("done", `{}`)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
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

	// Verify the fact was written to SQLite.
	facts, err := store.AllActiveFacts()
	if err != nil {
		t.Fatalf("AllActiveFacts: %v", err)
	}
	if len(facts) == 0 {
		t.Fatal("expected at least one fact to be saved, got none")
	}

	found := false
	for _, f := range facts {
		if f.Fact == "User prefers stealth builds in FromSoft games" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("fact not found in DB; saved facts: %v", facts)
	}

	// Verify the loop made at least 2 LLM calls: one for save_fact, one for done.
	if got := int(callCount.Load()); got < 2 {
		t.Errorf("expected ≥2 LLM calls (save_fact + done), got %d", got)
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

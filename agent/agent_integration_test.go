package agent

// Integration tests for agent.Run() — the main conversation loop.
//
// These tests spin up two real HTTP servers via net/http/httptest:
//   - agentSrv: simulates the AgentLLM (think/reply/done orchestration)
//   - chatSrv:  simulates the ChatLLM (generates the user-visible reply)
//
// The agent loop itself (compaction, layer building, tool dispatch) runs
// against a real in-memory SQLite store — same approach as memory_agent_test.go.
// We don't mock the store because it's cheap and testing against a real DB
// catches schema issues that a mock would silently swallow.
//
// Most RunParams fields default to nil. agent.Run() degrades gracefully:
// - EmbedClient nil  → semantic search skipped (FTS fallback)
// - TavilyClient nil → web_search returns a clear error string
// - TraceCallback nil → no trace emitted (not tested here)
// - EventBus nil     → TUI events silently dropped
//
// The one non-nil-safe field is ScrubVault: scrub.Deanonymize calls
// vault.Entries() on the receiver, so we always pass scrub.NewVault().

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"her/config"
	"her/llm"
	"her/memory"
	"her/scrub"
)

// mockChatResponse builds the JSON body the chat mock server sends back.
// Unlike mockToolCallResponse (which makes the model call a tool), this
// is a regular assistant message — the format ChatCompletion expects.
//
// content must be at least 10 characters to pass the isDegenerate check
// in execReply. Anything shorter causes a retry (and a second mock call).
func mockChatResponse(content string) map[string]any {
	return map[string]any{
		"choices": []map[string]any{
			{
				"message": map[string]any{
					"content": content,
					"role":    "assistant",
				},
				"finish_reason": "stop",
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

// minimalCfg returns the minimum Config needed for agent.Run() to proceed
// without panicking. Memory fields are set to typical values to avoid
// edge cases in compaction and window trimming.
func minimalCfg() *config.Config {
	return &config.Config{
		Identity: config.IdentityConfig{Her: "Mira", User: "Autumn"},
		Memory: config.MemoryConfig{
			RecentMessages:   6,
			MaxHistoryTokens: 8000,
			MaxFactsInContext: 5,
		},
	}
}

// buildRunParams assembles a RunParams pointing at the given mock servers,
// with an in-memory store, minimal config, and a StatusCallback that captures
// the first reply text. Most optional fields are left nil.
//
// The StatusCallback is the first-reply path in execReply — before ReplyCalled
// is set, the text is delivered here (simulating editing the Telegram placeholder).
func buildRunParams(t *testing.T, agentURL, chatURL string, captured *string) RunParams {
	t.Helper()
	store, err := memory.NewStore(":memory:", 0)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return RunParams{
		AgentLLM:            llm.NewClient(agentURL, "test-key", "test-model", 0.1, 4096),
		ChatLLM:             llm.NewClient(chatURL, "test-key", "test-model", 0.1, 4096),
		Store:               store,
		Cfg:                 minimalCfg(),
		ScrubVault:          scrub.NewVault(), // must not be nil — Deanonymize calls vault.Entries()
		ScrubbedUserMessage: "Hello there",
		ConversationID:      "test-conv",
		StatusCallback: func(text string) error {
			if captured != nil {
				*captured = text
			}
			return nil
		},
	}
}

// TestRun_BasicTurn verifies the happy path: the agent calls think → reply → done
// and the reply text reaches the StatusCallback and RunResult.ReplyText.
//
// This is the minimum viable agent turn: one reasoning step, one reply, done.
func TestRun_BasicTurn(t *testing.T) {
	var agentCallCount atomic.Int32

	agentSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := agentCallCount.Add(1)
		switch n {
		case 1:
			writeMockSSEToolCall(w, "think", `{"thought":"The user said hello. I should greet them warmly."}`)
		case 2:
			writeMockSSEToolCall(w, "reply", `{"instruction":"Greet the user warmly and ask how they are."}`)
		default:
			writeMockSSEToolCall(w, "done", `{}`)
		}
	}))
	defer agentSrv.Close()

	chatSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(mockChatResponse("Hey, good to hear from you! How are things going?"))
	}))
	defer chatSrv.Close()

	var captured string
	params := buildRunParams(t, agentSrv.URL, chatSrv.URL, &captured)

	result, err := Run(params)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.ReplyText == "" {
		t.Fatal("ReplyText is empty — reply was never delivered")
	}
	if result.ReplyText != captured {
		t.Errorf("ReplyText = %q, StatusCallback got %q — should be the same", result.ReplyText, captured)
	}
	// think + reply + done = 3 tool calls minimum
	if result.ToolCalls < 3 {
		t.Errorf("ToolCalls = %d, want ≥3 (think+reply+done)", result.ToolCalls)
	}
}

// TestRun_ToolFailureTurn verifies that an unknown tool call doesn't crash the loop.
// The agent calls an unregistered tool, receives a clear error string back, then
// continues on to reply and done. This is the log-and-skip resilience behavior.
//
// Before Phase 2, an unregistered tool could cause a panic. Now tools.Execute
// returns "error: tool 'X' not registered" and the agent sees it as a tool result.
func TestRun_ToolFailureTurn(t *testing.T) {
	var agentCallCount atomic.Int32

	agentSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := agentCallCount.Add(1)
		switch n {
		case 1:
			writeMockSSEToolCall(w, "think", `{"thought":"Let me try a tool."}`)
		case 2:
			// This tool doesn't exist — tools.Execute returns a clear error string.
			writeMockSSEToolCall(w, "completely_nonexistent_tool_xyz", `{}`)
		case 3:
			writeMockSSEToolCall(w, "reply", `{"instruction":"Respond to the user."}`)
		default:
			writeMockSSEToolCall(w, "done", `{}`)
		}
	}))
	defer agentSrv.Close()

	chatSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(mockChatResponse("Here is my response to you!"))
	}))
	defer chatSrv.Close()

	result, err := Run(buildRunParams(t, agentSrv.URL, chatSrv.URL, nil))
	if err != nil {
		t.Fatalf("Run returned error after tool failure: %v", err)
	}
	if result.ReplyText == "" {
		t.Fatal("ReplyText is empty — agent loop did not survive the tool failure")
	}
}

// TestRun_ContinuationTurn verifies the continuation window mechanism.
// The agent exhausts all 15 iterations of window 0 without calling done,
// which triggers a new window (window 1) with a progress summary injected.
// The agent then calls reply + done in window 1.
//
// This is the "agent ran out of time" recovery path described in REFACTOR.md §
// "Continuation windows (issue #48)". Without it, a complex turn that needed
// more than 15 LLM calls would silently end with no reply.
func TestRun_ContinuationTurn(t *testing.T) {
	var agentCallCount atomic.Int32

	agentSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(agentCallCount.Add(1))
		switch {
		case n <= 15:
			// Fill all 15 iterations of window 0 with think calls.
			// Each has unique content to avoid the think-loop detector
			// (agent.go:548), which bails if the same thought repeats twice.
			writeMockSSEToolCall(w, "think",
				fmt.Sprintf(`{"thought":"still working through the problem, step %d of 15"}`, n))
		case n == 16:
			// Window 1 starts. Continuation context was injected — the agent
			// should now reply to update the user on progress.
			writeMockSSEToolCall(w, "reply", `{"instruction":"Here is my update after working through it."}`)
		default:
			writeMockSSEToolCall(w, "done", `{}`)
		}
	}))
	defer agentSrv.Close()

	chatSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(mockChatResponse("Okay, I worked through all the steps. Here's what I found!"))
	}))
	defer chatSrv.Close()

	result, err := Run(buildRunParams(t, agentSrv.URL, chatSrv.URL, nil))
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.ReplyText == "" {
		t.Fatal("ReplyText is empty — continuation window did not recover")
	}
	// Verify the continuation actually fired: window 0 used 15 calls,
	// window 1 used at least 2 more (reply + done) = 17 total minimum.
	if got := int(agentCallCount.Load()); got < 17 {
		t.Errorf("AgentLLM call count = %d, want ≥17 (15 window-0 + reply + done)", got)
	}
}

// TestRun_DeferredSearchLoad verifies that use_tools("search") correctly loads
// the web_search and web_read tools into the active tool set mid-turn.
//
// The agent calls use_tools → web_search (which fails gracefully because
// TavilyClient is nil) → reply → done. The key behavior: web_search is
// dispatched as a known handler (not "unknown tool") after use_tools loads it.
// A nil TavilyClient returns a clear error string, not a panic.
func TestRun_DeferredSearchLoad(t *testing.T) {
	var agentCallCount atomic.Int32

	agentSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := agentCallCount.Add(1)
		switch n {
		case 1:
			// Load the search category. use_tools reads categories.yaml and
			// adds web_search + web_read to the active toolDefs slice.
			writeMockSSEToolCall(w, "use_tools", `{"tools":["search"]}`)
		case 2:
			// web_search is now a known handler. With nil TavilyClient, it
			// returns "error: web search not configured..." — not "unknown tool".
			writeMockSSEToolCall(w, "web_search", `{"query":"test query"}`)
		case 3:
			writeMockSSEToolCall(w, "reply", `{"instruction":"Here is what I found (or couldn't find)."}`)
		default:
			writeMockSSEToolCall(w, "done", `{}`)
		}
	}))
	defer agentSrv.Close()

	chatSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(mockChatResponse("I searched but didn't find specific results for that."))
	}))
	defer chatSrv.Close()

	result, err := Run(buildRunParams(t, agentSrv.URL, chatSrv.URL, nil))
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.ReplyText == "" {
		t.Fatal("ReplyText is empty — deferred search load did not complete cleanly")
	}
}

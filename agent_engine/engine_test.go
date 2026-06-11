package agent_engine

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"her/llm"
	"her/tools"
)

// ---------------------------------------------------------------------------
// Mock LLM server — returns scripted SSE responses
// ---------------------------------------------------------------------------

// scriptedResponse defines one LLM response in a test scenario.
type scriptedResponse struct {
	toolCalls    []llm.ToolCall // if non-empty, model returns tool calls
	content      string         // text content (used when no tool calls)
	finishReason string         // "tool_calls", "stop", etc.
}

// newMockLLM creates an httptest server that returns scripted SSE responses
// in order. Each call to the /chat/completions endpoint pops the next
// response from the queue.
//
// Returns the server (caller must defer Close) and an llm.Client pointed at it.
func newMockLLM(t *testing.T, responses []scriptedResponse) (*httptest.Server, *llm.Client) {
	t.Helper()
	var mu sync.Mutex
	callIndex := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		idx := callIndex
		callIndex++
		mu.Unlock()

		if idx >= len(responses) {
			t.Errorf("mock LLM: unexpected call #%d (only %d responses scripted)", idx, len(responses))
			http.Error(w, "no more scripted responses", http.StatusInternalServerError)
			return
		}

		resp := responses[idx]
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		finishReason := resp.finishReason
		if finishReason == "" {
			if len(resp.toolCalls) > 0 {
				finishReason = "tool_calls"
			} else {
				finishReason = "stop"
			}
		}

		// Emit tool call chunks (one per tool call, index 0 only since
		// the client aborts on index >= 1).
		if len(resp.toolCalls) > 0 {
			tc := resp.toolCalls[0]
			argsJSON, _ := json.Marshal(tc.Function.Arguments)
			// Unwrap the double-encoding: json.Marshal on a string adds quotes.
			argsStr := string(argsJSON)
			// We need raw arguments string in the SSE, not quoted.
			if len(argsStr) >= 2 && argsStr[0] == '"' {
				argsStr = tc.Function.Arguments
			}

			chunk := fmt.Sprintf(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"%s","type":"function","function":{"name":"%s","arguments":"%s"}}]},"finish_reason":null}],"model":"test-model","usage":null}`,
				tc.ID, tc.Function.Name, escapeJSONString(argsStr))
			fmt.Fprintf(w, "data: %s\n\n", chunk)
		}

		// Content chunk (if any).
		if resp.content != "" {
			chunk := fmt.Sprintf(`{"choices":[{"delta":{"content":"%s"},"finish_reason":null}],"model":"test-model"}`,
				escapeJSONString(resp.content))
			fmt.Fprintf(w, "data: %s\n\n", chunk)
		}

		// Final chunk with finish reason and usage.
		finalChunk := fmt.Sprintf(`{"choices":[{"delta":{},"finish_reason":"%s"}],"model":"test-model","usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150,"cost":0.001}}`,
			finishReason)
		fmt.Fprintf(w, "data: %s\n\n", finalChunk)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))

	client := llm.NewClient(srv.URL, "test-key", "test-model", 0.7, 1000)
	return srv, client
}

// escapeJSONString escapes special chars for embedding in a JSON string value.
func escapeJSONString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, "\t", `\t`)
	return s
}

// ---------------------------------------------------------------------------
// Minimal tool handler for tests
// ---------------------------------------------------------------------------

// stubDoneTool registers a "done" tool that sets DoneCalled=true.
// Returns the tool definitions slice.
func stubToolDefs() []llm.ToolDef {
	return []llm.ToolDef{
		{
			Type: "function",
			Function: llm.ToolFunctionDef{
				Name:        "think",
				Description: "Think about something",
				Parameters:  map[string]any{"type": "object", "properties": map[string]any{"thought": map[string]any{"type": "string"}}},
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunctionDef{
				Name:        "done",
				Description: "Signal completion",
				Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
			},
		},
	}
}

func toolCall(id, name, args string) llm.ToolCall {
	return llm.ToolCall{
		ID:   id,
		Type: "function",
		Function: llm.FunctionCall{
			Name:      name,
			Arguments: args,
		},
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestRunLoop_BasicDoneSignal(t *testing.T) {
	// Scenario: model calls think, then done. Loop exits with "done".
	srv, client := newMockLLM(t, []scriptedResponse{
		{toolCalls: []llm.ToolCall{toolCall("tc1", "think", `{"thought":"hello"}`)}},
		{toolCalls: []llm.ToolCall{toolCall("tc2", "done", `{}`)}},
	})
	defer srv.Close()

	tctx := &tools.Context{AgentName: "test"}

	// Register a minimal done handler that sets DoneCalled.
	doneCalled := false
	origExecute := tools.Execute
	_ = origExecute // tools.Execute is a function, not easily mockable

	// Instead of mocking Execute, we'll set DoneCalled manually via PostTool.
	cfg := EngineConfig{
		Name:     "test",
		LLM:      client,
		ToolDefs: stubToolDefs(),
		ToolCtx:  tctx,
		Messages: []llm.ChatMessage{
			{Role: "system", Content: "You are a test agent."},
			{Role: "user", Content: "Test message"},
		},
		IterationsPerWindow: 10,
		MaxContinuations:    1,
		PostTool: func(tc llm.ToolCall, result string, isError bool) {
			if tc.Function.Name == "done" {
				doneCalled = true
			}
		},
	}

	// Manually set DoneCalled since tools.Execute won't find a registered "done" handler
	// in test context (the done tool registers itself via blank import).
	// We use PostTool to detect the done call and set the flag.
	originalPostTool := cfg.PostTool
	cfg.PostTool = func(tc llm.ToolCall, result string, isError bool) {
		if tc.Function.Name == "done" {
			tctx.DoneCalled = true
		}
		if originalPostTool != nil {
			originalPostTool(tc, result, isError)
		}
	}

	result, err := RunLoop(cfg)
	if err != nil {
		t.Fatalf("RunLoop error: %v", err)
	}

	if result.ExitReason != ExitDone {
		t.Errorf("expected exit reason %q, got %q", ExitDone, result.ExitReason)
	}
	if result.Iterations != 2 {
		t.Errorf("expected 2 iterations, got %d", result.Iterations)
	}
	if result.ToolCalls != 2 {
		t.Errorf("expected 2 tool calls, got %d", result.ToolCalls)
	}
	if !doneCalled {
		t.Error("PostTool hook was not called for done")
	}
}

func TestRunLoop_NoToolCallsExit(t *testing.T) {
	// Scenario: model returns text with no tool calls.
	srv, client := newMockLLM(t, []scriptedResponse{
		{content: "I'm done thinking.", finishReason: "stop"},
	})
	defer srv.Close()

	tctx := &tools.Context{AgentName: "test"}
	cfg := EngineConfig{
		Name:     "test",
		LLM:      client,
		ToolDefs: stubToolDefs(),
		ToolCtx:  tctx,
		Messages: []llm.ChatMessage{
			{Role: "system", Content: "Test"},
			{Role: "user", Content: "Test"},
		},
		IterationsPerWindow: 10,
		MaxContinuations:    1,
	}

	result, err := RunLoop(cfg)
	if err != nil {
		t.Fatalf("RunLoop error: %v", err)
	}

	if result.ExitReason != ExitNoToolCalls {
		t.Errorf("expected exit reason %q, got %q", ExitNoToolCalls, result.ExitReason)
	}
	if result.Iterations != 1 {
		t.Errorf("expected 1 iteration, got %d", result.Iterations)
	}
	if result.ToolCalls != 0 {
		t.Errorf("expected 0 tool calls, got %d", result.ToolCalls)
	}
}

func TestRunLoop_OnNoToolCallsHook(t *testing.T) {
	// Scenario: model returns text, but OnNoToolCalls handles it (returns true).
	// Then the model returns a tool call with done on the next iteration.
	srv, client := newMockLLM(t, []scriptedResponse{
		{content: "done", finishReason: "stop"},
		{toolCalls: []llm.ToolCall{toolCall("tc1", "done", `{}`)}},
	})
	defer srv.Close()

	tctx := &tools.Context{AgentName: "test"}
	hookCalled := false

	cfg := EngineConfig{
		Name:     "test",
		LLM:      client,
		ToolDefs: stubToolDefs(),
		ToolCtx:  tctx,
		Messages: []llm.ChatMessage{
			{Role: "system", Content: "Test"},
			{Role: "user", Content: "Test"},
		},
		IterationsPerWindow: 10,
		MaxContinuations:    1,
		OnNoToolCalls: func(resp *llm.ChatResponse) bool {
			hookCalled = true
			return true // handled — don't break
		},
		PostTool: func(tc llm.ToolCall, result string, isError bool) {
			if tc.Function.Name == "done" {
				tctx.DoneCalled = true
			}
		},
	}

	result, err := RunLoop(cfg)
	if err != nil {
		t.Fatalf("RunLoop error: %v", err)
	}

	if !hookCalled {
		t.Error("OnNoToolCalls hook was not called")
	}
	if result.ExitReason != ExitDone {
		t.Errorf("expected exit reason %q, got %q", ExitDone, result.ExitReason)
	}
	if result.Iterations != 2 {
		t.Errorf("expected 2 iterations, got %d", result.Iterations)
	}
}

func TestRunLoop_ContinuationWindow(t *testing.T) {
	// Scenario: exhaust 2 iterations per window, then done in the continuation.
	// Window 0: think, think (exhausted)
	// Window 1: done
	srv, client := newMockLLM(t, []scriptedResponse{
		{toolCalls: []llm.ToolCall{toolCall("tc1", "think", `{"thought":"first"}`)}},
		{toolCalls: []llm.ToolCall{toolCall("tc2", "think", `{"thought":"second"}`)}},
		{toolCalls: []llm.ToolCall{toolCall("tc3", "done", `{}`)}},
	})
	defer srv.Close()

	tctx := &tools.Context{AgentName: "test"}
	var continuationMsg string

	cfg := EngineConfig{
		Name:     "test",
		LLM:      client,
		ToolDefs: stubToolDefs(),
		ToolCtx:  tctx,
		Messages: []llm.ChatMessage{
			{Role: "system", Content: "Test"},
			{Role: "user", Content: "Test"},
		},
		IterationsPerWindow: 2,
		MaxContinuations:    2,
		ContinuationMsg: func(window, maxWindows int, summary string) string {
			continuationMsg = fmt.Sprintf("Window %d of %d", window, maxWindows)
			return continuationMsg
		},
		PostTool: func(tc llm.ToolCall, result string, isError bool) {
			if tc.Function.Name == "done" {
				tctx.DoneCalled = true
			}
		},
	}

	result, err := RunLoop(cfg)
	if err != nil {
		t.Fatalf("RunLoop error: %v", err)
	}

	if result.ExitReason != ExitDone {
		t.Errorf("expected exit reason %q, got %q", ExitDone, result.ExitReason)
	}
	if result.Iterations != 3 {
		t.Errorf("expected 3 iterations, got %d", result.Iterations)
	}
	if continuationMsg == "" {
		t.Error("ContinuationMsg hook was not called")
	}
	if continuationMsg != "Window 1 of 2" {
		t.Errorf("expected continuation msg 'Window 1 of 2', got %q", continuationMsg)
	}
}

func TestRunLoop_MaxContinuationsExhausted(t *testing.T) {
	// Scenario: 1 iter per window, 1 continuation. 2 thinks = exhausted.
	srv, client := newMockLLM(t, []scriptedResponse{
		{toolCalls: []llm.ToolCall{toolCall("tc1", "think", `{"thought":"one"}`)}},
		{toolCalls: []llm.ToolCall{toolCall("tc2", "think", `{"thought":"two"}`)}},
	})
	defer srv.Close()

	tctx := &tools.Context{AgentName: "test"}

	cfg := EngineConfig{
		Name:                "test",
		LLM:                 client,
		ToolDefs:            stubToolDefs(),
		ToolCtx:             tctx,
		Messages:            []llm.ChatMessage{{Role: "system", Content: "Test"}, {Role: "user", Content: "Test"}},
		IterationsPerWindow: 1,
		MaxContinuations:    1,
	}

	result, err := RunLoop(cfg)
	if err != nil {
		t.Fatalf("RunLoop error: %v", err)
	}

	if result.ExitReason != ExitMaxContinuations {
		t.Errorf("expected exit reason %q, got %q", ExitMaxContinuations, result.ExitReason)
	}
}

func TestRunLoop_ToolChoiceFirst(t *testing.T) {
	// Verify ToolChoiceFirst is passed on the first call.
	// We can't directly inspect the request from here, but we can verify
	// the config field is respected (the mock server doesn't care about
	// tool_choice, so this is a structural test).
	srv, client := newMockLLM(t, []scriptedResponse{
		{toolCalls: []llm.ToolCall{toolCall("tc1", "done", `{}`)}},
	})
	defer srv.Close()

	tctx := &tools.Context{AgentName: "test"}

	cfg := EngineConfig{
		Name:            "test",
		LLM:             client,
		ToolDefs:        stubToolDefs(),
		ToolCtx:         tctx,
		Messages:        []llm.ChatMessage{{Role: "system", Content: "Test"}, {Role: "user", Content: "Test"}},
		ToolChoiceFirst: "required",
		PostTool: func(tc llm.ToolCall, result string, isError bool) {
			if tc.Function.Name == "done" {
				tctx.DoneCalled = true
			}
		},
	}

	result, err := RunLoop(cfg)
	if err != nil {
		t.Fatalf("RunLoop error: %v", err)
	}
	if result.ExitReason != ExitDone {
		t.Errorf("expected %q, got %q", ExitDone, result.ExitReason)
	}
}

func TestRunLoop_PreToolSkip(t *testing.T) {
	// Scenario: PreTool returns skip=true for think, allowing done through.
	srv, client := newMockLLM(t, []scriptedResponse{
		{toolCalls: []llm.ToolCall{toolCall("tc1", "think", `{"thought":"skip me"}`)}},
		{toolCalls: []llm.ToolCall{toolCall("tc2", "done", `{}`)}},
	})
	defer srv.Close()

	tctx := &tools.Context{AgentName: "test"}
	skippedTools := 0

	cfg := EngineConfig{
		Name:     "test",
		LLM:      client,
		ToolDefs: stubToolDefs(),
		ToolCtx:  tctx,
		Messages: []llm.ChatMessage{{Role: "system", Content: "Test"}, {Role: "user", Content: "Test"}},
		PreTool: func(tc llm.ToolCall, tctx *tools.Context) (string, bool) {
			if tc.Function.Name == "think" {
				skippedTools++
				return "skipped by test", true
			}
			return "", false
		},
		PostTool: func(tc llm.ToolCall, result string, isError bool) {
			if tc.Function.Name == "done" {
				tctx.DoneCalled = true
			}
		},
	}

	result, err := RunLoop(cfg)
	if err != nil {
		t.Fatalf("RunLoop error: %v", err)
	}

	if skippedTools != 1 {
		t.Errorf("expected 1 skipped tool, got %d", skippedTools)
	}
	if result.ExitReason != ExitDone {
		t.Errorf("expected %q, got %q", ExitDone, result.ExitReason)
	}
	// The skipped tool still counts as a tool call.
	if result.ToolCalls != 2 {
		t.Errorf("expected 2 tool calls, got %d", result.ToolCalls)
	}
}

func TestRunLoop_PostIterationBreak(t *testing.T) {
	// Scenario: PostIteration returns true on first iteration = loop breaks.
	srv, client := newMockLLM(t, []scriptedResponse{
		{toolCalls: []llm.ToolCall{toolCall("tc1", "think", `{"thought":"one"}`)}},
	})
	defer srv.Close()

	tctx := &tools.Context{AgentName: "test"}

	cfg := EngineConfig{
		Name:     "test",
		LLM:      client,
		ToolDefs: stubToolDefs(),
		ToolCtx:  tctx,
		Messages: []llm.ChatMessage{{Role: "system", Content: "Test"}, {Role: "user", Content: "Test"}},
		PostIteration: func(iteration, window int, resp *llm.ChatResponse) bool {
			return true // always break
		},
	}

	result, err := RunLoop(cfg)
	if err != nil {
		t.Fatalf("RunLoop error: %v", err)
	}

	if result.ExitReason != ExitHookBreak {
		t.Errorf("expected %q, got %q", ExitHookBreak, result.ExitReason)
	}
	// PostIteration fires BEFORE tool execution, so no tools should have run.
	if result.ToolCalls != 0 {
		t.Errorf("expected 0 tool calls, got %d", result.ToolCalls)
	}
}

func TestRunLoop_TracingBuiltIn(t *testing.T) {
	// Verify that traces are automatically generated via FormatTrace.
	srv, client := newMockLLM(t, []scriptedResponse{
		{toolCalls: []llm.ToolCall{toolCall("tc1", "think", `{"thought":"hmm"}`)}},
		{content: "done"},
	})
	defer srv.Close()

	tctx := &tools.Context{AgentName: "test"}
	var traceOutput []string

	cfg := EngineConfig{
		Name:     "test",
		LLM:      client,
		ToolDefs: stubToolDefs(),
		ToolCtx:  tctx,
		Messages: []llm.ChatMessage{{Role: "system", Content: "Test"}, {Role: "user", Content: "Test"}},
		TraceCallback: func(text string) error {
			traceOutput = append(traceOutput, text)
			return nil
		},
	}

	result, err := RunLoop(cfg)
	if err != nil {
		t.Fatalf("RunLoop error: %v", err)
	}

	if len(result.TraceLines) == 0 {
		t.Error("expected trace lines to be populated")
	}
	if len(traceOutput) == 0 {
		t.Error("expected TraceCallback to be called")
	}
}

func TestRunLoop_LiteToolHook(t *testing.T) {
	// Verify LiteToolHook fires with tool names.
	srv, client := newMockLLM(t, []scriptedResponse{
		{toolCalls: []llm.ToolCall{toolCall("tc1", "think", `{"thought":"x"}`)}},
		{content: "done"},
	})
	defer srv.Close()

	tctx := &tools.Context{AgentName: "test"}
	var liteTools []string

	cfg := EngineConfig{
		Name:     "test",
		LLM:      client,
		ToolDefs: stubToolDefs(),
		ToolCtx:  tctx,
		Messages: []llm.ChatMessage{{Role: "system", Content: "Test"}, {Role: "user", Content: "Test"}},
		LiteToolHook: func(name string) {
			liteTools = append(liteTools, name)
		},
	}

	_, err := RunLoop(cfg)
	if err != nil {
		t.Fatalf("RunLoop error: %v", err)
	}

	if len(liteTools) != 1 {
		t.Errorf("expected 1 lite tool hook call, got %d", len(liteTools))
	}
	if len(liteTools) > 0 && liteTools[0] != "think" {
		t.Errorf("expected lite tool 'think', got %q", liteTools[0])
	}
}

func TestRunLoop_OnLoopExit(t *testing.T) {
	// Verify OnLoopExit fires with the correct exit reason.
	srv, client := newMockLLM(t, []scriptedResponse{
		{content: "nothing to do"},
	})
	defer srv.Close()

	tctx := &tools.Context{AgentName: "test"}
	var exitReason string

	cfg := EngineConfig{
		Name:     "test",
		LLM:      client,
		ToolDefs: stubToolDefs(),
		ToolCtx:  tctx,
		Messages: []llm.ChatMessage{{Role: "system", Content: "Test"}, {Role: "user", Content: "Test"}},
		OnLoopExit: func(reason string, messages []llm.ChatMessage) {
			exitReason = reason
		},
	}

	result, err := RunLoop(cfg)
	if err != nil {
		t.Fatalf("RunLoop error: %v", err)
	}

	if exitReason != ExitNoToolCalls {
		t.Errorf("OnLoopExit got reason %q, expected %q", exitReason, ExitNoToolCalls)
	}
	if result.ExitReason != exitReason {
		t.Errorf("result.ExitReason %q != OnLoopExit reason %q", result.ExitReason, exitReason)
	}
}

func TestRunLoop_ActiveToolGuard(t *testing.T) {
	// Scenario: ActiveToolGuard rejects think, allows done.
	srv, client := newMockLLM(t, []scriptedResponse{
		{toolCalls: []llm.ToolCall{toolCall("tc1", "think", `{"thought":"blocked"}`)}},
		{toolCalls: []llm.ToolCall{toolCall("tc2", "done", `{}`)}},
	})
	defer srv.Close()

	tctx := &tools.Context{AgentName: "test"}
	rejectedTools := 0

	cfg := EngineConfig{
		Name:     "test",
		LLM:      client,
		ToolDefs: stubToolDefs(),
		ToolCtx:  tctx,
		Messages: []llm.ChatMessage{{Role: "system", Content: "Test"}, {Role: "user", Content: "Test"}},
		ActiveToolGuard: func(tc llm.ToolCall) (string, bool) {
			if tc.Function.Name == "think" {
				rejectedTools++
				return "error: tool not available", true
			}
			return "", false
		},
		PostTool: func(tc llm.ToolCall, result string, isError bool) {
			if tc.Function.Name == "done" {
				tctx.DoneCalled = true
			}
		},
	}

	result, err := RunLoop(cfg)
	if err != nil {
		t.Fatalf("RunLoop error: %v", err)
	}

	if rejectedTools != 1 {
		t.Errorf("expected 1 rejected tool, got %d", rejectedTools)
	}
	if result.ExitReason != ExitDone {
		t.Errorf("expected %q, got %q", ExitDone, result.ExitReason)
	}
	// Rejected tools don't count in ToolCalls (they never executed).
	if result.ToolCalls != 1 {
		t.Errorf("expected 1 tool call (done only), got %d", result.ToolCalls)
	}
}

func TestRunLoop_LLMError(t *testing.T) {
	// Scenario: mock server returns 500 = LLM error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := llm.NewClient(srv.URL, "test-key", "test-model", 0.7, 1000)
	tctx := &tools.Context{AgentName: "test"}

	cfg := EngineConfig{
		Name:     "test",
		LLM:      client,
		ToolDefs: stubToolDefs(),
		ToolCtx:  tctx,
		Messages: []llm.ChatMessage{{Role: "system", Content: "Test"}, {Role: "user", Content: "Test"}},
	}

	result, err := RunLoop(cfg)
	if err != nil {
		t.Fatalf("RunLoop should not return error (it handles LLM errors internally), got: %v", err)
	}

	if result.ExitReason != ExitError {
		t.Errorf("expected exit reason %q, got %q", ExitError, result.ExitReason)
	}
}

// ---------------------------------------------------------------------------
// Helper tests
// ---------------------------------------------------------------------------

func TestBuildContinuationSummary(t *testing.T) {
	lines := []string{
		"🧠 <b>think:</b> <i>should I search?</i>",
		"🔍 <b>web_search:</b> coffee Portland",
	}
	summary := BuildContinuationSummary(lines)

	if strings.Contains(summary, "<b>") {
		t.Error("summary should not contain HTML tags")
	}
	if !strings.Contains(summary, "think:") {
		t.Error("summary should contain tool names")
	}
}

func TestBuildContinuationSummary_Truncation(t *testing.T) {
	// Build a trace list that exceeds 500 chars.
	var lines []string
	for i := 0; i < 50; i++ {
		lines = append(lines, fmt.Sprintf("🔧 <b>tool_%d:</b> some result text here", i))
	}
	summary := BuildContinuationSummary(lines)

	if len(summary) > 504 { // 500 + "..."
		t.Errorf("summary too long: %d chars", len(summary))
	}
	if !strings.HasSuffix(summary, "...") {
		t.Error("truncated summary should end with ...")
	}
}

func TestTruncateLog(t *testing.T) {
	tests := []struct {
		input  string
		maxLen int
		want   string
	}{
		{"short", 10, "short"},
		{"this is long", 7, "this is..."},
		{"has\nnewlines\nin it", 20, "has newlines in it"},
	}
	for _, tt := range tests {
		got := TruncateLog(tt.input, tt.maxLen)
		if got != tt.want {
			t.Errorf("TruncateLog(%q, %d) = %q, want %q", tt.input, tt.maxLen, got, tt.want)
		}
	}
}

func TestCoerce(t *testing.T) {
	tests := []struct {
		val, def, cap, want int
	}{
		{0, 15, 50, 15},   // zero → default
		{-1, 15, 50, 15},  // negative → default
		{10, 15, 50, 10},  // in range → val
		{100, 15, 50, 50}, // over cap → cap
	}
	for _, tt := range tests {
		got := coerce(tt.val, tt.def, tt.cap)
		if got != tt.want {
			t.Errorf("coerce(%d, %d, %d) = %d, want %d", tt.val, tt.def, tt.cap, got, tt.want)
		}
	}
}

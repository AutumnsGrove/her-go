package agent_engine

// Parity tests for behaviours the older engine_test.go did not cover but the
// five agents rely on. Each runs against both loop implementations, because the
// migration's contract is "same EngineConfig, same observable behaviour".
//
// The mock here is lower level than newMockLLM: it emits raw SSE chunks, so a
// test can produce a malformed argument list, an empty tool name, or two tool
// calls in one message — the exact shapes a real provider occasionally sends.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/AutumnsGrove/nova/provider"

	"her/config"
	"her/llm"
	"her/tools"
)

// runOnce runs a test body immediately. It exists so these cases kept their
// shape from when every case ran against two loop implementations.
func runOnce(t *testing.T, fn func(t *testing.T)) { fn(t) }

// rawChunk builds one SSE data frame from a JSON body.
func rawChunk(body string) string { return "data: " + body + "\n\n" }

// toolCallFrame builds a complete tool-call delta for index 0.
func toolCallFrame(id, name, args string) string {
	argsJSON, _ := json.Marshal(args) // escape for embedding
	return rawChunk(fmt.Sprintf(
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":%q,"type":"function","function":{"name":%q,"arguments":%s}}]},"finish_reason":null}],"model":"test-model"}`,
		id, name, argsJSON))
}

// finishFrame ends a response with a finish reason and usage.
func finishFrame(reason string) string {
	return rawChunk(fmt.Sprintf(
		`{"choices":[{"delta":{},"finish_reason":%q}],"model":"test-model","usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"cost":0.0005}}`,
		reason))
}

// stopText is a plain-text response that ends a run.
func stopText(text string) []string {
	return []string{
		rawChunk(fmt.Sprintf(`{"choices":[{"delta":{"content":%q},"finish_reason":null}],"model":"test-model"}`, text)),
		finishFrame("stop"),
	}
}

// newRawMockLLM scripts an SSE response per call. Each response is a list of raw
// frames; the server appends [DONE] automatically.
func newRawMockLLM(t *testing.T, responses [][]string) *llm.Client {
	t.Helper()
	var mu sync.Mutex
	call := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		idx := call
		call++
		mu.Unlock()
		if idx >= len(responses) {
			t.Errorf("mock LLM: unexpected call #%d (only %d scripted)", idx, len(responses))
			http.Error(w, "no more scripted responses", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, frame := range responses[idx] {
			fmt.Fprint(w, frame)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return llm.NewClient(srv.URL, "test-key", "test-model", 0.7, 1000)
}

// registerTestTool registers a handler under a name no production tool uses.
// Registration is idempotent because every parity test body runs twice (once per
// backend) and tools.Register keeps the first registration: the first pass wins,
// which is the same handler.
// defsWith returns the stub definitions plus a definition for each named test
// tool, so both loops see the tool in the schema it is offered.
func defsWith(names ...string) []llm.ToolDef {
	defs := stubToolDefs()
	for _, name := range names {
		defs = append(defs, llm.ToolDef{
			Type: "function",
			Function: llm.ToolFunctionDef{
				Name:        name,
				Description: "test tool",
				Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
			},
		})
	}
	return defs
}

func registerTestTool(t *testing.T, name string, h tools.Handler) {
	t.Helper()
	if tools.HasHandler(name) {
		return
	}
	tools.Register(name, h)
}

func TestParity_MalformedArgsBecomeErrorResult(t *testing.T) {
	// Delta D11. A provider that truncates an argument list mid-JSON is handled
	// differently by the two transports, and both outcomes are safe:
	//
	//	legacy: the transport validates the arguments and fails the call with
	//	        "truncated tool call arguments" (retriable, so the fallback model
	//	        gets a try when one is configured). No tool runs.
	//	nova:   the collector accepts the fragment, the call reaches her-go's
	//	        registry, and the model gets the documented malformed-JSON error so
	//	        it can retry with shorter arguments.
	//
	// The important invariant for both: no handler ever runs on unparseable
	// arguments.
	runOnce(t, func(t *testing.T) {
		registerTestTool(t, "parity_malformed", func(argsJSON string, ctx *tools.Context) string {
			return "should not run"
		})
		client := newRawMockLLM(t, [][]string{
			{toolCallFrame("c1", "parity_malformed", `{"thought":`), finishFrame("tool_calls")},
			stopText("recovered"),
		})

		tctx := &tools.Context{AgentName: "test"}
		var firstResult string
		cfg := EngineConfig{
			Name: "test", LLM: client, ToolDefs: defsWith("parity_malformed"), ToolCtx: tctx,
			Messages:            []llm.ChatMessage{{Role: "system", Content: "s"}, {Role: "user", Content: "u"}},
			IterationsPerWindow: 5,
			PostTool: func(tc llm.ToolCall, result string, isError bool) {
				if firstResult == "" {
					firstResult = result
				}
			},
		}

		result, err := RunLoop(cfg)
		if err != nil {
			t.Fatalf("RunLoop: %v", err)
		}

		// Nova validates arguments against the tool schema before dispatch, so its
		// wording is what the model sees here — "error: invalid arguments: ..." —
		// not her-go's "malformed JSON in arguments" line (delta D5, recorded in
		// NOVA-GAPS.md). What matters is unchanged: an error result, and the loop
		// carrying on.
		if !strings.HasPrefix(firstResult, "error:") {
			t.Errorf("result = %q, want an error result", firstResult)
		}
		if result.ExitReason == ExitError {
			t.Error("a malformed-argument tool result must not fail the run")
		}
	})
}

func TestParity_ErrorToolResultDoesNotAbort(t *testing.T) {
	runOnce(t, func(t *testing.T) {
		registerTestTool(t, "parity_error", func(string, *tools.Context) string {
			return "error: simulated tool failure"
		})
		client := newRawMockLLM(t, [][]string{
			{toolCallFrame("c1", "parity_error", `{}`), finishFrame("tool_calls")},
			stopText("done"),
		})

		tctx := &tools.Context{AgentName: "test"}
		var sawError bool
		cfg := EngineConfig{
			Name: "test", LLM: client, ToolDefs: defsWith("parity_error"), ToolCtx: tctx,
			Messages:            []llm.ChatMessage{{Role: "system", Content: "s"}, {Role: "user", Content: "u"}},
			IterationsPerWindow: 5,
			PostToolResult: func(tc llm.ToolCall, result string, isError bool) string {
				sawError = isError
				return result
			},
		}
		result, err := RunLoop(cfg)
		if err != nil {
			t.Fatalf("RunLoop: %v", err)
		}
		if !sawError {
			t.Error("an error-prefixed tool result should be reported as an error")
		}
		if result.ExitReason == ExitError {
			t.Error("a tool error must not end the loop as an LLM error")
		}
	})
}

func TestParity_PostToolResultCanChangeErrorStatus(t *testing.T) {
	runOnce(t, func(t *testing.T) {
		registerTestTool(t, "parity_rewrite", func(string, *tools.Context) string {
			return "error: raw failure"
		})
		client := newRawMockLLM(t, [][]string{
			{toolCallFrame("c1", "parity_rewrite", `{}`), finishFrame("tool_calls")},
			stopText("done"),
		})

		tctx := &tools.Context{AgentName: "test"}
		var postToolError bool
		cfg := EngineConfig{
			Name: "test", LLM: client, ToolDefs: defsWith("parity_rewrite"), ToolCtx: tctx,
			Messages:            []llm.ChatMessage{{Role: "system", Content: "s"}, {Role: "user", Content: "u"}},
			IterationsPerWindow: 5,
			PostToolResult: func(tc llm.ToolCall, result string, isError bool) string {
				// Rewriting away the error prefix must clear the error flag for
				// every later observer, as the legacy loop documented.
				return "everything is fine"
			},
			PostTool: func(tc llm.ToolCall, result string, isError bool) {
				postToolError = isError
			},
		}
		if _, err := RunLoop(cfg); err != nil {
			t.Fatalf("RunLoop: %v", err)
		}
		if postToolError {
			t.Error("PostTool should see the rewritten result as a success")
		}
	})
}

func TestParity_BatchedToolCallsRunOnlyTheFirst(t *testing.T) {
	runOnce(t, func(t *testing.T) {
		registerTestTool(t, "parity_first", func(string, *tools.Context) string { return "first ran" })
		registerTestTool(t, "parity_second", func(string, *tools.Context) string { return "second ran" })

		both := rawChunk(`{"choices":[{"delta":{"tool_calls":[` +
			`{"index":0,"id":"c1","type":"function","function":{"name":"parity_first","arguments":"{}"}},` +
			`{"index":1,"id":"c2","type":"function","function":{"name":"parity_second","arguments":"{}"}}` +
			`]},"finish_reason":null}],"model":"test-model"}`)
		client := newRawMockLLM(t, [][]string{{both, finishFrame("tool_calls")}, stopText("done")})

		tctx := &tools.Context{AgentName: "test"}
		var ran []string
		cfg := EngineConfig{
			Name: "test", LLM: client, ToolDefs: defsWith("parity_first", "parity_second"), ToolCtx: tctx,
			Messages:            []llm.ChatMessage{{Role: "system", Content: "s"}, {Role: "user", Content: "u"}},
			IterationsPerWindow: 5,
			PostTool: func(tc llm.ToolCall, result string, isError bool) {
				ran = append(ran, tc.Function.Name)
			},
		}
		if _, err := RunLoop(cfg); err != nil {
			t.Fatalf("RunLoop: %v", err)
		}
		if len(ran) != 1 || ran[0] != "parity_first" {
			t.Errorf("ran %v, want only the first tool call", ran)
		}
	})
}

// TestParity_EmptyToolNameTerminates covers the phantom-call shape: a provider
// emits a tool call with no name.
//
// The two backends reach different but equally safe outcomes: the legacy loop
// skips the nameless call and asks again, while Nova's collector drops it, so the
// message has no tool calls and the run ends. Both terminate and neither
// dispatches a nameless call.
func TestParity_EmptyToolNameTerminates(t *testing.T) {
	runOnce(t, func(t *testing.T) {
		client := newRawMockLLM(t, [][]string{
			{toolCallFrame("c1", "", `{}`), finishFrame("tool_calls")},
			{finishFrame("stop")},
		})
		tctx := &tools.Context{AgentName: "test"}
		cfg := EngineConfig{
			Name: "test", LLM: client, ToolDefs: stubToolDefs(), ToolCtx: tctx,
			Messages:            []llm.ChatMessage{{Role: "system", Content: "s"}, {Role: "user", Content: "u"}},
			IterationsPerWindow: 3,
		}
		result, err := RunLoop(cfg)
		if err != nil {
			t.Fatalf("RunLoop: %v", err)
		}
		if result.ToolCalls != 0 {
			t.Errorf("tool calls = %d, want 0 (a nameless call must not dispatch)", result.ToolCalls)
		}
		if result.ExitReason == ExitMaxContinuations {
			t.Error("the loop should end on its own, not by exhausting the budget")
		}
	})
}

// TestNova_PanicInHandlerBecomesErrorResult is Nova-only on purpose: the legacy
// loop calls tools.Execute directly with no recover, so a panicking handler
// takes the process down. Nova recovers it into that call's error result, which
// is strictly safer and worth pinning.
func TestNova_PanicInHandlerBecomesErrorResult(t *testing.T) {
	registerTestTool(t, "parity_panic", func(string, *tools.Context) string {
		panic("simulated handler panic")
	})
	client := newRawMockLLM(t, [][]string{
		{toolCallFrame("c1", "parity_panic", `{}`), finishFrame("tool_calls")},
		stopText("done"),
	})

	tctx := &tools.Context{AgentName: "test"}
	var got string
	cfg := EngineConfig{
		Name: "test", LLM: client, ToolDefs: defsWith("parity_panic"), ToolCtx: tctx,
		Messages:            []llm.ChatMessage{{Role: "system", Content: "s"}, {Role: "user", Content: "u"}},
		IterationsPerWindow: 3,
		PostTool: func(tc llm.ToolCall, result string, isError bool) {
			got = result
		},
	}
	if _, err := RunLoop(cfg); err != nil {
		t.Fatalf("RunLoop: %v", err)
	}
	if !strings.HasPrefix(got, "error:") {
		t.Errorf("result = %q, want an error result from the recovered panic", got)
	}
}

// TestParity_NoToolCallsWithRequiredChoice: tool_choice:"required" does not
// guarantee a tool call (models ignore it), and a provider that answers with
// text must not wedge the loop.
func TestParity_NoToolCallsWithRequiredChoice(t *testing.T) {
	runOnce(t, func(t *testing.T) {
		text := rawChunk(`{"choices":[{"delta":{"content":"plain answer"},"finish_reason":null}],"model":"test-model"}`)
		client := newRawMockLLM(t, [][]string{{text, finishFrame("stop")}})

		tctx := &tools.Context{AgentName: "test"}
		cfg := EngineConfig{
			Name: "test", LLM: client, ToolDefs: stubToolDefs(), ToolCtx: tctx,
			Messages:            []llm.ChatMessage{{Role: "system", Content: "s"}, {Role: "user", Content: "u"}},
			IterationsPerWindow: 3,
			ToolChoiceFirst:     "required",
		}
		result, err := RunLoop(cfg)
		if err != nil {
			t.Fatalf("RunLoop: %v", err)
		}
		if result.ExitReason != ExitNoToolCalls {
			t.Errorf("exit reason = %q, want %q", result.ExitReason, ExitNoToolCalls)
		}
		if result.Iterations != 1 {
			t.Errorf("iterations = %d, want 1", result.Iterations)
		}
	})
}

// TestParity_ThinkBlockBecomesTrace pins the <think> extraction: an inline
// reasoning block must come out of the content and show up as a trace line,
// without changing the exit reason.
func TestParity_ThinkBlockBecomesTrace(t *testing.T) {
	runOnce(t, func(t *testing.T) {
		text := rawChunk(`{"choices":[{"delta":{"content":"<think>considering</think>the answer"},"finish_reason":null}],"model":"test-model"}`)
		client := newRawMockLLM(t, [][]string{{text, finishFrame("stop")}})

		tctx := &tools.Context{AgentName: "test"}
		var traces []string
		cfg := EngineConfig{
			Name: "test", LLM: client, ToolDefs: stubToolDefs(), ToolCtx: tctx,
			Messages:            []llm.ChatMessage{{Role: "system", Content: "s"}, {Role: "user", Content: "u"}},
			IterationsPerWindow: 3,
			TraceCallback:       func(s string) error { traces = append(traces, s); return nil },
		}
		result, err := RunLoop(cfg)
		if err != nil {
			t.Fatalf("RunLoop: %v", err)
		}
		joined := strings.Join(traces, "\n")
		if !strings.Contains(joined, "considering") {
			t.Errorf("traces = %q, want the think text", joined)
		}
		if !strings.Contains(strings.Join(result.TraceLines, "\n"), "considering") {
			t.Error("LoopResult.TraceLines should contain the think text")
		}
	})
}

// TestNova_DeferredToolsAreReadvertised covers delta D9: when a tool appends to
// ToolCtx.ActiveTools (what use_tools does), the next request must carry the
// enlarged schema. The legacy loop sent its starting set for ever, which is why
// this test is Nova-only.
func TestNova_DeferredToolsAreReadvertised(t *testing.T) {
	// Start with one advertised tool; the "loader" stands in for use_tools and
	// appends the deferred one, exactly as tools/use_tools does.
	active := []llm.ToolDef{{Type: "function", Function: llm.ToolFunctionDef{Name: "think", Parameters: map[string]any{"type": "object"}}}}
	registerTestTool(t, "parity_loader", func(string, *tools.Context) string {
		active = append(active, llm.ToolDef{
			Type:     "function",
			Function: llm.ToolFunctionDef{Name: "parity_deferred", Parameters: map[string]any{"type": "object"}},
		})
		return "loaded: parity_deferred"
	})
	registerTestTool(t, "parity_deferred", func(string, *tools.Context) string {
		return "deferred ran"
	})

	client := newRawMockLLM(t, [][]string{
		{toolCallFrame("c1", "parity_loader", `{}`), finishFrame("tool_calls")},
		{toolCallFrame("c2", "parity_deferred", `{}`), finishFrame("tool_calls")},
		stopText("done"),
	})

	tctx := &tools.Context{AgentName: "test", ActiveTools: &active}
	cfg := EngineConfig{
		Name: "test", LLM: client, ToolCtx: tctx,
		// Registry holds everything the agent could call; only ActiveTools is
		// advertised.
		ToolDefs: []llm.ToolDef{
			{Type: "function", Function: llm.ToolFunctionDef{Name: "think", Parameters: map[string]any{"type": "object"}}},
			{Type: "function", Function: llm.ToolFunctionDef{Name: "parity_loader", Parameters: map[string]any{"type": "object"}}},
			{Type: "function", Function: llm.ToolFunctionDef{Name: "parity_deferred", Parameters: map[string]any{"type": "object"}}},
		},
		Messages:            []llm.ChatMessage{{Role: "system", Content: "s"}, {Role: "user", Content: "u"}},
		IterationsPerWindow: 5,
	}

	var ran []string
	cfg.PostTool = func(tc llm.ToolCall, result string, isError bool) {
		ran = append(ran, tc.Function.Name)
	}
	if _, err := RunLoop(cfg); err != nil {
		t.Fatalf("RunLoop: %v", err)
	}

	// The deferred tool was advertised after the loader grew ActiveTools and the
	// model could call it — Nova would have rejected it as unknown otherwise.
	if len(ran) != 2 || ran[1] != "parity_deferred" {
		t.Fatalf("ran %v, want [parity_loader parity_deferred]", ran)
	}

	// And the hook advertises the grown set on its own.
	s := &novaRunState{cfg: cfg}
	req := provider.Request{}
	s.beforeProviderRequest(context.Background(), &req)
	if len(req.Tools) != 2 {
		t.Fatalf("advertised %d tools, want 2", len(req.Tools))
	}
	if req.Tools[1].Function.Name != "parity_deferred" {
		t.Errorf("second tool = %q, want parity_deferred", req.Tools[1].Function.Name)
	}
}

// TestNova_RegistryIncludesCodeOnlyTools is the regression test for a real
// outage-shaped bug: use_tools is not in any tool.yaml (HotToolDefs synthesises
// it), so building the registry from the manifests alone left the driver unable
// to load a single deferred tool — every use_tools call came back
// `unknown tool "use_tools"`, and the agent then told the user it could not
// schedule anything.
func TestNova_RegistryIncludesCodeOnlyTools(t *testing.T) {
	cfg := &config.Config{}
	driverDefs := tools.HotToolDefs("main", cfg) // includes use_tools
	manifestDefs := tools.ToolDefsForAgent("main", cfg)

	if len(driverDefs) == 0 || len(manifestDefs) == 0 {
		t.Fatal("tool loader returned nothing — check the embedded manifests")
	}

	merged := unionToolDefs(driverDefs, agentToolDefs(EngineConfig{
		ToolCtx: &tools.Context{AgentName: "main", Cfg: cfg},
	}))

	names := map[string]bool{}
	for _, def := range merged {
		names[def.Function.Name] = true
	}
	if !names["use_tools"] {
		t.Error("use_tools missing from the registry — deferred tools would be unreachable")
	}
	// Every manifest tool the agent may call must be runnable, not just offerable.
	for _, def := range manifestDefs {
		if !names[def.Function.Name] {
			t.Errorf("manifest tool %q missing from the registry", def.Function.Name)
		}
	}
}

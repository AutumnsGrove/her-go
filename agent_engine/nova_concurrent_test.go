package agent_engine

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"her/llm"
	"her/tools"
)

// TestNova_ConcurrentRunsShareOneClient guards the per-run metrics hook.
//
// In production the memory, mood and introspection agents each hold one
// long-lived *llm.Client and run in goroutines after the reply, so turn N's
// run can still be in flight when turn N+1's starts. If a run installs its
// metrics hook by mutating the shared client, two overlapping runs overwrite
// each other: calls get attributed to the wrong run or lost, and `go test
// -race` reports the write. Each run must see exactly its own provider calls.
//
// This is the Go version of a classic shared-mutable-default bug in Python:
// the state lives on an object many callers hold, not on the call itself.
func TestNova_ConcurrentRunsShareOneClient(t *testing.T) {
	const runs = 8

	responses := make([][]string, runs)
	for i := range responses {
		responses[i] = stopText("ok")
	}
	client := newRawMockLLM(t, responses)

	var (
		wg      sync.WaitGroup
		start   = make(chan struct{})
		results = make([]*LoopResult, runs)
		errs    = make([]error, runs)
	)
	for i := range runs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release every goroutine together to maximise overlap
			results[i], errs[i] = RunLoop(EngineConfig{
				Name:     "test",
				LLM:      client,
				ToolDefs: defsWith(),
				ToolCtx:  &tools.Context{AgentName: "test"},
				Messages: []llm.ChatMessage{
					{Role: "system", Content: "s"},
					{Role: "user", Content: "u"},
				},
				IterationsPerWindow: 3,
			})
		}(i)
	}
	close(start)
	wg.Wait()

	for i := range runs {
		if errs[i] != nil {
			t.Fatalf("run %d: %v", i, errs[i])
		}
		if results[i].Iterations != 1 {
			t.Errorf("run %d recorded %d provider calls, want exactly 1", i, results[i].Iterations)
		}
		if results[i].TotalCost <= 0 {
			t.Errorf("run %d recorded cost %v, want its own call's cost", i, results[i].TotalCost)
		}
	}
}

// A provider failure that survives the fallback used to vanish: the loop
// returned a nil error with ExitError, the cause was discarded, and OnError
// (documented on EngineConfig) was never called. The cause must reach OnError.
func TestNova_ProviderFailureReachesOnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized) // not retriable: no fallback can help
		_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
	}))
	defer srv.Close()

	var gotErr error
	result, err := RunLoop(EngineConfig{
		Name:     "test",
		LLM:      llm.NewClient(srv.URL, "key", "test-model", 0.7, 100),
		ToolDefs: defsWith(),
		ToolCtx:  &tools.Context{AgentName: "test"},
		Messages: []llm.ChatMessage{
			{Role: "system", Content: "s"},
			{Role: "user", Content: "u"},
		},
		OnError: func(err error, iteration, window int) bool {
			gotErr = err
			return false
		},
	})
	if err != nil {
		t.Fatalf("RunLoop returned %v; the failure is reported through ExitReason and OnError", err)
	}
	if result.ExitReason != ExitError {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, ExitError)
	}
	if gotErr == nil {
		t.Fatal("OnError was not called with the provider error")
	}
	if !strings.Contains(gotErr.Error(), "401") {
		t.Errorf("OnError got %q, want the provider's status in it", gotErr)
	}
}

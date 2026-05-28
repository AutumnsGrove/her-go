package gateway

// Tests for the gradio HTTP adapter. We use net/http/httptest to spin up
// a real HTTP server backed by the adapter's mux — same handler code path
// as production, no mocking of the HTTP layer itself.
//
// The key challenge: handleChat blocks waiting for a pipeline reply on a
// channel keyed by conversation ID. Tests that exercise the full chat path
// inject a reply in a goroutine after the request is in-flight.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"her/config"
	"her/tui"
)

// newTestGradioAdapter creates a gradioAdapter wired for testing.
// We skip Start() (which would bind a real port) and instead mount the
// handlers directly on an httptest.Server.
func newTestGradioAdapter(t *testing.T) *gradioAdapter {
	t.Helper()
	cfg := config.AdapterConfig{
		Name: "test-gradio",
		Type: "gradio",
		Port: 0,
	}
	a, err := newGradioAdapter(cfg, nil)
	if err != nil {
		t.Fatalf("newGradioAdapter: %v", err)
	}
	return a.(*gradioAdapter)
}

// buildTestServer mounts the adapter's handlers on an httptest.Server so
// tests can make real HTTP requests without binding a port.
func buildTestServer(a *gradioAdapter) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/chat", a.handleChat)
	mux.HandleFunc("POST /api/clear", a.handleClear)
	mux.HandleFunc("GET /api/status", a.handleStatus)
	mux.HandleFunc("GET /api/traces", a.handleTraceSSE)
	return httptest.NewServer(corsMiddleware(mux))
}

// injectReply mimics the pipeline: it polls the adapter's pending map for
// the given conversation ID and writes a reply as soon as the channel appears.
// Call this in a goroutine before the blocking POST /api/chat.
func injectReply(a *gradioAdapter, convID, text string) {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		a.pendingMu.Lock()
		ch, ok := a.pending[convID]
		a.pendingMu.Unlock()
		if ok {
			ch <- OutboundMsg{Text: text}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ---- /api/chat tests --------------------------------------------------------

func TestGradioHandleChat_EmptyMessage(t *testing.T) {
	a := newTestGradioAdapter(t)
	srv := buildTestServer(a)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/chat", "application/json", strings.NewReader(`{"message":""}`))
	if err != nil {
		t.Fatalf("POST /api/chat: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for empty message, got %d", resp.StatusCode)
	}
}

func TestGradioHandleChat_InvalidJSON(t *testing.T) {
	a := newTestGradioAdapter(t)
	srv := buildTestServer(a)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/chat", "application/json", strings.NewReader(`not json at all`))
	if err != nil {
		t.Fatalf("POST /api/chat: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid JSON, got %d", resp.StatusCode)
	}
}

func TestGradioHandleChat_WithPipelineReply(t *testing.T) {
	a := newTestGradioAdapter(t)
	srv := buildTestServer(a)
	defer srv.Close()

	// Pre-seed a known convID so we can target the pending channel precisely.
	const convID = "test-conv-pipeline"
	a.setConvID(convID)

	// Inject the pipeline reply in a background goroutine. It polls until
	// the pending channel is registered, then writes to it — this is the
	// same flow as the real gateway's runAdapter loop calling Send().
	go injectReply(a, convID, "hello back")

	body, _ := json.Marshal(chatRequest{Message: "hello", ConversationID: convID})
	resp, err := http.Post(srv.URL+"/api/chat", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /api/chat: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}

	var got chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Reply != "hello back" {
		t.Errorf("reply: got %q, want %q", got.Reply, "hello back")
	}
	if got.ConversationID != convID {
		t.Errorf("conversation_id: got %q, want %q", got.ConversationID, convID)
	}
}

func TestGradioHandleChat_HelpCommandRouting(t *testing.T) {
	a := newTestGradioAdapter(t)
	srv := buildTestServer(a)
	defer srv.Close()

	// Register a /help command that returns immediately — no pipeline needed.
	// The command handler satisfies the CommandHandler type alias defined in
	// adapter.go: func(context.Context, string) (string, error).
	called := false
	a.RegisterCommands([]CommandDef{
		{
			Name:        "help",
			Description: "Show available commands",
			Handler: func(_ context.Context, _ string) (string, error) {
				called = true
				return "here is help text", nil
			},
		},
	})

	body, _ := json.Marshal(chatRequest{Message: "/help"})
	resp, err := http.Post(srv.URL+"/api/chat", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /api/chat /help: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 for /help command, got %d", resp.StatusCode)
	}
	if !called {
		t.Error("expected /help handler to be invoked")
	}

	var got chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Reply != "here is help text" {
		t.Errorf("reply: got %q, want %q", got.Reply, "here is help text")
	}
}

func TestGradioHandleChat_UnknownCommandFallsThrough(t *testing.T) {
	a := newTestGradioAdapter(t)
	srv := buildTestServer(a)
	defer srv.Close()

	// No commands registered. /nonexistent has no handler, so tryCommand
	// returns ("", false) and the message falls through to the pipeline path.
	// We verify this by injecting a reply — if the message were swallowed
	// by the command layer the handler would block and never read it.
	const convID = "test-conv-fallthrough"
	a.setConvID(convID)

	go injectReply(a, convID, "pipeline handled it")

	body, _ := json.Marshal(chatRequest{Message: "/nonexistent", ConversationID: convID})
	resp, err := http.Post(srv.URL+"/api/chat", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /api/chat: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 (pipeline reply), got %d", resp.StatusCode)
	}

	var got chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Reply != "pipeline handled it" {
		t.Errorf("reply: got %q, want %q", got.Reply, "pipeline handled it")
	}
}

// ---- /api/clear tests -------------------------------------------------------

func TestGradioHandleClear_ResetsConversationID(t *testing.T) {
	a := newTestGradioAdapter(t)
	srv := buildTestServer(a)
	defer srv.Close()

	a.setConvID("original-id")

	resp, err := http.Post(srv.URL+"/api/clear", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /api/clear: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}

	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	newID := body["conversation_id"]
	if newID == "" {
		t.Error("expected non-empty conversation_id in clear response")
	}
	if newID == "original-id" {
		t.Error("expected conversation_id to change after /clear, but it stayed the same")
	}
	if body["status"] != "cleared" {
		t.Errorf("status: got %q, want %q", body["status"], "cleared")
	}

	// The adapter's internal state must match the returned ID.
	if a.getConvID() != newID {
		t.Errorf("adapter convID %q != response convID %q", a.getConvID(), newID)
	}
}

// ---- /api/status tests ------------------------------------------------------

func TestGradioHandleStatus_ReturnsOK(t *testing.T) {
	a := newTestGradioAdapter(t)
	srv := buildTestServer(a)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/status")
	if err != nil {
		t.Fatalf("GET /api/status: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("expected JSON Content-Type, got %q", ct)
	}

	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("status: got %q, want %q", body["status"], "ok")
	}
	if body["adapter"] != "test-gradio" {
		t.Errorf("adapter: got %q, want %q", body["adapter"], "test-gradio")
	}
}

// ---- CORS middleware tests --------------------------------------------------

func TestCORSMiddleware_AllowsLocalhostOrigins(t *testing.T) {
	// corsMiddleware only echoes the Origin header back for localhost/127.0.0.1.
	// External origins get no Access-Control-Allow-Origin header.
	handler := corsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv := httptest.NewServer(handler)
	defer srv.Close()

	tests := []struct {
		name        string
		origin      string
		wantAllowed bool
	}{
		{"localhost port", "http://localhost:7860", true},
		{"localhost 3000", "http://localhost:3000", true},
		{"127.0.0.1 port", "http://127.0.0.1:3000", true},
		{"external domain", "https://example.com", false},
		{"external https", "https://evil.com", false},
		{"no origin", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer resp.Body.Close()

			got := resp.Header.Get("Access-Control-Allow-Origin")
			if tt.wantAllowed {
				if got != tt.origin {
					t.Errorf("Access-Control-Allow-Origin: got %q, want %q", got, tt.origin)
				}
			} else {
				// External origins must not be echoed back.
				if got == tt.origin && tt.origin != "" {
					t.Errorf("external origin %q should not be allowed, but header was set", tt.origin)
				}
			}
		})
	}
}

func TestCORSMiddleware_PreflightReturns204(t *testing.T) {
	// Use a channel instead of a bare bool to avoid a data race: the handler
	// runs in the httptest server's goroutine while the test reads in its own.
	// In Go, concurrent reads and writes to a variable without synchronisation
	// are undefined behaviour — the race detector will flag it.
	innerCalled := make(chan struct{}, 1)
	handler := corsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		innerCalled <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	srv := httptest.NewServer(handler)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodOptions, srv.URL, nil)
	req.Header.Set("Origin", "http://localhost:7860")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("OPTIONS request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("expected 204 for preflight, got %d", resp.StatusCode)
	}
	// Non-blocking check: if something was sent the inner handler ran, which
	// means corsMiddleware incorrectly forwarded the OPTIONS request.
	select {
	case <-innerCalled:
		t.Error("inner handler should not be called for OPTIONS preflight")
	default:
		// correct — nothing was sent to the channel
	}
}

// ---- MaxBytesReader / oversized body test -----------------------------------

func TestGradioHandleChat_RejectsOversizedBody(t *testing.T) {
	a := newTestGradioAdapter(t)
	srv := buildTestServer(a)
	defer srv.Close()

	// 11 MB payload exceeds the 10 MB MaxBytesReader limit in handleChat.
	// json.Decoder will fail when the reader is truncated, returning 400.
	bigMsg := strings.Repeat("x", 11<<20)
	body, _ := json.Marshal(chatRequest{Message: bigMsg})

	resp, err := http.Post(srv.URL+"/api/chat", "application/json", bytes.NewReader(body))
	if err != nil {
		// Some transports surface the truncation as a connection reset.
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		t.Error("expected non-200 for oversized body, got 200")
	}
}

// ---- validImageMIME tests ---------------------------------------------------

func TestValidImageMIME(t *testing.T) {
	tests := []struct {
		mime string
		want bool
	}{
		{"image/png", true},
		{"image/jpeg", true},
		{"image/gif", true},
		{"image/webp", true},
		{"image/bmp", false},
		{"image/tiff", false},
		{"image/svg+xml", false},
		{"application/octet-stream", false},
		{"text/plain", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.mime, func(t *testing.T) {
			if got := validImageMIME(tt.mime); got != tt.want {
				t.Errorf("validImageMIME(%q) = %v, want %v", tt.mime, got, tt.want)
			}
		})
	}
}

// ---- SSE bus event streaming tests ------------------------------------------

func TestGradioSSE_NoBusReturns503(t *testing.T) {
	a := newTestGradioAdapter(t) // bus is nil
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/traces", a.handleTraceSSE)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/traces")
	if err != nil {
		t.Fatalf("SSE request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when bus is nil, got %d", resp.StatusCode)
	}
}

func TestGradioSSE_BusEventsStreamToClient(t *testing.T) {
	bus := tui.NewBus()
	defer bus.Close()

	cfg := config.AdapterConfig{
		Name:   "test-gradio-sse",
		Type:   "gradio",
		Traces: true,
	}
	a, err := newGradioAdapter(cfg, bus)
	if err != nil {
		t.Fatalf("newGradioAdapter: %v", err)
	}
	ga := a.(*gradioAdapter)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/traces", ga.handleTraceSSE)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Connect an SSE client.
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(srv.URL + "/api/traces")
	if err != nil {
		t.Fatalf("SSE connect: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// Give the SSE handler time to subscribe to the bus.
	time.Sleep(50 * time.Millisecond)

	// Emit a tool call event on the bus.
	bus.Emit(tui.ToolCallEvent{
		Time:     time.Now(),
		TurnID:   42,
		Source:   "main",
		ToolName: "think",
		Args:     `{"thought":"test"}`,
		Result:   "ok",
	})

	// Read the SSE output — should contain the event.
	buf := make([]byte, 4096)
	n, err := resp.Body.Read(buf)
	if err != nil {
		t.Fatalf("reading SSE: %v", err)
	}
	body := string(buf[:n])

	if !strings.Contains(body, "event: tool_call") {
		t.Errorf("expected 'event: tool_call' in SSE output, got: %s", body)
	}
	if !strings.Contains(body, `"tool_name":"think"`) {
		t.Errorf("expected tool_name in SSE data, got: %s", body)
	}
}

// ---- tryCommand unit tests --------------------------------------------------

func TestGradioTryCommand_ClearResetsConvID(t *testing.T) {
	a := newTestGradioAdapter(t)
	a.setConvID("old-conv-id")

	result, handled := a.tryCommand(context.Background(), "/clear", "old-conv-id")
	if !handled {
		t.Error("expected /clear to be handled")
	}
	if result == "" {
		t.Error("expected non-empty result text for /clear")
	}
	if a.getConvID() == "old-conv-id" {
		t.Error("expected convID to be reset after /clear, but it was unchanged")
	}
}

func TestGradioTryCommand_UnknownCommandNotHandled(t *testing.T) {
	a := newTestGradioAdapter(t)

	_, handled := a.tryCommand(context.Background(), "/nosuchcmd", "conv-1")
	if handled {
		t.Error("expected unknown command to return handled=false")
	}
}

func TestGradioTryCommand_RegisteredCommandDispatched(t *testing.T) {
	a := newTestGradioAdapter(t)
	called := false
	a.RegisterCommands([]CommandDef{
		{
			Name: "ping",
			Handler: func(_ context.Context, _ string) (string, error) {
				called = true
				return "pong", nil
			},
		},
	})

	result, handled := a.tryCommand(context.Background(), "/ping", "conv-1")
	if !handled {
		t.Error("expected /ping to be handled")
	}
	if !called {
		t.Error("expected /ping handler to be called")
	}
	if result != "pong" {
		t.Errorf("result: got %q, want %q", result, "pong")
	}
}

func TestGradioTryCommand_CompactWithoutHandler(t *testing.T) {
	a := newTestGradioAdapter(t)
	// compactHandler is nil by default — should still be "handled" with a
	// fallback message rather than returning false.
	result, handled := a.tryCommand(context.Background(), "/compact", "conv-1")
	if !handled {
		t.Error("expected /compact to be handled even without a compactHandler wired")
	}
	if result == "" {
		t.Error("expected non-empty result for /compact with no handler")
	}
}

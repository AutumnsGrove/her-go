package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"her/config"
)

// gradioAdapter implements the Adapter interface for a local Gradio
// (or any HTTP) chat frontend. It serves two endpoints:
//
//   - POST /api/chat   — receive user message, reply is returned by the
//     gateway after pipeline processing (not by this handler directly)
//   - GET  /api/traces — SSE stream of agent trace events
//   - GET  /api/status — health check
//   - POST /api/clear  — reset conversation
//
// The Gradio Python script (scripts/dev_chat.py) connects to these
// endpoints and renders a browser-based chat UI with an optional
// trace panel.
type gradioAdapter struct {
	cfg    config.AdapterConfig
	port   int
	traces bool

	msgCh    chan InboundMsg
	server   *http.Server
	commands []CommandDef

	// SSE trace subscribers — each connected /api/traces client gets a channel.
	tracesMu sync.Mutex
	traceSubs []chan TraceEvent

	// Pending replies — HTTP handler blocks until pipeline produces a result.
	pendingMu sync.Mutex
	pending   map[string]chan OutboundMsg // keyed by request ID

	// Conversation state for this adapter instance.
	convID   string
	convIDMu sync.Mutex
}

func newGradioAdapter(acfg config.AdapterConfig) (Adapter, error) {
	port := acfg.Port
	if port == 0 {
		port = 7860
	}

	return &gradioAdapter{
		cfg:     acfg,
		port:    port,
		traces:  acfg.Traces,
		msgCh:   make(chan InboundMsg, 16),
		pending: make(map[string]chan OutboundMsg),
	}, nil
}

func (a *gradioAdapter) Name() string { return a.cfg.Name }

func (a *gradioAdapter) Capabilities() CapSet {
	return CapSet{
		Stream: false, // Phase 1: no streaming, just request/response
		Typing: false,
	}
}

func (a *gradioAdapter) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/chat", a.handleChat)
	mux.HandleFunc("POST /api/clear", a.handleClear)
	mux.HandleFunc("GET /api/status", a.handleStatus)
	if a.traces {
		mux.HandleFunc("GET /api/traces", a.handleTraceSSE)
	}

	a.server = &http.Server{
		Addr:    fmt.Sprintf(":%d", a.port),
		Handler: corsMiddleware(mux),
	}

	log.Infof("gradio adapter: listening on http://localhost:%d", a.port)

	errCh := make(chan error, 1)
	go func() {
		if err := a.server.ListenAndServe(); err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		return nil
	case err := <-errCh:
		return err
	}
}

func (a *gradioAdapter) Stop() error {
	if a.server == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return a.server.Shutdown(ctx)
}

func (a *gradioAdapter) Receive() <-chan InboundMsg {
	return a.msgCh
}

// Send delivers the pipeline's reply to the blocked HTTP handler.
// The chat handler is waiting on a channel keyed by conversation ID.
func (a *gradioAdapter) Send(msg OutboundMsg) error {
	a.pendingMu.Lock()
	ch, ok := a.pending[a.getConvID()]
	a.pendingMu.Unlock()

	if ok {
		ch <- msg
	}
	return nil
}

func (a *gradioAdapter) SendStatus(text string) error {
	return nil
}

func (a *gradioAdapter) StartTyping() func() {
	return func() {}
}

func (a *gradioAdapter) OnTraceEvent(evt TraceEvent) {
	a.tracesMu.Lock()
	defer a.tracesMu.Unlock()
	for _, ch := range a.traceSubs {
		select {
		case ch <- evt:
		default:
			// subscriber too slow, drop event
		}
	}
}

func (a *gradioAdapter) RegisterCommands(cmds []CommandDef) {
	a.commands = cmds
}

// --- HTTP Handlers ---

type chatRequest struct {
	Message        string `json:"message"`
	ConversationID string `json:"conversation_id,omitempty"`
}

type chatResponse struct {
	Reply          string `json:"reply"`
	ConversationID string `json:"conversation_id"`
}

func (a *gradioAdapter) handleChat(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.Message == "" {
		http.Error(w, "message is required", http.StatusBadRequest)
		return
	}

	convID := req.ConversationID
	if convID == "" {
		convID = a.getOrCreateConvID()
	} else {
		a.setConvID(convID)
	}

	// Create a channel for the pipeline reply.
	replyCh := make(chan OutboundMsg, 1)
	a.pendingMu.Lock()
	a.pending[convID] = replyCh
	a.pendingMu.Unlock()

	defer func() {
		a.pendingMu.Lock()
		delete(a.pending, convID)
		a.pendingMu.Unlock()
	}()

	// Send the message to the gateway for pipeline processing.
	a.msgCh <- InboundMsg{
		Text:           req.Message,
		ConversationID: convID,
		AdapterName:    a.Name(),
		Timestamp:      time.Now(),
	}

	// Block until the pipeline responds (or timeout).
	select {
	case result := <-replyCh:
		resp := chatResponse{
			Reply:          result.Text,
			ConversationID: convID,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)

	case <-time.After(2 * time.Minute):
		http.Error(w, "request timed out", http.StatusGatewayTimeout)
	}
}

func (a *gradioAdapter) handleClear(w http.ResponseWriter, r *http.Request) {
	newID := fmt.Sprintf("gradio-%d", time.Now().UnixMilli())
	a.setConvID(newID)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"conversation_id": newID,
		"status":          "cleared",
	})
}

func (a *gradioAdapter) handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status":  "ok",
		"adapter": a.Name(),
		"mode":    "gateway",
	})
}

// handleTraceSSE streams trace events to connected clients via
// Server-Sent Events. Each client gets its own channel and receives
// events as they happen.
func (a *gradioAdapter) handleTraceSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := make(chan TraceEvent, 64)
	a.tracesMu.Lock()
	a.traceSubs = append(a.traceSubs, ch)
	a.tracesMu.Unlock()

	defer func() {
		a.tracesMu.Lock()
		for i, sub := range a.traceSubs {
			if sub == ch {
				a.traceSubs = append(a.traceSubs[:i], a.traceSubs[i+1:]...)
				break
			}
		}
		a.tracesMu.Unlock()
	}()

	for {
		select {
		case <-r.Context().Done():
			return
		case evt := <-ch:
			data, _ := json.Marshal(evt)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

// --- Conversation ID helpers ---

func (a *gradioAdapter) getConvID() string {
	a.convIDMu.Lock()
	defer a.convIDMu.Unlock()
	return a.convID
}

func (a *gradioAdapter) setConvID(id string) {
	a.convIDMu.Lock()
	a.convID = id
	a.convIDMu.Unlock()
}

func (a *gradioAdapter) getOrCreateConvID() string {
	a.convIDMu.Lock()
	defer a.convIDMu.Unlock()
	if a.convID == "" {
		a.convID = fmt.Sprintf("gradio-%d", time.Now().UnixMilli())
	}
	return a.convID
}

// corsMiddleware wraps a handler with permissive CORS headers for
// local development. Gradio runs on a different port than the Go
// server, so cross-origin requests need to be allowed.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "http://localhost:7860")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

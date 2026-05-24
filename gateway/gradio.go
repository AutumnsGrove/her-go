package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
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

	// compactHandler is set by the gateway after pipeline creation.
	// It needs the conversation ID, which only the adapter knows.
	compactHandler func(ctx context.Context, convID string) (string, error)

	// logCommand logs slash command usage for analytics parity with
	// Telegram. Set by the gateway after pipeline creation.
	logCommand func(command string, conversationID, args string)

	// SSE trace subscribers — each connected /api/traces client gets a channel.
	tracesMu    sync.Mutex
	traceSubs   []chan TraceEvent
	maxSSEConns int

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
		port = 7777 // Go API port — Gradio Python UI runs separately on :7860
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
	ImageBase64    string `json:"image_base64,omitempty"`
	ImageMIME      string `json:"image_mime,omitempty"`
}

type chatResponse struct {
	Reply          string `json:"reply"`
	ConversationID string `json:"conversation_id"`
}

func (a *gradioAdapter) handleChat(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 10<<20) // 10 MB limit
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

	// Intercept slash commands before they hit the pipeline.
	if strings.HasPrefix(req.Message, "/") {
		if result, handled := a.tryCommand(r.Context(), req.Message, convID); handled {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(chatResponse{
				Reply:          result,
				ConversationID: convID,
			})
			return
		}
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

	// Validate image MIME if an image was included.
	imgBase64 := req.ImageBase64
	imgMIME := req.ImageMIME
	if imgBase64 != "" && !validImageMIME(imgMIME) {
		imgBase64 = ""
		imgMIME = ""
		log.Infof("gradio: dropped image with unsupported MIME: %s", req.ImageMIME)
	}

	// Send the message to the gateway for pipeline processing.
	a.msgCh <- InboundMsg{
		Text:           req.Message,
		ConversationID: convID,
		AdapterName:    a.Name(),
		Timestamp:      time.Now(),
		ImageBase64:    imgBase64,
		ImageMIME:      imgMIME,
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

// tryCommand checks if a message is a registered command and executes it.
// Returns the result text and true if handled, or ("", false) if the
// message should fall through to the pipeline.
func (a *gradioAdapter) tryCommand(ctx context.Context, message, convID string) (string, bool) {
	parts := strings.SplitN(message, " ", 2)
	cmdName := strings.TrimPrefix(parts[0], "/")
	args := ""
	if len(parts) > 1 {
		args = parts[1]
	}

	// /clear is adapter-specific — it resets the conversation ID.
	if cmdName == "clear" {
		a.logCmd("/clear", convID, args)
		newID := fmt.Sprintf("gradio-%d", time.Now().UnixMilli())
		a.setConvID(newID)
		return "Context cleared. Fresh start!", true
	}

	// /compact needs the current conversation ID from the adapter,
	// so it's handled here rather than as a generic CommandDef.
	if cmdName == "compact" {
		a.logCmd("/compact", convID, args)
		if a.compactHandler != nil {
			result, err := a.compactHandler(ctx, convID)
			if err != nil {
				return fmt.Sprintf("Error: %v", err), true
			}
			return result, true
		}
		return "Compact not available.", true
	}

	for _, cmd := range a.commands {
		if cmd.Name == cmdName {
			a.logCmd("/"+cmdName, convID, args)
			result, err := cmd.Handler(ctx, args)
			if err != nil {
				return fmt.Sprintf("Error: %v", err), true
			}
			return result, true
		}
	}

	return "", false
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

	// Limit concurrent SSE connections to prevent file descriptor exhaustion.
	maxConns := a.maxSSEConns
	if maxConns == 0 {
		maxConns = 10
	}
	a.tracesMu.Lock()
	if len(a.traceSubs) >= maxConns {
		a.tracesMu.Unlock()
		http.Error(w, "too many trace connections", http.StatusServiceUnavailable)
		return
	}
	a.tracesMu.Unlock()

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

// logCmd logs a command to the store for analytics, if a logger is wired.
func (a *gradioAdapter) logCmd(command, convID, args string) {
	if a.logCommand != nil {
		a.logCommand(command, convID, args)
	}
}

// validImageMIME checks whether a MIME type is a supported image format.
func validImageMIME(mime string) bool {
	switch mime {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	}
	return false
}

// corsMiddleware wraps a handler with permissive CORS headers for
// local development. Gradio runs on a different port than the Go
// server, so cross-origin requests need to be allowed.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if strings.HasPrefix(origin, "http://localhost:") || strings.HasPrefix(origin, "http://127.0.0.1:") {
			w.Header().Set("Access-Control-Allow-Origin", origin)
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

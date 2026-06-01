// Package gateway — websocket.go implements a WebSocket adapter for
// real-time chat with token-by-token streaming.
//
// The WebSocket adapter serves an HTTP endpoint that upgrades to WebSocket.
// Each connection gets its own conversation. Messages are JSON-framed:
//
//	Inbound:  {"type":"message", "text":"...", "request_id":"uuid"}
//	Outbound: {"type":"stream_token", "token":"Hi"}
//	          {"type":"stream_end"}
//	          {"type":"reply", "text":"full reply text"}
//	          {"type":"status", "text":"recalling memories..."}
//	          {"type":"typing", "active":true}
//	          {"type":"trace", "event_type":"...", "data":{...}}
//
// This is the adapter that powers the local SvelteKit chat app and,
// eventually, the hosted web product.
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
	"her/tui"

	"github.com/gorilla/websocket"
)

// wsConn represents a single WebSocket client connection.
type wsConn struct {
	conn   *websocket.Conn
	convID string
	sendMu sync.Mutex // serialize writes to the WebSocket
	id     string     // unique connection identifier
}

// writeJSON serializes msg as JSON and writes it to the WebSocket.
// The mutex ensures concurrent writes (streaming tokens + status updates)
// don't interleave — WebSocket is a framed protocol, but the gorilla
// library isn't thread-safe for concurrent writes.
func (c *wsConn) writeJSON(msg any) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	return c.conn.WriteJSON(msg)
}

// wsAdapter implements the Adapter interface for WebSocket connections.
type wsAdapter struct {
	cfg    config.AdapterConfig
	port   int
	msgCh  chan InboundMsg
	server *http.Server
	bus    *tui.Bus

	commands []CommandDef

	// Active connection tracking. The gateway processes messages
	// sequentially from msgCh, so only one connection is being
	// served at a time. This tracks which connection should receive
	// status updates and streaming tokens during processing.
	activeConn   *wsConn
	activeConnMu sync.Mutex

	// All connected clients, keyed by connection ID.
	connsMu sync.RWMutex
	conns   map[string]*wsConn
}

// WebSocket protocol message types (JSON-framed).
type wsInboundMsg struct {
	Type      string `json:"type"`       // "message"
	Text      string `json:"text"`
	RequestID string `json:"request_id"` // client-generated UUID for correlation
}

type wsOutboundMsg struct {
	Type      string `json:"type"`                 // "reply", "stream_token", "stream_end", "status", "typing", "trace", "error"
	Text      string `json:"text,omitempty"`        // for reply, status, error
	Token     string `json:"token,omitempty"`       // for stream_token
	Active    bool   `json:"active,omitempty"`      // for typing
	RequestID string `json:"request_id,omitempty"`  // echoed from inbound
	EventType string `json:"event_type,omitempty"`  // for trace events
	Data      any    `json:"data,omitempty"`         // for trace event payloads
}

func newWSAdapter(acfg config.AdapterConfig, bus *tui.Bus) (Adapter, error) {
	port := acfg.Port
	if port == 0 {
		port = 7778
	}

	return &wsAdapter{
		cfg:   acfg,
		port:  port,
		msgCh: make(chan InboundMsg, 16),
		bus:   bus,
		conns: make(map[string]*wsConn),
	}, nil
}

func (a *wsAdapter) Name() string { return a.cfg.Name }

func (a *wsAdapter) Capabilities() CapSet {
	return CapSet{
		Stream: true, // token-by-token streaming over WebSocket
		Typing: true,
		Edit:   false, // WebSocket appends messages, doesn't edit
	}
}

func (a *wsAdapter) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", a.handleUpgrade)
	mux.HandleFunc("GET /api/status", a.handleStatus)

	a.server = &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", a.port),
		Handler: corsMiddleware(mux),
	}

	log.Infof("websocket adapter: listening on ws://localhost:%d/ws", a.port)

	errCh := make(chan error, 1)
	go func() {
		if err := a.server.ListenAndServe(); err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()

	// Start trace broadcasting if the event bus is available.
	if a.bus != nil {
		go a.broadcastTraces(ctx)
	}

	select {
	case <-ctx.Done():
		return nil
	case err := <-errCh:
		return err
	}
}

func (a *wsAdapter) Stop() error {
	if a.server == nil {
		return nil
	}

	// Close all WebSocket connections.
	a.connsMu.RLock()
	for _, conn := range a.conns {
		_ = conn.conn.Close()
	}
	a.connsMu.RUnlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return a.server.Shutdown(ctx)
}

func (a *wsAdapter) Receive() <-chan InboundMsg {
	return a.msgCh
}

// Send delivers the final reply to the active connection.
// Called by the gateway after pipeline.Process() completes.
func (a *wsAdapter) Send(msg OutboundMsg) error {
	a.activeConnMu.Lock()
	conn := a.activeConn
	a.activeConn = nil
	a.activeConnMu.Unlock()

	if conn == nil {
		return fmt.Errorf("no active connection for reply delivery")
	}

	msgType := "reply"
	if msg.IsError {
		msgType = "error"
	}
	return conn.writeJSON(wsOutboundMsg{
		Type: msgType,
		Text: msg.Text,
	})
}

// SendStatus sends a status update to the active connection.
// This is also how the final reply gets delivered for gateway
// frontends (via gatewayFrontend.EditStatus).
func (a *wsAdapter) SendStatus(text string) error {
	a.activeConnMu.Lock()
	conn := a.activeConn
	a.activeConnMu.Unlock()

	if conn == nil {
		return nil
	}
	return conn.writeJSON(wsOutboundMsg{
		Type: "status",
		Text: text,
	})
}

func (a *wsAdapter) StartTyping() func() {
	a.activeConnMu.Lock()
	conn := a.activeConn
	a.activeConnMu.Unlock()

	if conn == nil {
		return func() {}
	}

	_ = conn.writeJSON(wsOutboundMsg{Type: "typing", Active: true})
	return func() {
		_ = conn.writeJSON(wsOutboundMsg{Type: "typing", Active: false})
	}
}

func (a *wsAdapter) RegisterCommands(cmds []CommandDef) {
	a.commands = cmds
}

// StreamToken sends a single streaming token to the active connection.
// Implements the Streamer interface so gatewayFrontend can wire up
// token-by-token delivery.
func (a *wsAdapter) StreamToken(token string) error {
	a.activeConnMu.Lock()
	conn := a.activeConn
	a.activeConnMu.Unlock()

	if conn == nil {
		return nil
	}
	return conn.writeJSON(wsOutboundMsg{
		Type:  "stream_token",
		Token: token,
	})
}

// StreamEnd signals that streaming is complete for the current reply.
func (a *wsAdapter) StreamEnd() {
	a.activeConnMu.Lock()
	conn := a.activeConn
	a.activeConnMu.Unlock()

	if conn == nil {
		return
	}
	_ = conn.writeJSON(wsOutboundMsg{Type: "stream_end"})
}

// --- WebSocket connection handling ---

// upgrader configures the WebSocket handshake. CheckOrigin allows any
// origin for local development — when auth is added later, this will
// validate JWT tokens during the upgrade.
var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // localhost only for now, no auth
	},
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

// handleUpgrade upgrades an HTTP request to a WebSocket connection
// and starts the read loop for this client.
func (a *wsAdapter) handleUpgrade(w http.ResponseWriter, r *http.Request) {
	// Cap concurrent connections to prevent resource exhaustion.
	a.connsMu.RLock()
	connCount := len(a.conns)
	a.connsMu.RUnlock()
	if connCount >= 10 {
		http.Error(w, "too many connections", http.StatusServiceUnavailable)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Error("websocket: upgrade failed", "err", err)
		return
	}

	// Set a read limit to prevent a single message from exhausting memory.
	conn.SetReadLimit(64 * 1024) // 64 KB

	connID := fmt.Sprintf("ws-%d-%d", time.Now().UnixNano(), connCount)
	convID := fmt.Sprintf("ws-%d-%d", time.Now().UnixMilli(), connCount)

	wsc := &wsConn{
		conn:   conn,
		convID: convID,
		id:     connID,
	}

	a.connsMu.Lock()
	a.conns[connID] = wsc
	a.connsMu.Unlock()

	log.Infof("websocket: client connected (id=%s, conv=%s)", connID, convID)

	// Configure keep-alive pings. If the client doesn't respond to a
	// ping within 60 seconds, the connection is considered dead.
	conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	// Start ping goroutine.
	go a.pingLoop(wsc)

	// Read loop — runs until the connection closes.
	a.readLoop(wsc)

	// Cleanup on disconnect.
	a.connsMu.Lock()
	delete(a.conns, connID)
	a.connsMu.Unlock()
	_ = conn.Close()

	log.Infof("websocket: client disconnected (id=%s)", connID)
}

// readLoop reads messages from a WebSocket connection and routes them
// to the gateway via msgCh.
func (a *wsAdapter) readLoop(wsc *wsConn) {
	for {
		_, rawMsg, err := wsc.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				log.Warn("websocket: read error", "id", wsc.id, "err", err)
			}
			return
		}

		var msg wsInboundMsg
		if err := json.Unmarshal(rawMsg, &msg); err != nil {
			_ = wsc.writeJSON(wsOutboundMsg{
				Type: "error",
				Text: "invalid JSON message",
			})
			continue
		}

		switch msg.Type {
		case "message":
			if msg.Text == "" {
				continue
			}
			a.handleClientMessage(wsc, msg)

		default:
			_ = wsc.writeJSON(wsOutboundMsg{
				Type: "error",
				Text: fmt.Sprintf("unknown message type: %s", msg.Type),
			})
		}
	}
}

// handleClientMessage processes a chat message from a WebSocket client.
func (a *wsAdapter) handleClientMessage(wsc *wsConn, msg wsInboundMsg) {
	// Intercept slash commands before they hit the pipeline.
	if strings.HasPrefix(msg.Text, "/") {
		if result, handled := a.tryCommand(context.Background(), msg.Text, wsc.convID); handled {
			_ = wsc.writeJSON(wsOutboundMsg{
				Type: "reply",
				Text: result,
			})
			return
		}
	}

	// Set this connection as the active one for reply/streaming routing.
	a.activeConnMu.Lock()
	a.activeConn = wsc
	a.activeConnMu.Unlock()

	// Push to the gateway for pipeline processing.
	a.msgCh <- InboundMsg{
		Text:           msg.Text,
		ConversationID: wsc.convID,
		AdapterName:    a.Name(),
		Timestamp:      time.Now(),
	}
}

// tryCommand checks if a message is a registered command and executes it.
func (a *wsAdapter) tryCommand(ctx context.Context, message, convID string) (string, bool) {
	parts := strings.SplitN(message, " ", 2)
	cmdName := strings.TrimPrefix(parts[0], "/")
	args := ""
	if len(parts) > 1 {
		args = parts[1]
	}

	if cmdName == "clear" {
		newID := fmt.Sprintf("ws-%d", time.Now().UnixMilli())
		// Find and update the connection's conversation ID.
		// Write lock because we're mutating convID.
		a.connsMu.Lock()
		for _, conn := range a.conns {
			if conn.convID == convID {
				conn.convID = newID
				break
			}
		}
		a.connsMu.Unlock()
		return "Context cleared. Fresh start!", true
	}

	for _, cmd := range a.commands {
		if cmd.Name == cmdName {
			result, err := cmd.Handler(ctx, args)
			if err != nil {
				return fmt.Sprintf("Error: %v", err), true
			}
			return result, true
		}
	}

	return "", false
}

// pingLoop sends WebSocket pings every 30 seconds to keep the connection
// alive and detect dead clients.
func (a *wsAdapter) pingLoop(wsc *wsConn) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		wsc.sendMu.Lock()
		err := wsc.conn.WriteMessage(websocket.PingMessage, nil)
		wsc.sendMu.Unlock()
		if err != nil {
			return
		}
	}
}

// broadcastTraces subscribes to the tui event bus and broadcasts trace
// events to all connected WebSocket clients. Each event is wrapped in
// a "trace" message type with the event kind and payload.
func (a *wsAdapter) broadcastTraces(ctx context.Context) {
	eventCh := a.bus.Subscribe(128)
	defer a.bus.Unsubscribe(eventCh)

	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-eventCh:
			if !ok {
				return
			}
			eventType, data := marshalBusEvent(evt)
			if eventType == "" {
				continue
			}

			msg := wsOutboundMsg{
				Type:      "trace",
				EventType: eventType,
				Data:      json.RawMessage(data),
			}

			a.connsMu.RLock()
			for _, conn := range a.conns {
				_ = conn.writeJSON(msg)
			}
			a.connsMu.RUnlock()
		}
	}
}

func (a *wsAdapter) handleStatus(w http.ResponseWriter, r *http.Request) {
	a.connsMu.RLock()
	connCount := len(a.conns)
	a.connsMu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"status":      "ok",
		"adapter":     a.Name(),
		"connections": connCount,
	})
}

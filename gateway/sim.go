package gateway

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"her/config"
)

// SimMessage is a single message in a simulation scenario.
type SimMessage struct {
	Text  string
	Image string // path to local image file (optional)
}

// SimResult holds the response for one sim turn.
type SimResult struct {
	Input    string
	Reply    string
	Duration time.Duration
	Error    error
}

// simAdapter implements the Adapter interface for simulation runs.
// It feeds pre-loaded messages through the gateway pipeline one at a
// time, collecting responses. This is the integration test adapter —
// it proves the full pipeline works end-to-end without any network
// transport.
type simAdapter struct {
	cfg      config.AdapterConfig
	messages []SimMessage

	msgCh    chan InboundMsg
	commands []CommandDef

	// Synchronous request/reply — same pattern as Gradio.
	pendingMu sync.Mutex
	pending   chan OutboundMsg

	// Results collected after the run completes.
	mu      sync.Mutex
	results []SimResult

	// Done is closed when all messages have been processed.
	Done chan struct{}
}

func newSimAdapter(acfg config.AdapterConfig, messages []SimMessage) (Adapter, error) {
	return &simAdapter{
		cfg:      acfg,
		messages: messages,
		msgCh:    make(chan InboundMsg, 1),
		Done:     make(chan struct{}),
	}, nil
}

func (a *simAdapter) Name() string { return a.cfg.Name }

func (a *simAdapter) Capabilities() CapSet {
	return CapSet{}
}

// Start drives the scenario — sends each message through the pipeline
// sequentially, waits for the reply, collects results. Blocks until
// all messages are processed or ctx is cancelled.
func (a *simAdapter) Start(ctx context.Context) error {
	convID := fmt.Sprintf("sim-%d", time.Now().UnixMilli())

	for i, msg := range a.messages {
		if ctx.Err() != nil {
			break
		}

		start := time.Now()
		log.Infof("sim: [%d/%d] sending: %s", i+1, len(a.messages), truncateSimText(msg.Text, 80))

		// Build the inbound message.
		inbound := InboundMsg{
			Text:           msg.Text,
			ConversationID: convID,
			AdapterName:    a.Name(),
			Timestamp:      time.Now(),
		}

		// Load image if specified.
		if msg.Image != "" {
			imgData, mime, err := loadImage(msg.Image)
			if err != nil {
				log.Errorf("sim: failed to load image %s: %v", msg.Image, err)
			} else {
				inbound.ImageBase64 = imgData
				inbound.ImageMIME = mime
			}
		}

		// Create reply channel and send message.
		replyCh := make(chan OutboundMsg, 1)
		a.pendingMu.Lock()
		a.pending = replyCh
		a.pendingMu.Unlock()

		a.msgCh <- inbound

		// Wait for pipeline response.
		var result SimResult
		result.Input = msg.Text

		select {
		case reply := <-replyCh:
			result.Reply = reply.Text
			result.Duration = time.Since(start)
		case <-ctx.Done():
			result.Error = ctx.Err()
		case <-time.After(5 * time.Minute):
			result.Error = fmt.Errorf("timeout after 5 minutes")
		}

		a.mu.Lock()
		a.results = append(a.results, result)
		a.mu.Unlock()

		if result.Error != nil {
			log.Errorf("sim: [%d/%d] error: %v", i+1, len(a.messages), result.Error)
		} else {
			log.Infof("sim: [%d/%d] reply (%s): %s", i+1, len(a.messages),
				result.Duration.Round(time.Millisecond), truncateSimText(result.Reply, 100))
		}
	}

	close(a.Done)
	return nil
}

func (a *simAdapter) Stop() error { return nil }

func (a *simAdapter) Receive() <-chan InboundMsg { return a.msgCh }

func (a *simAdapter) Send(msg OutboundMsg) error {
	a.pendingMu.Lock()
	ch := a.pending
	a.pendingMu.Unlock()

	if ch != nil {
		ch <- msg
	}
	return nil
}

func (a *simAdapter) SendStatus(text string) error  { return nil }
func (a *simAdapter) StartTyping() func()            { return func() {} }
func (a *simAdapter) RegisterCommands(cmds []CommandDef) { a.commands = cmds }

// Results returns the collected sim results after Done is closed.
func (a *simAdapter) Results() []SimResult {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]SimResult{}, a.results...)
}

// loadImage reads a local image file and returns base64-encoded data + MIME type.
func loadImage(path string) (string, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}

	mime := http.DetectContentType(data)
	encoded := base64.StdEncoding.EncodeToString(data)
	return encoded, mime, nil
}

func truncateSimText(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

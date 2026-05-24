// Package gateway defines the transport-agnostic layer between
// messaging platforms (Telegram, Gradio, etc.) and the agent pipeline.
// Adapters convert platform-specific I/O into these standard types.
package gateway

import "time"

// InboundMsg is a message entering the system from any adapter.
type InboundMsg struct {
	Text           string
	Audio          []byte // nil if text-only
	ImageBase64    string // populated if the message includes an image
	ImageMIME      string
	ConversationID string // adapter-assigned, unique per adapter instance
	AdapterName    string // "telegram", "gradio", etc.
	Timestamp      time.Time
}

// OutboundMsg is a response leaving the system to any adapter.
type OutboundMsg struct {
	Text    string
	Audio   []byte // nil if text-only (TTS handled by voice service)
	IsError bool
}

// Command is a platform-agnostic command invocation. Each adapter
// translates its native format (Telegram /cmd, Gradio button, etc.)
// into this struct.
type Command struct {
	Name string // "traces", "mood", "clear", "help", etc.
	Args string // everything after the command name
}

// CapSet declares which optional features an adapter supports.
// The gateway checks these before invoking optional methods —
// unsupported features are silently skipped.
type CapSet struct {
	Edit     bool // edit previously sent messages in-place
	Stream   bool // token-by-token streaming
	Paginate bool // multi-page messages with navigation controls
	Typing   bool // typing indicators
	Audio    bool // voice message send/receive
	Confirm  bool // interactive yes/no confirmations
}

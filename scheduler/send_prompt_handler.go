package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
)

// sendPromptHandler runs a prompt through the full driver agent loop.
// This is the "smart" counterpart to sendMessageHandler — instead of
// static text, the driver agent gets the prompt as a system instruction
// and generates a contextual response (aware of memories, mood, etc.).
//
// Emits events through deps.ScheduledPromptFn — a callback set by
// cmd/run.go that constructs the AgentEvent and sends it on the event
// channel. We use a callback instead of importing agent/ directly to
// avoid an import cycle (agent → tools → scheduler → agent).
type sendPromptHandler struct{}

func (sendPromptHandler) Kind() string       { return "send_prompt" }
func (sendPromptHandler) ConfigPath() string { return "" }

func (h sendPromptHandler) Execute(_ context.Context, payload json.RawMessage, deps *Deps) error {
	var p struct {
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return fmt.Errorf("send_prompt: bad payload: %w", err)
	}
	if p.Prompt == "" {
		return fmt.Errorf("send_prompt: empty prompt")
	}

	if deps.ScheduledPromptFn == nil {
		return fmt.Errorf("send_prompt: no prompt callback configured")
	}

	return deps.ScheduledPromptFn(p.Prompt)
}

func init() {
	Register(sendPromptHandler{})
}

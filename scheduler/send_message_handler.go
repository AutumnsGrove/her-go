package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
)

// sendMessageHandler sends a static Telegram message on a cron schedule.
// This is the simplest scheduled action — no LLM, no agent loop, just
// a direct message. Think of it as a basic reminder.
type sendMessageHandler struct{}

func (sendMessageHandler) Kind() string       { return "send_message" }
func (sendMessageHandler) ConfigPath() string { return "" }

func (h sendMessageHandler) Execute(_ context.Context, payload json.RawMessage, deps *Deps) error {
	var p struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return fmt.Errorf("send_message: bad payload: %w", err)
	}
	if p.Message == "" {
		return fmt.Errorf("send_message: empty message")
	}

	// Append schedule context so the user knows this came from a schedule
	// and can easily delete it ("delete this reminder").
	message := p.Message
	if deps.TaskContext != nil && deps.TaskContext.ID > 0 {
		message = fmt.Sprintf("%s\n\n<i>📅 Scheduled reminder #%d</i>",
			p.Message, deps.TaskContext.ID)
	}

	_, err := deps.Send(deps.ChatID, message)
	return err
}

func init() {
	Register(sendMessageHandler{})
}

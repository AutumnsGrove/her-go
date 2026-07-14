package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"her/memory"
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

	_, err := deps.Send(deps.ChatID, p.Message)
	if err != nil {
		return err
	}

	// Persist the sent message with its schedule_id (migration 000021) so
	// the schedule_context layer can find it later and enable "delete this
	// reminder" UX without the user ever seeing the schedule ID. Without
	// this, send_message reminders fire but leave no trace the agent can
	// use to resolve a contextual "delete it" — only send_prompt schedules
	// got this for free, via tools/reply/handler.go's SaveMessage call.
	if store, ok := deps.Store.(memory.Store); ok {
		var scheduleID int64
		if deps.TaskContext != nil {
			scheduleID = deps.TaskContext.ID
		}
		var convID string
		if deps.GetConversationID != nil {
			convID = deps.GetConversationID(deps.ChatID)
		} else {
			convID = conversationIDForChat(store, deps.ChatID)
		}
		if _, err := store.SaveMessage("assistant", p.Message, p.Message, convID, scheduleID); err != nil {
			return fmt.Errorf("send_message: sent but failed to record message: %w", err)
		}
	}

	return nil
}

// conversationIDForChat is the fallback used when deps.GetConversationID
// isn't wired (e.g. handler tests). It mirrors bot.getConversationID's
// lookup logic — resume the chat's latest conversation, or mint a fresh
// ID — without importing the bot package.
func conversationIDForChat(store memory.Store, chatID int64) string {
	prefix := fmt.Sprintf("tg_%d", chatID)
	if existing := store.LatestConversationID(prefix); existing != "" {
		return existing
	}
	return fmt.Sprintf("tg_%d_%d", chatID, time.Now().Unix())
}

func init() {
	Register(sendMessageHandler{})
}

// callbacks.go — Inline keyboard callback handling.
//
// Routes callback queries when users click inline buttons (pagination,
// confirmations, etc.). Each button's Unique field maps to a handler
// registered in registerCallbackHandlers.
package bot

import (
	"encoding/json"
	"fmt"
	"strings"

	"her/memory"

	tele "gopkg.in/telebot.v4"
)

// registerCallbackHandlers sets up handlers for inline button callbacks.
// Each Action value used in Button types needs a corresponding handler
// registered here. Adding a new button type is a one-liner.
//
// Called from New() during bot initialization.
func (b *Bot) registerCallbackHandlers() {
	// Agent confirmation buttons (Action: "confirm") — handles Yes/No
	// for destructive actions triggered by the reply_confirm tool.
	b.tb.Handle(&tele.InlineButton{Unique: "confirm"}, b.handleConfirmCallback)

	// Pagination buttons (Action: "page") — handles ◀/▶ navigation
	// for any command that produces output longer than 4096 chars.
	b.tb.Handle(&tele.InlineButton{Unique: "page"}, b.handlePageCallback)
}

// --- Agent Confirmation Callback ---

// handleConfirmCallback fires when the user clicks Yes or No on a
// confirmation prompt sent by the reply_confirm agent tool. It looks up
// the pending action by Telegram message ID, executes or cancels it,
// and edits the message to show the result.
//
// This is the same pattern as handleMoodCallback — read the button's
// data, take action, edit the message — but agent-driven instead of
// scheduler-driven. The key difference is that we look up a stored
// pending action instead of knowing what to do from the button value.
func (b *Bot) handleConfirmCallback(c tele.Context) error {
	data := strings.TrimSpace(c.Callback().Data)

	// c.Callback().Message is the message that has the inline buttons.
	// Its ID is what we used to key the pending_confirmations table.
	msgID := int64(c.Callback().Message.ID)

	// Look up the pending confirmation by Telegram message ID.
	pending, err := b.store.GetPendingConfirmation(msgID)
	if err != nil {
		log.Error("looking up pending confirmation", "msg_id", msgID, "err", err)
		return c.Respond(&tele.CallbackResponse{Text: "Something went wrong"})
	}
	if pending == nil {
		// Already resolved (double-click) or expired (>1 hour old).
		return c.Respond(&tele.CallbackResponse{Text: "This confirmation has expired"})
	}

	// Get the conversation ID so we can record the outcome in history.
	// Without this, the agent has no idea the user responded to the
	// confirmation — it sees "I sent you a confirmation" in its own
	// reply but never sees the resolution, leading to confused follow-ups
	// like "did you see the confirmation I sent?"
	chatID := c.Callback().Message.Chat.ID
	convID := b.getConversationID(chatID)

	if data == "no" {
		// User cancelled — mark as cancelled and update the message.
		_ = b.store.ResolvePendingConfirmation(pending.ID, "cancelled")
		_ = c.Respond(&tele.CallbackResponse{Text: "Cancelled"})
		_ = c.Edit("❌ " + pending.Description + " — cancelled")
		log.Info("confirmation cancelled", "id", pending.ID, "action", pending.ActionType)

		// Record in conversation history so the agent knows.
		note := fmt.Sprintf("[User cancelled: %s]", pending.Description)
		b.store.SaveMessage("user", note, note, convID)
		return nil
	}

	// data == "yes" — execute the pending action.
	result, err := b.executeConfirmedAction(pending)
	if err != nil {
		_ = b.store.ResolvePendingConfirmation(pending.ID, "error")
		_ = c.Respond(&tele.CallbackResponse{Text: "Something went wrong"})
		_ = c.Edit("⚠️ " + pending.Description + " — failed: " + err.Error())
		log.Error("executing confirmed action", "id", pending.ID, "action", pending.ActionType, "err", err)
		return nil
	}

	_ = b.store.ResolvePendingConfirmation(pending.ID, "confirmed")
	_ = c.Respond(&tele.CallbackResponse{Text: "Done!"})
	_ = c.Edit("✅ " + result)
	log.Info("confirmation executed", "id", pending.ID, "action", pending.ActionType, "result", result)

	// Record the confirmed action in conversation history so the agent
	// knows the user already responded. Without this, the next turn's
	// context has no record of the resolution — the agent sees its own
	// "I sent you a confirmation" but not the user's click.
	note := fmt.Sprintf("[User confirmed: %s]", result)
	b.store.SaveMessage("user", note, note, convID)
	return nil
}

// executeConfirmedAction runs the actual destructive action described
// by a pending confirmation. Each action_type maps to a specific store
// method. Adding new confirmable actions means adding a case here AND
// in agent/confirm.go's validConfirmActions map.
//
// This runs directly against b.store — no agent loop or toolContext
// needed. That's the beauty of storing the action in the DB: the
// callback handler is self-contained.
func (b *Bot) executeConfirmedAction(pending *memory.PendingConfirmation) (string, error) {
	switch pending.ActionType {
	case "delete_expense":
		// Supports both single ID and multiple IDs:
		//   {"id": 42}       → delete one expense
		//   {"ids": [1,2,3]} → delete several expenses in one confirmation
		var payload struct {
			ID  int64   `json:"id"`
			IDs []int64 `json:"ids"`
		}
		if err := json.Unmarshal(pending.ActionPayload, &payload); err != nil {
			return "", fmt.Errorf("bad payload: %v", err)
		}

		// Normalize: if single ID was provided, treat it as a one-element list.
		ids := payload.IDs
		if payload.ID > 0 && len(ids) == 0 {
			ids = []int64{payload.ID}
		}
		if len(ids) == 0 {
			return "", fmt.Errorf("no expense IDs provided")
		}

		var deleted int
		for _, id := range ids {
			if id <= 0 {
				continue
			}
			if err := b.store.DeleteExpense(id); err != nil {
				log.Error("deleting expense in batch", "id", id, "err", err)
				continue // best-effort — delete what we can
			}
			deleted++
		}
		if deleted == 0 {
			return "", fmt.Errorf("no expenses were deleted")
		}
		if deleted == 1 {
			return fmt.Sprintf("Expense #%d deleted", ids[0]), nil
		}
		return fmt.Sprintf("%d expenses deleted", deleted), nil

	case "remove_memory":
		var payload struct {
			FactID int64 `json:"fact_id"`
		}
		if err := json.Unmarshal(pending.ActionPayload, &payload); err != nil {
			return "", fmt.Errorf("bad payload: %v", err)
		}
		if payload.FactID <= 0 {
			return "", fmt.Errorf("invalid fact ID: %d", payload.FactID)
		}
		if err := b.store.DeactivateMemory(payload.FactID); err != nil {
			return "", err
		}
		return fmt.Sprintf("Memory #%d removed", payload.FactID), nil

	default:
		return "", fmt.Errorf("unknown action type: %s", pending.ActionType)
	}
}

// Package agent — reply_confirm tool handler.
//
// This tool sends a Yes/No confirmation prompt via Telegram inline
// keyboard buttons before executing destructive actions. It's the
// agent-driven counterpart to the scheduler-driven mood/med check-ins.
//
// Flow:
//  1. Agent calls reply_confirm(message, action_type, action_payload)
//  2. This handler sends a keyboard message via sendConfirmCallback
//  3. The pending action is stored in SQLite keyed by Telegram message ID
//  4. Agent continues to reply + done as normal
//  5. User clicks Yes → bot/callbacks.go executes the action
//  6. User clicks No → bot/callbacks.go marks it cancelled
//
// The action executes completely outside the agent loop — no toolContext
// needed. The callback handler calls store methods directly.
package agent

import (
	"encoding/json"
	"fmt"
)

// validConfirmActions lists the action types that reply_confirm supports.
// Each one maps to a specific store method in bot/callbacks.go's
// executeConfirmedAction. Adding a new action type requires updating
// both this map and the switch in executeConfirmedAction.
//
// Go doesn't have a Set type like Python. The idiomatic approach is a
// map[string]bool — checking membership is just validConfirmActions["key"].
var validConfirmActions = map[string]bool{
	"delete_expense":  true,
	"remove_fact":     true,
	"delete_schedule": true,
}

// execReplyConfirm handles the reply_confirm tool call. It sends a
// confirmation message with inline keyboard buttons and stores the
// pending action in SQLite. The actual action execution happens later
// in bot/callbacks.go when the user clicks Yes.
//
// Parameters (from agent):
//   - message:        string — the confirmation question to display
//   - action_type:    enum — what to execute on confirmation
//   - action_payload: string — JSON payload for the action
func execReplyConfirm(argsJSON string, tctx *toolContext) string {
	var args struct {
		Message       string `json:"message"`
		ActionType    string `json:"action_type"`
		ActionPayload string `json:"action_payload"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}

	// Validate the action type is one we know how to execute.
	if !validConfirmActions[args.ActionType] {
		return fmt.Sprintf("error: unsupported action_type %q — valid types: delete_expense, remove_fact, delete_schedule", args.ActionType)
	}

	// Validate the payload is well-formed JSON. Without this check,
	// the callback handler would fail when trying to unmarshal it.
	if !json.Valid([]byte(args.ActionPayload)) {
		return "error: action_payload must be valid JSON"
	}

	// The sendConfirmCallback is nil-safe — if not provided (e.g., in
	// tests or non-Telegram contexts), we can't send buttons.
	if tctx.sendConfirmCallback == nil {
		return "error: confirmation buttons not available in this context"
	}

	if tctx.store == nil {
		return "error: database not available"
	}

	// Send the confirmation message with Yes/No buttons via the bot.
	// The closure provided by bot/telegram.go builds the inline keyboard
	// and returns the Telegram message ID so we can key the pending
	// confirmation to it.
	telegramMsgID, err := tctx.sendConfirmCallback(args.Message)
	if err != nil {
		log.Error("sending confirmation keyboard", "err", err)
		return fmt.Sprintf("error sending confirmation: %v", err)
	}

	// Store the pending confirmation in SQLite. When the user clicks
	// a button, handleConfirmCallback in bot/callbacks.go looks this
	// up by telegram_msg_id and either executes or cancels the action.
	_, err = tctx.store.CreatePendingConfirmation(
		telegramMsgID,
		args.ActionType,
		json.RawMessage(args.ActionPayload),
		args.Message,
	)
	if err != nil {
		log.Error("storing pending confirmation", "err", err)
		return fmt.Sprintf("error storing confirmation: %v", err)
	}

	log.Infof("  reply_confirm: sent %s confirmation (msg=%d)", args.ActionType, telegramMsgID)
	return "confirmation sent — the action will execute when the user clicks Yes. Proceed to reply and done."
}

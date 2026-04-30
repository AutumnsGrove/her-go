package memory

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// ---------------------------------------------------------------------------
// PII Vault
// ---------------------------------------------------------------------------

// SavePIIVaultEntry persists a Tier 2 token↔original mapping for audit trail.
func (s *SQLiteStore) SavePIIVaultEntry(messageID int64, token, originalValue, entityType string) error {
	_, err := s.db.Exec(
		`INSERT INTO pii_vault (message_id, token, original_value, entity_type)
		 VALUES (?, ?, ?, ?)`,
		messageID, token, originalValue, entityType,
	)
	if err != nil {
		return fmt.Errorf("saving PII vault entry: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Pending Confirmations
// ---------------------------------------------------------------------------
//
// These support the reply_confirm agent tool. When the agent wants to
// execute a destructive action (delete expense, remove fact, etc.), it
// sends a confirmation message with Yes/No buttons instead of executing
// immediately. The pending confirmation is stored here, and the callback
// handler looks it up when the user clicks a button.
//
// This is similar to how mood check-ins work (save data when user clicks
// an inline button), but agent-driven instead of scheduler-driven.

// PendingConfirmation represents a destructive action waiting for user
// approval via an inline keyboard button click.
type PendingConfirmation struct {
	ID             int64
	TelegramMsgID  int64
	ActionType     string          // e.g., "delete_expense", "remove_fact", "delete_schedule"
	ActionPayload  json.RawMessage // JSON blob with action-specific params
	Description    string          // human-readable description shown after resolution
	CreatedAt      time.Time
	ResolvedAt     *time.Time // nil until the user clicks a button
	ResolvedAction *string    // "confirmed", "cancelled", or "error"
}

// CreatePendingConfirmation stores a new pending confirmation keyed by
// the Telegram message ID of the confirmation message. The callback
// handler will look this up when the user clicks Yes or No.
//
// This follows the same pattern as SaveMoodEntry — simple INSERT, return
// the auto-generated ID. The telegramMsgID comes from the bot's Send()
// call, which returns the message object with its ID.
func (s *SQLiteStore) CreatePendingConfirmation(telegramMsgID int64, actionType string, actionPayload json.RawMessage, description string) (int64, error) {
	result, err := s.db.Exec(
		`INSERT INTO pending_confirmations (telegram_msg_id, action_type, action_payload, description)
		 VALUES (?, ?, ?, ?)`,
		telegramMsgID, actionType, string(actionPayload), description,
	)
	if err != nil {
		return 0, fmt.Errorf("creating pending confirmation: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("getting pending confirmation ID: %w", err)
	}
	return id, nil
}

// GetPendingConfirmation looks up an unresolved confirmation by the
// Telegram message ID. Returns nil (not error) if not found, already
// resolved, or older than 1 hour (expired).
//
// The 1-hour TTL prevents stale confirmations from executing days later
// if the user scrolls back and clicks an old button. This is a soft
// safety net — the worst case is the user has to re-ask.
func (s *SQLiteStore) GetPendingConfirmation(telegramMsgID int64) (*PendingConfirmation, error) {
	row := s.db.QueryRow(
		`SELECT id, telegram_msg_id, action_type, action_payload, description, created_at
		 FROM pending_confirmations
		 WHERE telegram_msg_id = ?
		   AND resolved_at IS NULL
		   AND created_at > datetime('now', '-1 hour')`,
		telegramMsgID,
	)

	var pc PendingConfirmation
	var payloadStr string
	err := row.Scan(&pc.ID, &pc.TelegramMsgID, &pc.ActionType, &payloadStr, &pc.Description, &pc.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil // not found or expired — not an error
	}
	if err != nil {
		return nil, fmt.Errorf("getting pending confirmation: %w", err)
	}
	pc.ActionPayload = json.RawMessage(payloadStr)
	return &pc, nil
}

// ResolvePendingConfirmation marks a confirmation as resolved with the
// given action ("confirmed", "cancelled", or "error"). This prevents
// double-clicks — once resolved, GetPendingConfirmation won't return it.
func (s *SQLiteStore) ResolvePendingConfirmation(id int64, action string) error {
	_, err := s.db.Exec(
		`UPDATE pending_confirmations
		 SET resolved_at = CURRENT_TIMESTAMP, resolved_action = ?
		 WHERE id = ?`,
		action, id,
	)
	if err != nil {
		return fmt.Errorf("resolving pending confirmation %d: %w", id, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Location history
// ---------------------------------------------------------------------------

// LocationEntry represents a row in the location_history table.
type LocationEntry struct {
	ID             int64
	Timestamp      string
	Latitude       float64
	Longitude      float64
	Label          string
	Source         string
	ConversationID string
}

// InsertLocation records a location event. source should be one of:
// "pin" (Telegram location share), "venue" (Telegram venue share),
// "text" (geocoded from text input), "search" (nearby_search query).
func (s *SQLiteStore) InsertLocation(lat, lon float64, label, source, conversationID string) error {
	_, err := s.db.Exec(
		`INSERT INTO location_history (latitude, longitude, label, source, conversation_id)
		 VALUES (?, ?, ?, ?, ?)`,
		lat, lon, label, source, conversationID,
	)
	if err != nil {
		return fmt.Errorf("inserting location: %w", err)
	}
	return nil
}

// LatestLocation returns the most recent location entry, or nil if none exist.
// Used by nearby_search as a fallback when no explicit location is provided.
func (s *SQLiteStore) LatestLocation() *LocationEntry {
	row := s.db.QueryRow(
		`SELECT id, timestamp, latitude, longitude, label, source, conversation_id
		 FROM location_history ORDER BY timestamp DESC LIMIT 1`,
	)

	var loc LocationEntry
	var label, convID *string
	err := row.Scan(&loc.ID, &loc.Timestamp, &loc.Latitude, &loc.Longitude, &label, &loc.Source, &convID)
	if err != nil {
		return nil
	}
	if label != nil {
		loc.Label = *label
	}
	if convID != nil {
		loc.ConversationID = *convID
	}
	return &loc
}

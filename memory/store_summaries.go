package memory

import (
	"database/sql"
	"fmt"
)

// SaveSummary stores a compacted summary of older messages.
// startID and endID mark the range of message IDs that were summarized.
// stream is "chat" or "agent" — each model maintains its own running summary.
func (s *Store) SaveSummary(conversationID, summary string, startID, endID int64, stream string) (int64, error) {
	result, err := s.db.Exec(
		`INSERT INTO summaries (conversation_id, summary, messages_start_id, messages_end_id, stream)
		 VALUES (?, ?, ?, ?, ?)`,
		conversationID, summary, startID, endID, stream,
	)
	if err != nil {
		return 0, fmt.Errorf("saving summary: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("getting summary ID: %w", err)
	}
	return id, nil
}

// LatestSummary returns the most recent summary for a conversation and stream.
// stream is "chat" or "agent". Returns empty string if no summary exists yet.
func (s *Store) LatestSummary(conversationID, stream string) (string, int64, error) {
	var summary string
	var endID int64
	err := s.db.QueryRow(
		`SELECT summary, messages_end_id FROM summaries
		 WHERE conversation_id = ? AND stream = ?
		 ORDER BY id DESC LIMIT 1`,
		conversationID, stream,
	).Scan(&summary, &endID)
	if err == sql.ErrNoRows {
		return "", 0, nil // no summary yet
	}
	if err != nil {
		return "", 0, fmt.Errorf("querying latest summary: %w", err)
	}
	return summary, endID, nil
}

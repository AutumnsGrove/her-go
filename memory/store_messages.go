package memory

import (
	"database/sql"
	"fmt"
	"time"
)

// Message represents a single conversation message (user or assistant).
type Message struct {
	ID              int64
	Timestamp       time.Time
	Role            string // "user" or "assistant"
	ContentRaw      string // original unscrubbed message
	ContentScrubbed string // PII-scrubbed version sent to LLM
	ConversationID  string
	TokenCount      int
}

// SaveMessage inserts a message into the database and returns its ID.
// This is called for both user messages and assistant responses.
func (s *Store) SaveMessage(role, contentRaw, contentScrubbed, conversationID string) (int64, error) {
	result, err := s.db.Exec(
		`INSERT INTO messages (role, content_raw, content_scrubbed, conversation_id)
		 VALUES (?, ?, ?, ?)`,
		role, contentRaw, contentScrubbed, conversationID,
	)
	if err != nil {
		return 0, fmt.Errorf("saving message: %w", err)
	}

	// LastInsertId returns the auto-generated ID. This is a method on
	// sql.Result — the object returned by Exec for INSERT/UPDATE/DELETE.
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("getting message ID: %w", err)
	}

	return id, nil
}

// GlobalRecentMessages retrieves the last N messages across ALL conversations,
// ordered oldest-first. Used by /reflect which needs recent context regardless
// of which conversation ID they belong to.
func (s *Store) GlobalRecentMessages(limit int) ([]Message, error) {
	rows, err := s.db.Query(
		`SELECT id, timestamp, role, content_raw, content_scrubbed, conversation_id, COALESCE(token_count, 0)
		 FROM (
			SELECT id, timestamp, role, content_raw, content_scrubbed, conversation_id, token_count
			FROM messages
			ORDER BY id DESC
			LIMIT ?
		 ) sub ORDER BY id ASC`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("querying global recent messages: %w", err)
	}
	defer rows.Close()

	var messages []Message
	for rows.Next() {
		var m Message
		var ts string
		var scrubbed sql.NullString
		if err := rows.Scan(&m.ID, &ts, &m.Role, &m.ContentRaw, &scrubbed, &m.ConversationID, &m.TokenCount); err != nil {
			return nil, fmt.Errorf("scanning message row: %w", err)
		}
		m.Timestamp, _ = time.Parse("2006-01-02 15:04:05", ts)
		if scrubbed.Valid {
			m.ContentScrubbed = scrubbed.String
		}
		messages = append(messages, m)
	}
	return messages, nil
}

// RecentMessages retrieves the last N messages for a conversation,
// ordered oldest-first so they can be fed directly into the LLM prompt.
func (s *Store) RecentMessages(conversationID string, limit int) ([]Message, error) {
	// The subquery grabs the last N rows (newest first), then the outer
	// query flips them to chronological order for the prompt.
	rows, err := s.db.Query(
		`SELECT id, timestamp, role, content_raw, content_scrubbed, conversation_id, COALESCE(token_count, 0)
		 FROM (
			SELECT id, timestamp, role, content_raw, content_scrubbed, conversation_id, token_count
			FROM messages
			WHERE conversation_id = ?
			ORDER BY id DESC
			LIMIT ?
		 ) sub ORDER BY id ASC`,
		conversationID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("querying recent messages: %w", err)
	}
	// defer runs when the enclosing function returns — it's Go's cleanup
	// mechanism. Like Python's "with" statement for context managers.
	// Always defer rows.Close() to avoid leaking database connections.
	defer rows.Close()

	var messages []Message
	// rows.Next() advances to the next row, like Python's iterator protocol.
	// When there are no more rows, it returns false and the loop exits.
	for rows.Next() {
		var m Message
		var ts string
		var scrubbed sql.NullString

		// Scan reads column values into Go variables. The order must match
		// the SELECT column order exactly. sql.NullString handles NULL values —
		// regular strings can't represent NULL in Go.
		if err := rows.Scan(&m.ID, &ts, &m.Role, &m.ContentRaw, &scrubbed, &m.ConversationID, &m.TokenCount); err != nil {
			return nil, fmt.Errorf("scanning message row: %w", err)
		}

		m.Timestamp, _ = time.Parse("2006-01-02 15:04:05", ts)
		if scrubbed.Valid {
			m.ContentScrubbed = scrubbed.String
		}
		messages = append(messages, m)
	}

	return messages, nil
}

// MessagesAfter retrieves all messages in a conversation after a given ID.
// Used by fact extraction to get the batch of messages to analyze.
func (s *Store) MessagesAfter(conversationID string, sinceID int64) ([]Message, error) {
	rows, err := s.db.Query(
		`SELECT id, timestamp, role, content_raw, content_scrubbed, conversation_id
		 FROM messages
		 WHERE conversation_id = ? AND id > ?
		 ORDER BY id ASC`,
		conversationID, sinceID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying messages after %d: %w", sinceID, err)
	}
	defer rows.Close()

	var messages []Message
	for rows.Next() {
		var m Message
		var ts string
		var scrubbed sql.NullString
		if err := rows.Scan(&m.ID, &ts, &m.Role, &m.ContentRaw, &scrubbed, &m.ConversationID); err != nil {
			return nil, fmt.Errorf("scanning message row: %w", err)
		}
		m.Timestamp, _ = time.Parse("2006-01-02 15:04:05", ts)
		if scrubbed.Valid {
			m.ContentScrubbed = scrubbed.String
		}
		messages = append(messages, m)
	}
	return messages, nil
}

// MessagesInRange returns messages between startID and endID inclusive.
func (s *Store) MessagesInRange(conversationID string, startID, endID int64) ([]Message, error) {
	rows, err := s.db.Query(
		`SELECT id, timestamp, role, content_raw, content_scrubbed, conversation_id
		 FROM messages
		 WHERE conversation_id = ? AND id >= ? AND id <= ?
		 ORDER BY id ASC`,
		conversationID, startID, endID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying messages in range: %w", err)
	}
	defer rows.Close()

	var messages []Message
	for rows.Next() {
		var m Message
		var ts string
		var scrubbed sql.NullString
		if err := rows.Scan(&m.ID, &ts, &m.Role, &m.ContentRaw, &scrubbed, &m.ConversationID); err != nil {
			return nil, fmt.Errorf("scanning message row: %w", err)
		}
		m.Timestamp, _ = time.Parse("2006-01-02 15:04:05", ts)
		if scrubbed.Valid {
			m.ContentScrubbed = scrubbed.String
		}
		messages = append(messages, m)
	}
	return messages, nil
}

// UpdateMessageScrubbed updates the scrubbed content for a message.
// We save the raw message first (for data safety), then update with the
// scrubbed version after PII processing completes.
func (s *Store) UpdateMessageScrubbed(messageID int64, scrubbed string) error {
	_, err := s.db.Exec(
		`UPDATE messages SET content_scrubbed = ? WHERE id = ?`,
		scrubbed, messageID,
	)
	if err != nil {
		return fmt.Errorf("updating scrubbed content: %w", err)
	}
	return nil
}

// UpdateMessageMedia stores the Telegram file ID and/or VLM description
// for a message that has media attached. Either field can be empty —
// we use COALESCE to only update non-empty values, so you can call this
// once for the file_id (from the bot) and again for the description
// (from the agent's view_image tool) without clobbering the other.
func (s *Store) UpdateMessageMedia(messageID int64, fileID, description string) error {
	_, err := s.db.Exec(
		`UPDATE messages SET
			media_file_id = COALESCE(NULLIF(?, ''), media_file_id),
			media_description = COALESCE(NULLIF(?, ''), media_description)
		 WHERE id = ?`,
		fileID, description, messageID,
	)
	if err != nil {
		return fmt.Errorf("updating message media: %w", err)
	}
	return nil
}

// UpdateMessageVoicePath stores the local file path to the original
// audio file for a voice memo message. Used for debugging and replay.
func (s *Store) UpdateMessageVoicePath(messageID int64, path string) error {
	_, err := s.db.Exec(
		`UPDATE messages SET voice_memo_path = ? WHERE id = ?`,
		path, messageID,
	)
	if err != nil {
		return fmt.Errorf("updating voice memo path: %w", err)
	}
	return nil
}

// UpdateMessageTokenCount sets the token count for a message after the
// LLM responds. For user messages this is the prompt token count, for
// assistant messages it's the completion token count.
func (s *Store) UpdateMessageTokenCount(messageID int64, tokenCount int) error {
	_, err := s.db.Exec(
		`UPDATE messages SET token_count = ? WHERE id = ?`,
		tokenCount, messageID,
	)
	if err != nil {
		return fmt.Errorf("updating token count: %w", err)
	}
	return nil
}

// MessageCountSince counts how many user messages exist in a conversation
// after a given message ID. Used to decide when to trigger fact extraction.
func (s *Store) MessageCountSince(conversationID string, sinceID int64) (int, error) {
	var count int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM messages
		 WHERE conversation_id = ? AND id > ? AND role = 'user'`,
		conversationID, sinceID,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("counting messages: %w", err)
	}
	return count, nil
}

// ConversationCountSince counts distinct conversation IDs in messages
// created after the given timestamp. Used to determine when to trigger
// a persona rewrite (every ~20 conversations).
func (s *Store) ConversationCountSince(since time.Time) (int, error) {
	var count int
	err := s.db.QueryRow(
		`SELECT COUNT(DISTINCT conversation_id) FROM messages WHERE timestamp > ?`,
		since.Format("2006-01-02 15:04:05"),
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("counting conversations: %w", err)
	}
	return count, nil
}

// LatestConversationID returns the most recent conversation_id used
// for a given chat identifier prefix (e.g., "tg_7570137189").
// Returns empty string if no conversations exist.
// This lets the bot resume the same conversation after a restart
// instead of generating a new ID and losing context.
func (s *Store) LatestConversationID(prefix string) string {
	var convID string
	err := s.db.QueryRow(
		`SELECT conversation_id FROM messages
		 WHERE conversation_id LIKE ? || '%'
		 ORDER BY id DESC LIMIT 1`,
		prefix,
	).Scan(&convID)
	if err != nil {
		return ""
	}
	return convID
}

// LastExtractionMessageID returns the highest source_message_id in the
// facts table for tracking where the last extraction left off. Returns 0
// if no facts exist yet.
func (s *Store) LastExtractionMessageID() (int64, error) {
	var id sql.NullInt64
	err := s.db.QueryRow(
		`SELECT MAX(source_message_id) FROM facts`,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("querying last extraction ID: %w", err)
	}
	if id.Valid {
		return id.Int64, nil
	}
	return 0, nil
}

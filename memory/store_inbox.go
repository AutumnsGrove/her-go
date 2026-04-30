package memory

import (
	"fmt"
	"time"
)

// InboxMessage is a single message in the inter-agent inbox. Agents use
// this to pass tasks and results to each other asynchronously. Think of
// it like Python's asyncio.Queue, but persisted in SQLite so messages
// survive restarts and can be inspected in the DB.
type InboxMessage struct {
	ID        int64
	CreatedAt time.Time
	Sender    string // "main", "memory", "mood"
	Recipient string // "main", "memory", "mood"
	MsgType   string // "cleanup", "split", "result", etc.
	Payload   string // JSON blob with task-specific data
}

// initInboxTable creates the inbox table and index. Called from initTables.
// Idempotent — safe to run on every startup.
func (s *SQLiteStore) initInboxTable() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS inbox (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		created_at  DATETIME DEFAULT CURRENT_TIMESTAMP,
		sender      TEXT NOT NULL,
		recipient   TEXT NOT NULL,
		msg_type    TEXT NOT NULL,
		payload     TEXT NOT NULL DEFAULT '{}',
		status      TEXT NOT NULL DEFAULT 'pending',
		consumed_at DATETIME
	)`)
	if err != nil {
		return fmt.Errorf("creating inbox table: %w", err)
	}

	_, err = s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_inbox_recipient_status ON inbox(recipient, status)`)
	if err != nil {
		return fmt.Errorf("creating inbox index: %w", err)
	}

	return nil
}

// SendInbox writes a message to the inbox for another agent to pick up.
// Returns the new message ID. The payload should be a JSON string — the
// inbox doesn't parse it, that's the recipient's job.
func (s *SQLiteStore) SendInbox(sender, recipient, msgType, payload string) (int64, error) {
	result, err := s.db.Exec(
		`INSERT INTO inbox (sender, recipient, msg_type, payload) VALUES (?, ?, ?, ?)`,
		sender, recipient, msgType, payload,
	)
	if err != nil {
		return 0, fmt.Errorf("inserting inbox message: %w", err)
	}
	return result.LastInsertId()
}

// ConsumeInbox atomically reads all pending messages for a recipient and
// marks them as consumed. Returns the messages oldest-first. This is a
// one-shot read — once consumed, messages won't appear again.
//
// The atomicity comes from SQLite's single-writer lock: the UPDATE runs
// in the same transaction as the SELECT, so no other goroutine can
// consume the same messages between reading and marking.
func (s *SQLiteStore) ConsumeInbox(recipient string) ([]InboxMessage, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("beginning inbox transaction: %w", err)
	}
	defer tx.Rollback() // no-op if committed

	rows, err := tx.Query(
		`SELECT id, created_at, sender, recipient, msg_type, payload
		 FROM inbox
		 WHERE recipient = ? AND status = 'pending'
		 ORDER BY created_at ASC`,
		recipient,
	)
	if err != nil {
		return nil, fmt.Errorf("querying inbox: %w", err)
	}
	defer rows.Close()

	var messages []InboxMessage
	var ids []any
	for rows.Next() {
		var m InboxMessage
		if err := rows.Scan(&m.ID, &m.CreatedAt, &m.Sender, &m.Recipient, &m.MsgType, &m.Payload); err != nil {
			return nil, fmt.Errorf("scanning inbox row: %w", err)
		}
		messages = append(messages, m)
		ids = append(ids, m.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating inbox rows: %w", err)
	}

	// Mark all fetched messages as consumed in one UPDATE.
	if len(ids) > 0 {
		// Build a placeholder string like "?,?,?" for the IN clause.
		// Go's database/sql doesn't support slice parameters directly —
		// you have to expand them yourself. This is one of Go's rough edges
		// compared to Python's sqlite3 module which handles lists natively.
		placeholders := ""
		for i := range ids {
			if i > 0 {
				placeholders += ","
			}
			placeholders += "?"
		}
		_, err = tx.Exec(
			fmt.Sprintf(
				`UPDATE inbox SET status = 'consumed', consumed_at = CURRENT_TIMESTAMP WHERE id IN (%s)`,
				placeholders,
			),
			ids...,
		)
		if err != nil {
			return nil, fmt.Errorf("marking inbox consumed: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing inbox transaction: %w", err)
	}

	return messages, nil
}

// PendingInboxCount returns how many unread messages are waiting for a
// recipient. Useful for quick checks without consuming the messages.
func (s *SQLiteStore) PendingInboxCount(recipient string) (int, error) {
	var count int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM inbox WHERE recipient = ? AND status = 'pending'`,
		recipient,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("counting inbox: %w", err)
	}
	return count, nil
}

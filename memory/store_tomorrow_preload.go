package memory

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// TomorrowPreload is a short note the dream cycle writes about what Mira
// should be ready to bring up in the next conversation. Single-row pattern:
// each dream cycle inserts a fresh row, the chat layer consumes it on the
// first turn of the next day, and old rows are kept for audit.
type TomorrowPreload struct {
	ID          int64
	GeneratedAt time.Time
	ExpiresAt   time.Time
	Content     string
	Consumed    bool
	ConsumedAt  *time.Time
}

// SaveTomorrowPreload inserts a new preload note. expiresAfter controls
// how long the note stays active before it's considered stale.
func (s *SQLiteStore) SaveTomorrowPreload(content string, expiresAfter time.Duration) (int64, error) {
	expiresAt := time.Now().UTC().Add(expiresAfter)
	result, err := s.db.Exec(
		`INSERT INTO tomorrow_preload (content, expires_at) VALUES (?, ?)`,
		content, expiresAt.Format("2006-01-02 15:04:05"),
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// ActiveTomorrowPreload returns the most recent unconsumed, unexpired
// preload note. Returns nil if none is active.
func (s *SQLiteStore) ActiveTomorrowPreload() (*TomorrowPreload, error) {
	var p TomorrowPreload
	var generatedAt, expiresAt string
	err := s.db.QueryRow(
		`SELECT id, generated_at, expires_at, content
		 FROM tomorrow_preload
		 WHERE consumed = 0 AND expires_at > datetime('now')
		 ORDER BY id DESC
		 LIMIT 1`,
	).Scan(&p.ID, &generatedAt, &expiresAt, &p.Content)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("querying active preload: %w", err)
	}
	p.GeneratedAt = parseTimestamp(generatedAt)
	p.ExpiresAt = parseTimestamp(expiresAt)
	return &p, nil
}

// ConsumeTomorrowPreload marks a preload as consumed so it won't be
// injected again. Called after the first chat turn of the day.
func (s *SQLiteStore) ConsumeTomorrowPreload(id int64) error {
	_, err := s.db.Exec(
		`UPDATE tomorrow_preload SET consumed = 1, consumed_at = CURRENT_TIMESTAMP WHERE id = ?`,
		id,
	)
	return err
}

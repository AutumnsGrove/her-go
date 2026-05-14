// Package memory — store_cards.go provides CRUD for the memory_cards and
// memory_log tables. Memory cards are dense, topic-based storage that
// replace the flat memories table. Each card holds a consolidated view
// of one topic (e.g. "financial", "my-identity") and is updated in-place
// as new information arrives.
//
// The memory_log is an append-only changelog — every card mutation is
// recorded with a delta description so the dream cycle (and humans)
// can trace what changed and when.
package memory

import (
	"database/sql"
	"fmt"
	"time"
)

// MemoryCard represents a single topic card in the memory system.
// Think of it like a dense index card — one per topic, continuously
// updated with the latest information.
type MemoryCard struct {
	ID        int64
	TopicSlug string
	Name      string
	Content   string
	Subject   string // "user" or "self"
	Protected bool   // seed cards can't be expired
	CreatedAt time.Time
	UpdatedAt time.Time
	Version   int
}

// MemoryLogEntry records a single change to a memory card.
// Append-only — entries are never updated or deleted.
type MemoryLogEntry struct {
	ID              int64
	CardID          int64
	Delta           string // what was added/changed
	Operation       string // "create", "update", "merge", "expire", "rewrite"
	SourceMessageID int64  // 0 for dream cycle operations
	CreatedAt       time.Time
}

// --- Store interface methods (add these to the Store interface in store.go) ---

// GetCard returns a single card by topic slug, or nil if not found.
func (s *SQLiteStore) GetCard(topicSlug string) (*MemoryCard, error) {
	row := s.db.QueryRow(`
		SELECT id, topic_slug, name, content, subject, protected,
		       created_at, updated_at, version
		FROM memory_cards WHERE topic_slug = ?`, topicSlug)

	c := &MemoryCard{}
	err := row.Scan(&c.ID, &c.TopicSlug, &c.Name, &c.Content, &c.Subject,
		&c.Protected, &c.CreatedAt, &c.UpdatedAt, &c.Version)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("GetCard(%s): %w", topicSlug, err)
	}
	return c, nil
}

// GetCardByID returns a single card by its numeric ID.
func (s *SQLiteStore) GetCardByID(id int64) (*MemoryCard, error) {
	row := s.db.QueryRow(`
		SELECT id, topic_slug, name, content, subject, protected,
		       created_at, updated_at, version
		FROM memory_cards WHERE id = ?`, id)

	c := &MemoryCard{}
	err := row.Scan(&c.ID, &c.TopicSlug, &c.Name, &c.Content, &c.Subject,
		&c.Protected, &c.CreatedAt, &c.UpdatedAt, &c.Version)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("GetCardByID(%d): %w", id, err)
	}
	return c, nil
}

// AllCards returns all active (non-expired) memory cards, ordered by subject
// then topic slug. Used by the memory agent to see the full card list and by
// the dream cycle to review everything.
func (s *SQLiteStore) AllCards() ([]MemoryCard, error) {
	rows, err := s.db.Query(`
		SELECT id, topic_slug, name, content, subject, protected,
		       created_at, updated_at, version
		FROM memory_cards
		ORDER BY subject, topic_slug`)
	if err != nil {
		return nil, fmt.Errorf("AllCards: %w", err)
	}
	defer rows.Close()

	var cards []MemoryCard
	for rows.Next() {
		var c MemoryCard
		if err := rows.Scan(&c.ID, &c.TopicSlug, &c.Name, &c.Content,
			&c.Subject, &c.Protected, &c.CreatedAt, &c.UpdatedAt, &c.Version); err != nil {
			return nil, fmt.Errorf("AllCards scan: %w", err)
		}
		cards = append(cards, c)
	}
	return cards, rows.Err()
}

// CardsBySubject returns all cards for a given subject ("user" or "self").
func (s *SQLiteStore) CardsBySubject(subject string) ([]MemoryCard, error) {
	rows, err := s.db.Query(`
		SELECT id, topic_slug, name, content, subject, protected,
		       created_at, updated_at, version
		FROM memory_cards
		WHERE subject = ?
		ORDER BY topic_slug`, subject)
	if err != nil {
		return nil, fmt.Errorf("CardsBySubject(%s): %w", subject, err)
	}
	defer rows.Close()

	var cards []MemoryCard
	for rows.Next() {
		var c MemoryCard
		if err := rows.Scan(&c.ID, &c.TopicSlug, &c.Name, &c.Content,
			&c.Subject, &c.Protected, &c.CreatedAt, &c.UpdatedAt, &c.Version); err != nil {
			return nil, fmt.Errorf("CardsBySubject scan: %w", err)
		}
		cards = append(cards, c)
	}
	return cards, rows.Err()
}

// UpdateCard rewrites a card's content in-place, increments the version,
// and appends a log entry recording the delta (what changed).
//
// Returns the updated card. Callers should re-embed the card after updating.
func (s *SQLiteStore) UpdateCard(topicSlug, newContent, delta string, sourceMessageID int64) (*MemoryCard, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("UpdateCard begin: %w", err)
	}
	defer tx.Rollback()

	// Update the card.
	res, err := tx.Exec(`
		UPDATE memory_cards
		SET content = ?, updated_at = CURRENT_TIMESTAMP, version = version + 1
		WHERE topic_slug = ?`, newContent, topicSlug)
	if err != nil {
		return nil, fmt.Errorf("UpdateCard update: %w", err)
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return nil, fmt.Errorf("UpdateCard: card %q not found", topicSlug)
	}

	// Get the card ID for the log entry.
	var cardID int64
	err = tx.QueryRow(`SELECT id FROM memory_cards WHERE topic_slug = ?`, topicSlug).Scan(&cardID)
	if err != nil {
		return nil, fmt.Errorf("UpdateCard get id: %w", err)
	}

	// Append log entry.
	_, err = tx.Exec(`
		INSERT INTO memory_log (card_id, delta, operation, source_message_id)
		VALUES (?, ?, 'update', ?)`, cardID, delta, sourceMessageID)
	if err != nil {
		return nil, fmt.Errorf("UpdateCard log: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("UpdateCard commit: %w", err)
	}

	return s.GetCard(topicSlug)
}

// CreateCard creates a new organic (unprotected) memory card and logs
// the creation. Returns the new card.
func (s *SQLiteStore) CreateCard(topicSlug, name, content, subject string, sourceMessageID int64) (*MemoryCard, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("CreateCard begin: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.Exec(`
		INSERT INTO memory_cards (topic_slug, name, content, subject, protected)
		VALUES (?, ?, ?, ?, 0)`, topicSlug, name, content, subject)
	if err != nil {
		return nil, fmt.Errorf("CreateCard insert: %w", err)
	}
	cardID, _ := res.LastInsertId()

	_, err = tx.Exec(`
		INSERT INTO memory_log (card_id, delta, operation, source_message_id)
		VALUES (?, ?, 'create', ?)`, cardID, content, sourceMessageID)
	if err != nil {
		return nil, fmt.Errorf("CreateCard log: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("CreateCard commit: %w", err)
	}

	return s.GetCardByID(cardID)
}

// ExpireCard deactivates an organic card by deleting it and logging the
// expiration. Returns an error if the card is protected.
func (s *SQLiteStore) ExpireCard(topicSlug, reason string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("ExpireCard begin: %w", err)
	}
	defer tx.Rollback()

	// Check protection.
	var cardID int64
	var protected bool
	err = tx.QueryRow(`SELECT id, protected FROM memory_cards WHERE topic_slug = ?`, topicSlug).
		Scan(&cardID, &protected)
	if err == sql.ErrNoRows {
		return fmt.Errorf("ExpireCard: card %q not found", topicSlug)
	}
	if err != nil {
		return fmt.Errorf("ExpireCard lookup: %w", err)
	}
	if protected {
		return fmt.Errorf("ExpireCard: card %q is protected and cannot be expired", topicSlug)
	}

	// Log before deleting.
	_, err = tx.Exec(`
		INSERT INTO memory_log (card_id, delta, operation)
		VALUES (?, ?, 'expire')`, cardID, reason)
	if err != nil {
		return fmt.Errorf("ExpireCard log: %w", err)
	}

	// Delete the card.
	_, err = tx.Exec(`DELETE FROM memory_cards WHERE id = ?`, cardID)
	if err != nil {
		return fmt.Errorf("ExpireCard delete: %w", err)
	}

	return tx.Commit()
}

// MergeCards combines two or more organic cards into one. The target card
// gets the merged content; source cards are expired. Returns the updated
// target card.
func (s *SQLiteStore) MergeCards(targetSlug string, sourceSlug string, mergedContent, reason string) (*MemoryCard, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("MergeCards begin: %w", err)
	}
	defer tx.Rollback()

	// Check source isn't protected.
	var sourceID int64
	var sourceProtected bool
	err = tx.QueryRow(`SELECT id, protected FROM memory_cards WHERE topic_slug = ?`, sourceSlug).
		Scan(&sourceID, &sourceProtected)
	if err != nil {
		return nil, fmt.Errorf("MergeCards source lookup: %w", err)
	}
	if sourceProtected {
		return nil, fmt.Errorf("MergeCards: source card %q is protected", sourceSlug)
	}

	// Update target content.
	var targetID int64
	err = tx.QueryRow(`SELECT id FROM memory_cards WHERE topic_slug = ?`, targetSlug).Scan(&targetID)
	if err != nil {
		return nil, fmt.Errorf("MergeCards target lookup: %w", err)
	}

	_, err = tx.Exec(`
		UPDATE memory_cards
		SET content = ?, updated_at = CURRENT_TIMESTAMP, version = version + 1
		WHERE topic_slug = ?`, mergedContent, targetSlug)
	if err != nil {
		return nil, fmt.Errorf("MergeCards update target: %w", err)
	}

	// Log merge on target.
	_, err = tx.Exec(`
		INSERT INTO memory_log (card_id, delta, operation)
		VALUES (?, ?, 'merge')`, targetID, fmt.Sprintf("merged from %s: %s", sourceSlug, reason))
	if err != nil {
		return nil, fmt.Errorf("MergeCards log target: %w", err)
	}

	// Log expire on source.
	_, err = tx.Exec(`
		INSERT INTO memory_log (card_id, delta, operation)
		VALUES (?, ?, 'expire')`, sourceID, fmt.Sprintf("merged into %s: %s", targetSlug, reason))
	if err != nil {
		return nil, fmt.Errorf("MergeCards log source: %w", err)
	}

	// Delete source.
	_, err = tx.Exec(`DELETE FROM memory_cards WHERE id = ?`, sourceID)
	if err != nil {
		return nil, fmt.Errorf("MergeCards delete source: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("MergeCards commit: %w", err)
	}

	return s.GetCard(targetSlug)
}

// RecentLogEntries returns log entries from the last `hours` hours,
// ordered by most recent first. Used by the dream cycle to see what
// changed since the last dream (typically 48h).
func (s *SQLiteStore) RecentLogEntries(hours int) ([]MemoryLogEntry, error) {
	rows, err := s.db.Query(`
		SELECT l.id, l.card_id, l.delta, l.operation, l.source_message_id, l.created_at
		FROM memory_log l
		WHERE l.created_at >= datetime('now', ? || ' hours')
		ORDER BY l.created_at DESC`, fmt.Sprintf("-%d", hours))
	if err != nil {
		return nil, fmt.Errorf("RecentLogEntries: %w", err)
	}
	defer rows.Close()

	var entries []MemoryLogEntry
	for rows.Next() {
		var e MemoryLogEntry
		var srcMsgID sql.NullInt64
		if err := rows.Scan(&e.ID, &e.CardID, &e.Delta, &e.Operation,
			&srcMsgID, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("RecentLogEntries scan: %w", err)
		}
		e.SourceMessageID = srcMsgID.Int64
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// CardLogEntries returns all log entries for a specific card, ordered
// by most recent first. Useful for viewing a card's full history.
func (s *SQLiteStore) CardLogEntries(cardID int64, limit int) ([]MemoryLogEntry, error) {
	rows, err := s.db.Query(`
		SELECT id, card_id, delta, operation, source_message_id, created_at
		FROM memory_log
		WHERE card_id = ?
		ORDER BY created_at DESC
		LIMIT ?`, cardID, limit)
	if err != nil {
		return nil, fmt.Errorf("CardLogEntries: %w", err)
	}
	defer rows.Close()

	var entries []MemoryLogEntry
	for rows.Next() {
		var e MemoryLogEntry
		var srcMsgID sql.NullInt64
		if err := rows.Scan(&e.ID, &e.CardID, &e.Delta, &e.Operation,
			&srcMsgID, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("CardLogEntries scan: %w", err)
		}
		e.SourceMessageID = srcMsgID.Int64
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

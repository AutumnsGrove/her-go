// Package memory — store_cards.go provides CRUD for the memory_cards and
// memory_log tables. Memory cards are organizational folders that group
// related individual memories by topic (e.g. "financial", "my-identity").
// Each card has a dreamer-maintained summary — the actual content lives
// in the individual memories linked via card_id.
//
// The memory_log is an append-only changelog — every card and memory
// mutation is recorded with a delta description so the dream cycle
// (and humans) can trace what changed and when.
package memory

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// MemoryCard represents a topic folder in the memory system.
// Each card groups related individual memories by topic. The Summary
// field is a brief dreamer-maintained overview — the actual knowledge
// lives in the Memory rows linked via card_id.
type MemoryCard struct {
	ID        int64
	TopicSlug string
	Name      string
	Summary   string
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
		SELECT id, topic_slug, name, summary, subject, protected,
		       created_at, updated_at, version
		FROM memory_cards WHERE topic_slug = ?`, topicSlug)

	c := &MemoryCard{}
	err := row.Scan(&c.ID, &c.TopicSlug, &c.Name, &c.Summary, &c.Subject,
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
		SELECT id, topic_slug, name, summary, subject, protected,
		       created_at, updated_at, version
		FROM memory_cards WHERE id = ?`, id)

	c := &MemoryCard{}
	err := row.Scan(&c.ID, &c.TopicSlug, &c.Name, &c.Summary, &c.Subject,
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
		SELECT id, topic_slug, name, summary, subject, protected,
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
		if err := rows.Scan(&c.ID, &c.TopicSlug, &c.Name, &c.Summary,
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
		SELECT id, topic_slug, name, summary, subject, protected,
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
		if err := rows.Scan(&c.ID, &c.TopicSlug, &c.Name, &c.Summary,
			&c.Subject, &c.Protected, &c.CreatedAt, &c.UpdatedAt, &c.Version); err != nil {
			return nil, fmt.Errorf("CardsBySubject scan: %w", err)
		}
		cards = append(cards, c)
	}
	return cards, rows.Err()
}

// UpdateCardSummary rewrites a card's dreamer-maintained summary, increments
// the version, and appends a log entry recording the delta.
//
// This is a dreamer-only operation — the real-time memory agent never
// calls this. Individual memories are the source of truth; the summary
// is a distillation maintained by the dream cycle.
func (s *SQLiteStore) UpdateCardSummary(topicSlug, newSummary, delta string, sourceMessageID int64) (*MemoryCard, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("UpdateCardSummary begin: %w", err)
	}
	defer tx.Rollback()

	// Update the card summary.
	res, err := tx.Exec(`
		UPDATE memory_cards
		SET summary = ?, updated_at = CURRENT_TIMESTAMP, version = version + 1
		WHERE topic_slug = ?`, newSummary, topicSlug)
	if err != nil {
		return nil, fmt.Errorf("UpdateCardSummary update: %w", err)
	}
	// SQLite's driver never errors on RowsAffected, but check for correctness.
	affected, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("UpdateCardSummary rows affected: %w", err)
	}
	if affected == 0 {
		return nil, fmt.Errorf("UpdateCardSummary: card %q not found", topicSlug)
	}

	// Get the card ID for the log entry.
	var cardID int64
	err = tx.QueryRow(`SELECT id FROM memory_cards WHERE topic_slug = ?`, topicSlug).Scan(&cardID)
	if err != nil {
		return nil, fmt.Errorf("UpdateCardSummary get id: %w", err)
	}

	// Append log entry.
	_, err = tx.Exec(`
		INSERT INTO memory_log (card_id, delta, operation, source_message_id)
		VALUES (?, ?, 'update', ?)`, cardID, delta, sourceMessageID)
	if err != nil {
		return nil, fmt.Errorf("UpdateCardSummary log: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("UpdateCardSummary commit: %w", err)
	}

	return s.GetCard(topicSlug)
}

// CreateCard creates a new organic (unprotected) memory card and logs
// the creation. Returns the new card.
func (s *SQLiteStore) CreateCard(topicSlug, name, subject string, sourceMessageID int64) (*MemoryCard, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("CreateCard begin: %w", err)
	}
	defer tx.Rollback()

	// New cards start with an empty summary — the dream cycle populates it.
	res, err := tx.Exec(`
		INSERT INTO memory_cards (topic_slug, name, summary, subject, protected)
		VALUES (?, ?, '', ?, 0)`, topicSlug, name, subject)
	if err != nil {
		return nil, fmt.Errorf("CreateCard insert: %w", err)
	}
	cardID, _ := res.LastInsertId()

	_, err = tx.Exec(`
		INSERT INTO memory_log (card_id, delta, operation, source_message_id)
		VALUES (?, ?, 'create', ?)`, cardID, fmt.Sprintf("created card: %s", name), sourceMessageID)
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

// MergeCards combines two organic cards into one. All memories from the
// source card are moved to the target card. The target card's summary is
// updated and the source card is deleted. Returns the updated target card.
func (s *SQLiteStore) MergeCards(targetSlug, sourceSlug, mergedSummary, reason string) (*MemoryCard, error) {
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

	// Look up target.
	var targetID int64
	err = tx.QueryRow(`SELECT id FROM memory_cards WHERE topic_slug = ?`, targetSlug).Scan(&targetID)
	if err != nil {
		return nil, fmt.Errorf("MergeCards target lookup: %w", err)
	}

	// Move all memories from source card to target card.
	_, err = tx.Exec(`UPDATE memories SET card_id = ? WHERE card_id = ?`, targetID, sourceID)
	if err != nil {
		return nil, fmt.Errorf("MergeCards move memories: %w", err)
	}

	// Update target summary.
	_, err = tx.Exec(`
		UPDATE memory_cards
		SET summary = ?, updated_at = CURRENT_TIMESTAMP, version = version + 1
		WHERE topic_slug = ?`, mergedSummary, targetSlug)
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

	// Delete source card.
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

// MemoryCardForMemory looks up which card a memory belongs to. Returns nil
// if the memory has no card_id or the card doesn't exist. Used by the
// forgetting guard to check whether a memory is in a protected card.
func (s *SQLiteStore) MemoryCardForMemory(memoryID int64) (*MemoryCard, error) {
	var c MemoryCard
	var updatedAt, summary sql.NullString
	err := s.db.QueryRow(`
		SELECT c.id, c.topic_slug, c.name, c.subject, c.summary, c.version, c.protected, c.updated_at
		FROM memory_cards c
		JOIN memories m ON m.card_id = c.id
		WHERE m.id = ?`, memoryID,
	).Scan(&c.ID, &c.TopicSlug, &c.Name, &c.Subject, &summary, &c.Version, &c.Protected, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("MemoryCardForMemory(%d): %w", memoryID, err)
	}
	if summary.Valid {
		c.Summary = summary.String
	}
	if updatedAt.Valid {
		c.UpdatedAt = parseTimestamp(updatedAt.String)
	}
	return &c, nil
}

// MemoriesByCard returns all active memories belonging to a specific card,
// ordered by most recent first. Used by the dream cycle to see a card's
// children, and by recall_memories for card-scoped search.
func (s *SQLiteStore) MemoriesByCard(cardID int64) ([]Memory, error) {
	rows, err := s.db.Query(`
		SELECT id, timestamp, memory, category, source_message_id,
		       importance, subject, tags, context
		FROM memories
		WHERE card_id = ? AND active = 1
		ORDER BY id DESC`, cardID)
	if err != nil {
		return nil, fmt.Errorf("MemoriesByCard(%d): %w", cardID, err)
	}
	defer rows.Close()

	var mems []Memory
	for rows.Next() {
		var m Memory
		var srcMsg sql.NullInt64
		var tags, ctx sql.NullString
		if err := rows.Scan(&m.ID, &m.Timestamp, &m.Content, &m.Category,
			&srcMsg, &m.Importance, &m.Subject, &tags, &ctx); err != nil {
			return nil, fmt.Errorf("MemoriesByCard scan: %w", err)
		}
		m.SourceMessageID = srcMsg.Int64
		m.Tags = tags.String
		m.Context = ctx.String
		mems = append(mems, m)
	}
	return mems, rows.Err()
}

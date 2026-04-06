package memory

import (
	"database/sql"
	"fmt"
	"time"
)

// --- Mood Tracking ---

// MoodEntry represents a single mood data point.
type MoodEntry struct {
	ID             int64
	Timestamp      time.Time
	Rating         int    // 1-5 scale: 1=bad, 2=rough, 3=meh, 4=good, 5=great
	Note           string // optional free-text context
	Tags           string // JSON: energy, stress, social context, etc.
	Source         string // "inferred", "manual", "checkin"
	ConversationID string
}

// RecentMoodNotes returns the notes from mood entries logged in the last
// `minutes` minutes. Used by the agent to check for semantic duplicates
// before logging a new mood — similar to how save_fact checks existing
// facts before inserting.
func (s *Store) RecentMoodNotes(minutes int) ([]string, error) {
	rows, err := s.db.Query(
		`SELECT COALESCE(note, '') FROM mood_entries
		 WHERE timestamp > datetime('now', ? || ' minutes')
		 ORDER BY timestamp DESC`,
		fmt.Sprintf("-%d", minutes),
	)
	if err != nil {
		return nil, fmt.Errorf("querying recent mood notes: %w", err)
	}
	defer rows.Close()

	var notes []string
	for rows.Next() {
		var note string
		if err := rows.Scan(&note); err != nil {
			return nil, fmt.Errorf("scanning mood note: %w", err)
		}
		if note != "" {
			notes = append(notes, note)
		}
	}
	return notes, nil
}

// SaveMoodEntry logs a mood data point. Source indicates where it came from:
// "inferred" = LLM guessed from conversation, "manual" = agent tool,
// "checkin" = proactive inline keyboard (v0.6).
func (s *Store) SaveMoodEntry(rating int, note, tags, source, conversationID string) (int64, error) {
	if rating < 1 {
		rating = 1
	}
	if rating > 5 {
		rating = 5
	}
	if source == "" {
		source = "inferred"
	}

	result, err := s.db.Exec(
		`INSERT INTO mood_entries (rating, note, tags, source, conversation_id)
		 VALUES (?, ?, ?, ?, ?)`,
		rating, note, tags, source, conversationID,
	)
	if err != nil {
		return 0, fmt.Errorf("saving mood entry: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("getting mood entry ID: %w", err)
	}
	return id, nil
}

// UpdateMoodEntry overwrites the rating and note on an existing mood entry.
// Used when the user's mood shifts within the dedup window — instead of
// being blocked by the time gate, the agent updates the existing entry.
func (s *Store) UpdateMoodEntry(id int64, rating int, note string) error {
	if rating < 1 {
		rating = 1
	}
	if rating > 5 {
		rating = 5
	}

	result, err := s.db.Exec(
		`UPDATE mood_entries SET rating = ?, note = ? WHERE id = ?`,
		rating, note, id,
	)
	if err != nil {
		return fmt.Errorf("updating mood entry: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking mood update: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("mood entry %d not found", id)
	}
	return nil
}

// LatestMoodEntry returns the most recent mood entry, or nil if none exist.
// Used by update_mood to find the entry to modify.
func (s *Store) LatestMoodEntry() (*MoodEntry, error) {
	var e MoodEntry
	var ts string
	err := s.db.QueryRow(
		`SELECT id, timestamp, rating, COALESCE(note, ''), COALESCE(tags, ''),
		        COALESCE(source, 'inferred'), COALESCE(conversation_id, '')
		 FROM mood_entries
		 ORDER BY timestamp DESC
		 LIMIT 1`,
	).Scan(&e.ID, &ts, &e.Rating, &e.Note, &e.Tags, &e.Source, &e.ConversationID)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("querying latest mood entry: %w", err)
	}
	e.Timestamp, _ = time.Parse("2006-01-02 15:04:05", ts)
	return &e, nil
}

// RecentMoodEntries returns the last N mood entries, newest first.
// Used to build mood trend context for the system prompt.
func (s *Store) RecentMoodEntries(limit int) ([]MoodEntry, error) {
	rows, err := s.db.Query(
		`SELECT id, timestamp, rating, COALESCE(note, ''), COALESCE(tags, ''),
		        COALESCE(source, 'inferred'), COALESCE(conversation_id, '')
		 FROM mood_entries
		 ORDER BY timestamp DESC
		 LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("querying mood entries: %w", err)
	}
	defer rows.Close()

	var entries []MoodEntry
	for rows.Next() {
		var e MoodEntry
		var ts string
		if err := rows.Scan(&e.ID, &ts, &e.Rating, &e.Note, &e.Tags, &e.Source, &e.ConversationID); err != nil {
			return nil, fmt.Errorf("scanning mood entry: %w", err)
		}
		e.Timestamp, _ = time.Parse("2006-01-02 15:04:05", ts)
		entries = append(entries, e)
	}
	return entries, nil
}

// MoodTrend returns the average mood rating over the last N entries.
// Returns 0.0 if no entries exist.
func (s *Store) MoodTrend(limit int) (float64, int, error) {
	var avg sql.NullFloat64
	var count int
	err := s.db.QueryRow(
		`SELECT AVG(rating), COUNT(*) FROM (
			SELECT rating FROM mood_entries ORDER BY timestamp DESC LIMIT ?
		)`, limit,
	).Scan(&avg, &count)
	if err != nil {
		return 0, 0, fmt.Errorf("calculating mood trend: %w", err)
	}
	if avg.Valid {
		return avg.Float64, count, nil
	}
	return 0, 0, nil
}

package memory

import (
	"fmt"
	"time"
)

// SavePersonaVersion stores a snapshot of persona.md content in the
// persona_versions table. Every rewrite is preserved for history/rollback.
// PersonaVersion represents one historical snapshot of persona.md.
type PersonaVersion struct {
	ID        int64
	Timestamp time.Time
	Content   string
	Trigger   string
}

// PersonaHistory returns the most recent N persona versions, newest first.
func (s *SQLiteStore) PersonaHistory(limit int) ([]PersonaVersion, error) {
	rows, err := s.db.Query(
		`SELECT id, timestamp, content, COALESCE(trigger, '') FROM persona_versions
		 ORDER BY id DESC LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("querying persona history: %w", err)
	}
	defer rows.Close()

	var versions []PersonaVersion
	for rows.Next() {
		var v PersonaVersion
		var ts string
		if err := rows.Scan(&v.ID, &ts, &v.Content, &v.Trigger); err != nil {
			return nil, fmt.Errorf("scanning persona version: %w", err)
		}
		v.Timestamp, _ = time.Parse("2006-01-02 15:04:05", ts)
		versions = append(versions, v)
	}
	return versions, nil
}

func (s *SQLiteStore) SavePersonaVersion(content, trigger string) (int64, error) {
	result, err := s.db.Exec(
		`INSERT INTO persona_versions (content, trigger) VALUES (?, ?)`,
		content, trigger,
	)
	if err != nil {
		return 0, fmt.Errorf("saving persona version: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("getting persona version ID: %w", err)
	}
	return id, nil
}

// SaveReflection stores a new reflection entry in the dedicated reflections
// table. Called by persona.Reflect() after a memory-dense conversation.
func (s *SQLiteStore) SaveReflection(content string, factCount int, userMessage, miraResponse string) (int64, error) {
	result, err := s.db.Exec(
		`INSERT INTO reflections (content, fact_count, user_message, mira_response) VALUES (?, ?, ?, ?)`,
		content, factCount, userMessage, miraResponse,
	)
	if err != nil {
		return 0, fmt.Errorf("saving reflection: %w", err)
	}
	return result.LastInsertId()
}

// FactCountSinceLastReflection counts how many facts have been saved
// since the most recent reflection. Used to trigger reflections based
// on accumulated new knowledge rather than per-turn counts.
// Now queries the reflections table directly instead of filtering facts.
func (s *SQLiteStore) FactCountSinceLastReflection() (int, error) {
	var lastReflectionTime string
	err := s.db.QueryRow(
		`SELECT timestamp FROM reflections ORDER BY id DESC LIMIT 1`,
	).Scan(&lastReflectionTime)

	var count int
	if err != nil {
		// No reflections yet. Count all active facts.
		s.db.QueryRow(`SELECT COUNT(*) FROM facts WHERE active = 1`).Scan(&count)
	} else {
		// Count facts created after the last reflection.
		s.db.QueryRow(
			`SELECT COUNT(*) FROM facts WHERE active = 1 AND timestamp > ?`,
			lastReflectionTime,
		).Scan(&count)
	}
	return count, nil
}

// TotalReflectionCount returns the total number of reflections stored.
// Used alongside PersonaRewriteCount to decide if a rewrite is due.
func (s *SQLiteStore) TotalReflectionCount() (int, error) {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM reflections`).Scan(&count)
	return count, err
}

// PersonaRewriteCount returns how many persona rewrites have occurred.
func (s *SQLiteStore) PersonaRewriteCount() (int, error) {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM persona_versions`).Scan(&count)
	return count, err
}

// LastPersonaTimestamp returns the timestamp of the most recent persona
// version. Returns zero time if no versions exist yet.
func (s *SQLiteStore) LastPersonaTimestamp() (time.Time, error) {
	var ts string
	err := s.db.QueryRow(
		`SELECT timestamp FROM persona_versions ORDER BY id DESC LIMIT 1`,
	).Scan(&ts)
	if err != nil {
		return time.Time{}, nil // no versions yet, return zero time
	}
	t, _ := time.Parse("2006-01-02 15:04:05", ts)
	return t, nil
}

// ReflectionsSince returns all reflections created after the given timestamp.
// The return type is []Reflection — the dedicated struct, not Fact.
func (s *SQLiteStore) ReflectionsSince(since time.Time) ([]Reflection, error) {
	rows, err := s.db.Query(
		`SELECT id, timestamp, content FROM reflections
		 WHERE timestamp > ?
		 ORDER BY timestamp ASC`,
		since.Format("2006-01-02 15:04:05"),
	)
	if err != nil {
		return nil, fmt.Errorf("querying reflections: %w", err)
	}
	defer rows.Close()

	var reflections []Reflection
	for rows.Next() {
		var r Reflection
		var ts string
		if err := rows.Scan(&r.ID, &ts, &r.Content); err != nil {
			return nil, fmt.Errorf("scanning reflection: %w", err)
		}
		r.Timestamp, _ = time.Parse("2006-01-02 15:04:05", ts)
		reflections = append(reflections, r)
	}
	return reflections, nil
}

// --- Trait Tracking ---

// Trait represents a single personality trait score, linked to a
// persona version. Numeric traits (warmth, directness, etc.) store
// float values as strings. Categorical traits (humor_style) store
// the category label directly.
type Trait struct {
	ID               int64
	TraitName        string
	Value            string // "0.72" for numeric, "dry" for categorical
	PersonaVersionID int64
	Timestamp        time.Time
}

// SaveTraits bulk-inserts trait scores for a persona version.
// Called after a persona rewrite to snapshot the current trait state.
func (s *SQLiteStore) SaveTraits(traits []Trait, personaVersionID int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("starting trait transaction: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(
		`INSERT INTO traits (trait_name, value, persona_version_id) VALUES (?, ?, ?)`,
	)
	if err != nil {
		return fmt.Errorf("preparing trait insert: %w", err)
	}
	defer stmt.Close()

	for _, t := range traits {
		if _, err := stmt.Exec(t.TraitName, t.Value, personaVersionID); err != nil {
			return fmt.Errorf("inserting trait %s: %w", t.TraitName, err)
		}
	}

	return tx.Commit()
}

// GetCurrentTraits returns the trait scores from the most recent
// persona version. Returns nil (not an error) if no traits exist yet.
func (s *SQLiteStore) GetCurrentTraits() ([]Trait, error) {
	rows, err := s.db.Query(
		`SELECT t.id, t.trait_name, t.value, t.persona_version_id, t.timestamp
		 FROM traits t
		 INNER JOIN persona_versions pv ON t.persona_version_id = pv.id
		 WHERE pv.id = (SELECT MAX(id) FROM persona_versions)
		 ORDER BY t.trait_name`,
	)
	if err != nil {
		return nil, fmt.Errorf("querying current traits: %w", err)
	}
	defer rows.Close()

	var traits []Trait
	for rows.Next() {
		var t Trait
		var ts string
		if err := rows.Scan(&t.ID, &t.TraitName, &t.Value, &t.PersonaVersionID, &ts); err != nil {
			return nil, fmt.Errorf("scanning trait: %w", err)
		}
		t.Timestamp, _ = time.Parse("2006-01-02 15:04:05", ts)
		traits = append(traits, t)
	}
	return traits, nil
}

// --- Dreaming State ---

// PersonaState holds the dreaming system's timing state. Read at startup
// to check if a catch-up dream is needed; updated after each reflection
// and rewrite so the gates work correctly across restarts.
type PersonaState struct {
	LastReflectionAt time.Time // zero = never reflected
	LastRewriteAt    time.Time // zero = never rewritten
}

// GetPersonaState returns the current dreaming state. Returns a zero-value
// PersonaState (both times zero) if the persona_state row doesn't exist yet
// — i.e., on a fresh install before the first dream runs.
func (s *SQLiteStore) GetPersonaState() (PersonaState, error) {
	var lastReflStr, lastRewriteStr string
	err := s.db.QueryRow(
		`SELECT COALESCE(last_reflection_at, ''), COALESCE(last_rewrite_at, '') FROM persona_state WHERE id = 1`,
	).Scan(&lastReflStr, &lastRewriteStr)
	if err != nil {
		// No row yet — return zero state.
		return PersonaState{}, nil
	}

	var state PersonaState
	if lastReflStr != "" {
		state.LastReflectionAt, _ = time.Parse("2006-01-02 15:04:05", lastReflStr)
	}
	if lastRewriteStr != "" {
		state.LastRewriteAt, _ = time.Parse("2006-01-02 15:04:05", lastRewriteStr)
	}
	return state, nil
}

// SetLastReflectionAt records when the most recent nightly reflection ran.
// Uses INSERT OR REPLACE to upsert the single row while preserving the
// last_rewrite_at column — SQLite's INSERT OR REPLACE deletes then inserts,
// so we must COALESCE to carry the existing value forward.
func (s *SQLiteStore) SetLastReflectionAt(t time.Time) error {
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO persona_state (id, last_reflection_at, last_rewrite_at)
		 VALUES (1, ?, (SELECT last_rewrite_at FROM persona_state WHERE id = 1))`,
		t.Format("2006-01-02 15:04:05"),
	)
	return err
}

// SetLastRewriteAt records when the most recent persona rewrite ran.
// Same COALESCE trick as SetLastReflectionAt to preserve last_reflection_at.
func (s *SQLiteStore) SetLastRewriteAt(t time.Time) error {
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO persona_state (id, last_reflection_at, last_rewrite_at)
		 VALUES (1, (SELECT last_reflection_at FROM persona_state WHERE id = 1), ?)`,
		t.Format("2006-01-02 15:04:05"),
	)
	return err
}

// UnconsumedReflectionCount returns how many reflections have been saved
// since the last persona rewrite. The dreaming gate requires at least N
// unconsumed reflections before a rewrite is allowed.
// If no rewrite has ever happened, counts ALL reflections.
func (s *SQLiteStore) UnconsumedReflectionCount() (int, error) {
	var count int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM reflections
		 WHERE timestamp > COALESCE(
		   (SELECT last_rewrite_at FROM persona_state WHERE id = 1),
		   '1970-01-01 00:00:00'
		 )`,
	).Scan(&count)
	return count, err
}

// GetTraitHistory returns historical values for a single trait across
// persona versions, newest first. Useful for showing how a trait has
// drifted over time.
func (s *SQLiteStore) GetTraitHistory(traitName string, limit int) ([]Trait, error) {
	rows, err := s.db.Query(
		`SELECT t.id, t.trait_name, t.value, t.persona_version_id, t.timestamp
		 FROM traits t
		 ORDER BY t.persona_version_id DESC
		 LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("querying trait history: %w", err)
	}
	defer rows.Close()

	var traits []Trait
	for rows.Next() {
		var t Trait
		var ts string
		if err := rows.Scan(&t.ID, &t.TraitName, &t.Value, &t.PersonaVersionID, &ts); err != nil {
			return nil, fmt.Errorf("scanning trait history: %w", err)
		}
		t.Timestamp, _ = time.Parse("2006-01-02 15:04:05", ts)
		traits = append(traits, t)
	}
	return traits, nil
}

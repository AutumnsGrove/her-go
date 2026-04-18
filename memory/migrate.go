package memory

import "fmt"

// MigrateFromLegacyFacts copies data from the old `facts`/`fact_links`/`vec_facts`
// tables into the new `memories`/`memory_links`/`vec_memories` tables.
//
// This is a one-shot operation — run it once after upgrading from the schema
// that used "facts" as the primary table. It is safe to run multiple times:
// INSERT OR IGNORE skips any rows that already exist in the destination.
//
// Returns the number of memories and links copied, and any error.
func (s *Store) MigrateFromLegacyFacts() (memoriesCopied int, linksCopied int, err error) {
	// Check that the legacy facts table actually exists before trying to copy.
	var name string
	row := s.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='facts'`)
	if err := row.Scan(&name); err != nil {
		return 0, 0, fmt.Errorf("legacy facts table not found — nothing to migrate")
	}

	// Copy memories.
	res, err := s.db.Exec(`
		INSERT OR IGNORE INTO memories
		SELECT id, timestamp, fact, category, source_message_id, importance,
		       active, subject, embedding, tags, embedding_text,
		       superseded_by, supersede_reason, context
		FROM facts
	`)
	if err != nil {
		return 0, 0, fmt.Errorf("copying facts → memories: %w", err)
	}
	rows, _ := res.RowsAffected()
	memoriesCopied = int(rows)

	// Copy memory links if fact_links exists.
	var linksTable string
	row = s.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='fact_links'`)
	if row.Scan(&linksTable) == nil {
		res, err = s.db.Exec(`
			INSERT OR IGNORE INTO memory_links
			SELECT source_id, target_id, similarity, created_at
			FROM fact_links
		`)
		if err != nil {
			return memoriesCopied, 0, fmt.Errorf("copying fact_links → memory_links: %w", err)
		}
		rows, _ = res.RowsAffected()
		linksCopied = int(rows)
	}

	// vec_memories: sqlite-vec virtual tables don't reliably support SELECT *.
	// Skip the copy — the `her run` startup backfill re-embeds any memories
	// that are missing from vec_memories automatically.

	return memoriesCopied, linksCopied, nil
}

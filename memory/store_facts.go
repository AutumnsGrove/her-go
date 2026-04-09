package memory

import (
	"database/sql"
	"fmt"
	"math"
	"time"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
)

// Fact represents an extracted piece of long-term memory.
// Subject is "user" for facts about the user, or "self" for Mira's
// own self-knowledge (her identity, observations, patterns).
type Fact struct {
	ID              int64
	Timestamp       time.Time
	Fact            string
	Category        string
	Subject         string // "user" or "self"
	SourceMessageID int64
	Importance      int
	Active          bool
	Tags            string    // comma-separated topic descriptors for semantic search
	Context         string    // optional note explaining WHY this fact matters (max 500 chars)
	Embedding       []float32 // cached tag embedding vector (nil if not yet computed)
	EmbeddingText   []float32 // cached text embedding vector (nil if not yet computed)
	Distance        float64   // populated by SemanticSearch — cosine distance from query (0 = identical)

	// Zettelkasten fields — knowledge graph edges and supersession tracking.
	SupersededBy    int64  // ID of the fact that replaced this one (0 = not superseded)
	SupersedeReason string // why this fact was replaced (e.g. "job changed")
	Source          string // populated during retrieval: "semantic", "importance", or "linked"
}

// Reflection represents a journal-like entry Mira writes after a
// memory-dense conversation. Separate from facts — reflections are
// private processing, not discrete pieces of information.
type Reflection struct {
	ID           int64
	Timestamp    time.Time
	Content      string
	FactCount    int
	UserMessage  string
	MiraResponse string
}

// serializeEmbedding converts a float32 slice to the binary format sqlite-vec
// expects. This is a thin wrapper around sqlite_vec.SerializeFloat32 which
// produces a little-endian packed byte array (4 bytes per float).
//
// Like numpy's .tobytes() or Python's struct.pack('<768f', *vec).
func serializeEmbedding(vec []float32) ([]byte, error) {
	if len(vec) == 0 {
		return nil, nil
	}
	return sqlite_vec.SerializeFloat32(vec)
}

// deserializeEmbedding converts bytes from the facts.embedding BLOB column
// back into a float32 slice. This reads the new float32 format (4 bytes/float)
// used by sqlite-vec. Legacy float64 BLOBs (8 bytes/float) from before the
// migration will have the wrong dimension and return nil — those facts need
// re-embedding via BackfillEmbeddings.
func deserializeEmbedding(data []byte) []float32 {
	if len(data) == 0 || len(data)%4 != 0 {
		return nil
	}
	// sqlite-vec's SerializeFloat32 produces little-endian float32 bytes.
	// We reverse it with math.Float32frombits.
	vec := make([]float32, len(data)/4)
	for i := range vec {
		off := i * 4
		bits := uint32(data[off]) | uint32(data[off+1])<<8 | uint32(data[off+2])<<16 | uint32(data[off+3])<<24
		vec[i] = math.Float32frombits(bits)
	}
	return vec
}

// SaveFact inserts an extracted fact into the database and its embedding
// into the vec_facts virtual table for KNN search.
// subject is "user" or "self". If sourceMessageID is 0, it's stored as NULL.
// embedding is the tag-based vector (used for KNN search via vec_facts).
// embeddingText is the raw-text vector (used for dedup and redundancy filtering).
// Both are optional — pass nil if not yet computed.
func (s *Store) SaveFact(fact, category, subject string, sourceMessageID int64, importance int, embedding []float32, embeddingText []float32, tags string, context string) (int64, error) {
	var srcID interface{} = sourceMessageID
	if sourceMessageID == 0 {
		srcID = nil
	}
	if subject == "" {
		subject = "user"
	}

	// Serialize the tag embedding to bytes for the BLOB column on the facts table.
	// This is the "source of truth" copy — vec_facts is the searchable index.
	var embBlob interface{}
	if len(embedding) > 0 {
		b, err := serializeEmbedding(embedding)
		if err != nil {
			return 0, fmt.Errorf("serializing embedding: %w", err)
		}
		embBlob = b
	}

	// Serialize the text embedding separately. This is only stored on the facts
	// table (not vec_facts) — it's used for dedup checks, not KNN search.
	var embTextBlob interface{}
	if len(embeddingText) > 0 {
		b, err := serializeEmbedding(embeddingText)
		if err != nil {
			return 0, fmt.Errorf("serializing text embedding: %w", err)
		}
		embTextBlob = b
	}

	// Normalize empty context to nil so it stores as NULL, not "".
	var ctxVal interface{}
	if context != "" {
		ctxVal = context
	}

	result, err := s.db.Exec(
		`INSERT INTO facts (fact, category, subject, source_message_id, importance, embedding, embedding_text, tags, context)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		fact, category, subject, srcID, importance, embBlob, embTextBlob, tags, ctxVal,
	)
	if err != nil {
		return 0, fmt.Errorf("saving fact: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("getting fact ID: %w", err)
	}

	// Also insert into the vec_facts virtual table so this fact is
	// searchable via KNN. The rowid matches facts.id for easy JOINs.
	if len(embedding) > 0 && s.EmbedDimension > 0 {
		vecBytes, err := serializeEmbedding(embedding)
		if err != nil {
			return id, nil // fact saved, vector index failed — non-fatal
		}
		if _, err := s.db.Exec(
			`INSERT INTO vec_facts(rowid, embedding) VALUES (?, ?)`,
			id, vecBytes,
		); err != nil {
			// Log but don't fail — the fact is saved, we just can't search it yet.
			// This handles the case where vec_facts doesn't exist (dimension=0).
			fmt.Printf("[memory] warning: vec_facts insert failed for fact %d: %v\n", id, err)
		}

		// Zettelkasten auto-linking: connect this fact to its nearest neighbors
		// in embedding space. Non-fatal — the fact is saved regardless.
		if s.AutoLinkCount > 0 {
			if err := s.AutoLinkFact(id, embedding); err != nil {
				log.Warn("auto-link failed", "fact_id", id, "err", err)
			}
		}
	}

	return id, nil
}

// UpdateFactEmbedding sets the cached embeddings for a fact and updates
// the vec_facts index. embedding is the tag-based vector for KNN search.
// embeddingText is the raw-text vector for dedup checks; pass nil to leave
// it unchanged (the SQL still writes NULL, so pass existing.EmbeddingText
// when you don't want to clear it).
func (s *Store) UpdateFactEmbedding(factID int64, embedding []float32, embeddingText []float32) error {
	vecBytes, err := serializeEmbedding(embedding)
	if err != nil {
		return fmt.Errorf("serializing embedding for fact %d: %w", factID, err)
	}

	// Serialize the text embedding; nil produces a nil byte slice which
	// SQLite stores as NULL — that's intentional for facts without a
	// text embedding yet.
	var textVecBytes interface{}
	if len(embeddingText) > 0 {
		b, err := serializeEmbedding(embeddingText)
		if err != nil {
			return fmt.Errorf("serializing text embedding for fact %d: %w", factID, err)
		}
		textVecBytes = b
	}

	// Update both BLOB columns on the facts table in one round-trip.
	if _, err := s.db.Exec(
		`UPDATE facts SET embedding = ?, embedding_text = ? WHERE id = ?`,
		vecBytes, textVecBytes, factID,
	); err != nil {
		return fmt.Errorf("updating embedding for fact %d: %w", factID, err)
	}

	// Upsert into vec_facts — DELETE + INSERT because vec0 virtual tables
	// don't support UPDATE. This is idempotent: if the row doesn't exist
	// yet (new backfill), the DELETE is a no-op.
	if s.EmbedDimension > 0 {
		s.db.Exec(`DELETE FROM vec_facts WHERE rowid = ?`, factID)
		if _, err := s.db.Exec(
			`INSERT INTO vec_facts(rowid, embedding) VALUES (?, ?)`,
			factID, vecBytes,
		); err != nil {
			return fmt.Errorf("updating vec_facts for fact %d: %w", factID, err)
		}
	}

	return nil
}

// RecentFacts retrieves the top-K active facts for a given subject,
// ordered by recency (descending).
func (s *Store) RecentFacts(subject string, limit int) ([]Fact, error) {
	rows, err := s.db.Query(
		`SELECT id, timestamp, fact, category, COALESCE(subject, 'user'), importance, COALESCE(tags, ''), embedding, embedding_text
		 FROM facts
		 WHERE active = 1 AND COALESCE(subject, 'user') = ?
		 ORDER BY timestamp DESC
		 LIMIT ?`,
		subject, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("querying facts: %w", err)
	}
	defer rows.Close()

	var facts []Fact
	for rows.Next() {
		var f Fact
		var ts string
		var embData []byte
		var embTextData []byte
		if err := rows.Scan(&f.ID, &ts, &f.Fact, &f.Category, &f.Subject, &f.Importance, &f.Tags, &embData, &embTextData); err != nil {
			return nil, fmt.Errorf("scanning fact row: %w", err)
		}
		f.Timestamp, _ = time.Parse("2006-01-02 15:04:05", ts)
		f.Active = true
		f.Embedding = deserializeEmbedding(embData)
		f.EmbeddingText = deserializeEmbedding(embTextData)
		facts = append(facts, f)
	}
	return facts, nil
}

// GetFactText returns the current text of a fact by ID. Returns an empty
// string and no error if the fact doesn't exist (soft-deleted or never
// created). Used by update_fact to show the classifier both the old and
// new text so it can evaluate what actually changed — without this, the
// classifier only sees the final text and can't catch inferred additions.
func (s *Store) GetFactText(factID int64) (string, error) {
	var text string
	err := s.db.QueryRow(`SELECT fact FROM facts WHERE id = ?`, factID).Scan(&text)
	if err != nil {
		return "", nil // fact not found — not an error, just empty
	}
	return text, nil
}

// UpdateFact modifies an existing fact's text, category, or importance.
func (s *Store) UpdateFact(factID int64, fact, category string, importance int, tags string) error {
	_, err := s.db.Exec(
		`UPDATE facts SET fact = ?, category = ?, importance = ?, tags = ? WHERE id = ?`,
		fact, category, importance, tags, factID,
	)
	if err != nil {
		return fmt.Errorf("updating fact %d: %w", factID, err)
	}
	return nil
}

// UpdateFactTags sets the topic tags for a fact without changing anything else.
// Used by `her retag` to backfill tags for existing facts.
func (s *Store) UpdateFactTags(factID int64, tags string) error {
	_, err := s.db.Exec(`UPDATE facts SET tags = ? WHERE id = ?`, tags, factID)
	return err
}

// DeactivateFact soft-deletes a fact by setting active = 0 and removing
// it from the vec_facts index. The fact stays in the DB for audit trail
// but won't appear in retrieval or vector search.
func (s *Store) DeactivateFact(factID int64) error {
	_, err := s.db.Exec(
		`UPDATE facts SET active = 0 WHERE id = ?`,
		factID,
	)
	if err != nil {
		return fmt.Errorf("deactivating fact %d: %w", factID, err)
	}
	// Remove from vec_facts so deactivated facts don't pollute KNN results.
	if s.EmbedDimension > 0 {
		s.db.Exec(`DELETE FROM vec_facts WHERE rowid = ?`, factID)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Zettelkasten: fact linking + supersession
// ---------------------------------------------------------------------------

// LinkFacts creates a bidirectional link between two facts with a similarity
// score. IDs are normalized (min, max) so the same pair can't be stored twice
// in different order — same trick social graph databases use for friendships.
//
// INSERT OR IGNORE means calling this with an already-linked pair is a no-op.
func (s *Store) LinkFacts(id1, id2 int64, similarity float64) error {
	// Normalize: always store (smaller ID, larger ID).
	// This is like sorting a tuple in Python: min/max guarantees one
	// canonical order regardless of which direction the link was found.
	source, target := id1, id2
	if id1 > id2 {
		source, target = id2, id1
	}
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO fact_links (source_id, target_id, similarity) VALUES (?, ?, ?)`,
		source, target, similarity,
	)
	if err != nil {
		return fmt.Errorf("linking facts %d↔%d: %w", source, target, err)
	}
	return nil
}

// LinkedFacts returns active facts linked to the given fact (1-hop traversal).
// Because links are normalized (source < target), we need to check both
// directions — that's why this uses a UNION query. Each sub-query can use
// its own index, which is faster than a single query with OR.
func (s *Store) LinkedFacts(factID int64, limit int) ([]Fact, error) {
	rows, err := s.db.Query(`
		SELECT f.id, f.timestamp, f.fact, f.category, COALESCE(f.subject, 'user'),
		       f.importance, COALESCE(f.tags, ''), fl.similarity
		FROM facts f
		JOIN fact_links fl ON fl.target_id = f.id
		WHERE fl.source_id = ? AND f.active = 1
		UNION
		SELECT f.id, f.timestamp, f.fact, f.category, COALESCE(f.subject, 'user'),
		       f.importance, COALESCE(f.tags, ''), fl.similarity
		FROM facts f
		JOIN fact_links fl ON fl.source_id = f.id
		WHERE fl.target_id = ? AND f.active = 1
		ORDER BY similarity DESC
		LIMIT ?`,
		factID, factID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("querying linked facts for %d: %w", factID, err)
	}
	defer rows.Close()

	var facts []Fact
	for rows.Next() {
		var f Fact
		var sim float64
		if err := rows.Scan(&f.ID, &f.Timestamp, &f.Fact, &f.Category,
			&f.Subject, &f.Importance, &f.Tags, &sim); err != nil {
			return nil, fmt.Errorf("scanning linked fact: %w", err)
		}
		f.Active = true
		// Convert similarity (0-1, higher=closer) to distance (0-2, lower=closer)
		// so linked facts use the same scale as KNN results. This lets the
		// distance filter in BuildMemoryContext treat them uniformly.
		f.Distance = 1 - sim
		f.Source = "linked"
		facts = append(facts, f)
	}
	return facts, nil
}

// AutoLinkFact finds the most similar existing facts and links them to the
// given fact. This is the Zettelkasten core — when a new fact is saved, it
// automatically connects to its neighbors in embedding space, building a
// knowledge graph over time.
//
// Uses the same KNN search as SemanticSearch but with the new fact's own
// embedding as the query. The fact itself will appear as distance=0, so
// we skip it explicitly.
func (s *Store) AutoLinkFact(factID int64, embedding []float32) error {
	if s.AutoLinkCount == 0 {
		return nil // linking disabled
	}
	if s.EmbedDimension == 0 {
		return nil // no vector index
	}

	queryBytes, err := serializeEmbedding(embedding)
	if err != nil {
		return fmt.Errorf("serializing embedding for auto-link: %w", err)
	}

	// Request extra results to account for the self-match and inactive facts.
	k := s.AutoLinkCount + 2
	rows, err := s.db.Query(`
		SELECT v.rowid, v.distance
		FROM vec_facts v
		JOIN facts f ON f.id = v.rowid
		WHERE v.embedding MATCH ?
		  AND k = ?
		  AND f.active = 1`,
		queryBytes, k,
	)
	if err != nil {
		return fmt.Errorf("KNN search for auto-link: %w", err)
	}
	defer rows.Close()

	linked := 0
	for rows.Next() && linked < s.AutoLinkCount {
		var neighborID int64
		var distance float64
		if err := rows.Scan(&neighborID, &distance); err != nil {
			continue
		}
		// Skip self — the new fact is already in vec_facts, so it shows up
		// as distance=0 in its own KNN results.
		if neighborID == factID {
			continue
		}
		// Convert cosine distance to similarity. sqlite-vec uses distance
		// (0=identical, 2=opposite), but our threshold is in similarity
		// terms (0.7 = "at least 70% similar").
		similarity := 1 - distance
		if similarity < s.AutoLinkThreshold {
			continue
		}
		if err := s.LinkFacts(factID, neighborID, similarity); err != nil {
			log.Warn("auto-link: failed to link", "fact", factID, "neighbor", neighborID, "err", err)
			continue
		}
		log.Debugf("auto-link: %d ↔ %d (similarity=%.3f)", factID, neighborID, similarity)
		linked++
	}
	return nil
}

// SupersedeFact marks a fact as replaced by a newer one. This is like
// DeactivateFact but records the supersession chain — which fact replaced
// it and why. The chain lets the agent naturally reference knowledge
// evolution: "you used to work at X, now at Y."
func (s *Store) SupersedeFact(oldID, newID int64, reason string) error {
	_, err := s.db.Exec(
		`UPDATE facts SET active = 0, superseded_by = ?, supersede_reason = ? WHERE id = ?`,
		newID, reason, oldID,
	)
	if err != nil {
		return fmt.Errorf("superseding fact %d → %d: %w", oldID, newID, err)
	}
	// Remove from vec_facts — same as DeactivateFact.
	if s.EmbedDimension > 0 {
		s.db.Exec(`DELETE FROM vec_facts WHERE rowid = ?`, oldID)
	}
	return nil
}

// GetFact returns a single fact by ID, including inactive (superseded) ones.
// Returns nil and no error if the fact doesn't exist. Used by update_fact
// to read the old fact's metadata before creating a supersession chain.
func (s *Store) GetFact(factID int64) (*Fact, error) {
	var f Fact
	var ts string
	var active bool
	var supersededBy sql.NullInt64
	var supersedeReason sql.NullString
	var context sql.NullString
	err := s.db.QueryRow(
		`SELECT id, timestamp, fact, category, subject, importance, tags, active,
		        superseded_by, supersede_reason, COALESCE(context, '')
		 FROM facts WHERE id = ?`, factID,
	).Scan(&f.ID, &ts, &f.Fact, &f.Category, &f.Subject, &f.Importance,
		&f.Tags, &active, &supersededBy, &supersedeReason, &context)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting fact %d: %w", factID, err)
	}
	f.Timestamp, _ = time.Parse("2006-01-02 15:04:05", ts)
	f.Active = active
	if supersededBy.Valid {
		f.SupersededBy = supersededBy.Int64
	}
	if supersedeReason.Valid {
		f.SupersedeReason = supersedeReason.String
	}
	if context.Valid {
		f.Context = context.String
	}
	return &f, nil
}

// FactHistory returns the full supersession chain containing a fact —
// all versions from the original to the current, ordered oldest → newest.
// Walks backward (who did factID replace?) and forward (what replaced factID?).
// Includes inactive facts — the whole point is seeing deactivated predecessors.
// Capped at 20 hops in each direction to prevent runaway traversal.
func (s *Store) FactHistory(factID int64) ([]Fact, error) {
	const maxHops = 20

	// Collect the starting fact.
	start, err := s.GetFact(factID)
	if err != nil {
		return nil, err
	}
	if start == nil {
		return nil, nil
	}

	// Walk backward: find predecessors (facts that were superseded to become this one).
	// "SELECT id FROM facts WHERE superseded_by = ?" gives us the previous version.
	var predecessors []Fact
	currentID := factID
	seen := map[int64]bool{factID: true}
	for i := 0; i < maxHops; i++ {
		var prevID int64
		err := s.db.QueryRow(
			`SELECT id FROM facts WHERE superseded_by = ?`, currentID,
		).Scan(&prevID)
		if err != nil {
			break // no predecessor — we've reached the start of the chain
		}
		if seen[prevID] {
			break // cycle detection
		}
		seen[prevID] = true
		f, err := s.GetFact(prevID)
		if err != nil || f == nil {
			break
		}
		predecessors = append(predecessors, *f)
		currentID = prevID
	}

	// Reverse predecessors so they go oldest → newest.
	for i, j := 0, len(predecessors)-1; i < j; i, j = i+1, j-1 {
		predecessors[i], predecessors[j] = predecessors[j], predecessors[i]
	}

	// Walk forward: find successors (facts that replaced this one).
	var successors []Fact
	currentID = factID
	for i := 0; i < maxHops; i++ {
		f, err := s.GetFact(currentID)
		if err != nil || f == nil || f.SupersededBy == 0 {
			break
		}
		nextID := f.SupersededBy
		if seen[nextID] {
			break // cycle detection
		}
		seen[nextID] = true
		next, err := s.GetFact(nextID)
		if err != nil || next == nil {
			break
		}
		successors = append(successors, *next)
		currentID = nextID
	}

	// Assemble: predecessors + start + successors
	chain := make([]Fact, 0, len(predecessors)+1+len(successors))
	chain = append(chain, predecessors...)
	chain = append(chain, *start)
	chain = append(chain, successors...)
	return chain, nil
}

// CountFactLinks returns the total number of links in the fact graph.
// Used by the relink command to report progress.
func (s *Store) CountFactLinks() (int, error) {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM fact_links`).Scan(&count)
	return count, err
}

// AllActiveFacts returns every active fact (both user and self).
// Used by the agent to see the full memory state when deciding
// what to update or consolidate. Includes cached embeddings.
func (s *Store) AllActiveFacts() ([]Fact, error) {
	rows, err := s.db.Query(
		`SELECT id, timestamp, fact, category, COALESCE(subject, 'user'), importance, COALESCE(tags, ''), embedding, embedding_text
		 FROM facts WHERE active = 1
		 ORDER BY subject ASC, timestamp DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("querying all active facts: %w", err)
	}
	defer rows.Close()

	var facts []Fact
	for rows.Next() {
		var f Fact
		var ts string
		var embData []byte
		var embTextData []byte
		if err := rows.Scan(&f.ID, &ts, &f.Fact, &f.Category, &f.Subject, &f.Importance, &f.Tags, &embData, &embTextData); err != nil {
			return nil, fmt.Errorf("scanning fact row: %w", err)
		}
		f.Timestamp, _ = time.Parse("2006-01-02 15:04:05", ts)
		f.Active = true
		f.Embedding = deserializeEmbedding(embData)
		f.EmbeddingText = deserializeEmbedding(embTextData)
		facts = append(facts, f)
	}
	return facts, nil
}

// SemanticSearch finds the top-K facts most similar to a query vector
// using sqlite-vec's KNN search. Returns facts with their cosine distance
// (0 = identical, up to 2 = opposite). Only returns active facts.
//
// This is the core of v0.4's "She Understands" — instead of just grabbing
// the most important facts, we find the facts most RELEVANT to what the
// user is talking about right now.
//
// Under the hood, sqlite-vec uses the MATCH operator on the vec0 virtual
// table. The query plan: KNN on vec_facts → get rowids → JOIN facts for
// metadata → filter out inactive facts.
func (s *Store) SemanticSearch(queryVec []float32, topK int) ([]Fact, error) {
	if s.EmbedDimension == 0 {
		return nil, fmt.Errorf("semantic search not available: embed dimension is 0")
	}

	queryBytes, err := serializeEmbedding(queryVec)
	if err != nil {
		return nil, fmt.Errorf("serializing query vector: %w", err)
	}

	// We request more than topK from vec_facts because some results may
	// be inactive facts (soft-deleted). We filter those out after the JOIN.
	// Requesting 2x is a reasonable buffer.
	rows, err := s.db.Query(
		`SELECT f.id, f.timestamp, f.fact, f.category, COALESCE(f.subject, 'user'),
		        f.importance, COALESCE(f.tags, ''), f.embedding_text, v.distance
		 FROM vec_facts v
		 JOIN facts f ON f.id = v.rowid
		 WHERE v.embedding MATCH ?
		   AND k = ?
		   AND f.active = 1
		 ORDER BY v.distance ASC`,
		queryBytes, topK*2,
	)
	if err != nil {
		return nil, fmt.Errorf("semantic search query: %w", err)
	}
	defer rows.Close()

	var facts []Fact
	for rows.Next() {
		var f Fact
		var ts string
		var embTextData []byte
		if err := rows.Scan(&f.ID, &ts, &f.Fact, &f.Category, &f.Subject, &f.Importance, &f.Tags, &embTextData, &f.Distance); err != nil {
			return nil, fmt.Errorf("scanning semantic search result: %w", err)
		}
		f.Timestamp, _ = time.Parse("2006-01-02 15:04:05", ts)
		f.Active = true
		f.Source = "semantic"
		f.EmbeddingText = deserializeEmbedding(embTextData)
		facts = append(facts, f)

		// Stop once we have enough active results.
		if len(facts) >= topK {
			break
		}
	}

	// Zettelkasten 1-hop traversal: for each primary KNN result, pull in
	// linked neighbors that didn't directly match the query. This is the
	// graph payoff — "what does she like to cook?" finds cooking facts via
	// KNN, then linked dietary preferences, grocery habits, etc. via links.
	if s.AutoLinkCount > 0 && len(facts) > 0 {
		seen := make(map[int64]bool, len(facts))
		for _, f := range facts {
			seen[f.ID] = true
		}
		var linkedFacts []Fact
		for _, f := range facts {
			neighbors, err := s.LinkedFacts(f.ID, 3)
			if err != nil {
				continue
			}
			for _, n := range neighbors {
				if !seen[n.ID] {
					seen[n.ID] = true
					linkedFacts = append(linkedFacts, n)
				}
			}
		}
		facts = append(facts, linkedFacts...)
	}

	return facts, nil
}

// BackfillEmbeddings returns all active facts that don't have an embedding
// yet (embedding BLOB is NULL or empty). The caller should embed each fact
// and call UpdateFactEmbedding to populate both the BLOB and vec_facts index.
func (s *Store) FactsWithoutEmbeddings() ([]Fact, error) {
	rows, err := s.db.Query(
		`SELECT id, timestamp, fact, category, COALESCE(subject, 'user'), importance, COALESCE(tags, '')
		 FROM facts
		 WHERE active = 1 AND (embedding IS NULL OR LENGTH(embedding) = 0)
		 ORDER BY id ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("querying facts without embeddings: %w", err)
	}
	defer rows.Close()

	var facts []Fact
	for rows.Next() {
		var f Fact
		var ts string
		if err := rows.Scan(&f.ID, &ts, &f.Fact, &f.Category, &f.Subject, &f.Importance, &f.Tags); err != nil {
			return nil, fmt.Errorf("scanning fact: %w", err)
		}
		f.Timestamp, _ = time.Parse("2006-01-02 15:04:05", ts)
		f.Active = true
		facts = append(facts, f)
	}
	return facts, nil
}

// VecFactsCount returns the number of rows in the vec_facts virtual table.
// Useful for checking if a backfill is needed (compare against total active facts).
func (s *Store) VecFactsCount() (int, error) {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM vec_facts`).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("counting vec_facts: %w", err)
	}
	return count, nil
}

// FindFactsByKeyword searches active facts for a keyword match.
// Used by /forget to help the user find facts to deactivate.
func (s *Store) FindFactsByKeyword(keyword string) ([]Fact, error) {
	rows, err := s.db.Query(
		`SELECT id, timestamp, fact, category, COALESCE(subject, 'user'), importance, COALESCE(tags, ''), embedding
		 FROM facts
		 WHERE active = 1 AND fact LIKE '%' || ? || '%'
		 ORDER BY timestamp DESC
		 LIMIT 10`,
		keyword,
	)
	if err != nil {
		return nil, fmt.Errorf("searching facts: %w", err)
	}
	defer rows.Close()

	var facts []Fact
	for rows.Next() {
		var f Fact
		var ts string
		var embData []byte
		if err := rows.Scan(&f.ID, &ts, &f.Fact, &f.Category, &f.Subject, &f.Importance, &f.Tags, &embData); err != nil {
			return nil, fmt.Errorf("scanning fact row: %w", err)
		}
		f.Timestamp, _ = time.Parse("2006-01-02 15:04:05", ts)
		f.Active = true
		f.Embedding = deserializeEmbedding(embData)
		facts = append(facts, f)
	}
	return facts, nil
}

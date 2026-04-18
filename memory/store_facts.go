package memory

import (
	"database/sql"
	"fmt"
	"math"
	"time"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
)

// Memory represents an extracted piece of long-term memory.
// Subject is "user" for memories about the user, or "self" for Mira's
// own self-knowledge (her identity, observations, patterns).
type Memory struct {
	ID              int64
	Timestamp       time.Time
	Content         string
	Category        string
	Subject         string // "user" or "self"
	SourceMessageID int64
	Importance      int
	Active          bool
	Tags            string    // comma-separated topic descriptors for semantic search
	Context         string    // optional note explaining WHY this memory matters (max 500 chars)
	Embedding       []float32 // cached tag embedding vector (nil if not yet computed)
	EmbeddingText   []float32 // cached text embedding vector (nil if not yet computed)
	Distance        float64   // populated by SemanticSearch — cosine distance from query (0 = identical)

	// Zettelkasten fields — knowledge graph edges and supersession tracking.
	SupersededBy    int64  // ID of the memory that replaced this one (0 = not superseded)
	SupersedeReason string // why this memory was replaced (e.g. "job changed")
	Source          string // populated during retrieval: "semantic", "importance", or "linked"
}

// Reflection represents a journal-like entry Mira writes after a
// memory-dense conversation. Separate from memories — reflections are
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

// deserializeEmbedding converts bytes from the memories.embedding BLOB column
// back into a float32 slice. This reads the new float32 format (4 bytes/float)
// used by sqlite-vec. Legacy float64 BLOBs (8 bytes/float) from before the
// migration will have the wrong dimension and return nil — those memories need
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

// SaveMemory inserts an extracted memory into the database and its embedding
// into the vec_memories virtual table for KNN search.
// subject is "user" or "self". If sourceMessageID is 0, it's stored as NULL.
// embedding is the tag-based vector (used for KNN search via vec_memories).
// embeddingText is the raw-text vector (used for dedup and redundancy filtering).
// Both are optional — pass nil if not yet computed.
func (s *Store) SaveMemory(content, category, subject string, sourceMessageID int64, importance int, embedding []float32, embeddingText []float32, tags string, context string) (int64, error) {
	var srcID interface{} = sourceMessageID
	if sourceMessageID == 0 {
		srcID = nil
	}
	if subject == "" {
		subject = "user"
	}

	// Serialize the tag embedding to bytes for the BLOB column on the memories table.
	// This is the "source of truth" copy — vec_memories is the searchable index.
	var embBlob interface{}
	if len(embedding) > 0 {
		b, err := serializeEmbedding(embedding)
		if err != nil {
			return 0, fmt.Errorf("serializing embedding: %w", err)
		}
		embBlob = b
	}

	// Serialize the text embedding separately. This is only stored on the memories
	// table (not vec_memories) — it's used for dedup checks, not KNN search.
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
		`INSERT INTO memories (memory, category, subject, source_message_id, importance, embedding, embedding_text, tags, context)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		content, category, subject, srcID, importance, embBlob, embTextBlob, tags, ctxVal,
	)
	if err != nil {
		return 0, fmt.Errorf("saving memory: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("getting memory ID: %w", err)
	}

	// Also insert into the vec_memories virtual table so this memory is
	// searchable via KNN. The rowid matches memories.id for easy JOINs.
	if len(embedding) > 0 && s.EmbedDimension > 0 {
		vecBytes, err := serializeEmbedding(embedding)
		if err != nil {
			return id, nil // memory saved, vector index failed — non-fatal
		}
		if _, err := s.db.Exec(
			`INSERT INTO vec_memories(rowid, embedding) VALUES (?, ?)`,
			id, vecBytes,
		); err != nil {
			// Log but don't fail — the memory is saved, we just can't search it yet.
			// This handles the case where vec_memories doesn't exist (dimension=0).
			fmt.Printf("[memory] warning: vec_memories insert failed for memory %d: %v\n", id, err)
		}

		// Zettelkasten auto-linking: connect this memory to its nearest neighbors
		// in embedding space. Non-fatal — the memory is saved regardless.
		if s.AutoLinkCount > 0 {
			if err := s.AutoLinkMemory(id, embedding); err != nil {
				log.Warn("auto-link failed", "memory_id", id, "err", err)
			}
		}
	}

	return id, nil
}

// UpdateMemoryEmbedding sets the cached embeddings for a memory and updates
// the vec_memories index. embedding is the tag-based vector for KNN search.
// embeddingText is the raw-text vector for dedup checks; pass nil to leave
// it unchanged (the SQL still writes NULL, so pass existing.EmbeddingText
// when you don't want to clear it).
func (s *Store) UpdateMemoryEmbedding(memoryID int64, embedding []float32, embeddingText []float32) error {
	vecBytes, err := serializeEmbedding(embedding)
	if err != nil {
		return fmt.Errorf("serializing embedding for memory %d: %w", memoryID, err)
	}

	// Serialize the text embedding; nil produces a nil byte slice which
	// SQLite stores as NULL — that's intentional for memories without a
	// text embedding yet.
	var textVecBytes interface{}
	if len(embeddingText) > 0 {
		b, err := serializeEmbedding(embeddingText)
		if err != nil {
			return fmt.Errorf("serializing text embedding for memory %d: %w", memoryID, err)
		}
		textVecBytes = b
	}

	// Update both BLOB columns on the memories table in one round-trip.
	if _, err := s.db.Exec(
		`UPDATE memories SET embedding = ?, embedding_text = ? WHERE id = ?`,
		vecBytes, textVecBytes, memoryID,
	); err != nil {
		return fmt.Errorf("updating embedding for memory %d: %w", memoryID, err)
	}

	// Upsert into vec_memories — DELETE + INSERT because vec0 virtual tables
	// don't support UPDATE. This is idempotent: if the row doesn't exist
	// yet (new backfill), the DELETE is a no-op.
	if s.EmbedDimension > 0 {
		s.db.Exec(`DELETE FROM vec_memories WHERE rowid = ?`, memoryID)
		if _, err := s.db.Exec(
			`INSERT INTO vec_memories(rowid, embedding) VALUES (?, ?)`,
			memoryID, vecBytes,
		); err != nil {
			return fmt.Errorf("updating vec_memories for memory %d: %w", memoryID, err)
		}
	}

	return nil
}

// RecentMemories retrieves the top-K active memories for a given subject,
// ordered by recency (descending).
func (s *Store) RecentMemories(subject string, limit int) ([]Memory, error) {
	rows, err := s.db.Query(
		`SELECT id, timestamp, memory, category, COALESCE(subject, 'user'), importance, COALESCE(tags, ''), embedding, embedding_text
		 FROM memories
		 WHERE active = 1 AND COALESCE(subject, 'user') = ?
		 ORDER BY timestamp DESC
		 LIMIT ?`,
		subject, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("querying memories: %w", err)
	}
	defer rows.Close()

	var memories []Memory
	for rows.Next() {
		var m Memory
		var ts string
		var embData []byte
		var embTextData []byte
		if err := rows.Scan(&m.ID, &ts, &m.Content, &m.Category, &m.Subject, &m.Importance, &m.Tags, &embData, &embTextData); err != nil {
			return nil, fmt.Errorf("scanning memory row: %w", err)
		}
		m.Timestamp, _ = time.Parse("2006-01-02 15:04:05", ts)
		m.Active = true
		m.Embedding = deserializeEmbedding(embData)
		m.EmbeddingText = deserializeEmbedding(embTextData)
		memories = append(memories, m)
	}
	return memories, nil
}

// GetMemoryContent returns the current text of a memory by ID. Returns an empty
// string and no error if the memory doesn't exist (soft-deleted or never
// created). Used by update_memory to show the classifier both the old and
// new text so it can evaluate what actually changed — without this, the
// classifier only sees the final text and can't catch inferred additions.
func (s *Store) GetMemoryContent(memoryID int64) (string, error) {
	var text string
	err := s.db.QueryRow(`SELECT memory FROM memories WHERE id = ?`, memoryID).Scan(&text)
	if err != nil {
		return "", nil // memory not found — not an error, just empty
	}
	return text, nil
}

// UpdateMemory modifies an existing memory's text, category, or importance.
func (s *Store) UpdateMemory(memoryID int64, content, category string, importance int, tags string) error {
	_, err := s.db.Exec(
		`UPDATE memories SET memory = ?, category = ?, importance = ?, tags = ? WHERE id = ?`,
		content, category, importance, tags, memoryID,
	)
	if err != nil {
		return fmt.Errorf("updating memory %d: %w", memoryID, err)
	}
	return nil
}

// UpdateMemoryTags sets the topic tags for a memory without changing anything else.
// Used by `her retag` to backfill tags for existing memories.
func (s *Store) UpdateMemoryTags(memoryID int64, tags string) error {
	_, err := s.db.Exec(`UPDATE memories SET tags = ? WHERE id = ?`, tags, memoryID)
	return err
}

// DeactivateMemory soft-deletes a memory by setting active = 0 and removing
// it from the vec_memories index. The memory stays in the DB for audit trail
// but won't appear in retrieval or vector search.
func (s *Store) DeactivateMemory(memoryID int64) error {
	_, err := s.db.Exec(
		`UPDATE memories SET active = 0 WHERE id = ?`,
		memoryID,
	)
	if err != nil {
		return fmt.Errorf("deactivating memory %d: %w", memoryID, err)
	}
	// Remove from vec_memories so deactivated memories don't pollute KNN results.
	if s.EmbedDimension > 0 {
		s.db.Exec(`DELETE FROM vec_memories WHERE rowid = ?`, memoryID)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Zettelkasten: memory linking + supersession
// ---------------------------------------------------------------------------

// LinkMemories creates a bidirectional link between two memories with a similarity
// score. IDs are normalized (min, max) so the same pair can't be stored twice
// in different order — same trick social graph databases use for friendships.
//
// INSERT OR IGNORE means calling this with an already-linked pair is a no-op.
func (s *Store) LinkMemories(id1, id2 int64, similarity float64) error {
	// Normalize: always store (smaller ID, larger ID).
	// This is like sorting a tuple in Python: min/max guarantees one
	// canonical order regardless of which direction the link was found.
	source, target := id1, id2
	if id1 > id2 {
		source, target = id2, id1
	}
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO memory_links (source_id, target_id, similarity) VALUES (?, ?, ?)`,
		source, target, similarity,
	)
	if err != nil {
		return fmt.Errorf("linking memories %d↔%d: %w", source, target, err)
	}
	return nil
}

// LinkedMemories returns active memories linked to the given memory (1-hop traversal).
// Because links are normalized (source < target), we need to check both
// directions — that's why this uses a UNION query. Each sub-query can use
// its own index, which is faster than a single query with OR.
func (s *Store) LinkedMemories(memoryID int64, limit int) ([]Memory, error) {
	rows, err := s.db.Query(`
		SELECT m.id, m.timestamp, m.memory, m.category, COALESCE(m.subject, 'user'),
		       m.importance, COALESCE(m.tags, ''), ml.similarity
		FROM memories m
		JOIN memory_links ml ON ml.target_id = m.id
		WHERE ml.source_id = ? AND m.active = 1
		UNION
		SELECT m.id, m.timestamp, m.memory, m.category, COALESCE(m.subject, 'user'),
		       m.importance, COALESCE(m.tags, ''), ml.similarity
		FROM memories m
		JOIN memory_links ml ON ml.source_id = m.id
		WHERE ml.target_id = ? AND m.active = 1
		ORDER BY similarity DESC
		LIMIT ?`,
		memoryID, memoryID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("querying linked memories for %d: %w", memoryID, err)
	}
	defer rows.Close()

	var memories []Memory
	for rows.Next() {
		var m Memory
		var sim float64
		if err := rows.Scan(&m.ID, &m.Timestamp, &m.Content, &m.Category,
			&m.Subject, &m.Importance, &m.Tags, &sim); err != nil {
			return nil, fmt.Errorf("scanning linked memory: %w", err)
		}
		m.Active = true
		// Convert similarity (0-1, higher=closer) to distance (0-2, lower=closer)
		// so linked memories use the same scale as KNN results. This lets the
		// distance filter in BuildMemoryContext treat them uniformly.
		m.Distance = 1 - sim
		m.Source = "linked"
		memories = append(memories, m)
	}
	return memories, nil
}

// AutoLinkMemory finds the most similar existing memories and links them to the
// given memory. This is the Zettelkasten core — when a new memory is saved, it
// automatically connects to its neighbors in embedding space, building a
// knowledge graph over time.
//
// Uses the same KNN search as SemanticSearch but with the new memory's own
// embedding as the query. The memory itself will appear as distance=0, so
// we skip it explicitly.
func (s *Store) AutoLinkMemory(memoryID int64, embedding []float32) error {
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

	// Request extra results to account for the self-match and inactive memories.
	k := s.AutoLinkCount + 2
	rows, err := s.db.Query(`
		SELECT v.rowid, v.distance
		FROM vec_memories v
		JOIN memories m ON m.id = v.rowid
		WHERE v.embedding MATCH ?
		  AND k = ?
		  AND m.active = 1`,
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
		// Skip self — the new memory is already in vec_memories, so it shows up
		// as distance=0 in its own KNN results.
		if neighborID == memoryID {
			continue
		}
		// Convert cosine distance to similarity. sqlite-vec uses distance
		// (0=identical, 2=opposite), but our threshold is in similarity
		// terms (0.7 = "at least 70% similar").
		similarity := 1 - distance
		if similarity < s.AutoLinkThreshold {
			continue
		}
		if err := s.LinkMemories(memoryID, neighborID, similarity); err != nil {
			log.Warn("auto-link: failed to link", "memory", memoryID, "neighbor", neighborID, "err", err)
			continue
		}
		log.Debugf("auto-link: %d ↔ %d (similarity=%.3f)", memoryID, neighborID, similarity)
		linked++
	}
	return nil
}

// SupersedeMemory marks a memory as replaced by a newer one. This is like
// DeactivateMemory but records the supersession chain — which memory replaced
// it and why. The chain lets the agent naturally reference knowledge
// evolution: "you used to work at X, now at Y."
func (s *Store) SupersedeMemory(oldID, newID int64, reason string) error {
	_, err := s.db.Exec(
		`UPDATE memories SET active = 0, superseded_by = ?, supersede_reason = ? WHERE id = ?`,
		newID, reason, oldID,
	)
	if err != nil {
		return fmt.Errorf("superseding memory %d → %d: %w", oldID, newID, err)
	}
	// Remove from vec_memories — same as DeactivateMemory.
	if s.EmbedDimension > 0 {
		s.db.Exec(`DELETE FROM vec_memories WHERE rowid = ?`, oldID)
	}
	return nil
}

// GetMemory returns a single memory by ID, including inactive (superseded) ones.
// Returns nil and no error if the memory doesn't exist. Used by update_memory
// to read the old memory's metadata before creating a supersession chain.
func (s *Store) GetMemory(memoryID int64) (*Memory, error) {
	var m Memory
	var ts string
	var active bool
	var supersededBy sql.NullInt64
	var supersedeReason sql.NullString
	var context sql.NullString
	err := s.db.QueryRow(
		`SELECT id, timestamp, memory, category, subject, importance, tags, active,
		        superseded_by, supersede_reason, COALESCE(context, '')
		 FROM memories WHERE id = ?`, memoryID,
	).Scan(&m.ID, &ts, &m.Content, &m.Category, &m.Subject, &m.Importance,
		&m.Tags, &active, &supersededBy, &supersedeReason, &context)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting memory %d: %w", memoryID, err)
	}
	m.Timestamp, _ = time.Parse("2006-01-02 15:04:05", ts)
	m.Active = active
	if supersededBy.Valid {
		m.SupersededBy = supersededBy.Int64
	}
	if supersedeReason.Valid {
		m.SupersedeReason = supersedeReason.String
	}
	if context.Valid {
		m.Context = context.String
	}
	return &m, nil
}

// MemoryHistory returns the full supersession chain containing a memory —
// all versions from the original to the current, ordered oldest → newest.
// Walks backward (who did memoryID replace?) and forward (what replaced memoryID?).
// Includes inactive memories — the whole point is seeing deactivated predecessors.
// Capped at 20 hops in each direction to prevent runaway traversal.
func (s *Store) MemoryHistory(memoryID int64) ([]Memory, error) {
	const maxHops = 20

	// Collect the starting memory.
	start, err := s.GetMemory(memoryID)
	if err != nil {
		return nil, err
	}
	if start == nil {
		return nil, nil
	}

	// Walk backward: find predecessors (memories that were superseded to become this one).
	// "SELECT id FROM memories WHERE superseded_by = ?" gives us the previous version.
	var predecessors []Memory
	currentID := memoryID
	seen := map[int64]bool{memoryID: true}
	for i := 0; i < maxHops; i++ {
		var prevID int64
		err := s.db.QueryRow(
			`SELECT id FROM memories WHERE superseded_by = ?`, currentID,
		).Scan(&prevID)
		if err != nil {
			break // no predecessor — we've reached the start of the chain
		}
		if seen[prevID] {
			break // cycle detection
		}
		seen[prevID] = true
		m, err := s.GetMemory(prevID)
		if err != nil || m == nil {
			break
		}
		predecessors = append(predecessors, *m)
		currentID = prevID
	}

	// Reverse predecessors so they go oldest → newest.
	for i, j := 0, len(predecessors)-1; i < j; i, j = i+1, j-1 {
		predecessors[i], predecessors[j] = predecessors[j], predecessors[i]
	}

	// Walk forward: find successors (memories that replaced this one).
	var successors []Memory
	currentID = memoryID
	for i := 0; i < maxHops; i++ {
		m, err := s.GetMemory(currentID)
		if err != nil || m == nil || m.SupersededBy == 0 {
			break
		}
		nextID := m.SupersededBy
		if seen[nextID] {
			break // cycle detection
		}
		seen[nextID] = true
		next, err := s.GetMemory(nextID)
		if err != nil || next == nil {
			break
		}
		successors = append(successors, *next)
		currentID = nextID
	}

	// Assemble: predecessors + start + successors
	chain := make([]Memory, 0, len(predecessors)+1+len(successors))
	chain = append(chain, predecessors...)
	chain = append(chain, *start)
	chain = append(chain, successors...)
	return chain, nil
}

// CountMemoryLinks returns the total number of links in the memory graph.
// Used by the relink command to report progress.
func (s *Store) CountMemoryLinks() (int, error) {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM memory_links`).Scan(&count)
	return count, err
}

// AllActiveMemories returns every active memory (both user and self).
// Used by the agent to see the full memory state when deciding
// what to update or consolidate. Includes cached embeddings.
func (s *Store) AllActiveMemories() ([]Memory, error) {
	rows, err := s.db.Query(
		`SELECT id, timestamp, memory, category, COALESCE(subject, 'user'), importance, COALESCE(tags, ''), embedding, embedding_text
		 FROM memories WHERE active = 1
		 ORDER BY subject ASC, timestamp DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("querying all active memories: %w", err)
	}
	defer rows.Close()

	var memories []Memory
	for rows.Next() {
		var m Memory
		var ts string
		var embData []byte
		var embTextData []byte
		if err := rows.Scan(&m.ID, &ts, &m.Content, &m.Category, &m.Subject, &m.Importance, &m.Tags, &embData, &embTextData); err != nil {
			return nil, fmt.Errorf("scanning memory row: %w", err)
		}
		m.Timestamp, _ = time.Parse("2006-01-02 15:04:05", ts)
		m.Active = true
		m.Embedding = deserializeEmbedding(embData)
		m.EmbeddingText = deserializeEmbedding(embTextData)
		memories = append(memories, m)
	}
	return memories, nil
}

// SemanticSearch finds the top-K memories most similar to a query vector
// using sqlite-vec's KNN search. Returns memories with their cosine distance
// (0 = identical, up to 2 = opposite). Only returns active memories.
//
// This is the core of v0.4's "She Understands" — instead of just grabbing
// the most important memories, we find the memories most RELEVANT to what the
// user is talking about right now.
//
// Under the hood, sqlite-vec uses the MATCH operator on the vec0 virtual
// table. The query plan: KNN on vec_memories → get rowids → JOIN memories for
// metadata → filter out inactive memories.
func (s *Store) SemanticSearch(queryVec []float32, topK int) ([]Memory, error) {
	if s.EmbedDimension == 0 {
		return nil, fmt.Errorf("semantic search not available: embed dimension is 0")
	}

	queryBytes, err := serializeEmbedding(queryVec)
	if err != nil {
		return nil, fmt.Errorf("serializing query vector: %w", err)
	}

	// We request more than topK from vec_memories because some results may
	// be inactive memories (soft-deleted). We filter those out after the JOIN.
	// Requesting 2x is a reasonable buffer.
	rows, err := s.db.Query(
		`SELECT m.id, m.timestamp, m.memory, m.category, COALESCE(m.subject, 'user'),
		        m.importance, COALESCE(m.tags, ''), m.embedding_text, v.distance
		 FROM vec_memories v
		 JOIN memories m ON m.id = v.rowid
		 WHERE v.embedding MATCH ?
		   AND k = ?
		   AND m.active = 1
		 ORDER BY v.distance ASC`,
		queryBytes, topK*2,
	)
	if err != nil {
		return nil, fmt.Errorf("semantic search query: %w", err)
	}
	defer rows.Close()

	var memories []Memory
	for rows.Next() {
		var m Memory
		var ts string
		var embTextData []byte
		if err := rows.Scan(&m.ID, &ts, &m.Content, &m.Category, &m.Subject, &m.Importance, &m.Tags, &embTextData, &m.Distance); err != nil {
			return nil, fmt.Errorf("scanning semantic search result: %w", err)
		}
		m.Timestamp, _ = time.Parse("2006-01-02 15:04:05", ts)
		m.Active = true
		m.Source = "semantic"
		m.EmbeddingText = deserializeEmbedding(embTextData)
		memories = append(memories, m)

		// Stop once we have enough active results.
		if len(memories) >= topK {
			break
		}
	}

	// Zettelkasten 1-hop traversal: for each primary KNN result, pull in
	// linked neighbors that didn't directly match the query. This is the
	// graph payoff — "what does she like to cook?" finds cooking memories via
	// KNN, then linked dietary preferences, grocery habits, etc. via links.
	if s.AutoLinkCount > 0 && len(memories) > 0 {
		seen := make(map[int64]bool, len(memories))
		for _, m := range memories {
			seen[m.ID] = true
		}
		var linkedMemories []Memory
		for _, m := range memories {
			neighbors, err := s.LinkedMemories(m.ID, 3)
			if err != nil {
				continue
			}
			for _, n := range neighbors {
				if !seen[n.ID] {
					seen[n.ID] = true
					linkedMemories = append(linkedMemories, n)
				}
			}
		}
		memories = append(memories, linkedMemories...)
	}

	return memories, nil
}

// MemoriesWithoutEmbeddings returns all active memories that don't have an
// embedding yet (embedding BLOB is NULL or empty). The caller should embed
// each memory and call UpdateMemoryEmbedding to populate both the BLOB and
// vec_memories index.
func (s *Store) MemoriesWithoutEmbeddings() ([]Memory, error) {
	rows, err := s.db.Query(
		`SELECT id, timestamp, memory, category, COALESCE(subject, 'user'), importance, COALESCE(tags, '')
		 FROM memories
		 WHERE active = 1 AND (embedding IS NULL OR LENGTH(embedding) = 0)
		 ORDER BY id ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("querying memories without embeddings: %w", err)
	}
	defer rows.Close()

	var memories []Memory
	for rows.Next() {
		var m Memory
		var ts string
		if err := rows.Scan(&m.ID, &ts, &m.Content, &m.Category, &m.Subject, &m.Importance, &m.Tags); err != nil {
			return nil, fmt.Errorf("scanning memory: %w", err)
		}
		m.Timestamp, _ = time.Parse("2006-01-02 15:04:05", ts)
		m.Active = true
		memories = append(memories, m)
	}
	return memories, nil
}

// VecMemoriesCount returns the number of rows in the vec_memories virtual table.
// Useful for checking if a backfill is needed (compare against total active memories).
func (s *Store) VecMemoriesCount() (int, error) {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM vec_memories`).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("counting vec_memories: %w", err)
	}
	return count, nil
}

// FindMemoriesByKeyword searches active memories for a keyword match.
// Used by /forget to help the user find memories to deactivate.
func (s *Store) FindMemoriesByKeyword(keyword string) ([]Memory, error) {
	rows, err := s.db.Query(
		`SELECT id, timestamp, memory, category, COALESCE(subject, 'user'), importance, COALESCE(tags, ''), embedding
		 FROM memories
		 WHERE active = 1 AND memory LIKE '%' || ? || '%'
		 ORDER BY timestamp DESC
		 LIMIT 10`,
		keyword,
	)
	if err != nil {
		return nil, fmt.Errorf("searching memories: %w", err)
	}
	defer rows.Close()

	var memories []Memory
	for rows.Next() {
		var m Memory
		var ts string
		var embData []byte
		if err := rows.Scan(&m.ID, &ts, &m.Content, &m.Category, &m.Subject, &m.Importance, &m.Tags, &embData); err != nil {
			return nil, fmt.Errorf("scanning memory row: %w", err)
		}
		m.Timestamp, _ = time.Parse("2006-01-02 15:04:05", ts)
		m.Active = true
		m.Embedding = deserializeEmbedding(embData)
		memories = append(memories, m)
	}
	return memories, nil
}

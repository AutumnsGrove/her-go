package memory

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"her/d1"
)

// ---------------------------------------------------------------------------
// SyncedStore — decorator that mirrors SQLite writes to Cloudflare D1
// ---------------------------------------------------------------------------
//
// This is the "push" half of the D1 shared-state sync. Every write to a
// synced table (messages, memories, persona, summaries, mood) gets pushed
// to D1 in the background after the local SQLite write succeeds.
//
// The pattern is called the "decorator" or "wrapper" pattern — you've seen
// it in Python as a class that holds a reference to another object and
// overrides some methods while delegating the rest. In Go, struct embedding
// gives us the delegation for free: any method we don't override passes
// straight through to the embedded *SQLiteStore.
//
// Key design decisions:
//   - Local write is ALWAYS the source of truth. D1 push is best-effort.
//   - If D1 is down, we log and continue — the user never sees an error.
//   - Writes are recorded in a local _d1_outbox table (transactional outbox
//     pattern). A carrier goroutine reads the outbox, fetches the actual
//     row data from SQLite, and pushes to D1. This is crash-safe: outbox
//     rows persist across restarts, so nothing is lost if D1 is temporarily
//     down or the process crashes mid-push.
//   - Embeddings are NOT pushed to D1 (D1 has no vector tables).

// Default sync tuning values — used when config omits the sync section.
// These can be overridden via config.yaml cloudflare.sync.*.
const (
	defaultBatchSize       = 50
	defaultCarrierPollSecs = 2
)

// SyncedStore wraps an SQLiteStore and pushes writes to D1 in the background.
// It satisfies the Store interface — callers don't need to know whether sync
// is enabled. Think of it like Python's functools.wraps but for an entire
// object: same interface, extra behavior layered on top.
type SyncedStore struct {
	// Embedding gives us all SQLiteStore methods for free. Any method we
	// don't explicitly override here "falls through" to the embedded store.
	// This is Go's version of inheritance — composition + delegation.
	*SQLiteStore

	d1Client *d1.Client   // Cloudflare D1 HTTP client
	notify   chan struct{} // signals the carrier that new outbox entries exist
	done     chan struct{} // closed when the carrier goroutine exits

	// Tuning knobs — set via config.yaml cloudflare.sync section.
	// Defaults are applied in NewSyncedStore; callers can override after
	// construction (same pattern as SQLiteStore.AutoLinkCount).
	BatchSize    int           // max outbox entries per carrier cycle (default 50)
	CarrierPoll  time.Duration // carrier polling interval (default 2s)
	PullPageSize int           // rows per D1 pull query (default 500)
}

// NewSyncedStore creates a SyncedStore that mirrors writes to D1.
// It creates the _d1_outbox table if it doesn't exist and starts a
// background carrier goroutine that processes outbox entries until
// Close() is called.
func NewSyncedStore(sqlite *SQLiteStore, d1Client *d1.Client) (*SyncedStore, error) {
	s := &SyncedStore{
		SQLiteStore: sqlite,
		d1Client:    d1Client,
		// Buffer size 1: we only need one "hey, there's work" signal at a
		// time. Multiple writes between carrier cycles just need one nudge.
		// Same idea as Python's threading.Event — set/clear, not a queue.
		notify:       make(chan struct{}, 1),
		done:         make(chan struct{}),
		BatchSize:    defaultBatchSize,
		CarrierPoll:  time.Duration(defaultCarrierPollSecs) * time.Second,
		PullPageSize: defaultPullPageSize,
	}

	if err := s.initOutbox(); err != nil {
		return nil, fmt.Errorf("initializing d1 outbox: %w", err)
	}

	go s.carrier()
	return s, nil
}

// ---------------------------------------------------------------------------
// Outbox table + helpers
// ---------------------------------------------------------------------------

// initOutbox creates the _d1_outbox table in local SQLite. This table
// stores pending D1 push jobs. Each row says "this table's row changed,
// go read it and push the current version to D1."
//
// The row_id_2 column handles composite primary keys (like memory_links
// which uses source_id + target_id). It's NULL for single-PK tables.
func (s *SyncedStore) initOutbox() error {
	_, err := s.SQLiteStore.db.Exec(`CREATE TABLE IF NOT EXISTS _d1_outbox (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		table_name TEXT    NOT NULL,
		row_id     INTEGER NOT NULL,
		row_id_2   INTEGER,
		op         TEXT    NOT NULL DEFAULT 'upsert',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	)`)
	return err
}

// writeOutbox records a pending D1 push for a single-PK table.
// This never returns an error to the caller — the local SQLite write
// already succeeded, so we just log and move on if the outbox insert fails.
func (s *SyncedStore) writeOutbox(table string, rowID int64, op string) {
	s.writeOutboxComposite(table, rowID, 0, op)
}

// writeOutboxComposite records a pending D1 push, supporting composite keys.
// If rowID2 is 0, it's stored as NULL (single-PK table). After inserting,
// it sends a non-blocking signal to wake the carrier goroutine.
//
// The non-blocking send is a Go idiom: if the carrier is already awake
// (channel has a value in it), we don't need to signal again. In Python
// terms, it's like Event.set() — idempotent, never blocks.
func (s *SyncedStore) writeOutboxComposite(table string, rowID, rowID2 int64, op string) {
	// Store NULL for row_id_2 when it's not used (single-PK tables).
	var r2 any
	if rowID2 != 0 {
		r2 = rowID2
	}

	_, err := s.SQLiteStore.db.Exec(
		`INSERT INTO _d1_outbox (table_name, row_id, row_id_2, op) VALUES (?, ?, ?, ?)`,
		table, rowID, r2, op,
	)
	if err != nil {
		log.Error("writing to d1 outbox", "table", table, "row_id", rowID, "err", err)
		return
	}

	// Wake the carrier. Non-blocking: if it's already notified, skip.
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// ---------------------------------------------------------------------------
// Table specs — tell the carrier how to read rows for each synced table
// ---------------------------------------------------------------------------

// tableSpec defines the column layout for a synced table. The carrier uses
// these to build SELECT (from local SQLite) and INSERT OR REPLACE (for D1)
// statements generically, without per-table special-casing.
type tableSpec struct {
	// selectCols are the columns to SELECT from local SQLite.
	selectCols string
	// d1Cols are the columns for the D1 INSERT OR REPLACE. Usually the
	// same as selectCols — we exclude embedding columns since D1 has no
	// vector tables.
	d1Cols string
	// placeholders is the VALUES (?, ?, ...) string matching d1Cols count.
	placeholders string
}

// syncedTableSpecs maps table names to their column specifications.
// When you add a new synced table, add an entry here and the carrier
// will know how to read and push rows for it automatically.
var syncedTableSpecs = map[string]tableSpec{
	"messages": {
		selectCols:   "id, timestamp, role, content_raw, content_scrubbed, conversation_id, token_count, voice_memo_path, media_file_id, media_description",
		d1Cols:       "id, timestamp, role, content_raw, content_scrubbed, conversation_id, token_count, voice_memo_path, media_file_id, media_description",
		placeholders: "?, ?, ?, ?, ?, ?, ?, ?, ?, ?",
	},
	"summaries": {
		selectCols:   "id, timestamp, conversation_id, summary, messages_start_id, messages_end_id, stream",
		d1Cols:       "id, timestamp, conversation_id, summary, messages_start_id, messages_end_id, stream",
		placeholders: "?, ?, ?, ?, ?, ?, ?",
	},
	"memories": {
		selectCols:   "id, timestamp, memory, category, source_message_id, importance, active, subject, tags, superseded_by, supersede_reason, context",
		d1Cols:       "id, timestamp, memory, category, source_message_id, importance, active, subject, tags, superseded_by, supersede_reason, context",
		placeholders: "?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?",
	},
	"memory_links": {
		selectCols:   "source_id, target_id, similarity, created_at",
		d1Cols:       "source_id, target_id, similarity, created_at",
		placeholders: "?, ?, ?, ?",
	},
	"reflections": {
		selectCols:   "id, timestamp, content, fact_count, user_message, mira_response",
		d1Cols:       "id, timestamp, content, fact_count, user_message, mira_response",
		placeholders: "?, ?, ?, ?, ?, ?",
	},
	"persona_versions": {
		selectCols:   "id, timestamp, content, trigger, conversation_count, reflection_ids",
		d1Cols:       "id, timestamp, content, trigger, conversation_count, reflection_ids",
		placeholders: "?, ?, ?, ?, ?, ?",
	},
	"traits": {
		selectCols:   "id, timestamp, trait_name, value, persona_version_id",
		d1Cols:       "id, timestamp, trait_name, value, persona_version_id",
		placeholders: "?, ?, ?, ?, ?",
	},
	"persona_state": {
		selectCols:   "id, last_reflection_at, last_rewrite_at, last_reflected_message_id",
		d1Cols:       "id, last_reflection_at, last_rewrite_at, last_reflected_message_id",
		placeholders: "?, ?, ?, ?",
	},
	"mood_entries": {
		selectCols:   "id, ts, kind, valence, labels, associations, note, source, confidence, conversation_id, created_at, updated_at",
		d1Cols:       "id, ts, kind, valence, labels, associations, note, source, confidence, conversation_id, created_at, updated_at",
		placeholders: "?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?",
	},
}

// ---------------------------------------------------------------------------
// Carrier — background goroutine that processes the outbox
// ---------------------------------------------------------------------------

// carrier is the background goroutine that reads outbox entries, fetches
// the corresponding row data from local SQLite, and pushes it to D1.
// It uses a hybrid notification approach: a channel signal for immediate
// processing + a ticker as a fallback poller. Runs until Close() is called.
//
// Think of it like a Python asyncio task that wakes up on an Event OR a
// timer — whichever fires first.
func (s *SyncedStore) carrier() {
	defer close(s.done)

	ticker := time.NewTicker(s.CarrierPoll)
	defer ticker.Stop()

	for {
		select {
		case _, ok := <-s.notify:
			if !ok {
				// Channel closed by Close() — process remaining entries and exit.
				s.processOutbox()
				return
			}
			s.processOutbox()
		case <-ticker.C:
			s.processOutbox()
		}
	}
}

// outboxEntry represents one row from the _d1_outbox table.
type outboxEntry struct {
	id      int64
	table   string
	rowID   int64
	rowID2  sql.NullInt64 // NULL for single-PK tables
	op      string
}

// processOutbox reads a batch of outbox entries, builds D1 statements for
// each, sends them in one batch, and deletes the processed entries on success.
// If D1 is unreachable, the entries stay in the outbox and will be retried
// on the next cycle.
func (s *SyncedStore) processOutbox() {
	// 1. Read a batch of outbox entries.
	rows, err := s.SQLiteStore.db.Query(
		`SELECT id, table_name, row_id, row_id_2, op FROM _d1_outbox ORDER BY id LIMIT ?`,
		s.BatchSize,
	)
	if err != nil {
		log.Error("reading d1 outbox", "err", err)
		return
	}
	defer rows.Close()

	var entries []outboxEntry
	for rows.Next() {
		var e outboxEntry
		if err := rows.Scan(&e.id, &e.table, &e.rowID, &e.rowID2, &e.op); err != nil {
			log.Error("scanning d1 outbox row", "err", err)
			continue
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		log.Error("iterating d1 outbox rows", "err", err)
	}

	if len(entries) == 0 {
		return
	}

	// 2. Build D1 statements for each entry.
	var stmts []d1.Statement
	var processedIDs []int64

	for _, e := range entries {
		var stmt *d1.Statement

		switch e.op {
		case "delete":
			stmt = s.buildDeleteStatement(e)
		case "upsert":
			stmt = s.buildUpsertStatement(e)
		default:
			log.Warn("unknown outbox op, skipping", "op", e.op, "id", e.id)
			// Still mark as processed — unknown ops shouldn't block the queue.
			processedIDs = append(processedIDs, e.id)
			continue
		}

		if stmt != nil {
			stmts = append(stmts, *stmt)
		}
		processedIDs = append(processedIDs, e.id)
	}

	if len(stmts) == 0 {
		// All entries were skipped (rows deleted locally before carrier ran).
		// Clean up the outbox entries so they don't accumulate.
		s.deleteOutboxEntries(processedIDs)
		return
	}

	// 3. Push to D1 in one batch.
	if _, err := s.d1Client.Batch(stmts); err != nil {
		// D1 is down or returned an error. Leave outbox entries for retry.
		// Local data is safe — this is purely best-effort sync.
		log.Error("d1 batch push failed", "statements", len(stmts), "err", err)
		return
	}

	log.Debug("d1 batch pushed", "statements", len(stmts))

	// 4. Success — delete processed outbox entries.
	s.deleteOutboxEntries(processedIDs)
}

// FlushOutbox drains the entire outbox, processing batches until no entries
// remain. Unlike the carrier goroutine (which processes one batch per tick),
// this runs synchronously and exhausts the queue. Used by PushAll to ensure
// any pending updates (e.g. importance score changes on existing rows) reach
// D1 before the incremental new-row push runs.
//
// Returns the total number of statements pushed, or the first error encountered.
func (s *SyncedStore) FlushOutbox() (int, error) {
	var totalFlushed int
	for {
		var count int
		err := s.SQLiteStore.db.QueryRow("SELECT COUNT(*) FROM _d1_outbox").Scan(&count)
		if err != nil || count == 0 {
			break
		}
		s.processOutbox()
		totalFlushed += count
	}
	return totalFlushed, nil
}

// buildDeleteStatement creates a DELETE statement for D1. For most tables,
// deletes use the id column. For memory_links, the composite key is used.
func (s *SyncedStore) buildDeleteStatement(e outboxEntry) *d1.Statement {
	if e.table == "memory_links" && e.rowID2.Valid {
		return &d1.Statement{
			SQL:    "DELETE FROM memory_links WHERE source_id = ? AND target_id = ?",
			Params: []any{e.rowID, e.rowID2.Int64},
		}
	}
	return &d1.Statement{
		SQL:    fmt.Sprintf("DELETE FROM %s WHERE id = ?", e.table),
		Params: []any{e.rowID},
	}
}

// buildUpsertStatement reads the current row from local SQLite and builds
// an INSERT OR REPLACE statement for D1. Returns nil if the row no longer
// exists locally (it was deleted between the write and carrier pickup).
func (s *SyncedStore) buildUpsertStatement(e outboxEntry) *d1.Statement {
	spec, ok := syncedTableSpecs[e.table]
	if !ok {
		log.Warn("no table spec for outbox entry, skipping", "table", e.table)
		return nil
	}

	// Read the row from local SQLite.
	var values []any
	var err error

	if e.table == "memory_links" && e.rowID2.Valid {
		values, err = s.readLink(e.rowID, e.rowID2.Int64)
	} else {
		values, err = s.readRow(e.table, e.rowID)
	}

	if err == sql.ErrNoRows {
		// Row was deleted locally between the write and carrier pickup.
		// Nothing to push — the outbox entry will be cleaned up.
		log.Debug("outbox row missing locally, skipping", "table", e.table, "row_id", e.rowID)
		return nil
	}
	if err != nil {
		log.Error("reading row for d1 push", "table", e.table, "row_id", e.rowID, "err", err)
		return nil
	}

	return &d1.Statement{
		SQL:    fmt.Sprintf("INSERT OR REPLACE INTO %s (%s) VALUES (%s)", e.table, spec.d1Cols, spec.placeholders),
		Params: values,
	}
}

// readRow fetches a single row from a table by its primary key (id column).
// Returns the column values as a slice of any, ready to be used as D1 params.
//
// The pattern here is a bit unusual if you're coming from Python: we create
// a slice of pointers (ptrs) that point into the values slice. sql.Scan
// needs pointers so it can write into each slot. In Python, cursor.fetchone()
// just gives you the values directly — Go makes the plumbing explicit.
func (s *SyncedStore) readRow(table string, rowID int64) ([]any, error) {
	spec := syncedTableSpecs[table]
	query := fmt.Sprintf("SELECT %s FROM %s WHERE id = ?", spec.selectCols, table)
	row := s.SQLiteStore.db.QueryRow(query, rowID)
	return scanRow(row, spec.selectCols)
}

// readLink fetches a memory_links row by its composite primary key.
func (s *SyncedStore) readLink(sourceID, targetID int64) ([]any, error) {
	spec := syncedTableSpecs["memory_links"]
	row := s.SQLiteStore.db.QueryRow(
		fmt.Sprintf("SELECT %s FROM memory_links WHERE source_id = ? AND target_id = ?", spec.selectCols),
		sourceID, targetID,
	)
	return scanRow(row, spec.selectCols)
}

// scanRow scans a *sql.Row into a []any slice. The number of columns is
// determined by counting commas in the selectCols string. Each value is
// scanned as *any, which lets the database driver choose the concrete type
// (int64, float64, string, []byte, nil for NULL).
func scanRow(row *sql.Row, selectCols string) ([]any, error) {
	cols := strings.Count(selectCols, ",") + 1
	values := make([]any, cols)
	ptrs := make([]any, cols)
	for i := range values {
		ptrs[i] = &values[i]
	}
	if err := row.Scan(ptrs...); err != nil {
		return nil, err
	}
	return values, nil
}

// deleteOutboxEntries removes processed entries from the outbox. Called
// after a successful D1 batch push. Uses a single DELETE with IN clause
// for efficiency.
func (s *SyncedStore) deleteOutboxEntries(ids []int64) {
	if len(ids) == 0 {
		return
	}

	// Build "?, ?, ?" placeholders and convert ids to []any for Exec.
	// Go doesn't let you splat an []int64 into an []any parameter directly
	// (no implicit conversions between slice types, unlike Python lists).
	placeholders := strings.Repeat("?,", len(ids))
	placeholders = placeholders[:len(placeholders)-1] // trim trailing comma

	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}

	_, err := s.SQLiteStore.db.Exec(
		fmt.Sprintf("DELETE FROM _d1_outbox WHERE id IN (%s)", placeholders),
		args...,
	)
	if err != nil {
		log.Error("deleting processed outbox entries", "err", err)
	}
}

// Close shuts down the carrier goroutine, waits for it to drain remaining
// outbox entries, then closes the underlying SQLiteStore. This ensures all
// pending D1 pushes are attempted before the process exits.
func (s *SyncedStore) Close() error {
	close(s.notify) // Signal the carrier to drain and exit.
	<-s.done        // Block until it's finished.
	return s.SQLiteStore.Close()
}

// ===========================================================================
// Method overrides — each calls SQLiteStore first, then writes to the outbox
// ===========================================================================
//
// The outbox pattern makes every override dead simple: delegate to SQLiteStore,
// then record "hey, this row changed" in the outbox. The carrier goroutine
// handles reading the actual data and pushing to D1. No more inline D1 SQL!

// ---------------------------------------------------------------------------
// Messages
// ---------------------------------------------------------------------------

// SaveMessage stores a message locally, then records it in the outbox for D1.
func (s *SyncedStore) SaveMessage(role, contentRaw, contentScrubbed, conversationID string) (int64, error) {
	id, err := s.SQLiteStore.SaveMessage(role, contentRaw, contentScrubbed, conversationID)
	if err != nil {
		return 0, err
	}
	s.writeOutbox("messages", id, "upsert")
	return id, nil
}

// UpdateMessageScrubbed updates the scrubbed content locally and records in outbox.
func (s *SyncedStore) UpdateMessageScrubbed(messageID int64, scrubbed string) error {
	if err := s.SQLiteStore.UpdateMessageScrubbed(messageID, scrubbed); err != nil {
		return err
	}
	s.writeOutbox("messages", messageID, "upsert")
	return nil
}

// UpdateMessageMedia stores media metadata locally and records in outbox.
func (s *SyncedStore) UpdateMessageMedia(messageID int64, fileID, description string) error {
	if err := s.SQLiteStore.UpdateMessageMedia(messageID, fileID, description); err != nil {
		return err
	}
	s.writeOutbox("messages", messageID, "upsert")
	return nil
}

// UpdateMessageVoicePath stores the voice memo path locally and records in outbox.
func (s *SyncedStore) UpdateMessageVoicePath(messageID int64, path string) error {
	if err := s.SQLiteStore.UpdateMessageVoicePath(messageID, path); err != nil {
		return err
	}
	s.writeOutbox("messages", messageID, "upsert")
	return nil
}

// UpdateMessageTokenCount sets the token count locally and records in outbox.
func (s *SyncedStore) UpdateMessageTokenCount(messageID int64, tokenCount int) error {
	if err := s.SQLiteStore.UpdateMessageTokenCount(messageID, tokenCount); err != nil {
		return err
	}
	s.writeOutbox("messages", messageID, "upsert")
	return nil
}

// ---------------------------------------------------------------------------
// Memories
// ---------------------------------------------------------------------------

// SaveMemory stores a memory locally and records in outbox for D1 push.
// Embeddings are not synced — D1 has no vector tables.
func (s *SyncedStore) SaveMemory(content, category, subject string, sourceMessageID int64, importance int, embedding []float32, embeddingText []float32, tags string, context string) (int64, error) {
	id, err := s.SQLiteStore.SaveMemory(content, category, subject, sourceMessageID, importance, embedding, embeddingText, tags, context)
	if err != nil {
		return 0, err
	}
	s.writeOutbox("memories", id, "upsert")
	return id, nil
}

// UpdateMemory modifies a memory's text/category/importance/tags locally
// and records in outbox.
func (s *SyncedStore) UpdateMemory(memoryID int64, content, category string, importance int, tags string) error {
	if err := s.SQLiteStore.UpdateMemory(memoryID, content, category, importance, tags); err != nil {
		return err
	}
	s.writeOutbox("memories", memoryID, "upsert")
	return nil
}

// UpdateMemoryTags sets just the tags column locally and records in outbox.
func (s *SyncedStore) UpdateMemoryTags(memoryID int64, tags string) error {
	if err := s.SQLiteStore.UpdateMemoryTags(memoryID, tags); err != nil {
		return err
	}
	s.writeOutbox("memories", memoryID, "upsert")
	return nil
}

// DeactivateMemory soft-deletes a memory locally and records in outbox.
// The vec_memories DELETE only happens locally — D1 has no vector tables.
func (s *SyncedStore) DeactivateMemory(memoryID int64) error {
	if err := s.SQLiteStore.DeactivateMemory(memoryID); err != nil {
		return err
	}
	s.writeOutbox("memories", memoryID, "upsert")
	return nil
}

// SupersedeMemory marks a memory as replaced locally and records in outbox.
// The vec_memories cleanup only happens locally — D1 has no vector tables.
func (s *SyncedStore) SupersedeMemory(oldID, newID int64, reason string) error {
	if err := s.SQLiteStore.SupersedeMemory(oldID, newID, reason); err != nil {
		return err
	}
	s.writeOutbox("memories", oldID, "upsert")
	return nil
}

// LinkMemories creates a bidirectional link locally and records in outbox.
// IDs are normalized so source < target, matching SQLiteStore's convention.
func (s *SyncedStore) LinkMemories(id1, id2 int64, similarity float64) error {
	if err := s.SQLiteStore.LinkMemories(id1, id2, similarity); err != nil {
		return err
	}

	// Normalize IDs the same way SQLiteStore does — always store
	// (smaller, larger) so the same pair can't appear in two orders.
	src, tgt := id1, id2
	if id1 > id2 {
		src, tgt = id2, id1
	}

	s.writeOutboxComposite("memory_links", src, tgt, "upsert")
	return nil
}

// NOTE: UpdateMemoryEmbedding is intentionally NOT overridden.
// D1 doesn't store embeddings, so the SQLiteStore method passes through
// via embedding — only the local SQLite gets the BLOB update.

// ---------------------------------------------------------------------------
// Persona
// ---------------------------------------------------------------------------

// SavePersonaVersion stores a persona snapshot locally and records in outbox.
func (s *SyncedStore) SavePersonaVersion(content, trigger string) (int64, error) {
	id, err := s.SQLiteStore.SavePersonaVersion(content, trigger)
	if err != nil {
		return 0, err
	}
	s.writeOutbox("persona_versions", id, "upsert")
	return id, nil
}

// SaveReflection stores a reflection locally and records in outbox.
func (s *SyncedStore) SaveReflection(content string, factCount int, userMessage, miraResponse string) (int64, error) {
	id, err := s.SQLiteStore.SaveReflection(content, factCount, userMessage, miraResponse)
	if err != nil {
		return 0, err
	}
	s.writeOutbox("reflections", id, "upsert")
	return id, nil
}

// SaveTraits bulk-inserts trait scores locally and records each in outbox.
// Since SQLiteStore.SaveTraits doesn't return the assigned IDs, we query
// them back by persona_version_id after the insert succeeds.
func (s *SyncedStore) SaveTraits(traits []Trait, personaVersionID int64) error {
	if err := s.SQLiteStore.SaveTraits(traits, personaVersionID); err != nil {
		return err
	}

	// Query back the trait IDs that were just inserted. We're in the same
	// package as SQLiteStore, so we can access s.SQLiteStore.db directly.
	// In Python terms, it's like accessing a "private" attribute from within
	// the same module — allowed and expected.
	rows, err := s.SQLiteStore.db.Query(
		"SELECT id FROM traits WHERE persona_version_id = ?", personaVersionID,
	)
	if err != nil {
		log.Error("querying trait IDs for outbox", "persona_version_id", personaVersionID, "err", err)
		return nil // local write succeeded, outbox is best-effort
	}
	defer rows.Close()

	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			log.Error("scanning trait ID for outbox", "err", err)
			continue
		}
		s.writeOutbox("traits", id, "upsert")
	}
	return nil
}

// SetLastReflectionAt records the last reflection time locally and records in outbox.
func (s *SyncedStore) SetLastReflectionAt(t time.Time) error {
	if err := s.SQLiteStore.SetLastReflectionAt(t); err != nil {
		return err
	}
	// persona_state is a singleton row — always id=1.
	s.writeOutbox("persona_state", 1, "upsert")
	return nil
}

// SetLastRewriteAt records the last rewrite time locally and records in outbox.
func (s *SyncedStore) SetLastRewriteAt(t time.Time) error {
	if err := s.SQLiteStore.SetLastRewriteAt(t); err != nil {
		return err
	}
	s.writeOutbox("persona_state", 1, "upsert")
	return nil
}

// SetLastReflectedMessageID advances the message watermark and records in outbox.
func (s *SyncedStore) SetLastReflectedMessageID(id int64) error {
	if err := s.SQLiteStore.SetLastReflectedMessageID(id); err != nil {
		return err
	}
	s.writeOutbox("persona_state", 1, "upsert")
	return nil
}

// ---------------------------------------------------------------------------
// Summaries
// ---------------------------------------------------------------------------

// SaveSummary stores a conversation summary locally and records in outbox.
func (s *SyncedStore) SaveSummary(conversationID, summary string, startID, endID int64, stream string) (int64, error) {
	id, err := s.SQLiteStore.SaveSummary(conversationID, summary, startID, endID, stream)
	if err != nil {
		return 0, err
	}
	s.writeOutbox("summaries", id, "upsert")
	return id, nil
}

// ---------------------------------------------------------------------------
// Mood
// ---------------------------------------------------------------------------

// SaveMoodEntry inserts a mood entry locally and records in outbox.
// Embeddings are not synced — D1 has no vector tables.
func (s *SyncedStore) SaveMoodEntry(entry *MoodEntry) (int64, error) {
	id, err := s.SQLiteStore.SaveMoodEntry(entry)
	if err != nil {
		return 0, err
	}
	s.writeOutbox("mood_entries", id, "upsert")
	return id, nil
}

// UpdateMoodEntry refines an existing mood entry locally and records in outbox.
func (s *SyncedStore) UpdateMoodEntry(id int64, entry *MoodEntry) error {
	if err := s.SQLiteStore.UpdateMoodEntry(id, entry); err != nil {
		return err
	}
	s.writeOutbox("mood_entries", id, "upsert")
	return nil
}

// DeleteMoodEntry removes a mood entry locally and records a delete in outbox.
func (s *SyncedStore) DeleteMoodEntry(id int64) error {
	if err := s.SQLiteStore.DeleteMoodEntry(id); err != nil {
		return err
	}
	s.writeOutbox("mood_entries", id, "delete")
	return nil
}

// Compile-time check: SyncedStore must satisfy the Store interface.
// This is a Go idiom — the blank identifier _ discards the value, but
// the compiler still verifies the type assertion. If SyncedStore is
// missing any Store methods, this line won't compile. Think of it like
// Python's abc.ABCMeta but checked at build time instead of import time.
var _ Store = (*SyncedStore)(nil)

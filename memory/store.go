// Package memory provides SQLite-backed storage for conversations, facts,
// PII vault entries, and metrics. Everything lives in one database file.
package memory

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"time"

	// The underscore import is a Go idiom: it imports the package purely for
	// its side effects (registering the SQLite driver with database/sql).
	// The package's init() function runs at startup and calls sql.Register().
	// You'll never call go-sqlite3 functions directly — you talk to it
	// through Go's standard database/sql interface.
	_ "github.com/mattn/go-sqlite3"
)

// Store wraps a SQLite database connection and provides methods for
// reading/writing messages, facts, metrics, and PII vault entries.
// In Go, this is how you build something like a Python class — a struct
// with methods attached to it.
type Store struct {
	db *sql.DB
}

// Message represents a single conversation message (user or assistant).
type Message struct {
	ID              int64
	Timestamp       time.Time
	Role            string // "user" or "assistant"
	ContentRaw      string // original unscrubbed message
	ContentScrubbed string // PII-scrubbed version sent to LLM
	ConversationID  string
	TokenCount      int
}

// Metric represents token usage and cost data for a single LLM call.
type Metric struct {
	ID               int64
	Timestamp        time.Time
	Model            string
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	CostUSD          float64
	LatencyMs        int
	MessageID        int64
}

// NewStore opens (or creates) the SQLite database at the given path
// and initializes all tables. The database file is created automatically
// by SQLite if it doesn't exist — no setup step needed.
func NewStore(dbPath string) (*Store, error) {
	// sql.Open doesn't actually connect — it just validates the driver name
	// and prepares the connection. The real connection happens on first query.
	// The "?_journal_mode=WAL" enables Write-Ahead Logging, which allows
	// concurrent reads while writing. Much better performance for our use case.
	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_foreign_keys=on")
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}

	// Ping actually tests the connection. This is where we'd catch issues
	// like bad file permissions or a corrupt database.
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("connecting to database: %w", err)
	}

	store := &Store{db: db}

	// Create all tables if they don't exist.
	if err := store.initTables(); err != nil {
		return nil, fmt.Errorf("initializing tables: %w", err)
	}

	return store, nil
}

// Close cleanly shuts down the database connection.
// Always call this when the app exits (usually via defer in main.go).
func (s *Store) Close() error {
	return s.db.Close()
}

// initTables creates all the tables defined in the spec. Using
// IF NOT EXISTS means this is safe to call every time the app starts —
// existing tables won't be touched.
func (s *Store) initTables() error {
	tables := []string{
		`CREATE TABLE IF NOT EXISTS messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
			role TEXT NOT NULL,
			content_raw TEXT NOT NULL,
			content_scrubbed TEXT,
			conversation_id TEXT,
			token_count INTEGER,
			voice_memo_path TEXT
		)`,

		`CREATE TABLE IF NOT EXISTS facts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
			fact TEXT NOT NULL,
			category TEXT,
			source_message_id INTEGER,
			importance INTEGER DEFAULT 5,
			active BOOLEAN DEFAULT 1,
			FOREIGN KEY (source_message_id) REFERENCES messages(id)
		)`,

		`CREATE TABLE IF NOT EXISTS summaries (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
			conversation_id TEXT,
			summary TEXT NOT NULL,
			messages_start_id INTEGER,
			messages_end_id INTEGER
		)`,

		`CREATE TABLE IF NOT EXISTS pii_vault (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			message_id INTEGER,
			token TEXT NOT NULL,
			original_value TEXT NOT NULL,
			entity_type TEXT NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (message_id) REFERENCES messages(id)
		)`,

		`CREATE TABLE IF NOT EXISTS reminders (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			trigger_at DATETIME NOT NULL,
			message TEXT NOT NULL,
			delivered BOOLEAN DEFAULT 0
		)`,

		`CREATE TABLE IF NOT EXISTS persona_versions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
			content TEXT NOT NULL,
			trigger TEXT,
			conversation_count INTEGER,
			reflection_ids TEXT
		)`,

		`CREATE TABLE IF NOT EXISTS traits (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
			trait_name TEXT NOT NULL,
			value TEXT NOT NULL,
			persona_version_id INTEGER,
			FOREIGN KEY (persona_version_id) REFERENCES persona_versions(id)
		)`,

		`CREATE TABLE IF NOT EXISTS metrics (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
			model TEXT NOT NULL,
			prompt_tokens INTEGER,
			completion_tokens INTEGER,
			total_tokens INTEGER,
			cost_usd REAL,
			latency_ms INTEGER,
			message_id INTEGER,
			FOREIGN KEY (message_id) REFERENCES messages(id)
		)`,
	}

	// Execute each CREATE TABLE statement. In Go, range is like Python's
	// enumerate() — it gives you both the index and value. We use _ to
	// ignore the index since we don't need it (the "blank identifier").
	for _, query := range tables {
		if _, err := s.db.Exec(query); err != nil {
			return fmt.Errorf("creating table: %w", err)
		}
	}

	// Migrations — add columns to existing tables.
	// ALTER TABLE ADD COLUMN is safe to run repeatedly in SQLite —
	// it errors if the column already exists, which we just ignore.
	// This is a simple migration strategy: no migration files, no
	// version tracking, just idempotent ALTER statements.
	migrations := []string{
		// subject: "user" for facts about the user, "self" for Mira's
		// own self-knowledge (observations, patterns, identity).
		`ALTER TABLE facts ADD COLUMN subject TEXT DEFAULT 'user'`,
		// embedding: cached vector from the embedding model, stored as
		// raw bytes ([]float64 serialized with binary.LittleEndian).
		// Avoids re-computing embeddings on every duplicate check.
		`ALTER TABLE facts ADD COLUMN embedding BLOB`,
	}
	for _, m := range migrations {
		s.db.Exec(m) // ignore errors (column already exists)
	}

	return nil
}

// SaveMessage inserts a message into the database and returns its ID.
// This is called for both user messages and assistant responses.
func (s *Store) SaveMessage(role, contentRaw, contentScrubbed, conversationID string) (int64, error) {
	result, err := s.db.Exec(
		`INSERT INTO messages (role, content_raw, content_scrubbed, conversation_id)
		 VALUES (?, ?, ?, ?)`,
		role, contentRaw, contentScrubbed, conversationID,
	)
	if err != nil {
		return 0, fmt.Errorf("saving message: %w", err)
	}

	// LastInsertId returns the auto-generated ID. This is a method on
	// sql.Result — the object returned by Exec for INSERT/UPDATE/DELETE.
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("getting message ID: %w", err)
	}

	return id, nil
}

// RecentMessages retrieves the last N messages for a conversation,
// ordered oldest-first so they can be fed directly into the LLM prompt.
func (s *Store) RecentMessages(conversationID string, limit int) ([]Message, error) {
	// The subquery grabs the last N rows (newest first), then the outer
	// query flips them to chronological order for the prompt.
	rows, err := s.db.Query(
		`SELECT id, timestamp, role, content_raw, content_scrubbed, conversation_id
		 FROM (
			SELECT id, timestamp, role, content_raw, content_scrubbed, conversation_id
			FROM messages
			WHERE conversation_id = ?
			ORDER BY id DESC
			LIMIT ?
		 ) sub ORDER BY id ASC`,
		conversationID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("querying recent messages: %w", err)
	}
	// defer runs when the enclosing function returns — it's Go's cleanup
	// mechanism. Like Python's "with" statement for context managers.
	// Always defer rows.Close() to avoid leaking database connections.
	defer rows.Close()

	var messages []Message
	// rows.Next() advances to the next row, like Python's iterator protocol.
	// When there are no more rows, it returns false and the loop exits.
	for rows.Next() {
		var m Message
		var ts string
		var scrubbed sql.NullString

		// Scan reads column values into Go variables. The order must match
		// the SELECT column order exactly. sql.NullString handles NULL values —
		// regular strings can't represent NULL in Go.
		if err := rows.Scan(&m.ID, &ts, &m.Role, &m.ContentRaw, &scrubbed, &m.ConversationID); err != nil {
			return nil, fmt.Errorf("scanning message row: %w", err)
		}

		m.Timestamp, _ = time.Parse("2006-01-02 15:04:05", ts)
		if scrubbed.Valid {
			m.ContentScrubbed = scrubbed.String
		}
		messages = append(messages, m)
	}

	return messages, nil
}

// SaveMetric logs token usage and cost data for an LLM call.
// If messageID is 0, it's stored as NULL (e.g., for agent calls).
func (s *Store) SaveMetric(model string, promptTokens, completionTokens, totalTokens int, costUSD float64, latencyMs int, messageID int64) error {
	var msgID interface{} = messageID
	if messageID == 0 {
		msgID = nil
	}
	_, err := s.db.Exec(
		`INSERT INTO metrics (model, prompt_tokens, completion_tokens, total_tokens, cost_usd, latency_ms, message_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		model, promptTokens, completionTokens, totalTokens, costUSD, latencyMs, msgID,
	)
	if err != nil {
		return fmt.Errorf("saving metric: %w", err)
	}
	return nil
}

// UpdateMessageScrubbed updates the scrubbed content for a message.
// We save the raw message first (for data safety), then update with the
// scrubbed version after PII processing completes.
func (s *Store) UpdateMessageScrubbed(messageID int64, scrubbed string) error {
	_, err := s.db.Exec(
		`UPDATE messages SET content_scrubbed = ? WHERE id = ?`,
		scrubbed, messageID,
	)
	if err != nil {
		return fmt.Errorf("updating scrubbed content: %w", err)
	}
	return nil
}

// UpdateMessageTokenCount sets the token count for a message after the
// LLM responds. For user messages this is the prompt token count, for
// assistant messages it's the completion token count.
func (s *Store) UpdateMessageTokenCount(messageID int64, tokenCount int) error {
	_, err := s.db.Exec(
		`UPDATE messages SET token_count = ? WHERE id = ?`,
		tokenCount, messageID,
	)
	if err != nil {
		return fmt.Errorf("updating token count: %w", err)
	}
	return nil
}

// Fact represents an extracted piece of long-term memory.
// Subject is "user" for facts about the user, or "self" for Mira's
// own self-knowledge (her identity, observations, patterns).
type Fact struct {
	ID              int64
	Timestamp       time.Time
	Fact            string
	Category        string
	Subject         string    // "user" or "self"
	SourceMessageID int64
	Importance      int
	Active          bool
	Embedding       []float64 // cached embedding vector (nil if not yet computed)
}

// encodeEmbedding serializes a float64 slice to bytes for SQLite BLOB storage.
// Each float64 is 8 bytes, written in little-endian order.
//
// This is like Python's struct.pack() or numpy's .tobytes() — converting
// in-memory floats to a compact binary representation. We use LittleEndian
// because that's what most modern CPUs use natively (x86, ARM).
func encodeEmbedding(vec []float64) []byte {
	if len(vec) == 0 {
		return nil
	}
	buf := make([]byte, len(vec)*8)
	for i, v := range vec {
		binary.LittleEndian.PutUint64(buf[i*8:], math.Float64bits(v))
	}
	return buf
}

// decodeEmbedding deserializes bytes back into a float64 slice.
func decodeEmbedding(data []byte) []float64 {
	if len(data) == 0 || len(data)%8 != 0 {
		return nil
	}
	vec := make([]float64, len(data)/8)
	for i := range vec {
		vec[i] = math.Float64frombits(binary.LittleEndian.Uint64(data[i*8:]))
	}
	return vec
}

// SaveFact inserts an extracted fact into the database.
// subject is "user" or "self". If sourceMessageID is 0, it's stored as NULL.
// embedding is optional — pass nil if not yet computed.
func (s *Store) SaveFact(fact, category, subject string, sourceMessageID int64, importance int, embedding []float64) (int64, error) {
	var srcID interface{} = sourceMessageID
	if sourceMessageID == 0 {
		srcID = nil
	}
	if subject == "" {
		subject = "user"
	}

	// Encode the embedding to bytes for BLOB storage.
	var embBlob interface{}
	if len(embedding) > 0 {
		embBlob = encodeEmbedding(embedding)
	}

	result, err := s.db.Exec(
		`INSERT INTO facts (fact, category, subject, source_message_id, importance, embedding)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		fact, category, subject, srcID, importance, embBlob,
	)
	if err != nil {
		return 0, fmt.Errorf("saving fact: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("getting fact ID: %w", err)
	}
	return id, nil
}

// UpdateFactEmbedding sets the cached embedding for a fact that was
// saved without one (e.g., facts created before embeddings were enabled).
func (s *Store) UpdateFactEmbedding(factID int64, embedding []float64) error {
	_, err := s.db.Exec(
		`UPDATE facts SET embedding = ? WHERE id = ?`,
		encodeEmbedding(embedding), factID,
	)
	if err != nil {
		return fmt.Errorf("updating embedding for fact %d: %w", factID, err)
	}
	return nil
}

// RecentFacts retrieves the top-K active facts for a given subject,
// ordered by importance (descending) then recency (descending).
func (s *Store) RecentFacts(subject string, limit int) ([]Fact, error) {
	rows, err := s.db.Query(
		`SELECT id, timestamp, fact, category, COALESCE(subject, 'user'), importance, embedding
		 FROM facts
		 WHERE active = 1 AND COALESCE(subject, 'user') = ?
		 ORDER BY importance DESC, timestamp DESC
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
		if err := rows.Scan(&f.ID, &ts, &f.Fact, &f.Category, &f.Subject, &f.Importance, &embData); err != nil {
			return nil, fmt.Errorf("scanning fact row: %w", err)
		}
		f.Timestamp, _ = time.Parse("2006-01-02 15:04:05", ts)
		f.Active = true
		f.Embedding = decodeEmbedding(embData)
		facts = append(facts, f)
	}
	return facts, nil
}

// MessageCountSince counts how many user messages exist in a conversation
// after a given message ID. Used to decide when to trigger fact extraction.
func (s *Store) MessageCountSince(conversationID string, sinceID int64) (int, error) {
	var count int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM messages
		 WHERE conversation_id = ? AND id > ? AND role = 'user'`,
		conversationID, sinceID,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("counting messages: %w", err)
	}
	return count, nil
}

// MessagesAfter retrieves all messages in a conversation after a given ID.
// Used by fact extraction to get the batch of messages to analyze.
func (s *Store) MessagesAfter(conversationID string, sinceID int64) ([]Message, error) {
	rows, err := s.db.Query(
		`SELECT id, timestamp, role, content_raw, content_scrubbed, conversation_id
		 FROM messages
		 WHERE conversation_id = ? AND id > ?
		 ORDER BY id ASC`,
		conversationID, sinceID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying messages after %d: %w", sinceID, err)
	}
	defer rows.Close()

	var messages []Message
	for rows.Next() {
		var m Message
		var ts string
		var scrubbed sql.NullString
		if err := rows.Scan(&m.ID, &ts, &m.Role, &m.ContentRaw, &scrubbed, &m.ConversationID); err != nil {
			return nil, fmt.Errorf("scanning message row: %w", err)
		}
		m.Timestamp, _ = time.Parse("2006-01-02 15:04:05", ts)
		if scrubbed.Valid {
			m.ContentScrubbed = scrubbed.String
		}
		messages = append(messages, m)
	}
	return messages, nil
}

// LastExtractionMessageID returns the highest source_message_id in the
// facts table for tracking where the last extraction left off. Returns 0
// if no facts exist yet.
func (s *Store) LastExtractionMessageID() (int64, error) {
	var id sql.NullInt64
	err := s.db.QueryRow(
		`SELECT MAX(source_message_id) FROM facts`,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("querying last extraction ID: %w", err)
	}
	if id.Valid {
		return id.Int64, nil
	}
	return 0, nil
}

// UpdateFact modifies an existing fact's text, category, or importance.
func (s *Store) UpdateFact(factID int64, fact, category string, importance int) error {
	_, err := s.db.Exec(
		`UPDATE facts SET fact = ?, category = ?, importance = ? WHERE id = ?`,
		fact, category, importance, factID,
	)
	if err != nil {
		return fmt.Errorf("updating fact %d: %w", factID, err)
	}
	return nil
}

// DeactivateFact soft-deletes a fact by setting active = 0.
// The fact stays in the DB for audit trail but won't appear in retrieval.
func (s *Store) DeactivateFact(factID int64) error {
	_, err := s.db.Exec(
		`UPDATE facts SET active = 0 WHERE id = ?`,
		factID,
	)
	if err != nil {
		return fmt.Errorf("deactivating fact %d: %w", factID, err)
	}
	return nil
}

// AllActiveFacts returns every active fact (both user and self).
// Used by the agent to see the full memory state when deciding
// what to update or consolidate. Includes cached embeddings.
func (s *Store) AllActiveFacts() ([]Fact, error) {
	rows, err := s.db.Query(
		`SELECT id, timestamp, fact, category, COALESCE(subject, 'user'), importance, embedding
		 FROM facts WHERE active = 1
		 ORDER BY subject ASC, importance DESC, timestamp DESC`,
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
		if err := rows.Scan(&f.ID, &ts, &f.Fact, &f.Category, &f.Subject, &f.Importance, &embData); err != nil {
			return nil, fmt.Errorf("scanning fact row: %w", err)
		}
		f.Timestamp, _ = time.Parse("2006-01-02 15:04:05", ts)
		f.Active = true
		f.Embedding = decodeEmbedding(embData)
		facts = append(facts, f)
	}
	return facts, nil
}

// SavePersonaVersion stores a snapshot of persona.md content in the
// persona_versions table. Every rewrite is preserved for history/rollback.
func (s *Store) SavePersonaVersion(content, trigger string) (int64, error) {
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

// Stats holds aggregate usage statistics for the /stats command.
type Stats struct {
	TotalMessages    int
	UserMessages     int
	MiraMessages     int
	TotalFacts       int
	UserFacts        int
	SelfFacts        int
	TotalTokens      int
	TotalCostUSD     float64
	ChatTokens       int
	ChatCostUSD      float64
	AgentTokens      int
	AgentCostUSD     float64
	AvgLatencyMs     int
	ConversationDays int // how many distinct days have messages
}

// GetStats computes aggregate usage statistics across all data.
// Uses several small queries rather than one giant join — clearer
// and fast enough for our scale.
func (s *Store) GetStats() (*Stats, error) {
	st := &Stats{}

	// Message counts by role.
	s.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&st.TotalMessages)
	s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE role = 'user'`).Scan(&st.UserMessages)
	s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE role = 'assistant'`).Scan(&st.MiraMessages)

	// Fact counts by subject.
	s.db.QueryRow(`SELECT COUNT(*) FROM facts WHERE active = 1`).Scan(&st.TotalFacts)
	s.db.QueryRow(`SELECT COUNT(*) FROM facts WHERE active = 1 AND COALESCE(subject, 'user') = 'user'`).Scan(&st.UserFacts)
	s.db.QueryRow(`SELECT COUNT(*) FROM facts WHERE active = 1 AND COALESCE(subject, 'user') = 'self'`).Scan(&st.SelfFacts)

	// Token + cost totals, split by chat vs agent model.
	// Chat models have latency_ms > 0 (agent calls log latency as 0).
	s.db.QueryRow(`SELECT COALESCE(SUM(total_tokens), 0), COALESCE(SUM(cost_usd), 0) FROM metrics`).Scan(&st.TotalTokens, &st.TotalCostUSD)
	s.db.QueryRow(`SELECT COALESCE(SUM(total_tokens), 0), COALESCE(SUM(cost_usd), 0) FROM metrics WHERE latency_ms > 0`).Scan(&st.ChatTokens, &st.ChatCostUSD)
	s.db.QueryRow(`SELECT COALESCE(SUM(total_tokens), 0), COALESCE(SUM(cost_usd), 0) FROM metrics WHERE latency_ms = 0`).Scan(&st.AgentTokens, &st.AgentCostUSD)

	// Average chat latency (exclude agent calls which have 0 latency).
	s.db.QueryRow(`SELECT COALESCE(AVG(latency_ms), 0) FROM metrics WHERE latency_ms > 0`).Scan(&st.AvgLatencyMs)

	// Distinct days with messages (gives a sense of how many days active).
	s.db.QueryRow(`SELECT COUNT(DISTINCT DATE(timestamp)) FROM messages`).Scan(&st.ConversationDays)

	return st, nil
}

// FindFactsByKeyword searches active facts for a keyword match.
// Used by /forget to help the user find facts to deactivate.
func (s *Store) FindFactsByKeyword(keyword string) ([]Fact, error) {
	rows, err := s.db.Query(
		`SELECT id, timestamp, fact, category, COALESCE(subject, 'user'), importance, embedding
		 FROM facts
		 WHERE active = 1 AND fact LIKE '%' || ? || '%'
		 ORDER BY importance DESC
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
		if err := rows.Scan(&f.ID, &ts, &f.Fact, &f.Category, &f.Subject, &f.Importance, &embData); err != nil {
			return nil, fmt.Errorf("scanning fact row: %w", err)
		}
		f.Timestamp, _ = time.Parse("2006-01-02 15:04:05", ts)
		f.Active = true
		f.Embedding = decodeEmbedding(embData)
		facts = append(facts, f)
	}
	return facts, nil
}

// SavePIIVaultEntry persists a Tier 2 token↔original mapping for audit trail.
func (s *Store) SavePIIVaultEntry(messageID int64, token, originalValue, entityType string) error {
	_, err := s.db.Exec(
		`INSERT INTO pii_vault (message_id, token, original_value, entity_type)
		 VALUES (?, ?, ?, ?)`,
		messageID, token, originalValue, entityType,
	)
	if err != nil {
		return fmt.Errorf("saving PII vault entry: %w", err)
	}
	return nil
}

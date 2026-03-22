// Package memory provides SQLite-backed storage for conversations, facts,
// PII vault entries, and metrics. Everything lives in one database file.
package memory

import (
	"database/sql"
	"fmt"
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
func (s *Store) SaveMetric(model string, promptTokens, completionTokens, totalTokens int, costUSD float64, latencyMs int, messageID int64) error {
	_, err := s.db.Exec(
		`INSERT INTO metrics (model, prompt_tokens, completion_tokens, total_tokens, cost_usd, latency_ms, message_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		model, promptTokens, completionTokens, totalTokens, costUSD, latencyMs, messageID,
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

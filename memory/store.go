// Package memory provides SQLite-backed storage for conversations, facts,
// PII vault entries, and metrics. Everything lives in one database file.
package memory

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	// The underscore import is a Go idiom: it imports the package purely for
	// its side effects (registering the SQLite driver with database/sql).
	// The package's init() function runs at startup and calls sql.Register().
	// You'll never call go-sqlite3 functions directly — you talk to it
	// through Go's standard database/sql interface.
	_ "github.com/mattn/go-sqlite3"

	// sqlite-vec adds vector search to SQLite via a virtual table module.
	// Auto() registers it as an auto-extension so every new connection gets it.
	// The cgo sub-package works with mattn/go-sqlite3 (our SQLite driver).
	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
)

// Store wraps a SQLite database connection and provides methods for
// reading/writing messages, facts, metrics, and PII vault entries.
// In Go, this is how you build something like a Python class — a struct
// with methods attached to it.
type Store struct {
	db             *sql.DB
	EmbedDimension int // vector dimension for the vec_facts table (e.g. 768)
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
//
// embedDimension is the vector size for the sqlite-vec index (e.g. 768
// for nomic-embed-text-v1.5). Pass 0 to skip creating the vector table
// (useful if embeddings aren't configured).
func NewStore(dbPath string, embedDimension int) (*Store, error) {
	// Register sqlite-vec as an auto-extension BEFORE opening any connections.
	// This uses sqlite3_auto_extension() under the hood — every new connection
	// automatically loads the vec0 virtual table module. Think of it like
	// Python's sqlite3.enable_load_extension(), but baked into the driver.
	sqlite_vec.Auto()

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

	store := &Store{db: db, EmbedDimension: embedDimension}

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

		// Scheduled tasks — the scheduler's task table. Supports one-shot
		// reminders (v0.2), recurring cron jobs, and conditional tasks (v0.6).
		// The scheduler polls this every minute for tasks where next_run <= now.
		// All state lives here so tasks survive restarts.
		`CREATE TABLE IF NOT EXISTS scheduled_tasks (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT,
			schedule_type TEXT NOT NULL,
			cron_expr TEXT,
			trigger_at DATETIME,
			task_type TEXT NOT NULL,
			payload JSON NOT NULL,
			enabled BOOLEAN DEFAULT 1,
			last_run DATETIME,
			next_run DATETIME,
			run_count INTEGER DEFAULT 0,
			max_runs INTEGER,
			priority TEXT NOT NULL DEFAULT 'normal',
			created_by TEXT DEFAULT 'user',
			source_message_id INTEGER,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (source_message_id) REFERENCES messages(id)
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

		// Searches — full audit trail of web/book/URL lookups.
		// Tracks what the agent searched for, what it got back, and
		// which user message triggered it. Essential for debugging
		// agent tool-call decisions and tuning search behavior.
		`CREATE TABLE IF NOT EXISTS searches (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
			message_id INTEGER,
			search_type TEXT NOT NULL,
			query TEXT NOT NULL,
			results TEXT,
			result_count INTEGER,
			FOREIGN KEY (message_id) REFERENCES messages(id)
		)`,

		// Agent turns — full trace of the agent's reasoning and tool calls.
		// Each row is one step in the agent loop: a think, a tool call,
		// a tool result, etc. Together they reconstruct the full chain
		// of reasoning for any given user message.
		`CREATE TABLE IF NOT EXISTS agent_turns (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
			message_id INTEGER,
			turn_index INTEGER,
			role TEXT NOT NULL,
			tool_name TEXT,
			tool_args TEXT,
			content TEXT,
			FOREIGN KEY (message_id) REFERENCES messages(id)
		)`,

		// Mood entries — tracks the user's emotional state over time.
		// Sources: "inferred" (LLM guesses from conversation), "manual"
		// (Mira logs it from what the user says), "checkin" (proactive
		// inline keyboard check-ins in v0.6).
		// Rating is 1-5: 1=bad, 2=rough, 3=meh, 4=good, 5=great.
		// Tags is a JSON object for structured metadata (energy, stress, etc.).
		`CREATE TABLE IF NOT EXISTS mood_entries (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
			rating INTEGER NOT NULL,
			note TEXT,
			tags TEXT,
			source TEXT DEFAULT 'inferred',
			conversation_id TEXT
		)`,

		// Expenses — structured financial data from receipt scanning.
		// This table is intentionally SEPARATE from facts — individual
		// transactions are not "memories" and should never pollute the
		// fact store. The agent saves high-level financial patterns as
		// facts (e.g., "user is budgeting carefully") but individual
		// purchases stay here.
		//
		// category is a fixed enum enforced in the agent tool handler.
		// date is stored as TEXT in YYYY-MM-DD format (SQLite doesn't
		// have a native DATE type — TEXT with a consistent format is
		// the standard approach, and it sorts correctly).
		`CREATE TABLE IF NOT EXISTS expenses (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			amount REAL NOT NULL,
			currency TEXT DEFAULT 'USD',
			vendor TEXT,
			category TEXT NOT NULL,
			date TEXT NOT NULL,
			note TEXT,
			source_message_id INTEGER,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (source_message_id) REFERENCES messages(id)
		)`,

		// Expense line items — individual items from a receipt.
		// Linked to the parent expense via expense_id. This allows
		// Financial Pulse to answer "how much did I spend on beverages?"
		// by querying across items, not just receipt totals.
		`CREATE TABLE IF NOT EXISTS expense_items (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			expense_id INTEGER NOT NULL,
			description TEXT NOT NULL,
			quantity INTEGER DEFAULT 1,
			unit_price REAL,
			total_price REAL,
			FOREIGN KEY (expense_id) REFERENCES expenses(id)
		)`,

		// Pending confirmations — stores actions that need user approval
		// via inline keyboard buttons before executing. Keyed by the
		// Telegram message ID of the confirmation message, so the callback
		// handler can look it up when the user clicks Yes or No.
		//
		// This is the infra behind the reply_confirm agent tool. The agent
		// sends a confirmation prompt with buttons, and the action_type +
		// action_payload describe what to execute on confirmation. Resolved
		// confirmations are kept for audit (resolved_at + resolved_action).
		`CREATE TABLE IF NOT EXISTS pending_confirmations (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			telegram_msg_id INTEGER NOT NULL,
			action_type TEXT NOT NULL,
			action_payload JSON NOT NULL,
			description TEXT NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			resolved_at DATETIME,
			resolved_action TEXT
		)`,

		// Reflections — Mira's private journal entries written after
		// memory-dense conversations. Separate from facts because reflections
		// are holistic processing, not discrete pieces of information.
		// fact_count: how many new facts triggered this reflection.
		// user_message / mira_response: the exchange that sparked it.
		`CREATE TABLE IF NOT EXISTS reflections (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
			content TEXT NOT NULL,
			fact_count INTEGER,
			user_message TEXT,
			mira_response TEXT
		)`,

		// Command log — tracks every slash command the user runs.
		// Useful for understanding usage patterns (how often /clear
		// is used, whether /compact is needed manually, etc.).
		`CREATE TABLE IF NOT EXISTS command_log (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
			command TEXT NOT NULL,
			chat_id INTEGER,
			conversation_id TEXT,
			args TEXT
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
		// media_file_id: Telegram file_id for photos/voice/documents.
		// Lets us re-download the file later via the Telegram API.
		`ALTER TABLE messages ADD COLUMN media_file_id TEXT`,
		// media_description: VLM-generated description of an attached image.
		// Stored alongside the message so we have a text record of what
		// the bot "saw" — useful for memory, search, and debugging.
		`ALTER TABLE messages ADD COLUMN media_description TEXT`,
		// voice_memo_path: path to original audio file for voice messages.
		// Already in the CREATE TABLE for new DBs, but existing DBs need this.
		`ALTER TABLE messages ADD COLUMN voice_memo_path TEXT`,
		// priority: "normal", "high", or "critical". Controls which damping
		// checks a task is subject to. Critical tasks (reminders, medication)
		// always fire. Existing tasks get "normal" which is the safe default.
		`ALTER TABLE scheduled_tasks ADD COLUMN priority TEXT NOT NULL DEFAULT 'normal'`,
		// tags: comma-separated topic descriptors for semantic search.
		// Facts are embedded by tags (not by fact text) so the vector space
		// organizes by TOPIC rather than by surface-level word overlap.
		// This prevents "User has burnout" from matching "tell me about code."
		`ALTER TABLE facts ADD COLUMN tags TEXT`,
		// embedding_text: cached text-based embedding vector (float32 BLOB).
		// Unlike the tag embedding (used for KNN/vec_facts), this is the
		// embedding of the raw fact text. It's used by checkDuplicate and
		// FilterRedundantFacts to catch situational duplicates that share
		// meaning but use different tag angles. Caching it avoids repeated
		// on-the-fly embedding calls during dedup checks.
		`ALTER TABLE facts ADD COLUMN embedding_text BLOB`,
	}
	for _, m := range migrations {
		s.db.Exec(m) // ignore errors (column already exists)
	}

	// Indexes — CREATE INDEX IF NOT EXISTS is idempotent like the tables.
	// This index lets the scheduler's polling loop quickly find tasks that
	// are due to run: it only includes enabled tasks, sorted by next_run.
	indexes := []string{
		`CREATE INDEX IF NOT EXISTS idx_scheduled_tasks_next_run
		 ON scheduled_tasks(next_run) WHERE enabled = 1`,

		// Expense indexes — date index for "how much this week/month" queries,
		// category index for per-category breakdowns. Both will be heavily
		// used by Financial Pulse (phase 2).
		`CREATE INDEX IF NOT EXISTS idx_expenses_date ON expenses(date)`,
		`CREATE INDEX IF NOT EXISTS idx_expenses_category ON expenses(category)`,
	}
	for _, idx := range indexes {
		if _, err := s.db.Exec(idx); err != nil {
			return fmt.Errorf("creating index: %w", err)
		}
	}

	// sqlite-vec virtual table for KNN vector search on fact embeddings.
	// vec0 is the virtual table module provided by the sqlite-vec extension.
	// The rowid maps to facts.id so we can JOIN back for metadata.
	// distance_metric=cosine means KNN results are ranked by cosine distance
	// (0 = identical, 2 = opposite) instead of the default L2/Euclidean.
	//
	// Virtual tables are a SQLite concept with no direct Python equivalent —
	// think of them as tables backed by a custom engine. The vec0 engine
	// stores vectors in an optimized format and implements fast approximate
	// nearest neighbor search behind the standard SQL interface.
	if s.EmbedDimension > 0 {
		vecDDL := fmt.Sprintf(
			`CREATE VIRTUAL TABLE IF NOT EXISTS vec_facts USING vec0(embedding float[%d] distance_metric=cosine)`,
			s.EmbedDimension,
		)
		if _, err := s.db.Exec(vecDDL); err != nil {
			return fmt.Errorf("creating vec_facts virtual table: %w", err)
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

// GlobalRecentMessages retrieves the last N messages across ALL conversations,
// ordered oldest-first. Used by /reflect which needs recent context regardless
// of which conversation ID they belong to.
func (s *Store) GlobalRecentMessages(limit int) ([]Message, error) {
	rows, err := s.db.Query(
		`SELECT id, timestamp, role, content_raw, content_scrubbed, conversation_id
		 FROM (
			SELECT id, timestamp, role, content_raw, content_scrubbed, conversation_id
			FROM messages
			ORDER BY id DESC
			LIMIT ?
		 ) sub ORDER BY id ASC`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("querying global recent messages: %w", err)
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

// SaveAgentTurn logs a single step in the agent's reasoning chain.
// turnIndex is the sequential position within the agent run (0, 1, 2...).
// role is "assistant" (agent's decision) or "tool" (tool result).
func (s *Store) SaveAgentTurn(messageID int64, turnIndex int, role, toolName, toolArgs, content string) error {
	var msgID interface{} = messageID
	if messageID == 0 {
		msgID = nil
	}
	_, err := s.db.Exec(
		`INSERT INTO agent_turns (message_id, turn_index, role, tool_name, tool_args, content)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		msgID, turnIndex, role, toolName, toolArgs, content,
	)
	if err != nil {
		return fmt.Errorf("saving agent turn: %w", err)
	}
	return nil
}

// SaveSearch logs a search operation (web, book, or URL read) for
// full observability. Tracks what was searched, what came back, and
// which user message triggered it.
func (s *Store) SaveSearch(messageID int64, searchType, query, results string, resultCount int) error {
	var msgID interface{} = messageID
	if messageID == 0 {
		msgID = nil
	}
	_, err := s.db.Exec(
		`INSERT INTO searches (message_id, search_type, query, results, result_count)
		 VALUES (?, ?, ?, ?, ?)`,
		msgID, searchType, query, results, resultCount,
	)
	if err != nil {
		return fmt.Errorf("saving search: %w", err)
	}
	return nil
}

// LogCommand records a slash command the user ran. This goes into the
// command_log table for usage analytics — how often /clear is used, etc.
func (s *Store) LogCommand(command string, chatID int64, conversationID, args string) {
	_, err := s.db.Exec(
		`INSERT INTO command_log (command, chat_id, conversation_id, args)
		 VALUES (?, ?, ?, ?)`,
		command, chatID, conversationID, args,
	)
	if err != nil {
		log.Error("saving command log", "err", err)
	}
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

// UpdateMessageMedia stores the Telegram file ID and/or VLM description
// for a message that has media attached. Either field can be empty —
// we use COALESCE to only update non-empty values, so you can call this
// once for the file_id (from the bot) and again for the description
// (from the agent's view_image tool) without clobbering the other.
func (s *Store) UpdateMessageMedia(messageID int64, fileID, description string) error {
	_, err := s.db.Exec(
		`UPDATE messages SET
			media_file_id = COALESCE(NULLIF(?, ''), media_file_id),
			media_description = COALESCE(NULLIF(?, ''), media_description)
		 WHERE id = ?`,
		fileID, description, messageID,
	)
	if err != nil {
		return fmt.Errorf("updating message media: %w", err)
	}
	return nil
}

// UpdateMessageVoicePath stores the local file path to the original
// audio file for a voice memo message. Used for debugging and replay.
func (s *Store) UpdateMessageVoicePath(messageID int64, path string) error {
	_, err := s.db.Exec(
		`UPDATE messages SET voice_memo_path = ? WHERE id = ?`,
		path, messageID,
	)
	if err != nil {
		return fmt.Errorf("updating voice memo path: %w", err)
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
	Subject         string // "user" or "self"
	SourceMessageID int64
	Importance      int
	Active          bool
	Tags            string    // comma-separated topic descriptors for semantic search
	Embedding       []float32 // cached tag embedding vector (nil if not yet computed)
	EmbeddingText   []float32 // cached text embedding vector (nil if not yet computed)
	Distance        float64   // populated by SemanticSearch — cosine distance from query (0 = identical)
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
func (s *Store) SaveFact(fact, category, subject string, sourceMessageID int64, importance int, embedding []float32, embeddingText []float32, tags string) (int64, error) {
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

	result, err := s.db.Exec(
		`INSERT INTO facts (fact, category, subject, source_message_id, importance, embedding, embedding_text, tags)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		fact, category, subject, srcID, importance, embBlob, embTextBlob, tags,
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
// ordered by importance (descending) then recency (descending).
func (s *Store) RecentFacts(subject string, limit int) ([]Fact, error) {
	rows, err := s.db.Query(
		`SELECT id, timestamp, fact, category, COALESCE(subject, 'user'), importance, COALESCE(tags, ''), embedding, embedding_text
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

// AllActiveFacts returns every active fact (both user and self).
// Used by the agent to see the full memory state when deciding
// what to update or consolidate. Includes cached embeddings.
func (s *Store) AllActiveFacts() ([]Fact, error) {
	rows, err := s.db.Query(
		`SELECT id, timestamp, fact, category, COALESCE(subject, 'user'), importance, COALESCE(tags, ''), embedding, embedding_text
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
		f.EmbeddingText = deserializeEmbedding(embTextData)
		facts = append(facts, f)

		// Stop once we have enough active results.
		if len(facts) >= topK {
			break
		}
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
func (s *Store) PersonaHistory(limit int) ([]PersonaVersion, error) {
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

// SaveSummary stores a compacted summary of older messages.
// startID and endID mark the range of message IDs that were summarized.
func (s *Store) SaveSummary(conversationID, summary string, startID, endID int64) (int64, error) {
	result, err := s.db.Exec(
		`INSERT INTO summaries (conversation_id, summary, messages_start_id, messages_end_id)
		 VALUES (?, ?, ?, ?)`,
		conversationID, summary, startID, endID,
	)
	if err != nil {
		return 0, fmt.Errorf("saving summary: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("getting summary ID: %w", err)
	}
	return id, nil
}

// LatestSummary returns the most recent summary for a conversation.
// Returns empty string if no summary exists yet.
func (s *Store) LatestSummary(conversationID string) (string, int64, error) {
	var summary string
	var endID int64
	err := s.db.QueryRow(
		`SELECT summary, messages_end_id FROM summaries
		 WHERE conversation_id = ?
		 ORDER BY id DESC LIMIT 1`,
		conversationID,
	).Scan(&summary, &endID)
	if err != nil {
		return "", 0, nil // no summary yet
	}
	return summary, endID, nil
}

// MessagesInRange returns messages between startID and endID inclusive.
func (s *Store) MessagesInRange(conversationID string, startID, endID int64) ([]Message, error) {
	rows, err := s.db.Query(
		`SELECT id, timestamp, role, content_raw, content_scrubbed, conversation_id
		 FROM messages
		 WHERE conversation_id = ? AND id >= ? AND id <= ?
		 ORDER BY id ASC`,
		conversationID, startID, endID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying messages in range: %w", err)
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
// CommandCount holds usage info for a single slash command.
type CommandCount struct {
	Command string
	Count   int
}

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
	TotalCommands    int
	CommandCounts    []CommandCount // per-command breakdown, sorted by count desc
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

	// Command usage from the command_log table.
	s.db.QueryRow(`SELECT COUNT(*) FROM command_log`).Scan(&st.TotalCommands)
	rows, err := s.db.Query(
		`SELECT command, COUNT(*) as cnt FROM command_log
		 GROUP BY command ORDER BY cnt DESC`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var cc CommandCount
			if rows.Scan(&cc.Command, &cc.Count) == nil {
				st.CommandCounts = append(st.CommandCounts, cc)
			}
		}
	}

	return st, nil
}

// ModelUsage holds per-model cost and token totals for the usage command.
type ModelUsage struct {
	Model            string
	Calls            int
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	CostUSD          float64
}

// PeriodUsage holds cost totals for a time period (today, 7d, 30d, all-time).
type PeriodUsage struct {
	Label    string
	Calls    int
	Tokens   int
	CostUSD  float64
}

// UsageReport bundles everything the `her usage` command needs.
type UsageReport struct {
	Periods  []PeriodUsage
	ByModel  []ModelUsage
}

// GetUsageReport builds a complete cost/token breakdown.
// Queries the metrics table with different time windows and a per-model
// GROUP BY. Each query is small and fast — SQLite handles this easily
// at our scale.
func (s *Store) GetUsageReport() (*UsageReport, error) {
	r := &UsageReport{}

	// Time-windowed totals: today, last 7 days, last 30 days, all-time.
	// DATE('now') gives today in UTC — same timezone SQLite uses for
	// DEFAULT CURRENT_TIMESTAMP, so the windows are consistent.
	periods := []struct {
		label string
		where string // SQL WHERE clause fragment
	}{
		{"Today", "timestamp >= DATE('now')"},
		{"Last 7 days", "timestamp >= DATE('now', '-7 days')"},
		{"Last 30 days", "timestamp >= DATE('now', '-30 days')"},
		{"All time", "1=1"},
	}

	for _, p := range periods {
		var pu PeriodUsage
		pu.Label = p.label
		err := s.db.QueryRow(
			fmt.Sprintf(`SELECT COUNT(*), COALESCE(SUM(total_tokens), 0), COALESCE(SUM(cost_usd), 0)
			 FROM metrics WHERE %s`, p.where),
		).Scan(&pu.Calls, &pu.Tokens, &pu.CostUSD)
		if err != nil {
			return nil, fmt.Errorf("querying period %s: %w", p.label, err)
		}
		r.Periods = append(r.Periods, pu)
	}

	// Per-model breakdown, sorted by total cost descending.
	rows, err := s.db.Query(
		`SELECT model, COUNT(*), COALESCE(SUM(prompt_tokens), 0),
		        COALESCE(SUM(completion_tokens), 0), COALESCE(SUM(total_tokens), 0),
		        COALESCE(SUM(cost_usd), 0)
		 FROM metrics
		 GROUP BY model
		 ORDER BY SUM(cost_usd) DESC`)
	if err != nil {
		return nil, fmt.Errorf("querying model usage: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var m ModelUsage
		if err := rows.Scan(&m.Model, &m.Calls, &m.PromptTokens, &m.CompletionTokens, &m.TotalTokens, &m.CostUSD); err != nil {
			return nil, fmt.Errorf("scanning model row: %w", err)
		}
		r.ByModel = append(r.ByModel, m)
	}

	return r, nil
}

// FindFactsByKeyword searches active facts for a keyword match.
// Used by /forget to help the user find facts to deactivate.
func (s *Store) FindFactsByKeyword(keyword string) ([]Fact, error) {
	rows, err := s.db.Query(
		`SELECT id, timestamp, fact, category, COALESCE(subject, 'user'), importance, COALESCE(tags, ''), embedding
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

// ConversationCountSince counts distinct conversation IDs in messages
// created after the given timestamp. Used to determine when to trigger
// a persona rewrite (every ~20 conversations).
func (s *Store) ConversationCountSince(since time.Time) (int, error) {
	var count int
	err := s.db.QueryRow(
		`SELECT COUNT(DISTINCT conversation_id) FROM messages WHERE timestamp > ?`,
		since.Format("2006-01-02 15:04:05"),
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("counting conversations: %w", err)
	}
	return count, nil
}

// SaveReflection stores a new reflection entry in the dedicated reflections
// table. Called by persona.Reflect() after a memory-dense conversation.
func (s *Store) SaveReflection(content string, factCount int, userMessage, miraResponse string) (int64, error) {
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
func (s *Store) FactCountSinceLastReflection() (int, error) {
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
func (s *Store) TotalReflectionCount() (int, error) {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM reflections`).Scan(&count)
	return count, err
}

// PersonaRewriteCount returns how many persona rewrites have occurred.
func (s *Store) PersonaRewriteCount() (int, error) {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM persona_versions`).Scan(&count)
	return count, err
}

// LastPersonaTimestamp returns the timestamp of the most recent persona
// version. Returns zero time if no versions exist yet.
func (s *Store) LastPersonaTimestamp() (time.Time, error) {
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
func (s *Store) ReflectionsSince(since time.Time) ([]Reflection, error) {
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

// LatestConversationID returns the most recent conversation_id used
// for a given chat identifier prefix (e.g., "tg_7570137189").
// Returns empty string if no conversations exist.
// This lets the bot resume the same conversation after a restart
// instead of generating a new ID and losing context.
func (s *Store) LatestConversationID(prefix string) string {
	var convID string
	err := s.db.QueryRow(
		`SELECT conversation_id FROM messages
		 WHERE conversation_id LIKE ? || '%'
		 ORDER BY id DESC LIMIT 1`,
		prefix,
	).Scan(&convID)
	if err != nil {
		return ""
	}
	return convID
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

// ─── Scheduled Tasks ────────────────────────────────────────────────
//
// These methods power the scheduler (Section 6 of SPEC.md).
// v0.2 uses only schedule_type="once" + task_type="send_message".
// The full schema supports recurring cron jobs and conditional tasks
// for v0.6.

// ScheduledTask represents a row in the scheduled_tasks table.
// Nullable fields use pointers — in Go, a *string can be nil while
// a plain string always has a value (its zero value is ""). This is
// how Go handles SQL NULLs without the sql.NullXxx wrapper types
// (which are clunkier to work with in application code).
type ScheduledTask struct {
	ID              int64
	Name            *string         // human-readable label, nullable
	ScheduleType    string          // "once", "recurring", "conditional"
	CronExpr        *string         // cron expression for recurring tasks
	TriggerAt       *time.Time      // for one-shot tasks: when to fire
	TaskType        string          // "send_message", "run_prompt", etc.
	Payload         json.RawMessage // task-specific config as raw JSON
	Enabled         bool
	LastRun         *time.Time
	NextRun         *time.Time
	RunCount        int
	MaxRuns         *int   // nil = unlimited
	Priority        string // "normal", "high", "critical" — controls damping behavior
	CreatedBy       string // "user", "system", "agent"
	SourceMessageID *int64
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// CreateScheduledTask inserts a new task and returns its ID.
// The caller sets next_run before inserting — for one-shot tasks this
// is just trigger_at, for recurring tasks (v0.6) it's computed from
// the cron expression.
func (s *Store) CreateScheduledTask(task *ScheduledTask) (int64, error) {
	// Default priority to "normal" if not set.
	priority := task.Priority
	if priority == "" {
		priority = "normal"
	}

	result, err := s.db.Exec(
		`INSERT INTO scheduled_tasks
		 (name, schedule_type, cron_expr, trigger_at, task_type, payload,
		  enabled, next_run, max_runs, priority, created_by, source_message_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		task.Name,
		task.ScheduleType,
		task.CronExpr,
		task.TriggerAt,
		task.TaskType,
		string(task.Payload),
		task.Enabled,
		task.NextRun,
		task.MaxRuns,
		priority,
		task.CreatedBy,
		task.SourceMessageID,
	)
	if err != nil {
		return 0, fmt.Errorf("creating scheduled task: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("getting scheduled task ID: %w", err)
	}
	return id, nil
}

// GetDueTasks returns all enabled tasks whose next_run is at or before
// the given time. This is the scheduler's polling query — called every
// minute by the ticker loop.
func (s *Store) GetDueTasks(now time.Time) ([]ScheduledTask, error) {
	rows, err := s.db.Query(
		`SELECT id, name, schedule_type, cron_expr, trigger_at, task_type,
		        payload, enabled, last_run, next_run, run_count, max_runs,
		        priority, created_by, source_message_id, created_at, updated_at
		 FROM scheduled_tasks
		 WHERE enabled = 1 AND next_run <= ?
		 ORDER BY next_run ASC`,
		now,
	)
	if err != nil {
		return nil, fmt.Errorf("querying due tasks: %w", err)
	}
	// defer rows.Close() ensures the database cursor is released even
	// if we return early due to an error. Same idea as Python's "with"
	// statement — cleanup runs no matter what.
	defer rows.Close()

	return scanScheduledTasks(rows)
}

// ListActiveTasks returns all enabled tasks, ordered by next run time.
// Used by the /schedule command to show what's coming up.
func (s *Store) ListActiveTasks() ([]ScheduledTask, error) {
	rows, err := s.db.Query(
		`SELECT id, name, schedule_type, cron_expr, trigger_at, task_type,
		        payload, enabled, last_run, next_run, run_count, max_runs,
		        priority, created_by, source_message_id, created_at, updated_at
		 FROM scheduled_tasks
		 WHERE enabled = 1
		 ORDER BY next_run ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("listing active tasks: %w", err)
	}
	defer rows.Close()

	return scanScheduledTasks(rows)
}

// MarkTaskRun updates a task after execution: sets last_run to now,
// increments run_count, and sets the new next_run. If the task has
// reached max_runs, it gets disabled instead.
func (s *Store) MarkTaskRun(taskID int64, nextRun *time.Time) error {
	now := time.Now()

	// First, increment run_count and set last_run.
	_, err := s.db.Exec(
		`UPDATE scheduled_tasks
		 SET last_run = ?, run_count = run_count + 1,
		     next_run = ?, updated_at = ?
		 WHERE id = ?`,
		now, nextRun, now, taskID,
	)
	if err != nil {
		return fmt.Errorf("marking task run: %w", err)
	}

	// Auto-disable if max_runs reached. This is a separate query to
	// keep the logic clear — SQLite is fast enough that two queries
	// to the same row is negligible.
	_, err = s.db.Exec(
		`UPDATE scheduled_tasks
		 SET enabled = 0, updated_at = ?
		 WHERE id = ? AND max_runs IS NOT NULL AND run_count >= max_runs`,
		now, taskID,
	)
	if err != nil {
		return fmt.Errorf("auto-disabling task: %w", err)
	}

	return nil
}

// UpdateScheduledTaskEnabled toggles a task's enabled state.
// Used by /schedule pause and /schedule resume.
func (s *Store) UpdateScheduledTaskEnabled(taskID int64, enabled bool) error {
	result, err := s.db.Exec(
		`UPDATE scheduled_tasks SET enabled = ?, updated_at = ? WHERE id = ?`,
		enabled, time.Now(), taskID,
	)
	if err != nil {
		return fmt.Errorf("updating task enabled: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("task %d not found", taskID)
	}
	return nil
}

// DeleteScheduledTask removes a task by ID.
func (s *Store) DeleteScheduledTask(taskID int64) error {
	result, err := s.db.Exec(
		`DELETE FROM scheduled_tasks WHERE id = ?`,
		taskID,
	)
	if err != nil {
		return fmt.Errorf("deleting task: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("task %d not found", taskID)
	}
	return nil
}

// scanScheduledTasks is a helper that reads rows into ScheduledTask structs.
// Factored out because both GetDueTasks and ListActiveTasks need the
// same scanning logic. In Go, sql.Rows.Scan fills variables by position —
// the order must match your SELECT columns exactly.
func scanScheduledTasks(rows *sql.Rows) ([]ScheduledTask, error) {
	var tasks []ScheduledTask
	for rows.Next() {
		var t ScheduledTask
		var payload string
		err := rows.Scan(
			&t.ID, &t.Name, &t.ScheduleType, &t.CronExpr, &t.TriggerAt,
			&t.TaskType, &payload, &t.Enabled, &t.LastRun, &t.NextRun,
			&t.RunCount, &t.MaxRuns, &t.Priority, &t.CreatedBy, &t.SourceMessageID,
			&t.CreatedAt, &t.UpdatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("scanning scheduled task: %w", err)
		}
		t.Payload = json.RawMessage(payload)
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

// --- Scheduler Helpers (v0.6 damping + defaults) ---

// CountTasksRunToday returns the number of non-once tasks that have
// executed since midnight in the given timezone. Used by the scheduler
// to enforce the max_proactive_per_day limit.
func (s *Store) CountTasksRunToday(now time.Time) (int, error) {
	startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	var count int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM scheduled_tasks
		 WHERE schedule_type != 'once' AND last_run >= ?`,
		startOfDay,
	).Scan(&count)
	return count, err
}

// DeferTask pushes a task's next_run to a later time. Used by the
// scheduler for quiet hours (defer to end of quiet window) and
// conversation-aware deferral (defer by 30 min if user is active).
func (s *Store) DeferTask(taskID int64, until time.Time) error {
	_, err := s.db.Exec(
		`UPDATE scheduled_tasks SET next_run = ?, updated_at = ? WHERE id = ?`,
		until, time.Now(), taskID,
	)
	return err
}

// GetTaskByName finds an enabled task by name and creator. Used by
// the scheduler's ensureDefaults() to check if a system-created
// default task already exists before creating it (idempotent).
func (s *Store) GetTaskByName(name string, createdBy string) (*ScheduledTask, error) {
	rows, err := s.db.Query(
		`SELECT id, name, schedule_type, cron_expr, trigger_at, task_type,
		        payload, enabled, last_run, next_run, run_count, max_runs,
		        priority, created_by, source_message_id, created_at, updated_at
		 FROM scheduled_tasks
		 WHERE name = ? AND created_by = ?
		 LIMIT 1`,
		name, createdBy,
	)
	if err != nil {
		return nil, fmt.Errorf("querying task by name: %w", err)
	}
	defer rows.Close()

	tasks, err := scanScheduledTasks(rows)
	if err != nil {
		return nil, err
	}
	if len(tasks) == 0 {
		return nil, nil // not found — callers check for nil
	}
	return &tasks[0], nil
}

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
func (s *Store) SaveTraits(traits []Trait, personaVersionID int64) error {
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
func (s *Store) GetCurrentTraits() ([]Trait, error) {
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

// GetTraitHistory returns historical values for a single trait across
// persona versions, newest first. Useful for showing how a trait has
// drifted over time.
func (s *Store) GetTraitHistory(traitName string, limit int) ([]Trait, error) {
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

// --- Expense Tracking ---

// ExpenseItem represents an individual line item from a receipt.
// Linked to a parent Expense via ExpenseID. Stored in expense_items table.
type ExpenseItem struct {
	ID          int64
	ExpenseID   int64
	Description string
	Quantity    int
	UnitPrice   float64
	TotalPrice  float64
}

// Expense represents a single expense entry from receipt scanning or manual input.
// This data is intentionally separate from facts — financial transactions are not
// "memories" and should never end up in the facts table.
type Expense struct {
	ID              int64
	Amount          float64
	Currency        string
	Vendor          string
	Category        string
	Date            string // YYYY-MM-DD format
	Note            string
	SourceMessageID int64
	CreatedAt       time.Time
}

// SaveExpense inserts a new expense record and returns its ID.
// Called by the scan_receipt agent tool after the agent parses OCR text
// (or a manual expense mention) into structured fields.
//
// Same pattern as SaveMoodEntry — validate inputs, insert, return ID.
// The category validation happens in the agent tool handler, not here,
// since the store layer is intentionally dumb about business logic.
func (s *Store) SaveExpense(amount float64, currency, vendor, category, date, note string, sourceMessageID int64) (int64, error) {
	if currency == "" {
		currency = "USD"
	}

	// Handle nullable source_message_id — same pattern as SaveMetric.
	// In Go, interface{} (now called 'any') can hold any value including nil.
	// SQL drivers treat nil as NULL. So we convert 0 → nil for the FK column.
	var srcID interface{} = sourceMessageID
	if sourceMessageID == 0 {
		srcID = nil
	}

	result, err := s.db.Exec(
		`INSERT INTO expenses (amount, currency, vendor, category, date, note, source_message_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		amount, currency, vendor, category, date, note, srcID,
	)
	if err != nil {
		return 0, fmt.Errorf("saving expense: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("getting expense ID: %w", err)
	}
	return id, nil
}

// SaveExpenseItem inserts a line item linked to a parent expense.
// Called in a loop after SaveExpense when the agent extracts individual
// items from receipt OCR text.
// DeleteExpense removes an expense and all its line items.
// Uses a transaction so both deletes succeed or neither does.
func (s *Store) DeleteExpense(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("starting transaction: %w", err)
	}
	// Delete child items first (foreign key), then parent expense.
	if _, err := tx.Exec(`DELETE FROM expense_items WHERE expense_id = ?`, id); err != nil {
		tx.Rollback()
		return fmt.Errorf("deleting expense items: %w", err)
	}
	result, err := tx.Exec(`DELETE FROM expenses WHERE id = ?`, id)
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("deleting expense: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		tx.Rollback()
		return fmt.Errorf("expense ID=%d not found", id)
	}
	return tx.Commit()
}

// UpdateExpense modifies fields on an existing expense. Only non-zero/non-empty
// values are updated — pass zero/empty to leave a field unchanged.
func (s *Store) UpdateExpense(id int64, amount float64, currency, vendor, category, date, note string) error {
	// Build SET clause dynamically — only include fields that have values.
	var sets []string
	var args []interface{}

	if amount > 0 {
		sets = append(sets, "amount = ?")
		args = append(args, amount)
	}
	if currency != "" {
		sets = append(sets, "currency = ?")
		args = append(args, currency)
	}
	if vendor != "" {
		sets = append(sets, "vendor = ?")
		args = append(args, vendor)
	}
	if category != "" {
		sets = append(sets, "category = ?")
		args = append(args, category)
	}
	if date != "" {
		sets = append(sets, "date = ?")
		args = append(args, date)
	}
	if note != "" {
		sets = append(sets, "note = ?")
		args = append(args, note)
	}

	if len(sets) == 0 {
		return fmt.Errorf("no fields to update")
	}

	query := fmt.Sprintf("UPDATE expenses SET %s WHERE id = ?", strings.Join(sets, ", "))
	args = append(args, id)

	result, err := s.db.Exec(query, args...)
	if err != nil {
		return fmt.Errorf("updating expense: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("expense ID=%d not found", id)
	}
	return nil
}

func (s *Store) SaveExpenseItem(expenseID int64, description string, quantity int, unitPrice, totalPrice float64) error {
	if quantity < 1 {
		quantity = 1
	}
	_, err := s.db.Exec(
		`INSERT INTO expense_items (expense_id, description, quantity, unit_price, total_price)
		 VALUES (?, ?, ?, ?, ?)`,
		expenseID, description, quantity, unitPrice, totalPrice,
	)
	if err != nil {
		return fmt.Errorf("saving expense item: %w", err)
	}
	return nil
}

// RecentExpenses returns the last N expenses with their line items, newest first.
// Used by the query_expenses tool to answer financial questions.
func (s *Store) RecentExpenses(limit int) ([]Expense, map[int64][]ExpenseItem, error) {
	rows, err := s.db.Query(
		`SELECT id, amount, COALESCE(currency, 'USD'), COALESCE(vendor, ''),
		        category, date, COALESCE(note, ''), COALESCE(source_message_id, 0),
		        created_at
		 FROM expenses
		 ORDER BY date DESC, created_at DESC
		 LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("querying expenses: %w", err)
	}
	defer rows.Close()

	var expenses []Expense
	for rows.Next() {
		var e Expense
		var ts string
		if err := rows.Scan(&e.ID, &e.Amount, &e.Currency, &e.Vendor,
			&e.Category, &e.Date, &e.Note, &e.SourceMessageID, &ts); err != nil {
			return nil, nil, fmt.Errorf("scanning expense: %w", err)
		}
		e.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", ts)
		expenses = append(expenses, e)
	}

	// Fetch line items for all returned expenses.
	items := make(map[int64][]ExpenseItem)
	for _, e := range expenses {
		itemRows, err := s.db.Query(
			`SELECT id, expense_id, description, quantity,
			        COALESCE(unit_price, 0), COALESCE(total_price, 0)
			 FROM expense_items WHERE expense_id = ?`,
			e.ID,
		)
		if err != nil {
			continue // non-fatal — expense still useful without items
		}
		for itemRows.Next() {
			var item ExpenseItem
			if err := itemRows.Scan(&item.ID, &item.ExpenseID, &item.Description,
				&item.Quantity, &item.UnitPrice, &item.TotalPrice); err != nil {
				continue
			}
			items[e.ID] = append(items[e.ID], item)
		}
		itemRows.Close()
	}

	return expenses, items, nil
}

// ExpenseSummary returns aggregate stats for expenses in a date range.
// Used by the query_expenses tool for "how much this week/month" questions.
func (s *Store) ExpenseSummary(startDate, endDate string) (total float64, byCategory map[string]float64, count int, err error) {
	byCategory = make(map[string]float64)

	rows, err := s.db.Query(
		`SELECT category, SUM(amount), COUNT(*)
		 FROM expenses
		 WHERE date >= ? AND date <= ?
		 GROUP BY category`,
		startDate, endDate,
	)
	if err != nil {
		return 0, nil, 0, fmt.Errorf("querying expense summary: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var cat string
		var sum float64
		var cnt int
		if err := rows.Scan(&cat, &sum, &cnt); err != nil {
			continue
		}
		byCategory[cat] = sum
		total += sum
		count += cnt
	}

	return total, byCategory, count, nil
}

// --- Pending Confirmations ---
//
// These support the reply_confirm agent tool. When the agent wants to
// execute a destructive action (delete expense, remove fact, etc.), it
// sends a confirmation message with Yes/No buttons instead of executing
// immediately. The pending confirmation is stored here, and the callback
// handler looks it up when the user clicks a button.
//
// This is similar to how mood check-ins work (save data when user clicks
// an inline button), but agent-driven instead of scheduler-driven.

// PendingConfirmation represents a destructive action waiting for user
// approval via an inline keyboard button click.
type PendingConfirmation struct {
	ID             int64
	TelegramMsgID  int64
	ActionType     string          // e.g., "delete_expense", "remove_fact", "delete_schedule"
	ActionPayload  json.RawMessage // JSON blob with action-specific params
	Description    string          // human-readable description shown after resolution
	CreatedAt      time.Time
	ResolvedAt     *time.Time // nil until the user clicks a button
	ResolvedAction *string    // "confirmed", "cancelled", or "error"
}

// CreatePendingConfirmation stores a new pending confirmation keyed by
// the Telegram message ID of the confirmation message. The callback
// handler will look this up when the user clicks Yes or No.
//
// This follows the same pattern as SaveMoodEntry — simple INSERT, return
// the auto-generated ID. The telegramMsgID comes from the bot's Send()
// call, which returns the message object with its ID.
func (s *Store) CreatePendingConfirmation(telegramMsgID int64, actionType string, actionPayload json.RawMessage, description string) (int64, error) {
	result, err := s.db.Exec(
		`INSERT INTO pending_confirmations (telegram_msg_id, action_type, action_payload, description)
		 VALUES (?, ?, ?, ?)`,
		telegramMsgID, actionType, string(actionPayload), description,
	)
	if err != nil {
		return 0, fmt.Errorf("creating pending confirmation: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("getting pending confirmation ID: %w", err)
	}
	return id, nil
}

// GetPendingConfirmation looks up an unresolved confirmation by the
// Telegram message ID. Returns nil (not error) if not found, already
// resolved, or older than 1 hour (expired).
//
// The 1-hour TTL prevents stale confirmations from executing days later
// if the user scrolls back and clicks an old button. This is a soft
// safety net — the worst case is the user has to re-ask.
func (s *Store) GetPendingConfirmation(telegramMsgID int64) (*PendingConfirmation, error) {
	row := s.db.QueryRow(
		`SELECT id, telegram_msg_id, action_type, action_payload, description, created_at
		 FROM pending_confirmations
		 WHERE telegram_msg_id = ?
		   AND resolved_at IS NULL
		   AND created_at > datetime('now', '-1 hour')`,
		telegramMsgID,
	)

	var pc PendingConfirmation
	var payloadStr string
	err := row.Scan(&pc.ID, &pc.TelegramMsgID, &pc.ActionType, &payloadStr, &pc.Description, &pc.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil // not found or expired — not an error
	}
	if err != nil {
		return nil, fmt.Errorf("getting pending confirmation: %w", err)
	}
	pc.ActionPayload = json.RawMessage(payloadStr)
	return &pc, nil
}

// ResolvePendingConfirmation marks a confirmation as resolved with the
// given action ("confirmed", "cancelled", or "error"). This prevents
// double-clicks — once resolved, GetPendingConfirmation won't return it.
func (s *Store) ResolvePendingConfirmation(id int64, action string) error {
	_, err := s.db.Exec(
		`UPDATE pending_confirmations
		 SET resolved_at = CURRENT_TIMESTAMP, resolved_action = ?
		 WHERE id = ?`,
		action, id,
	)
	if err != nil {
		return fmt.Errorf("resolving pending confirmation %d: %w", id, err)
	}
	return nil
}

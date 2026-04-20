// Package memory provides SQLite-backed storage for conversations, facts,
// PII vault entries, and metrics. Everything lives in one database file.
package memory

import (
	"database/sql"
	"fmt"

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
// reading/writing messages, memories, metrics, and PII vault entries.
// In Go, this is how you build something like a Python class — a struct
// with methods attached to it.
type Store struct {
	db             *sql.DB
	EmbedDimension int // vector dimension for the vec_memories table (e.g. 768)

	// Zettelkasten memory linking — auto-connect new memories to similar existing ones.
	// When a memory is saved, we KNN-search for neighbors and create bidirectional
	// links. During retrieval, 1-hop traversal pulls in related memories that didn't
	// directly match the query. Think of it as Python's networkx graph, but
	// stored in SQLite so it persists across restarts.
	AutoLinkCount     int     // max links per new memory (0 = disabled)
	AutoLinkThreshold float64 // min cosine similarity to create a link (0.0-1.0)
}

// NewStore opens (or creates) the SQLite database at the given path
// and initializes all tables. The database file is created automatically
// by SQLite if it doesn't exist — no setup step needed.
//
// embedDimension is the vector size for the sqlite-vec index (e.g. 768
// for nomic-embed-text-v1.5). Pass 0 to skip creating the vector table
// (useful if embeddings aren't configured).
//
// NOTE: The initial CREATE TABLE statements still create the legacy "facts",
// "fact_links", and "vec_facts" tables for backward compatibility with older
// databases. The migration block at the end creates the renamed "memories",
// "memory_links", and "vec_memories" tables and copies data over. All runtime
// queries use the new table names.
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

		// Classifier decision log — records every classifier verdict
		// (both SAVE and rejections) for observability and prompt tuning.
		// write_type: "fact", "self_fact"
		// verdict: "SAVE", "LOW_VALUE", etc.
		// rewrite/accepted: reserved for future rewrite suggestions (#42)
		`CREATE TABLE IF NOT EXISTS classifier_log (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
			conversation_id TEXT,
			write_type TEXT NOT NULL,
			verdict TEXT NOT NULL,
			content TEXT NOT NULL,
			reason TEXT,
			rewrite TEXT,
			accepted BOOLEAN
		)`,

		// location_history stores every location the user shares or
		// searches near. Separate from facts — locations are structured
		// data, not free-text memories. Useful for future analysis
		// (routine detection, visit frequency, etc.).
		// source: 'pin' (Telegram), 'venue' (Telegram venue), 'text' (geocoded address), 'search' (nearby_search query)
		`CREATE TABLE IF NOT EXISTS location_history (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
			latitude REAL NOT NULL,
			longitude REAL NOT NULL,
			label TEXT,
			source TEXT NOT NULL,
			conversation_id TEXT
		)`,

		// persona_state is a single-row table (CHECK id = 1 enforces this) that
		// tracks the dreaming system's timing state. The dreamer goroutine reads
		// this to decide if catch-up is needed at startup, and writes it after
		// each reflection/rewrite so the gates work correctly across restarts.
		//
		// Why a single-row table instead of a key-value store? Type safety.
		// Two DATETIME columns are harder to accidentally mis-use than a bag
		// of string key-value pairs. The CHECK constraint makes the intent clear.
		`CREATE TABLE IF NOT EXISTS persona_state (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			last_reflection_at DATETIME,
			last_rewrite_at    DATETIME
		)`,

		// Scheduler tasks — backs the extension-based scheduler package
		// (`scheduler/`). One row per registered task; extensions declare
		// themselves via task.yaml files and the runner dispatches by
		// kind. See docs/plans/PLAN-mood-tracking-redesign.md for the
		// full design.
		`CREATE TABLE IF NOT EXISTS scheduler_tasks (
			id                 INTEGER PRIMARY KEY AUTOINCREMENT,
			kind               TEXT NOT NULL,
			cron_expr          TEXT,
			next_fire          DATETIME NOT NULL,
			payload_json       TEXT NOT NULL DEFAULT '{}',
			retry_max_attempts INTEGER NOT NULL DEFAULT 0,
			retry_backoff      TEXT NOT NULL DEFAULT 'none',
			retry_initial_wait INTEGER NOT NULL DEFAULT 0,
			last_run_at        DATETIME,
			last_error         TEXT,
			attempt_count      INTEGER NOT NULL DEFAULT 0,
			created_at         DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_scheduler_tasks_next_fire
			ON scheduler_tasks(next_fire)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_scheduler_tasks_kind
			ON scheduler_tasks(kind)`,

		// Mood entries — Apple-style state-of-mind tracking. Each row is
		// either a momentary snapshot (one specific moment) or a daily
		// rollup (how the day felt overall). See the `mood/` package and
		// docs/plans/PLAN-mood-tracking-redesign.md.
		//
		//   valence      — 1-7, very unpleasant → very pleasant
		//   labels       — JSON array of strings from mood/vocab.yaml
		//   associations — JSON array of domain tags (Work, Family, …)
		//   source       — "inferred" | "confirmed" | "manual"
		//   confidence   — 0-1; only meaningful for inferred entries
		//   embedding    — cached note+labels vector for KNN dedup
		`CREATE TABLE IF NOT EXISTS mood_entries (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			ts              DATETIME DEFAULT CURRENT_TIMESTAMP,
			kind            TEXT NOT NULL,
			valence         INTEGER NOT NULL,
			labels          TEXT NOT NULL DEFAULT '[]',
			associations   TEXT NOT NULL DEFAULT '[]',
			note           TEXT NOT NULL DEFAULT '',
			source         TEXT NOT NULL,
			confidence     REAL NOT NULL DEFAULT 0,
			conversation_id TEXT,
			embedding      BLOB,
			created_at     DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_mood_entries_ts
			ON mood_entries(ts)`,
		`CREATE INDEX IF NOT EXISTS idx_mood_entries_kind_ts
			ON mood_entries(kind, ts)`,

		// Pending mood proposals — medium-confidence inference proposals
		// the user hasn't tapped yet. Telegram message ID is stored so the
		// expiry sweeper can edit the inline keyboard in place when the
		// proposal times out. Once the user taps (or the sweeper acts),
		// the row moves to status=confirmed/rejected/expired.
		`CREATE TABLE IF NOT EXISTS pending_mood_proposals (
			id                  INTEGER PRIMARY KEY AUTOINCREMENT,
			ts                  DATETIME DEFAULT CURRENT_TIMESTAMP,
			telegram_chat_id    INTEGER NOT NULL,
			telegram_message_id INTEGER NOT NULL,
			proposal_json       TEXT NOT NULL,
			status              TEXT NOT NULL DEFAULT 'pending',
			expires_at          DATETIME NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_pending_mood_status_expires
			ON pending_mood_proposals(status, expires_at)`,
	}

	// Pre-migration: detect and rebuild stale mood_entries table.
	// The pre-redesign schema had (timestamp, rating, note, tags) which is
	// structurally incompatible with the Apple-style schema (ts, kind,
	// valence, labels, associations, confidence). CREATE TABLE IF NOT EXISTS
	// silently keeps the old schema, then CREATE INDEX ON mood_entries(ts)
	// crashes because ts doesn't exist. We detect the old schema by checking
	// for the "rating" column and drop + recreate if found.
	var hasOldMood int
	s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('mood_entries') WHERE name = 'rating'`).Scan(&hasOldMood)
	if hasOldMood > 0 {
		s.db.Exec(`DROP TABLE IF EXISTS mood_entries`)
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

		// Zettelkasten supersession — when a fact is replaced by a newer version,
		// we record what replaced it and why. This preserves knowledge evolution:
		// "used to work at Company A" → superseded by → "now at Company B".
		`ALTER TABLE facts ADD COLUMN superseded_by INTEGER REFERENCES facts(id)`,
		`ALTER TABLE facts ADD COLUMN supersede_reason TEXT`,

		// Zettelkasten fact context — optional note explaining WHY a fact
		// matters or how it connects to other knowledge. Enriches the text
		// embedding so semantic search is aware of relationships.
		`ALTER TABLE facts ADD COLUMN context TEXT`,

		// stream: "chat" or "agent" — allows separate summaries for each model.
		// The chat summary captures conversational flow; the agent summary
		// captures tool call history and decisions. Existing rows default to
		// "chat" since that's what they were before this column existed.
		`ALTER TABLE summaries ADD COLUMN stream TEXT NOT NULL DEFAULT 'chat'`,
	}
	for _, m := range migrations {
		s.db.Exec(m) // ignore errors (column already exists)
	}

	// Mood table migration: add updated_at for the mood update/refine path.
	// When a mood entry is refined (same emotional arc, new detail), we
	// update the row in place and stamp updated_at so the original ts is
	// preserved as "when the mood first appeared."
	s.db.Exec(`ALTER TABLE mood_entries ADD COLUMN updated_at DATETIME`)

	// memories — primary storage for learned facts about the user and self.
	// Run `her migrate` once to copy data from the legacy `facts` table.
	s.db.Exec(`CREATE TABLE IF NOT EXISTS memories (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
		memory TEXT NOT NULL,
		category TEXT,
		source_message_id INTEGER,
		importance INTEGER DEFAULT 5,
		active BOOLEAN DEFAULT 1,
		subject TEXT DEFAULT 'user',
		embedding BLOB,
		tags TEXT,
		embedding_text BLOB,
		superseded_by INTEGER REFERENCES memories(id),
		supersede_reason TEXT,
		context TEXT,
		FOREIGN KEY (source_message_id) REFERENCES messages(id)
	)`)

	// Zettelkasten memory links — adjacency list connecting related memories.
	// IDs are normalized (source < target) to avoid duplicate bidirectional edges.
	s.db.Exec(`CREATE TABLE IF NOT EXISTS memory_links (
		source_id  INTEGER NOT NULL,
		target_id  INTEGER NOT NULL,
		similarity REAL NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (source_id, target_id),
		FOREIGN KEY (source_id) REFERENCES memories(id),
		FOREIGN KEY (target_id) REFERENCES memories(id)
	)`)

	s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_memory_links_source ON memory_links(source_id)`)
	s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_memory_links_target ON memory_links(target_id)`)

	// Index for the dual-stream summary lookups. LatestSummary queries
	// (conversation_id, stream) on every message — without this, SQLite
	// does a full table scan that degrades as summaries accumulate.
	s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_summaries_conv_stream ON summaries(conversation_id, stream)`)

	// Indexes — CREATE INDEX IF NOT EXISTS is idempotent like the tables.
	indexes := []string{
		// Expense indexes — date index for "how much this week/month" queries,
		// category index for per-category breakdowns.
		`CREATE INDEX IF NOT EXISTS idx_expenses_date ON expenses(date)`,
		`CREATE INDEX IF NOT EXISTS idx_expenses_category ON expenses(category)`,
	}
	for _, idx := range indexes {
		if _, err := s.db.Exec(idx); err != nil {
			return fmt.Errorf("creating index: %w", err)
		}
	}

	// sqlite-vec virtual table for KNN vector search on memory embeddings.
	// vec0 is the virtual table module provided by the sqlite-vec extension.
	// The rowid maps to memories.id so we can JOIN back for metadata.
	// distance_metric=cosine means KNN results are ranked by cosine distance
	// (0 = identical, 2 = opposite) instead of the default L2/Euclidean.
	//
	// Virtual tables are a SQLite concept with no direct Python equivalent —
	// think of them as tables backed by a custom engine. The vec0 engine
	// stores vectors in an optimized format and implements fast approximate
	// nearest neighbor search behind the standard SQL interface.
	if s.EmbedDimension > 0 {
		vecDDL := fmt.Sprintf(
			`CREATE VIRTUAL TABLE IF NOT EXISTS vec_memories USING vec0(embedding float[%d] distance_metric=cosine)`,
			s.EmbedDimension,
		)
		if _, err := s.db.Exec(vecDDL); err != nil {
			return fmt.Errorf("creating vec_memories virtual table: %w", err)
		}

		// Parallel virtual table for mood entries — same vec0 engine,
		// same cosine distance metric, different rowid space. Keyed by
		// mood_entries.id for JOINing back to the parent row. Powers the
		// embedding-based dedup pass in the mood agent.
		moodVecDDL := fmt.Sprintf(
			`CREATE VIRTUAL TABLE IF NOT EXISTS vec_moods USING vec0(embedding float[%d] distance_metric=cosine)`,
			s.EmbedDimension,
		)
		if _, err := s.db.Exec(moodVecDDL); err != nil {
			return fmt.Errorf("creating vec_moods virtual table: %w", err)
		}
	}

	// Inter-agent inbox — lets agents pass tasks and results to each other.
	if err := s.initInboxTable(); err != nil {
		return err
	}

	return nil
}

-- D1 Shared State Schema
--
-- These tables mirror the local SQLite schema for cross-machine sync.
-- Embedding columns (BLOB) are excluded — each machine generates its own
-- embeddings locally after pulling new rows.
--
-- Applied via: wrangler d1 execute her-db --file d1/schema.sql

-- ---------------------------------------------------------------------------
-- Messages
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS messages (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp         DATETIME DEFAULT CURRENT_TIMESTAMP,
    role              TEXT NOT NULL,
    content_raw       TEXT NOT NULL,
    content_scrubbed  TEXT,
    conversation_id   TEXT,
    token_count       INTEGER,
    voice_memo_path   TEXT,
    media_file_id     TEXT,
    media_description TEXT
);

-- ---------------------------------------------------------------------------
-- Summaries
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS summaries (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp         DATETIME DEFAULT CURRENT_TIMESTAMP,
    conversation_id   TEXT,
    summary           TEXT NOT NULL,
    messages_start_id INTEGER,
    messages_end_id   INTEGER,
    stream            TEXT NOT NULL DEFAULT 'chat'
);

CREATE INDEX IF NOT EXISTS idx_summaries_conv_stream
    ON summaries(conversation_id, stream);

-- ---------------------------------------------------------------------------
-- Memories (no embedding / embedding_text columns)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS memories (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp         DATETIME DEFAULT CURRENT_TIMESTAMP,
    memory            TEXT NOT NULL,
    category          TEXT,
    source_message_id INTEGER,
    importance        INTEGER DEFAULT 5,
    active            BOOLEAN DEFAULT 1,
    subject           TEXT DEFAULT 'user',
    tags              TEXT,
    superseded_by     INTEGER,
    supersede_reason  TEXT,
    context           TEXT
);

-- ---------------------------------------------------------------------------
-- Memory Links
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS memory_links (
    source_id  INTEGER NOT NULL,
    target_id  INTEGER NOT NULL,
    similarity REAL NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (source_id, target_id)
);

CREATE INDEX IF NOT EXISTS idx_memory_links_source ON memory_links(source_id);
CREATE INDEX IF NOT EXISTS idx_memory_links_target ON memory_links(target_id);

-- ---------------------------------------------------------------------------
-- Reflections
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS reflections (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp     DATETIME DEFAULT CURRENT_TIMESTAMP,
    content       TEXT NOT NULL,
    fact_count    INTEGER,
    user_message  TEXT,
    mira_response TEXT
);

-- ---------------------------------------------------------------------------
-- Persona Versions
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS persona_versions (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp          DATETIME DEFAULT CURRENT_TIMESTAMP,
    content            TEXT NOT NULL,
    trigger            TEXT,
    conversation_count INTEGER,
    reflection_ids     TEXT
);

-- ---------------------------------------------------------------------------
-- Traits
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS traits (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp          DATETIME DEFAULT CURRENT_TIMESTAMP,
    trait_name         TEXT NOT NULL,
    value              TEXT NOT NULL,
    persona_version_id INTEGER
);

-- ---------------------------------------------------------------------------
-- Persona State (singleton row)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS persona_state (
    id                 INTEGER PRIMARY KEY CHECK (id = 1),
    last_reflection_at DATETIME,
    last_rewrite_at    DATETIME
);

-- ---------------------------------------------------------------------------
-- Mood Entries (no embedding column)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS mood_entries (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    ts              DATETIME DEFAULT CURRENT_TIMESTAMP,
    kind            TEXT NOT NULL,
    valence         INTEGER NOT NULL,
    labels          TEXT NOT NULL DEFAULT '[]',
    associations    TEXT NOT NULL DEFAULT '[]',
    note            TEXT NOT NULL DEFAULT '',
    source          TEXT NOT NULL,
    confidence      REAL NOT NULL DEFAULT 0,
    conversation_id TEXT,
    created_at      DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at      DATETIME
);

CREATE INDEX IF NOT EXISTS idx_mood_entries_ts ON mood_entries(ts);
CREATE INDEX IF NOT EXISTS idx_mood_entries_kind_ts ON mood_entries(kind, ts);

-- ---------------------------------------------------------------------------
-- Sync Metadata
--
-- Tracks the last synced row ID per table. Each machine maintains its own
-- copy of this table in local SQLite AND in D1. On pull, a machine fetches
-- rows with id > last_synced_id for each table.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS _sync_meta (
    table_name     TEXT PRIMARY KEY,
    last_synced_id INTEGER NOT NULL DEFAULT 0
);

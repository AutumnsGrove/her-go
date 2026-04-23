-- Core tables: messages, summaries, pii_vault

CREATE TABLE IF NOT EXISTS messages (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
    role TEXT NOT NULL,
    content_raw TEXT NOT NULL,
    content_scrubbed TEXT,
    conversation_id TEXT,
    token_count INTEGER,
    voice_memo_path TEXT,
    media_file_id TEXT,
    media_description TEXT
);

CREATE TABLE IF NOT EXISTS summaries (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
    conversation_id TEXT,
    summary TEXT NOT NULL,
    messages_start_id INTEGER,
    messages_end_id INTEGER,
    stream TEXT NOT NULL DEFAULT 'chat'
);

CREATE TABLE IF NOT EXISTS pii_vault (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    message_id INTEGER,
    token TEXT NOT NULL,
    original_value TEXT NOT NULL,
    entity_type TEXT NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (message_id) REFERENCES messages(id)
);

-- Index for dual-stream summary lookups
CREATE INDEX IF NOT EXISTS idx_summaries_conv_stream ON summaries(conversation_id, stream);

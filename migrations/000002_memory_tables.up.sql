-- Memory storage (facts → memories rename, with Zettelkasten linking)

CREATE TABLE IF NOT EXISTS memories (
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
);

CREATE TABLE IF NOT EXISTS memory_links (
    source_id  INTEGER NOT NULL,
    target_id  INTEGER NOT NULL,
    similarity REAL NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (source_id, target_id),
    FOREIGN KEY (source_id) REFERENCES memories(id),
    FOREIGN KEY (target_id) REFERENCES memories(id)
);

CREATE INDEX IF NOT EXISTS idx_memory_links_source ON memory_links(source_id);
CREATE INDEX IF NOT EXISTS idx_memory_links_target ON memory_links(target_id);

-- Legacy facts table for backward compatibility (will be migrated separately)
CREATE TABLE IF NOT EXISTS facts (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
    fact TEXT NOT NULL,
    category TEXT,
    source_message_id INTEGER,
    importance INTEGER DEFAULT 5,
    active BOOLEAN DEFAULT 1,
    subject TEXT DEFAULT 'user',
    embedding BLOB,
    tags TEXT,
    embedding_text BLOB,
    superseded_by INTEGER REFERENCES facts(id),
    supersede_reason TEXT,
    context TEXT,
    FOREIGN KEY (source_message_id) REFERENCES messages(id)
);

-- Metrics, agent turns, searches, command log

CREATE TABLE IF NOT EXISTS metrics (
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
);

CREATE TABLE IF NOT EXISTS agent_turns (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
    message_id INTEGER,
    turn_index INTEGER,
    role TEXT NOT NULL,
    tool_name TEXT,
    tool_args TEXT,
    content TEXT,
    FOREIGN KEY (message_id) REFERENCES messages(id)
);

CREATE TABLE IF NOT EXISTS searches (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
    message_id INTEGER,
    search_type TEXT NOT NULL,
    query TEXT NOT NULL,
    results TEXT,
    result_count INTEGER,
    FOREIGN KEY (message_id) REFERENCES messages(id)
);

CREATE TABLE IF NOT EXISTS command_log (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
    command TEXT NOT NULL,
    chat_id INTEGER,
    conversation_id TEXT,
    args TEXT
);

CREATE TABLE IF NOT EXISTS classifier_log (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
    conversation_id TEXT,
    write_type TEXT NOT NULL,
    verdict TEXT NOT NULL,
    content TEXT NOT NULL,
    reason TEXT,
    rewrite TEXT,
    accepted BOOLEAN
);

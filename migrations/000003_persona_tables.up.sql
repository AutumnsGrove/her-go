-- Persona evolution tables

CREATE TABLE IF NOT EXISTS persona_versions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
    content TEXT NOT NULL,
    trigger TEXT,
    conversation_count INTEGER,
    reflection_ids TEXT
);

CREATE TABLE IF NOT EXISTS traits (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
    trait_name TEXT NOT NULL,
    value TEXT NOT NULL,
    persona_version_id INTEGER,
    FOREIGN KEY (persona_version_id) REFERENCES persona_versions(id)
);

CREATE TABLE IF NOT EXISTS persona_state (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    last_reflection_at DATETIME,
    last_rewrite_at    DATETIME
);

CREATE TABLE IF NOT EXISTS reflections (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
    content TEXT NOT NULL,
    fact_count INTEGER,
    user_message TEXT,
    mira_response TEXT
);

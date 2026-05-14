-- Memory Cards: dense topic-based memory storage.
-- Replaces the flat "memories" table with consolidated topic cards
-- and an append-only changelog for traceability.

CREATE TABLE IF NOT EXISTS memory_cards (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    topic_slug  TEXT    UNIQUE NOT NULL,
    name        TEXT    NOT NULL,
    content     TEXT    NOT NULL DEFAULT '',
    subject     TEXT    NOT NULL DEFAULT 'user',
    protected   BOOLEAN NOT NULL DEFAULT 0,
    created_at  DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at  DATETIME DEFAULT CURRENT_TIMESTAMP,
    version     INTEGER NOT NULL DEFAULT 1
);

CREATE TABLE IF NOT EXISTS memory_log (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    card_id           INTEGER NOT NULL REFERENCES memory_cards(id),
    delta             TEXT    NOT NULL,
    operation         TEXT    NOT NULL,
    source_message_id INTEGER,
    created_at        DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_memory_log_card_id ON memory_log(card_id);
CREATE INDEX IF NOT EXISTS idx_memory_log_created_at ON memory_log(created_at);
CREATE INDEX IF NOT EXISTS idx_memory_cards_subject ON memory_cards(subject);

-- Seed user cards (protected, cannot be expired by dream cycle).
INSERT OR IGNORE INTO memory_cards (topic_slug, name, subject, protected) VALUES
    ('identity',      'Identity',        'user', 1),
    ('health',        'Health',           'user', 1),
    ('financial',     'Financial',        'user', 1),
    ('family',        'Family',           'user', 1),
    ('relationships', 'Relationships',    'user', 1),
    ('work',          'Work & Career',    'user', 1),
    ('interests',     'Interests',        'user', 1),
    ('projects',      'Projects',         'user', 1),
    ('routines',      'Routines',         'user', 1);

-- Seed self cards (protected, Mira's identity).
INSERT OR IGNORE INTO memory_cards (topic_slug, name, subject, protected) VALUES
    ('my-identity',      'My Identity',      'self', 1),
    ('my-emotions',      'My Emotions',       'self', 1),
    ('my-communication', 'My Communication',  'self', 1),
    ('my-relationship',  'My Relationship',   'self', 1),
    ('my-growth',        'My Growth',         'self', 1);

-- ============================================================
-- 000013: Memory Cards — schema + seed cards
-- ============================================================
-- Creates the card-based memory folder system. Cards are
-- organizational containers; individual memories link to them
-- via card_id. Card summaries are maintained by the dream cycle.

-- --------------------------------------------------------
-- Schema
-- --------------------------------------------------------

CREATE TABLE IF NOT EXISTS memory_cards (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    topic_slug  TEXT    UNIQUE NOT NULL,
    name        TEXT    NOT NULL,
    summary     TEXT    NOT NULL DEFAULT '',
    subject     TEXT    NOT NULL DEFAULT 'user',
    protected   BOOLEAN NOT NULL DEFAULT 0,
    created_at  DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at  DATETIME DEFAULT CURRENT_TIMESTAMP,
    version     INTEGER NOT NULL DEFAULT 1
);

ALTER TABLE memories ADD COLUMN card_id INTEGER REFERENCES memory_cards(id);

CREATE TABLE IF NOT EXISTS memory_log (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    card_id           INTEGER REFERENCES memory_cards(id),
    memory_id         INTEGER REFERENCES memories(id),
    delta             TEXT    NOT NULL,
    operation         TEXT    NOT NULL,
    source_message_id INTEGER,
    created_at        DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_memory_log_card_id ON memory_log(card_id);
CREATE INDEX IF NOT EXISTS idx_memory_log_created_at ON memory_log(created_at);
CREATE INDEX IF NOT EXISTS idx_memory_cards_subject ON memory_cards(subject);
CREATE INDEX IF NOT EXISTS idx_memories_card_id ON memories(card_id);

-- --------------------------------------------------------
-- Seed cards
-- --------------------------------------------------------

-- User cards (protected — cannot be expired by dream cycle)
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

-- Self cards (protected — Mira's identity)
INSERT OR IGNORE INTO memory_cards (topic_slug, name, subject, protected) VALUES
    ('my-identity',      'My Identity',       'self', 1),
    ('my-emotions',      'My Emotions',       'self', 1),
    ('my-communication', 'My Communication',  'self', 1),
    ('my-relationship',  'My Relationship',   'self', 1),
    ('my-growth',        'My Growth',         'self', 1);

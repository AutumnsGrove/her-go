ALTER TABLE mood_entries ADD COLUMN superseded_by INTEGER REFERENCES mood_entries(id);
ALTER TABLE mood_entries ADD COLUMN supersede_reason TEXT;
CREATE INDEX IF NOT EXISTS idx_mood_entries_superseded ON mood_entries(superseded_by);

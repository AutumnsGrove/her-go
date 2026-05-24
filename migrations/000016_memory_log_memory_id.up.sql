-- Adds memory_id column to memory_log if it was created without it.
-- Migration 000013 included memory_id in the CREATE TABLE, but databases
-- where memory_log already existed (from an earlier code path) missed it
-- because CREATE TABLE IF NOT EXISTS skips existing tables.
ALTER TABLE memory_log ADD COLUMN memory_id INTEGER REFERENCES memories(id);

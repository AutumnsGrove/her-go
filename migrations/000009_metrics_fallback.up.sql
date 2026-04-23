-- Track whether an LLM call used the fallback model due to rate limits
-- or other retriable errors on the primary. Helps spot the "Haiku tax" —
-- free-tier models that silently fall back to paid models on 429s.

ALTER TABLE metrics ADD COLUMN is_fallback BOOLEAN DEFAULT 0;

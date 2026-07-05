-- Track OpenRouter prompt cache efficiency and provider routing per LLM call.
-- cache_read_tokens: tokens served from provider-side prompt cache (cheap).
-- cache_write_tokens: tokens written to cache for future reuse.
-- provider: infrastructure name that served the request (e.g., "Moonshot", "Groq").

ALTER TABLE metrics ADD COLUMN cache_read_tokens INTEGER DEFAULT 0;
ALTER TABLE metrics ADD COLUMN cache_write_tokens INTEGER DEFAULT 0;
ALTER TABLE metrics ADD COLUMN provider TEXT DEFAULT '';

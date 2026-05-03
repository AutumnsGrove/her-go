-- Add message watermark to persona_state so the dreamer knows which messages
-- it has already reflected on. Prevents duplicate reflections on quiet days.
ALTER TABLE persona_state ADD COLUMN last_reflected_message_id INTEGER DEFAULT 0;

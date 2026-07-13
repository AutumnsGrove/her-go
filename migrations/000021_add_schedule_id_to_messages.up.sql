-- Add schedule_id column to messages table to track which scheduled task
-- triggered a message. This enables "delete this reminder" UX without parsing
-- message text or showing schedule IDs to users.

ALTER TABLE messages ADD COLUMN schedule_id INTEGER;

-- Index for fast lookups when checking recent scheduled messages
CREATE INDEX IF NOT EXISTS idx_messages_schedule_id ON messages(schedule_id)
    WHERE schedule_id IS NOT NULL;

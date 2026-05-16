-- Add agent_role to metrics so cost can be broken down by which agent
-- made the call (driver, memory, mood, introspection, chat, dream, etc.)
-- rather than only by model name. Critical when multiple agents use the
-- same model (e.g., Kimi K2 serves memory, mood, and introspection).

ALTER TABLE metrics ADD COLUMN agent_role TEXT DEFAULT '';

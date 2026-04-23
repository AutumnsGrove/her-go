-- Track fallback model usage in sim metrics (mirrors production migration 000009).
-- Enables the sim report to show primary vs fallback cost breakdown and
-- flag which calls hit the "Haiku tax."

ALTER TABLE sim_metrics ADD COLUMN is_fallback BOOLEAN DEFAULT 0;

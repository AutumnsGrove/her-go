-- Mirror production migration 000015: add agent_role to sim_metrics for
-- per-agent cost breakdown in sim reports.

ALTER TABLE sim_metrics ADD COLUMN agent_role TEXT DEFAULT '';

-- Migration 18: unmeasured-session count on aggregate_metrics
--
-- Counts, cumulatively, every ended session whose usage was never
-- recorded, the same population the four token columns already
-- exclude. Backfilled from run_history, which is never pruned and
-- has carried this same verdict per row since migration 012.

ALTER TABLE aggregate_metrics ADD COLUMN unmeasured_sessions INTEGER NOT NULL DEFAULT 0;

UPDATE aggregate_metrics
SET unmeasured_sessions = (SELECT COUNT(*) FROM run_history WHERE tokens_measured = 0)
WHERE key = 'agent_totals';

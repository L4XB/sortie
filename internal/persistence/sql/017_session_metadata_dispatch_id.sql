-- Migration 17: record the dispatch ID that last stored a session_metadata row
--
-- The default is empty. session_metadata holds one current-state row per
-- issue, and a row written before this column exists belongs to no
-- running dispatch: it must never match one, so a pre-migration row
-- reads back as no match rather than as a coincidental match against an
-- empty dispatch ID.

ALTER TABLE session_metadata ADD COLUMN dispatch_id TEXT NOT NULL DEFAULT '';

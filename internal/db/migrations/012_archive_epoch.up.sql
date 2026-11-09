-- The point-in-time archive epoch of an operated postgres instance. A major
-- version change or its rollback starts a new one: its WAL is archived to its
-- own local directory and its own object prefix, so nothing of the old major
-- (or of the timeline 1 the new cluster starts) can collide with it.
ALTER TABLE operated_instances ADD COLUMN archive_epoch INTEGER NOT NULL DEFAULT 0;

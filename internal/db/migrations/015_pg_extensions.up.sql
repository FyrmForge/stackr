-- Postgres extensions on operated instances. preload is the startup modules
-- after pg_stat_statements; extensions is what the instance's slices may
-- list; a slice tile's own extensions is its list. Existing rows keep
-- exactly what they ran with: no modules, no extensions.
ALTER TABLE operated_instances ADD COLUMN preload TEXT NOT NULL DEFAULT '[]';
ALTER TABLE operated_instances ADD COLUMN extensions TEXT NOT NULL DEFAULT '[]';
ALTER TABLE tiles ADD COLUMN extensions TEXT NOT NULL DEFAULT '[]';

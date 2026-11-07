-- The WAL segment current when a base backup finished: the base is usable only
-- once shipped WAL covers through it.
ALTER TABLE backup_runs ADD COLUMN wal_stop TEXT;

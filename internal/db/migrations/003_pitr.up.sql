-- Point-in-time recovery: days of WAL kept for an instance (0 = off), and
-- the WAL segment a base backup started at.
ALTER TABLE managed_instances ADD COLUMN pitr_days INTEGER NOT NULL DEFAULT 0;
ALTER TABLE backup_runs ADD COLUMN wal_start TEXT;

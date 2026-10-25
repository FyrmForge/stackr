-- The recovery key generation a panel backup was encrypted to (0 on a volume run).
ALTER TABLE backup_runs ADD COLUMN recovery_gen INTEGER NOT NULL DEFAULT 0;

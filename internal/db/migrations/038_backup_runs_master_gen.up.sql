-- The master key generation a panel backup carries (0 on a volume run).
ALTER TABLE backup_runs ADD COLUMN master_gen INTEGER NOT NULL DEFAULT 0;

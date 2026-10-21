-- What a run's container used, read from docker stats while it ran. NULL when
-- no reading was taken.
ALTER TABLE runs ADD COLUMN peak_mem_bytes INTEGER;
ALTER TABLE runs ADD COLUMN cpu_ms INTEGER;

-- A postgres slice's url setting: what its url, host and port outputs point
-- at, the instance (direct, NULL reads as direct) or its pooler (pooled).
ALTER TABLE tiles ADD COLUMN slice_url TEXT CHECK (slice_url IN ('direct', 'pooled'));

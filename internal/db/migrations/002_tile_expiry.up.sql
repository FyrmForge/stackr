-- A branch slice carries the time it is swept; NULL = never.
ALTER TABLE tiles ADD COLUMN expires_at DATETIME;

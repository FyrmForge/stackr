-- A branch slice carries the time it is swept (NULL = never) and the slice
-- it was copied from (its delete skips the pre-drop instance dump).
ALTER TABLE tiles ADD COLUMN expires_at DATETIME;
ALTER TABLE tiles ADD COLUMN branch_of TEXT;

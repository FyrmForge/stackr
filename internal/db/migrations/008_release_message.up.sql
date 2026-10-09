-- Migration H: a release keeps the first line of the commit message it was cut from.
ALTER TABLE releases ADD COLUMN message TEXT NOT NULL DEFAULT '';

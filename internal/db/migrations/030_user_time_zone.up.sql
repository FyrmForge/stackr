-- The zone the panel shows times in: '' is the browser's, else an IANA name
-- (UTC included).
ALTER TABLE users ADD COLUMN time_zone TEXT NOT NULL DEFAULT '';

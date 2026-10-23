-- A connection's last check (connectors core, phase 2): when the core last
-- asked its provider, and the explained refusal it got ('' = working).
ALTER TABLE connectors ADD COLUMN checked_at DATETIME;
ALTER TABLE connectors ADD COLUMN check_error TEXT NOT NULL DEFAULT '';

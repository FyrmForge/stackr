-- Metrics history was on unless switched off; it now defaults to off. An
-- install that already ran (it has a node row; a fresh database has none
-- yet) and never stored the switch keeps it on, so its history goes on.
INSERT OR IGNORE INTO settings (key, value)
SELECT 'observe_enabled', 'true' WHERE EXISTS (SELECT 1 FROM nodes);

-- security_events stays append only except while a prune transaction holds the
-- settings row security_events_pruning (service: pruneOld, 90 days).
DROP TRIGGER security_events_no_delete;
CREATE TRIGGER security_events_no_delete BEFORE DELETE ON security_events
WHEN NOT EXISTS (SELECT 1 FROM settings WHERE key = 'security_events_pruning')
BEGIN SELECT RAISE(ABORT, 'security_events is append only'); END;

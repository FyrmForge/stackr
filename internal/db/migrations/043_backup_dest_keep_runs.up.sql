-- A destination's row holds the only copy of its archive key. Runs used to
-- go with it (ON DELETE CASCADE); now a delete is refused while runs exist.
-- Exempt: an org's own destinations when the org itself is deleted.
CREATE TRIGGER backup_destinations_keep_runs
BEFORE DELETE ON backup_destinations
WHEN EXISTS (SELECT 1 FROM backup_runs WHERE dest_id = OLD.id)
 AND (OLD.org_id IS NULL OR EXISTS (SELECT 1 FROM orgs WHERE id = OLD.org_id))
BEGIN
    SELECT RAISE(ABORT, 'backup destination still has runs');
END;

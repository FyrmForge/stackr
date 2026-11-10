-- An org delete must not wipe its backups. A destination that still has runs
-- moves to server scope (org_id NULL), unshared, named "<slug>/<name>" (a
-- name no user can type) with the former slug on former_org. Its runs and the
-- archive key stay readable by the admin and age out like any other. A
-- destination with no runs goes with the org as before. The org id is a
-- uuid, never reused, so a new org taking the slug sees nothing of it.
ALTER TABLE backup_destinations ADD COLUMN former_org TEXT NOT NULL DEFAULT '';

CREATE TRIGGER orgs_keep_backups BEFORE DELETE ON orgs BEGIN
    UPDATE backup_destinations
       SET org_id = NULL, shared = 0, former_org = old.slug,
           name = old.slug || '/' || name
     WHERE org_id = old.id
       AND EXISTS (SELECT 1 FROM backup_runs WHERE dest_id = backup_destinations.id);
END;

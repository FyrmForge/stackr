-- An org's backup destination on a private address runs only once a server
-- admin approved that host:port; a job remembers the API key behind its actor
-- so a key bound to an org never auto-approves host access.
ALTER TABLE backup_destinations ADD COLUMN approved_endpoint TEXT NOT NULL DEFAULT '';
ALTER TABLE backup_destinations ADD COLUMN approved_by TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN actor_key_id TEXT NOT NULL DEFAULT '';

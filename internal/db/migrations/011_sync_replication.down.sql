ALTER TABLE operated_members DROP COLUMN sync_in;
ALTER TABLE operated_members DROP COLUMN sync_mode;
ALTER TABLE operated_instances DROP COLUMN sync_required;
ALTER TABLE operated_instances DROP COLUMN sync_limit_ms;
ALTER TABLE operated_instances DROP COLUMN prefer_sync;

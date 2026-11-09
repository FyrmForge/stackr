-- Synchronous replication of an operated postgres instance. prefer_sync turns
-- it on; sync_limit_ms is the round trip under which an auto standby is waited
-- for; sync_required makes writes wait even when no standby answers.
ALTER TABLE operated_instances ADD COLUMN prefer_sync INTEGER NOT NULL DEFAULT 0;
ALTER TABLE operated_instances ADD COLUMN sync_limit_ms INTEGER NOT NULL DEFAULT 10;
ALTER TABLE operated_instances ADD COLUMN sync_required INTEGER NOT NULL DEFAULT 0;
-- A standby's mode (auto, always or never) and whether the primary waits for
-- it now (sync_in); a promote prefers a standby that was waited for.
ALTER TABLE operated_members ADD COLUMN sync_mode TEXT NOT NULL DEFAULT 'auto';
ALTER TABLE operated_members ADD COLUMN sync_in INTEGER NOT NULL DEFAULT 0;

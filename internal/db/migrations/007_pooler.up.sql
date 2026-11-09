-- The password of the stackr_pooler role the instance's pgbouncer logs in
-- with (encrypted). Empty until the pooler is first made.
ALTER TABLE operated_instances ADD COLUMN pooler_password TEXT NOT NULL DEFAULT '';

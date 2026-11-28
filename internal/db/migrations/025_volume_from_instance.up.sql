-- A volume an operated instance owned, stamped at orphaning: instance_id is
-- cleared when the instance row goes, so this is what refuses a Restore.
ALTER TABLE volumes ADD COLUMN from_instance INTEGER NOT NULL DEFAULT 0;

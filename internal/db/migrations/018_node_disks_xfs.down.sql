ALTER TABLE volumes DROP COLUMN max_size_mb;
DROP INDEX disk_folders_disk_project;
ALTER TABLE disk_folders DROP COLUMN project;
ALTER TABLE node_disks DROP COLUMN device_bytes;
ALTER TABLE node_disks DROP COLUMN fs_uuid;
ALTER TABLE node_disks DROP COLUMN device_id;

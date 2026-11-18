-- Node disks, xfs kind. device_id/fs_uuid/device_bytes identify the block
-- device ('' / 0 for a path disk). project is a folder's xfs project number
-- (0 = none, path kind); unique per disk only when set. max_size_mb is a
-- volume's size cap (NULL = uncapped). Existing rows keep behaving as before.
ALTER TABLE node_disks ADD COLUMN device_id TEXT NOT NULL DEFAULT '';
ALTER TABLE node_disks ADD COLUMN fs_uuid TEXT NOT NULL DEFAULT '';
ALTER TABLE node_disks ADD COLUMN device_bytes INTEGER NOT NULL DEFAULT 0;
ALTER TABLE disk_folders ADD COLUMN project INTEGER NOT NULL DEFAULT 0;
CREATE UNIQUE INDEX disk_folders_disk_project ON disk_folders (disk_id, project) WHERE project > 0;
ALTER TABLE volumes ADD COLUMN max_size_mb INTEGER;

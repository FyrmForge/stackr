-- Node disks: one disk per node (node_id UNIQUE, no FK: the row outlives a
-- forgotten node so the disk can be adopted) and the folders on it, one per
-- Docker volume name. kind has no CHECK (validated in Go) so a later kind
-- needs no table rebuild. The op_* columns carry an unfinished adopt (op_env_ids is its
-- env id set as a JSON array); '' = none.
CREATE TABLE node_disks (
    id              TEXT     PRIMARY KEY,
    node_id         TEXT     NOT NULL UNIQUE,
    kind            TEXT     NOT NULL,
    mount_path      TEXT     NOT NULL,
    state           TEXT     NOT NULL,
    op_node_id      TEXT     NOT NULL DEFAULT '',
    op_from_node_id TEXT     NOT NULL DEFAULT '',
    op_mount_path   TEXT     NOT NULL DEFAULT '',
    op_env_ids      TEXT     NOT NULL DEFAULT '',
    created_at      DATETIME NOT NULL
);
CREATE TABLE disk_folders (
    id         TEXT     PRIMARY KEY,
    disk_id    TEXT     NOT NULL REFERENCES node_disks (id) ON DELETE CASCADE,
    name       TEXT     NOT NULL,
    created_at DATETIME NOT NULL,
    UNIQUE (disk_id, name)
);

-- The members of an operated instance: one primary and, on other nodes,
-- replicas. The primary's node follows its env, so its node_id, container and
-- volume stay empty (derived from the instance tile); a replica names all
-- three. state: what the panel wants (removing = a replica being taken away, kept
-- until its network is gone). fenced: the node refuses to start the member until it rejoins.
-- subnet is the replica's network on its node (the cross-node firewall flow
-- source).
CREATE TABLE operated_members (
    id           TEXT      PRIMARY KEY,
    instance_id  TEXT      NOT NULL REFERENCES operated_instances (id) ON DELETE CASCADE,
    role         TEXT      NOT NULL CHECK (role IN ('primary', 'replica')),
    node_id      TEXT      NOT NULL DEFAULT '',
    container    TEXT      NOT NULL DEFAULT '',
    volume       TEXT      NOT NULL DEFAULT '',
    subnet       TEXT      NOT NULL DEFAULT '',
    state        TEXT      NOT NULL DEFAULT 'running' CHECK (state IN ('running', 'stopped', 'removing')),
    fenced       INTEGER   NOT NULL DEFAULT 0,
    created_at   DATETIME  NOT NULL,
    updated_at   DATETIME  NOT NULL
);
CREATE UNIQUE INDEX operated_members_primary ON operated_members (instance_id) WHERE role = 'primary';
CREATE UNIQUE INDEX operated_members_node ON operated_members (instance_id, node_id) WHERE role = 'replica';

-- Members are postgres only. An instance made before this has no record of
-- whether its user stopped it, so its primary starts 'stopped': nothing
-- reads that without a standby, and adding the first standby (or the next
-- deploy or start) sets it from the real container.
INSERT INTO operated_members (id, instance_id, role, state, created_at, updated_at)
    SELECT lower(hex(randomblob(16))), id, 'primary', 'stopped', created_at, created_at
    FROM operated_instances WHERE engine = 'postgres';

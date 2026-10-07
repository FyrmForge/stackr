-- Migration C: the migration blitz. routes, shares and host_grants; see
-- docs/rewrite/tasks/migration-blitz.md.

-- routes: a host that goes to an address outside stackr; admin only.
-- host is one owner with domains.host and domain_resources.host (checked in
-- code).
CREATE TABLE routes (
    id          TEXT      PRIMARY KEY,
    host        TEXT      NOT NULL UNIQUE,
    mode        TEXT      NOT NULL CHECK (mode IN ('passthrough', 'http', 'https')),
    target      TEXT      NOT NULL,
    insecure    INTEGER   NOT NULL,
    created_at  DATETIME  NOT NULL
);

-- shares: an org's NFS or SMB export tiles mount a sub path of. user and
-- password_ref are ${{ org.params }} refs, never values.
CREATE TABLE shares (
    id            TEXT      PRIMARY KEY,
    org_id        TEXT      NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    slug          TEXT      NOT NULL,
    kind          TEXT      NOT NULL CHECK (kind IN ('nfs', 'smb')),
    source        TEXT      NOT NULL,
    options       TEXT      NOT NULL,
    user          TEXT      NOT NULL,
    password_ref  TEXT      NOT NULL,
    created_at    DATETIME  NOT NULL,
    UNIQUE (org_id, slug)
);

-- host_grants: the host access a server admin approved for a stack. lines is
-- the approved host mount lines, sorted, newline-joined.
CREATE TABLE host_grants (
    id           TEXT      PRIMARY KEY,
    stack_id     TEXT      NOT NULL UNIQUE REFERENCES stacks (id) ON DELETE CASCADE,
    lines        TEXT      NOT NULL,
    privileged   INTEGER   NOT NULL,
    approved_by  TEXT      NOT NULL REFERENCES users (id),
    created_at   DATETIME  NOT NULL
);

-- Registry tokens: one row per token the registry front issued, the audit
-- trail. The token itself is signed and stateless; the row id is its jti.
-- kind is who asked: node (a worker), builder, user or admin.
CREATE TABLE registry_tokens (
    id          TEXT      PRIMARY KEY,
    kind        TEXT      NOT NULL CHECK (kind IN ('node', 'builder', 'user', 'admin')),
    subject_id  TEXT      NOT NULL,
    scope       TEXT      NOT NULL, -- granted access, "repo:actions" per space-separated entry
    created_at  DATETIME  NOT NULL,
    expires_at  DATETIME  NOT NULL
);
CREATE INDEX registry_tokens_subject ON registry_tokens (subject_id, created_at);

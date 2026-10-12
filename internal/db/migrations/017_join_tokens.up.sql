-- Join tokens: one per worker an admin invites. Only the hash of the token
-- is kept. used_at is stamped by the one join request that spends it; that
-- request then fills node_id (the pending node row, no FK: a forgotten node
-- must not erase the history), the CSR, the WireGuard key and the facts the
-- node reported. confirm fills cert_pem, reject stamps rejected_at.
CREATE TABLE join_tokens (
    id             TEXT      PRIMARY KEY,
    hash           TEXT      NOT NULL UNIQUE,
    node_name      TEXT      NOT NULL,
    tags           TEXT      NOT NULL,
    created_by     TEXT      NOT NULL,
    created_at     DATETIME  NOT NULL,
    expires_at     DATETIME  NOT NULL,
    used_at        DATETIME,
    node_id        TEXT      NOT NULL,
    csr            TEXT      NOT NULL,
    wg_public_key  TEXT      NOT NULL,
    facts          TEXT      NOT NULL,
    cert_pem       TEXT      NOT NULL,
    rejected_at    DATETIME
);

CREATE INDEX join_tokens_node ON join_tokens (node_id);

-- Node certs: every cert the cluster CA issued, so one can be revoked.
-- serial is the cert serial in hex; node_id is empty for the controller.
CREATE TABLE node_certs (
    serial      TEXT      PRIMARY KEY,
    node_id     TEXT      NOT NULL,
    role        TEXT      NOT NULL CHECK (role IN ('controller', 'worker')),
    not_after   DATETIME  NOT NULL,
    revoked_at  DATETIME,
    created_at  DATETIME  NOT NULL
);

CREATE INDEX node_certs_node ON node_certs (node_id);

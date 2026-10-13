-- cluster_ca: the CA's state, one row (id is always 1). The keys stay in
-- files on the controller; this holds what the files cannot say. current_fp
-- and previous_fp are root fingerprints (hex SHA-256 of the DER), previous_fp
-- is '' outside a rollover. rollover_started_at is set when a rollover begins
-- and rollover_finished_at when the old root left every trust list;
-- started > finished (or finished NULL) = one is running. break_glass_at is
-- the last break glass. node_certs.issuer is the fingerprint of the root a
-- cert was issued from ('' for certs from before this migration).
CREATE TABLE cluster_ca (
    id                   INTEGER   PRIMARY KEY CHECK (id = 1),
    current_fp           TEXT      NOT NULL,
    previous_fp          TEXT      NOT NULL DEFAULT '',
    rollover_started_at  DATETIME,
    rollover_finished_at DATETIME,
    break_glass_at       DATETIME
);

ALTER TABLE node_certs ADD COLUMN issuer TEXT NOT NULL DEFAULT '';

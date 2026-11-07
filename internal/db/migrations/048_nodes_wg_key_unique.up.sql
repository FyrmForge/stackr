-- One WireGuard key per node: a recovering node cannot take a key another
-- node already holds. Pending nodes may have none yet.
CREATE UNIQUE INDEX nodes_wg_public_key ON nodes(wg_public_key) WHERE wg_public_key != '';

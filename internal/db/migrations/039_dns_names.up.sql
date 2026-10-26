-- DNS names for the panel, nodes and part of a domain (RFC, 26 Oct 2026).
-- dns_domains.scope: the name stackr manages at and under, inside the zone
-- (stackr.example.com on example.com); '' = the whole domain.
ALTER TABLE dns_domains ADD COLUMN scope TEXT NOT NULL DEFAULT '';

-- nodes.address: the node's own DNS name, free text; '' = none.
-- join_tokens.node_address carries the one typed at add-node to the row.
ALTER TABLE nodes ADD COLUMN address TEXT NOT NULL DEFAULT '';
ALTER TABLE join_tokens ADD COLUMN node_address TEXT NOT NULL DEFAULT '';

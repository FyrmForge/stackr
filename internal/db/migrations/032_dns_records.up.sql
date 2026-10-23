-- Managed domains and records (docs/rewrite/tasks/dns-connectors.md, D3).
-- dns_records: the records stackr made on a managed domain, the only ones it
-- ever changes or deletes. One per zone, name and type; domain_id is a tile
-- domain that wants it. Both links are SET NULL so a deleted tile domain or
-- connector leaves the row behind until stackr has deleted the record.
CREATE TABLE dns_records (
    id             TEXT      PRIMARY KEY,
    dns_domain_id  TEXT      REFERENCES dns_domains (id) ON DELETE SET NULL,
    zone           TEXT      NOT NULL,
    name           TEXT      NOT NULL,
    type           TEXT      NOT NULL,
    value          TEXT      NOT NULL,
    ttl            INTEGER   NOT NULL,
    domain_id      TEXT      REFERENCES domains (id) ON DELETE SET NULL,
    created_at     DATETIME  NOT NULL,
    UNIQUE (zone, name, type)
);

-- resource_id: the domain resource stackr made for a managed domain, NULL
-- when it reused one that already held the host (the root, say).
ALTER TABLE dns_domains ADD COLUMN resource_id TEXT REFERENCES domain_resources (id) ON DELETE SET NULL;

-- public_ip: the address records point at instead of the detected one (a
-- floating IP, a load balancer). '' = detected.
ALTER TABLE nodes ADD COLUMN public_ip TEXT NOT NULL DEFAULT '';

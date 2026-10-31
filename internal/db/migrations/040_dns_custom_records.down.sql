CREATE TABLE dns_records_old (
    id             TEXT      PRIMARY KEY,
    dns_domain_id  TEXT      REFERENCES dns_domains (id) ON DELETE SET NULL,
    zone           TEXT      NOT NULL,
    name           TEXT      NOT NULL,
    type           TEXT      NOT NULL,
    value          TEXT      NOT NULL,
    ttl            INTEGER   NOT NULL,
    domain_id      TEXT      REFERENCES domains (id) ON DELETE SET NULL,
    created_at     DATETIME  NOT NULL,
    adopted        INTEGER   NOT NULL DEFAULT 0,
    UNIQUE (zone, name, type)
);
INSERT INTO dns_records_old (id, dns_domain_id, zone, name, type, value, ttl, domain_id, created_at, adopted)
    SELECT id, dns_domain_id, zone, name, type, value, ttl, domain_id, created_at, adopted FROM dns_records
    WHERE kind != 'custom' AND handed_back_at IS NULL;
DROP TABLE dns_records;
ALTER TABLE dns_records_old RENAME TO dns_records;
ALTER TABLE dns_domains DROP COLUMN records;
ALTER TABLE domains DROP COLUMN records;

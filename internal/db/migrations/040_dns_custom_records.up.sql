-- Custom DNS records (RFC "Change scope and custom DNS records", Part 2).
--
-- Declared records live on their owner as a JSON list of {id, name, type,
-- value}: domains.records for an app hostname's (name relative to the host,
-- '' = the host itself), dns_domains.records for a managed domain's own
-- (name relative to the domain, '@' = the apex). They live and die with
-- that row.
ALTER TABLE domains ADD COLUMN records TEXT NOT NULL DEFAULT '[]';
ALTER TABLE dns_domains ADD COLUMN records TEXT NOT NULL DEFAULT '[]';

-- dns_records is rebuilt for three columns and a looser key.
--   kind: app (an A/AAAA for a tile hostname), name (the panel's or a
--     node's own address), custom (a declared record). With domain_id:
--     app + domain_id = App; custom + domain_id = App custom record;
--     custom, no domain_id = Domain custom record; app, no domain_id =
--     Unused (its hostname is gone; an adopted row stays so).
--   put_back_at: the last time stackr put a custom record back after it
--     changed outside stackr.
--   handed_back_at: a narrowing handed the record back to the user; the
--     row stays only to say so, stackr never touches the record again.
-- Address rows stay one per zone, name and type; custom rows one per
-- value too (a name holds several TXT). Handed-back rows are in neither.
CREATE TABLE dns_records_new (
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
    kind           TEXT      NOT NULL DEFAULT 'app',
    put_back_at    DATETIME,
    handed_back_at DATETIME
);
INSERT INTO dns_records_new (id, dns_domain_id, zone, name, type, value, ttl, domain_id, created_at, adopted)
    SELECT id, dns_domain_id, zone, name, type, value, ttl, domain_id, created_at, adopted FROM dns_records;
DROP TABLE dns_records;
ALTER TABLE dns_records_new RENAME TO dns_records;
CREATE UNIQUE INDEX dns_records_addr ON dns_records (zone, name, type)
    WHERE kind != 'custom' AND handed_back_at IS NULL;
CREATE UNIQUE INDEX dns_records_custom ON dns_records (zone, name, type, value)
    WHERE kind = 'custom' AND handed_back_at IS NULL;

ALTER TABLE nodes DROP COLUMN public_ip;
ALTER TABLE dns_domains DROP COLUMN resource_id;
DROP TABLE IF EXISTS dns_records;

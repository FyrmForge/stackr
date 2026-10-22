DROP TABLE IF EXISTS dns_domains;
DELETE FROM connectors WHERE EXISTS (SELECT 1 FROM json_each(capabilities) WHERE value = 'dns');
ALTER TABLE connectors DROP COLUMN capabilities;

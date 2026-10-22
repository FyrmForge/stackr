DROP TABLE IF EXISTS dns_domains;
DELETE FROM connectors WHERE capabilities <> '["git"]';
ALTER TABLE connectors DROP COLUMN capabilities;

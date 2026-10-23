DROP INDEX IF EXISTS connectors_org_kind;
ALTER TABLE connectors RENAME COLUMN settings TO config;
ALTER TABLE connectors RENAME COLUMN kind TO provider;

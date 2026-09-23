-- migration-guard: allow reverses 002, which only added this column
ALTER TABLE api_keys DROP COLUMN org_id;

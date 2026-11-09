-- A postgres instance's own settings: the user's overrides of the computed
-- defaults, a JSON object of name to value.
ALTER TABLE managed_instances ADD COLUMN settings TEXT NOT NULL DEFAULT '{}';

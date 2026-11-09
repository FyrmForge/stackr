-- A postgres instance's own settings: the user's overrides of the computed
-- defaults, a JSON object of name to value.
ALTER TABLE operated_instances ADD COLUMN settings TEXT NOT NULL DEFAULT '{}';
-- The postgres major the instance's data was made by: its data path and start
-- flags follow it, and an image of another major is refused. 0 = no engine
-- that has one.
ALTER TABLE operated_instances ADD COLUMN major INTEGER NOT NULL DEFAULT 0;

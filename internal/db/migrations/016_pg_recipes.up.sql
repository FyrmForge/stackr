-- Postgres recipes. ext_build is a recipe-built instance's build record
-- ('{}' = not recipe-built); pg_recipes holds admin-added recipes and the
-- switch-off rows of built-ins; backup_runs.image_key is the saved image a
-- base backup needs ('' = none). Existing rows keep behaving as before.
ALTER TABLE operated_instances ADD COLUMN ext_build TEXT NOT NULL DEFAULT '{}';
CREATE TABLE pg_recipes (
    id         TEXT      PRIMARY KEY,
    spec       TEXT      NOT NULL DEFAULT '{}',
    enabled    INTEGER   NOT NULL DEFAULT 1,
    share      TEXT      NOT NULL DEFAULT '[]',
    created_at TIMESTAMP NOT NULL
);
ALTER TABLE backup_runs ADD COLUMN image_key TEXT NOT NULL DEFAULT '';

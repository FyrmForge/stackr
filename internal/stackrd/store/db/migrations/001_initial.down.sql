-- Full teardown, children before parents: SQLite enforces foreign keys, so
-- dropping a parent while a child still references it fails. The order is
-- topological on the foreign keys, not reverse creation order: the baseline
-- creates its tables alphabetically, so api_keys exists before the users it
-- references.

DROP TABLE IF EXISTS api_keys;
DROP TABLE IF EXISTS backup_runs;
DROP TABLE IF EXISTS backups;
DROP TABLE IF EXISTS config_plans;
DROP TABLE IF EXISTS connectors;
DROP TABLE IF EXISTS cron_runs;
DROP TABLE IF EXISTS deployments;
DROP TABLE IF EXISTS domains;
DROP TABLE IF EXISTS invites;
DROP TABLE IF EXISTS managed_resources;
DROP TABLE IF EXISTS metrics;
DROP TABLE IF EXISTS node_positions;
DROP TABLE IF EXISTS notifications;
DROP TABLE IF EXISTS org_members;
DROP TABLE IF EXISTS provisions;
DROP TABLE IF EXISTS registries;
DROP TABLE IF EXISTS resource_bindings;
DROP TABLE IF EXISTS resource_outputs;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS settings;
DROP TABLE IF EXISTS staged_changes;
DROP TABLE IF EXISTS tiles;
DROP TABLE IF EXISTS users;
DROP TABLE IF EXISTS variables;
DROP TABLE IF EXISTS domain_resources;
DROP TABLE IF EXISTS annotations;
DROP TABLE IF EXISTS secret_links;
DROP TABLE IF EXISTS audit_events;
DROP TABLE IF EXISTS graph_groups;
DROP TABLE IF EXISTS storage_paths;
DROP TABLE IF EXISTS org_config_plans;
DROP TABLE IF EXISTS env_intended;
DROP TABLE IF EXISTS compose_tiles_archive;
DROP TABLE IF EXISTS join_keys;
DROP TABLE IF EXISTS work_items;
DROP TABLE IF EXISTS org_registry_credentials;
DROP TABLE IF EXISTS backup_destinations;
DROP TABLE IF EXISTS environments;
DROP TABLE IF EXISTS stacks;
DROP TABLE IF EXISTS storage;
DROP TABLE IF EXISTS orgs;
DROP TABLE IF EXISTS servers;

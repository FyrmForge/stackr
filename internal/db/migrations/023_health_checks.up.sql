-- Health checks: the tile's two checks get one flat naming scheme,
-- health_<kind>_<field>. The HTTP check gains status and timing columns
-- (0 = default); the exec check keeps its values under the new names.
ALTER TABLE tiles RENAME COLUMN health_path TO health_http_path;
ALTER TABLE tiles RENAME COLUMN healthcheck_cmd TO health_exec_cmd;
ALTER TABLE tiles RENAME COLUMN healthcheck_interval_s TO health_exec_interval_s;
ALTER TABLE tiles RENAME COLUMN healthcheck_timeout_s TO health_exec_timeout_s;
ALTER TABLE tiles RENAME COLUMN healthcheck_retries TO health_exec_retries;
ALTER TABLE tiles RENAME COLUMN healthcheck_start_period_s TO health_exec_start_period_s;
ALTER TABLE tiles ADD COLUMN health_http_status INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tiles ADD COLUMN health_http_interval_s INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tiles ADD COLUMN health_http_timeout_s INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tiles ADD COLUMN health_http_retries INTEGER NOT NULL DEFAULT 0;
-- Store the command and path trimmed, as tile validation does, so the first
-- save after upgrade sees no change.
UPDATE tiles SET
    health_exec_cmd = trim(health_exec_cmd, ' ' || char(9) || char(10) || char(13)),
    health_http_path = trim(health_http_path, ' ' || char(9) || char(10) || char(13));
-- Exec timings mean nothing without a command.
UPDATE tiles SET health_exec_interval_s = 0, health_exec_timeout_s = 0,
    health_exec_retries = 0, health_exec_start_period_s = 0
WHERE health_exec_cmd = '';

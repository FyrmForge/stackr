-- job_tiles: one row per id in a job's lock_set (tiles, envs, plans, "*"), kept
-- by triggers so every write path stays in step. Lets ListTouching use an
-- index instead of json_each over every job.
CREATE TABLE job_tiles (
    job_id   TEXT  NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    tile_id  TEXT  NOT NULL,
    PRIMARY KEY (job_id, tile_id)
);
CREATE INDEX job_tiles_tile ON job_tiles (tile_id, job_id);

INSERT OR IGNORE INTO job_tiles (job_id, tile_id)
SELECT j.id, v.value FROM jobs j, json_each(j.lock_set) v;

CREATE TRIGGER jobs_tiles_insert AFTER INSERT ON jobs
BEGIN
    INSERT OR IGNORE INTO job_tiles (job_id, tile_id)
    SELECT NEW.id, value FROM json_each(NEW.lock_set);
END;

CREATE TRIGGER jobs_tiles_update AFTER UPDATE OF lock_set ON jobs
WHEN OLD.lock_set IS NOT NEW.lock_set
BEGIN
    DELETE FROM job_tiles WHERE job_id = NEW.id;
    INSERT OR IGNORE INTO job_tiles (job_id, tile_id)
    SELECT NEW.id, value FROM json_each(NEW.lock_set);
END;

DROP INDEX idx_jobs_state;
CREATE INDEX idx_jobs_state_created ON jobs (state, created_at);
CREATE INDEX idx_jobs_finished ON jobs (finished_at);
CREATE INDEX runs_status ON runs (status);
CREATE INDEX backup_runs_volume ON backup_runs (volume_id);
CREATE INDEX backup_runs_dest ON backup_runs (dest_id);
CREATE INDEX backup_runs_kind ON backup_runs (kind);
CREATE INDEX release_tiles_image ON release_tiles (image_id);
CREATE INDEX registry_tokens_expires ON registry_tokens (expires_at);
CREATE INDEX idempotency_keys_created ON idempotency_keys (created_at);

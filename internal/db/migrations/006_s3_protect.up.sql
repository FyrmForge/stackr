-- A bucket slice's data protection: days old versions are kept (NULL = the
-- default, 0 = versioning off), a hard size cap in GB (NULL = none) and the
-- origins allowed by CORS.
ALTER TABLE tiles ADD COLUMN version_days INTEGER;
ALTER TABLE tiles ADD COLUMN quota_gb INTEGER;
ALTER TABLE tiles ADD COLUMN cors_origins TEXT NOT NULL DEFAULT '[]';

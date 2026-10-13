-- registry_images: one row per image version in the cluster registry (a repo
-- and a manifest digest), written by the registry leaf on every import and
-- every build push. The registry decides what exists; a row adds detail and
-- one without an image is not shown. org_id is NULL for stackr's own images
-- (source 'system', repo under stackr.system/). platforms is the CPU types
-- behind the digest, comma separated ("amd64,arm64"). upstream_ref is the
-- outside ref an import came from, '' for builds. tag is the one the version
-- landed under; later tags on the same digest are not recorded.
CREATE TABLE registry_images (
    id            TEXT      PRIMARY KEY,
    org_id        TEXT      REFERENCES orgs (id) ON DELETE CASCADE,
    repo          TEXT      NOT NULL,
    tag           TEXT      NOT NULL,
    digest        TEXT      NOT NULL,
    platforms     TEXT      NOT NULL,
    source        TEXT      NOT NULL CHECK (source IN ('built', 'imported', 'system')),
    upstream_ref  TEXT      NOT NULL,
    size          INTEGER   NOT NULL,
    created_by    TEXT      NOT NULL,
    created_at    DATETIME  NOT NULL,
    UNIQUE (repo, digest)
);

CREATE INDEX registry_images_org ON registry_images (org_id);

-- Image watch reads the image's org.opencontainers.image.version label when
-- it sees a new digest. images.last_version is the label of last_digest (the
-- cache, one read per digest); release_tiles.version is the label of the
-- digest a release pins. '' = no label or not read.
ALTER TABLE images ADD COLUMN last_version TEXT NOT NULL DEFAULT '';
ALTER TABLE release_tiles ADD COLUMN version TEXT NOT NULL DEFAULT '';
-- release_tiles.version_err is how the label read went when the release was
-- cut: the failure text, '' = read fine or not tried. It never changes after.
ALTER TABLE release_tiles ADD COLUMN version_err TEXT NOT NULL DEFAULT '';
-- images.version_err says the last label read failed ('' = fine); set by image
-- watch on a failed read, cleared on success.
ALTER TABLE images ADD COLUMN version_err TEXT NOT NULL DEFAULT '';

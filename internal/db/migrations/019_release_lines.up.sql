-- Releases remember what they were built on and which line they were cut for,
-- so a new cut can build on the release that waits (see the release leaf's
-- Newest). built_on is a release id (no foreign key: cleanup may delete the
-- release it names, which reads as "not on the line"); line is 'env:<env id>'
-- or 'branch:<name>'. Releases cut before this keep both NULL.
ALTER TABLE releases ADD COLUMN built_on TEXT;
ALTER TABLE releases ADD COLUMN line TEXT;

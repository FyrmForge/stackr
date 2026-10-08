-- Migration G: elevated access. Grants are per tile (their lines carry the
-- tile slug, `<slug> <perm>`), so the privileged column goes and the old
-- stack-wide grants are wiped: stacks ask again. Tiles gain lan lines and the
-- host network flag.
DELETE FROM host_grants;
ALTER TABLE host_grants DROP COLUMN privileged;

ALTER TABLE tiles ADD COLUMN lan TEXT NOT NULL DEFAULT ''; -- lines: ip|cidr[:port] or all
ALTER TABLE tiles ADD COLUMN host_network INTEGER NOT NULL DEFAULT 0;

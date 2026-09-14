# Automatic rollback

Status: parked 2026-09-08; nothing built.

The proposal would restore every service in a failed promotion batch to its
previous image, controlled per org, stack or environment and disabled by
default. It remains parked because health semantics and migrations need a
separate product decision; manual rollback already exists.

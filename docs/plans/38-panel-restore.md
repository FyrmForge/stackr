# Panel backup and restore

Status: shipped and restored end-to-end on the rig 2026-09-14.

Panel backups are `stackr.tar.gz` archives containing the SQLite database,
master key and version metadata. Restore validates the archive, stops the
panel, installs the database and key atomically, fixes ownership and starts
the service. See `infra/backup`, `config/secrets` and `scripts/restore.sh`.

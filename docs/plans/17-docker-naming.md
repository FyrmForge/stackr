# Docker naming

Status: shipped and rig-verified 2026-09-06.

Runtime resources use the `stkr-` prefix, include the organization segment
where tenancy requires it, and tag build images by commit. Existing volume
names remain stable because renaming a Docker volume would detach its data.
Naming helpers live in the repo models and infrastructure packages.

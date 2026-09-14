# Security and tenancy review fixes

Status: shipped 2026-09-11.

Panel-wide API operations became admin-only, mutating org routes require
write access, graph and tile redirects enforce tenancy, credential endpoints
are rate-limited, Git URLs are allow-listed, and CI actions are SHA-pinned.
Coverage lives beside the API, middleware, deploy and repo implementations.

# Ignored store writes

Status: shipped 2026-09-14.

State-changing store calls whose errors were discarded now return or record
failure, including logout session deletion and asynchronous infrastructure
paths. Best-effort cleanup remains explicit where retrying the primary
operation is safer. Regression tests cover the user-visible state cases.

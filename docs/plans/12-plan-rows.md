# Declared values as plan rows

Status: shipped 2026-09-05.

Declared vars and secrets render as first-class plan changes and can be set
from the plan without leaving it. Stack and org rows share the plan
components; validation and persistence remain in `stackconf`, `orgconf` and
the repo layer.

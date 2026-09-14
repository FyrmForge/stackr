# Config overhaul

Status: shipped 2026-08-18, completed by later config work.

The config format moved to `shared:`, `base:` and per-environment tiles;
managed slices, run policies, generated and required secrets, domain resources,
graph preferences and org config all landed. The current contract lives in
`docs/features/config-as-code.md`; implementations live under
`internal/stackrd/config/stackconf` and `internal/stackrd/config/orgconf`.

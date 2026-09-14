# `stackr plan preview`

Status: shipped 2026-08-19.

The CLI can submit a local config bundle for a non-persisting dry run, render
the server's change set and return distinct clean, changed and error exit
codes. The command lives in `internal/cli/cmd`; planning remains owned by
`internal/stackrd/config/stackconf`.

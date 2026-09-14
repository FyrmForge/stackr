# CLI UX rewrite

Status: shipped 2026-08-21.

The CLI moved to Cobra and Fang with consistent nouns, aliases, prompts,
tables, JSON output and exit behavior. Commands and shared rendering live in
`internal/cli/cmd`; the original command matrix is summarized in
[plan 08](08-cli-ux-matrices.md).

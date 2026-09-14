# Releases page

Status: shipped 2026-09-08.

Each stack has one page showing known builds, what commit each environment is
running, pending plans and promotion actions. It can promote a built commit
with or without a config plan. The page lives in
`internal/stackrd/handlers/web/handler/project/releases.go` and its template.

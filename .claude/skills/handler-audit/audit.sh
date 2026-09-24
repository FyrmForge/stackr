#!/usr/bin/env bash
# Read-only grep pass for the handler-audit skill. Prints candidate lines as
# file:line:text under one heading per category. Candidates, not verdicts:
# the skill reads each hit and drops false positives.
# Usage: audit.sh [dir ...]   (default: internal/web/handler internal/api/handler)
set -u
dirs=("$@")
[ ${#dirs[@]} -eq 0 ] && dirs=(internal/web/handler internal/api/handler)

go_grep() { grep -rnE --include='*.go' --exclude='*_templ.go' --exclude='*_test.go' "$1" "${dirs[@]}" 2>/dev/null; }

section() { echo "== $1"; }

section "store (handler holds or calls a store, repo, sqlx, SQL)"
go_grep 'internal/repo|internal/service/internal/store|repo\.Store|[Ss]tore\b[^"]*\.|\bstore\.|sqlx|database/sql|\.(QueryRow|QueryContext|ExecContext|Select|Get)\(ctx'

section "docker (handler touches Docker or an infra wrapper)"
go_grep 'docker|ContainerStart|ContainerStop|ContainerCreate|internal/service/internal/(docker|proxy|git|s3)'

section "auth (handler checks who the user is or may do)"
go_grep 'GetSubject(ID)?\(|\.Role\b|IsAdmin|authz\.|can\(|StatusForbidden|StatusUnauthorized|CheckPassword|ValidateSession|SessionManager\.|sessionManager\.'

section "domain decision (status computed, default chosen, rule applied)"
go_grep '\.(Status|State|Kind|Position)\s*(=|==|!=)[^=]|switch [^{]*\.(Status|State|Kind|Role)|time\.Now\(\)|uuid\.New|strings\.ToLower|[Dd]efault[A-Z]?[a-z]*\s*(:=|=)|if [^{]*(len\(|>=|<=| > | < )'

section "templ decisions (if/for/switch with logic in a .templ)"
grep -rnE --include='*.templ' '^\s*(if|for|switch)\b.*(==|!=|&&|\|\||len\(|<|>|!)' "${dirs[@]}" 2>/dev/null
exit 0

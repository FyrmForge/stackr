#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd -- "$SCRIPT_DIR/.." && pwd)"

if [[ -f "$PROJECT_ROOT/.env" ]]; then
	set -a
	# shellcheck disable=SC1091
	source "$PROJECT_ROOT/.env"
	set +a
fi

# Apply hamr dev's port walks on top of .env so this script connects to the
# right port when hamr dev shifted DATABASE_URL off its configured default.
# `--dir` resolves walks/.env from the project root regardless of where the
# script is invoked from. `hamr env` exits 0 with no output when no walks
# are recorded so the eval is a safe no-op in that case.
if command -v hamr >/dev/null 2>&1; then
	eval "$(hamr env --dir "$PROJECT_ROOT" --export 2>/dev/null || true)"
fi
if ! command -v sqlite3 >/dev/null 2>&1; then
	echo "sqlite3 not found. Install it via your package manager." >&2
	exit 1
fi

DB_PATH="${DATABASE_PATH:-$PROJECT_ROOT/data/stackr.db}"
exec sqlite3 "$DB_PATH"

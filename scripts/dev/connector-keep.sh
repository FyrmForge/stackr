#!/usr/bin/env bash
# Keep the test box's GitHub connectors across a wipe, so nobody has to redo
# the GitHub App flow every time. The rows (including the App's private key)
# never leave the box: save writes them to a file beside the data dir, restore
# reads it back. Do not cat the file.
#
#   scripts/dev/connector-keep.sh save                # before a wipe
#   scripts/dev/connector-keep.sh restore <org-slug>  # after deploy + org setup
#
# Restore rewrites org_id to the named org: a wiped box recreates the org
# under a new id, and connectors cascade-delete with their org.
#
# Development-only helper. SQLite runs through a throwaway container because
# the test host has neither sqlite3 nor sudo.
set -euo pipefail

HOST="${STACKR_DEPLOY_HOST:?set STACKR_DEPLOY_HOST to the test manager}"
DATA_DIR="${STACKR_DATA_DIR:-$HOME/stackr/data}"
KEEP="${STACKR_CONNECTOR_KEEP:-$HOME/stackr/connectors-keep.json}"
cmd="${1:-}"

py() {
  # $1: python source; the data dir is /d, the keep file's dir is /k
  ssh "$HOST" "docker run --rm -i -v $DATA_DIR:/d -v \$(dirname $KEEP):/k python:3-alpine python3 -" <<<"$1"
}

case "$cmd" in
save)
  py "
import sqlite3, json
c = sqlite3.connect('/d/stackr.db')
rows = [dict(zip(('id','org_id','provider','name','config','created_at'), r))
        for r in c.execute('SELECT id, org_id, provider, name, config, created_at FROM connectors')]
json.dump(rows, open('/k/$(basename "$KEEP")', 'w'))
print(f'saved {len(rows)} connector(s) to $KEEP')
"
  ;;
restore)
  slug="${2:?org slug required}"
  py "
import sqlite3, json
c = sqlite3.connect('/d/stackr.db')
org = c.execute('SELECT id FROM orgs WHERE slug = ?', ('$slug',)).fetchone()
if not org:
    raise SystemExit('no org with slug $slug on the box')
rows = json.load(open('/k/$(basename "$KEEP")'))
n = 0
for r in rows:
    if c.execute('SELECT 1 FROM connectors WHERE id = ?', (r['id'],)).fetchone():
        continue
    c.execute('INSERT INTO connectors (id, org_id, provider, name, config, created_at) VALUES (?,?,?,?,?,?)',
              (r['id'], org[0], r['provider'], r['name'], r['config'], r['created_at']))
    n += 1
c.commit()
print(f'restored {n} connector(s) into org $slug')
"
  ;;
*)
  echo "usage: $0 save | restore <org-slug>" >&2
  exit 1
  ;;
esac

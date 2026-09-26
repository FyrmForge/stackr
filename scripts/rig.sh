#!/usr/bin/env bash
# The test rig: save what a pave must not lose, pave, restore.
#
#   scripts/rig.sh save                    snapshot connectors, master key, install.json, Caddy certs
#   scripts/rig.sh ship <version>          load the image and installer of a `make release` onto the rig
#   scripts/rig.sh pave <version>          save, wipe the DB and tiles, install <version> fresh
#   scripts/rig.sh restore [--org <slug>] [<snapshot>]
#                                          put the saved connectors back under <slug> (default: the only org)
#   scripts/rig.sh snapshots               list snapshots
#
# Snapshots live on the rig in $PRESERVE/<timestamp>/ with `latest` pointing at
# the newest. Connector rows are copied as stored (the config column is
# encrypted with keys/master.key, which a pave keeps); a restore refuses when
# the rig's master key no longer matches the snapshot's. Connector ids are
# kept, so GitHub's webhook URL (/hooks/connectors/<id>) stays valid.
#
# ponytail: sqlite through a python:3-alpine container because the data dir
# is root-owned and the rig has no sqlite3; a v0 connectors.json (plaintext)
# cannot be restored here, it needs the app's encryption.
set -euo pipefail

RIG="${RIG:-stackr-test.inanga-degree.ts.net}"
DATA="${RIG_DATA:-/var/lib/stackr}"
PRESERVE="${RIG_PRESERVE:-/home/darthvader/stackr-preserve/rig}"
INSTALLER="${RIG_INSTALLER:-/home/darthvader/stackr-install}"
DOMAIN="${RIG_DOMAIN:-stackr-test.vulpe.dev}"
EMAIL="${RIG_EMAIL:-dumitru.v.dv@gmail.com}"
IMAGE="ghcr.io/fyrmforge/stackr"

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd -- "$SCRIPT_DIR/.." && pwd)"

rig() { ssh "$RIG" "$@"; }

# py runs a python script on the rig with the data dir at /d and the
# snapshot dir at /s.
py() {
	local snap="$1"
	shift
	rig docker run --rm -i -v "$DATA:/d" -v "$snap:/s" python:3-alpine python3 - "$@"
}

save() {
	local ts snap
	ts="$(date -u +%Y%m%dT%H%M%SZ)"
	snap="$PRESERVE/$ts"
	rig mkdir -p "$snap"
	py "$snap" <<'PY'
import json, shutil, sqlite3
db = sqlite3.connect("/d/stackr.db")
rows = db.execute("SELECT id, org_id, provider, name, host, config, created_at FROM connectors").fetchall()
cols = ["id", "org_id", "provider", "name", "host", "config", "created_at"]
orgs = dict(db.execute("SELECT id, slug FROM orgs").fetchall())
out = [dict(zip(cols, r)) | {"org_slug": orgs.get(r[1], "")} for r in rows]
json.dump(out, open("/s/connectors.json", "w"), indent=1)
shutil.copy("/d/keys/master.key", "/s/master.key")
shutil.copy("/d/install.json", "/s/install.json")
print(f"{len(out)} connector(s) saved")
PY
	rig docker run --rm -v stackr-caddy:/c -v "$snap:/s" alpine tar czf /s/caddy.tgz -C /c .
	rig ln -sfn "$snap" "$PRESERVE/latest"
	echo "snapshot $ts"
}

ship() {
	local v="$1"
	docker save "$IMAGE:$v" | rig docker load
	scp -q "$PROJECT_ROOT/bin/stackr-install-linux-amd64" "$RIG:$INSTALLER"
	echo "shipped $v"
}

install() {
	rig docker run --rm --privileged --net=host --pid=host -v /:/host alpine \
		chroot /host "$INSTALLER" --domain "$DOMAIN" --panel-host "$DOMAIN" --email "$EMAIL" >/dev/null 2>&1 || true
	rig docker ps --format "'{{.Names}} {{.Image}} {{.Status}}'"
}

pave() {
	local v="$1"
	save
	rig 'docker ps -aq --filter name=stackr- | xargs -r docker rm -f >/dev/null; docker rm -f stackr stackr-proxy >/dev/null 2>&1 || true'
	rig 'docker volume ls -q | grep -E "^stackr-(db|vol|stor)-" | xargs -r docker volume rm >/dev/null'
	rig docker run --rm -v "$DATA:/d" alpine sh -c "'rm -rf /d/stackr.db /d/stackr.db-shm /d/stackr.db-wal /d/jobs /d/runs /d/backups'"
	# keys/, install.json and the stackr-caddy volume (certs) stay.
	install
	echo "paved to $v; register the admin, then: scripts/rig.sh restore --org <slug>"
}

restore() {
	local org="" snap="latest"
	while [[ $# -gt 0 ]]; do
		case "$1" in
		--org) org="$2"; shift 2 ;;
		*) snap="$1"; shift ;;
		esac
	done
	local dir="$PRESERVE/$snap"
	if ! rig docker volume inspect stackr-caddy >/dev/null 2>&1; then
		rig docker volume create stackr-caddy >/dev/null
		rig docker run --rm -v stackr-caddy:/c -v "$dir:/s" alpine tar xzf /s/caddy.tgz -C /c
		echo "caddy certs restored"
	fi
	ORG="$org" py "$dir" <<'PY'
import json, os, sqlite3, sys
if open("/d/keys/master.key").read() != open("/s/master.key").read():
    sys.exit("master.key differs from the snapshot's: the saved connectors cannot be decrypted here")
db = sqlite3.connect("/d/stackr.db", timeout=10)
orgs = db.execute("SELECT id, slug FROM orgs").fetchall()
want = os.environ.get("ORG") or ""
if want:
    ids = [i for i, s in orgs if s == want]
    if not ids:
        sys.exit(f"no org with slug {want!r}; orgs: {[s for _, s in orgs]}")
    org_id = ids[0]
elif len(orgs) == 1:
    org_id = orgs[0][0]
else:
    sys.exit(f"pass --org <slug>; orgs: {[s for _, s in orgs]}")
n = 0
for c in json.load(open("/s/connectors.json")):
    cur = db.execute(
        "INSERT OR IGNORE INTO connectors (id, org_id, provider, name, host, config, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
        (c["id"], org_id, c["provider"], c["name"], c["host"], c["config"], c["created_at"]),
    )
    n += cur.rowcount
db.commit()
print(f"{n} connector(s) restored")
PY
}

snapshots() {
	rig ls -1 "$PRESERVE" 2>/dev/null || true
}

cmd="${1:-}"
shift || true
case "$cmd" in
save) save ;;
ship) ship "$@" ;;
pave) pave "$@" ;;
restore) restore "$@" ;;
snapshots) snapshots ;;
*)
	sed -n '2,15p' "$0"
	exit 1
	;;
esac

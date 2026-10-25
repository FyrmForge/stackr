#!/usr/bin/env bash
# The test rig: save what a pave must not lose, pave, restore.
#
#   scripts/rig.sh save                    snapshot connectors, master key, install.json, Caddy certs
#   scripts/rig.sh ship <version>          load the images and installer of a `make release` onto the rig
#   scripts/rig.sh pave <version>          save, wipe the DB and tiles, install <version> fresh
#   scripts/rig.sh upgrade <version>       ship, then replace the stackr and proxy containers, DB kept
#   scripts/rig.sh restore [--org <slug>] [<snapshot>]
#                                          put the saved connectors back under <slug> (default: the only org)
#   scripts/rig.sh snapshots               list snapshots
#
# Snapshots live on the rig in $PRESERVE/<timestamp>/ with `latest` pointing at
# the newest. Connector rows are copied as stored, every column (settings is
# encrypted with keys/master.key, which a pave keeps; the panel reshapes an
# old blob as it starts), with their shares by org slug. A server connector
# stays the server's; an org's goes back to the org of its slug, else --org.
# A restore refuses when the rig's master key no longer matches the snapshot's. Connector ids are
# kept, so GitHub's webhook URL (/hooks/connectors/<id>) stays valid.
#
# ponytail: sqlite through a python:3-alpine container because the data dir
# is root-owned and the rig has no sqlite3; a v0 connectors.json (plaintext)
# cannot be restored here, it needs the app's encryption.
set -euo pipefail

RIG="${RIG:-root@192.168.1.109}"
DATA="${RIG_DATA:-/var/lib/stackr}"
PRESERVE="${RIG_PRESERVE:-/home/darthvader/stackr-preserve/rig}"
INSTALLER="${RIG_INSTALLER:-/root/stackr-install}"
DOMAIN="${RIG_DOMAIN:-stackr-lxc.vulpe.dev}"
EMAIL="${RIG_EMAIL:-dumitru.v.dv@gmail.com}"
# The rig's own network proxy: Caddy trusts its X-Forwarded-For, so the panel
# sees real client addresses (the login limit keys on them). Never Tailscale.
FRONT_PROXY="${RIG_FRONT_PROXY:-192.168.1.100/32}"
ADMIN_EMAIL="${RIG_ADMIN_EMAIL:-admin@test.com}"
ADMIN_PASSWORD="${RIG_ADMIN_PASSWORD:-$(sed -n 's/^UNIVERSAL_STACKR_PASSWORD=//p' "$(git rev-parse --show-toplevel)/.env" 2>/dev/null)}"
[[ -n "$ADMIN_PASSWORD" ]] || { echo "set UNIVERSAL_STACKR_PASSWORD in .env" >&2; exit 1; }
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
db.row_factory = sqlite3.Row
orgs = dict(db.execute("SELECT id, slug FROM orgs").fetchall())
# every column as stored, under the current names (033 renamed provider and
# config to kind and settings), plus the org's slug and the share slugs
ren = {"provider": "kind", "config": "settings"}
out = []
for r in db.execute("SELECT * FROM connectors"):
    c = {ren.get(k, k): r[k] for k in r.keys()}
    c["org_slug"] = orgs.get(c["org_id"], "")
    c["share_slugs"] = [orgs.get(o, "") for (o,) in
        db.execute("SELECT org_id FROM connector_shares WHERE connector_id = ?", (c["id"],))]
    out.append(c)
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
	docker save "$IMAGE:$v" "$IMAGE-observe:$v" | rig docker load
	scp -q "$PROJECT_ROOT/bin/stackr-install-linux-amd64" "$RIG:$INSTALLER"
	echo "shipped $v"
}

# install makes the rig's admin on a fresh DB (an upgrade keeps the one it
# has). The password is the rig's fixed test login, so passing it inline is fine.
install() {
	# Straight over ssh, not from a container: the installer may restart
	# Docker (the log cap), which would kill a container it ran in.
	rig "umask 077;" STACKR_ADMIN_PASSWORD="$(printf %q "$ADMIN_PASSWORD")" "$INSTALLER" --domain "$DOMAIN" --panel-host "$DOMAIN" --email "$EMAIL" --proxy "$FRONT_PROXY" \
		--admin-email "$ADMIN_EMAIL" '>/root/rig-install.log' '2>&1' ||
		{ echo "installer failed; see /root/rig-install.log on the rig (it holds the recovery key)" >&2; exit 1; }
	rig docker ps --format "'{{.Names}} {{.Image}} {{.Status}}'"
}

pave() {
	local v="$1"
	save
	rig 'docker ps -aq --filter name=stackr- | xargs -r docker rm -f >/dev/null; docker rm -f stackr stackr-proxy >/dev/null 2>&1 || true'
	rig 'docker volume ls -q | grep -E "^stackr-(db|vol|stor)-" | xargs -r docker volume rm >/dev/null'
	rig 'docker network ls -q --filter name=stackr- | xargs -r docker network rm >/dev/null 2>&1 || true'
	rig docker run --rm -v "$DATA:/d" alpine sh -c "'rm -rf /d/stackr.db /d/stackr.db-shm /d/stackr.db-wal /d/jobs /d/runs /d/backups'"
	# keys/, install.json and the stackr-caddy volume (certs) stay.
	install
	echo "paved to $v; log in as $ADMIN_EMAIL, make the org, then: scripts/rig.sh restore --org <slug>"
}

# upgrade keeps the DB: the installer upgrades a panel on another version in
# place (archive, swap, health, workers). Re-shipping the same version is a
# no-op, so bump the dev number.
upgrade() {
	ship "$1"
	install
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
slugs = {s: i for i, s in orgs}
cols = [r[1] for r in db.execute("PRAGMA table_info(connectors)")]
n = 0
for c in json.load(open("/s/connectors.json")):
    # a snapshot from before 033 has the old column names
    c = {{"provider": "kind", "config": "settings"}.get(k, k): v for k, v in c.items()}
    if c.get("org_id") is not None:  # a server connector stays the server's
        c["org_id"] = slugs.get(c.get("org_slug", ""), org_id)
    row = {k: c[k] for k in cols if k in c}
    cur = db.execute(
        f"INSERT OR IGNORE INTO connectors ({', '.join(row)}) VALUES ({', '.join('?' * len(row))})",
        tuple(row.values()),
    )
    n += cur.rowcount
    for s in c.get("share_slugs", []):
        if s in slugs:
            db.execute("INSERT OR IGNORE INTO connector_shares (connector_id, org_id) VALUES (?, ?)", (c["id"], slugs[s]))
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
upgrade) upgrade "$@" ;;
restore) restore "$@" ;;
snapshots) snapshots ;;
*)
	sed -n '2,15p' "$0"
	exit 1
	;;
esac

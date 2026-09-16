#!/usr/bin/env bash
# Restore a stackr panel backup onto this host.
#
# The archive is what /admin/backups produces for a panel backup: stackr.db,
# keys/master.key and VERSION. It carries the master key, so treat it like a
# password file and delete it when you are done.
#
# Fetching the archive is yours. This script touches nothing but the local
# data dir and the stackr service.
set -euo pipefail

usage() {
  echo "usage: $0 <archive.tar.gz> [data-dir]" >&2
  echo "  data-dir defaults to /var/lib/stackr" >&2
  exit 2
}

[ $# -ge 1 ] || usage
ARCHIVE="$1"
DATA_DIR="${2:-/var/lib/stackr}"

[ -f "$ARCHIVE" ] || { echo "no such archive: $ARCHIVE" >&2; exit 1; }
[ -d "$DATA_DIR" ] || { echo "no such data directory: $DATA_DIR" >&2; exit 1; }
command -v docker >/dev/null || { echo "docker is not installed" >&2; exit 1; }

# Refuse an archive that is missing a member rather than half-restore one. A
# database without its key is exactly the failure this script exists to end.
members=$(tar tzf "$ARCHIVE")
for want in stackr.db keys/master.key VERSION; do
  echo "$members" | grep -qx "$want" || {
    echo "archive is missing $want, refusing to restore" >&2
    echo "  a panel archive written before this script will not have the key" >&2
    exit 1
  }
done

# Everything that can fail has to fail before the service goes down. Running
# this as a normal user against a root-owned data dir would otherwise scale the
# panel to zero and then die on the unpack, leaving it down and unrestored.
probe="$DATA_DIR/.restore-probe.$$"
if ! touch "$probe" 2>/dev/null; then
  echo "cannot write $DATA_DIR, run this as root (or with sudo)" >&2
  exit 1
fi
rm -f "$probe"
if ! mkdir -p "$DATA_DIR/keys" 2>/dev/null; then
  echo "cannot create $DATA_DIR/keys, run this as root (or with sudo)" >&2
  exit 1
fi
if ! docker service inspect stackr >/dev/null 2>&1; then
  echo "no stackr service on this host, is this the manager?" >&2
  exit 1
fi

archive_version=$(tar xzf "$ARCHIVE" -O VERSION | tr -d '[:space:]')
running_image=$(docker service inspect stackr \
  --format '{{.Spec.TaskTemplate.ContainerSpec.Image}}')

# An archive from a newer build than the running one holds a schema the
# running binary predates, so the service moves to the archive's release.
# An older archive keeps the current image and migrates forward on boot.
# Only a release tag on both sides can be compared; :latest or a local build
# is left alone and reported below.
semver='^[0-9]+\.[0-9]+\.[0-9]+$'
archive_tag=${archive_version#v}
running_tag=${running_image%%@*}
running_tag=${running_tag##*:}
pin=""
if printf '%s' "$archive_tag" | grep -Eq "$semver" &&
  printf '%s' "$running_tag" | grep -Eq "$semver" &&
  [ "$archive_tag" != "$running_tag" ] &&
  [ "$(printf '%s\n%s\n' "$running_tag" "$archive_tag" | sort -V | tail -1)" = "$archive_tag" ]; then
  pin=$archive_tag
fi

echo "archive version: ${archive_version:-unknown}"
echo "running image:   $running_image"
[ -n "$pin" ] && echo "pins stackr to:  ghcr.io/fyrmforge/stackr:$pin"
echo "data directory:  $DATA_DIR"
echo
echo "This overwrites $DATA_DIR/stackr.db and $DATA_DIR/keys/master.key."
printf 'Type yes to continue: '
read -r reply
[ "$reply" = yes ] || { echo "aborted"; exit 1; }

# Pulls fail on a bad tag or no network, so they run before the service goes
# down.
if [ -n "$pin" ]; then
  echo "--- pulling $pin ---"
  docker pull "ghcr.io/fyrmforge/stackr:$pin" >/dev/null
  docker pull "ghcr.io/fyrmforge/stackr-proxyrelay:$pin" >/dev/null
fi

# Down before the files move: the panel holds the database open, and a swarm
# service restarts a replacement within seconds of a plain container stop.
# Scaling to zero is the only stop that holds.
echo "--- stopping stackr ---"
docker service scale stackr=0 >/dev/null

# Keep what is being replaced. The operator picked the archive; if it turns out
# to be the wrong one, this is the way back. The WAL and SHM go with it: the
# database on its own is not the prior state, the database plus its WAL is.
stamp=$(date -u +%Y%m%dT%H%M%SZ)
for f in stackr.db stackr.db-wal stackr.db-shm keys/master.key; do
  [ -f "$DATA_DIR/$f" ] && cp -a "$DATA_DIR/$f" "$DATA_DIR/$f.before-$stamp"
done

echo "--- unpacking ---"
tar xzf "$ARCHIVE" -C "$DATA_DIR" stackr.db keys/master.key
# The archive's database is a whole, checkpointed copy. A WAL left over from
# the database it replaced belongs to a different file, and sqlite will replay
# it on open: a corrupt schema and a panel that will not start. Verified on the
# rig, where exactly that happened.
rm -f "$DATA_DIR/stackr.db-wal" "$DATA_DIR/stackr.db-shm"
chmod 600 "$DATA_DIR/stackr.db" "$DATA_DIR/keys/master.key"

if [ -n "$pin" ]; then
  echo "--- pinning stackr to $pin ---"
  # Port forwarding creates relays by this local tag only.
  docker tag "ghcr.io/fyrmforge/stackr-proxyrelay:$pin" stkr-proxyrelay:local
  # STACKR_IMAGE picks the node agents' image; it moves with the panel.
  docker service update --detach \
    --image "ghcr.io/fyrmforge/stackr:$pin" \
    --env-add "STACKR_IMAGE=ghcr.io/fyrmforge/stackr:$pin" stackr >/dev/null
fi

echo "--- starting stackr ---"
docker service scale stackr=1 >/dev/null

echo
echo "Restored. The previous files are beside the new ones, suffixed .before-$stamp."
echo "Deployed services were not touched: stackr reconciles them as it starts."
if [ -n "$archive_version" ] && [ -z "$pin" ]; then
  case "$running_image" in
    *"$archive_version"*) ;;
    *) echo "Note: the archive was written by $archive_version, which is not the running image." ;;
  esac
fi

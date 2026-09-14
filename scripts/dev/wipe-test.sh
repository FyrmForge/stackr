#!/usr/bin/env bash
# Nuke the test VM back to stock: OS + Docker + Tailscale, nothing else.
# Every container, image, volume and network on the box goes, not just
# stackr's, plus the data dir. The panel is gone afterwards, run
# scripts/dev/deploy-test.sh to put it back.
#
#   STACKR_DEPLOY_HOST=manager.example.test ./scripts/dev/wipe-test.sh \
#       --nodes worker.example.test
#
# --nodes takes the workers too, comma separated. Without it only the manager
# is wiped and every worker is left orphaned, still holding the old swarm's
# volumes.
#
# Certificates and GitHub connectors are kept by DEFAULT, because losing
# either costs more than the wipe is worth: re-issuing every name burns Let's
# Encrypt's duplicate-certificate limit (5 per week per name set), and a lost
# connector means redoing the GitHub App flow by hand before any
# config-as-code test can run at all. --no-keep-certs and --no-keep-connectors
# opt out when a run is specifically about the first-boot path.
#
# A half wipe is worse than none. Deleting just stackr.db under a running
# Traefik is what caused the 2026-09-01 incident: Traefik kept serving a
# config from before the wipe and every route written afterwards was ignored
# (docs/plans/13-proxy-probe.md).
#
# This is a destructive development helper; it intentionally has no dry-run
# or rollback mode.
set -euo pipefail

HOST="${STACKR_DEPLOY_HOST:?set STACKR_DEPLOY_HOST to the test manager}"
DATA_DIR="${STACKR_DATA_DIR:-$HOME/stackr/data}"
# Kept by default: see the header. The flags only turn them off.
KEEP_CERTS=1
KEEP_CONNECTORS=1
# Worker nodes to wipe alongside the manager, comma separated. Empty by
# default, so the plain call still wipes only this box.
NODES=""
while [ $# -gt 0 ]; do
  case "$1" in
    # The keep flags are the default now; still accepted so an old command
    # line (or an old note) does not fail.
    --keep-certs) KEEP_CERTS=1 ;;
    --keep-connectors) KEEP_CONNECTORS=1 ;;
    --no-keep-certs) KEEP_CERTS="" ;;
    --no-keep-connectors) KEEP_CONNECTORS="" ;;
    --nodes) NODES="${2:-}"; shift ;;
    --nodes=*) NODES="${1#--nodes=}" ;;
    *) echo "unknown argument: $1" >&2; exit 1 ;;
  esac
  shift
done

echo "This removes EVERY docker object on $HOST and deletes $DATA_DIR."
[ -n "$NODES" ] && echo "It also removes every docker object on: ${NODES//,/ }"
echo "Keeping: certificates=${KEEP_CERTS:-no} connectors=${KEEP_CONNECTORS:-no}"
read -r -p "Type the hostname to confirm: " ans
[[ "$ans" == "$HOST" ]] || { echo "aborted"; exit 1; }

# Workers first. Once the manager has left the swarm it cannot tell a worker
# anything, and a worker left holding the old swarm's tasks and volumes is
# exactly the half wipe this script exists to avoid: it rejoins carrying
# volumes the new panel has no rows for, and a pinned tile silently comes up
# on stale data.
if [ -n "$NODES" ]; then
  for node in ${NODES//,/ }; do
    echo "--- wiping node $node ---"
    # No data dir on a worker: the panel's is the only one, and the agent's
    # key lives in a volume the prune below takes.
    ssh "$node" "
      set -uo pipefail
      docker swarm leave --force >/dev/null 2>&1 || true
      ids=\$(docker ps -q); [ -n \"\$ids\" ] && docker stop \$ids >/dev/null
      docker system prune -a --volumes -f | tail -1
      docker volume prune -a -f | tail -1
      docker network rm stkr >/dev/null 2>&1 || true
      docker ps -a --format '  {{.Names}}' | grep . || echo '  no containers'
      docker volume ls -q | wc -l | sed 's/^/  volumes: /'
    " || echo "  !! $node unreachable, wipe it by hand before rejoining it"
  done
fi

if [ -n "$KEEP_CONNECTORS" ]; then
  # The file lives beside the data dir, so it survives the wipe. Put it back
  # with scripts/dev/connector-keep.sh restore <org-slug> once the org exists.
  # Not fatal: a box with no connectors yet has nothing to save, and that must
  # not stop the wipe.
  "$(dirname "$0")/connector-keep.sh" save || echo "  (nothing saved; carrying on)"
fi

ssh "$HOST" "
  set -uo pipefail
  KEEP_CERTS='$KEEP_CERTS'

  if [ -n \"\$KEEP_CERTS\" ]; then
    # Re-issuing every name on each wipe burns Let's Encrypt's
    # duplicate-certificate limit (5 per week per name set).
    echo '--- saving certificates ---'
    docker run --rm -v \$(dirname $DATA_DIR):/p alpine:3 sh -c 'mkdir -p /p/certs-keep && cp -a /p/\$(basename $DATA_DIR)/traefik/acme*.json /p/certs-keep/ 2>/dev/null' || true
  fi

  echo '--- stopping containers ---'
  # The panel is a swarm service, so stopping its container just makes swarm
  # start another one. The service has to go first.
  docker service rm stackr >/dev/null 2>&1 || true
  ids=\$(docker ps -q); [ -n \"\$ids\" ] && docker stop \$ids >/dev/null

  echo '--- deleting data ---'
  # Root-owned (traefik writes acme.json as 600 root) and ssh has no TTY for
  # sudo, so delete it from inside a container with the parent mounted.
  docker run --rm -v \$(dirname $DATA_DIR):/p alpine:3 rm -rf /p/\$(basename $DATA_DIR)

  if [ -n \"\$KEEP_CERTS\" ]; then
    echo '--- restoring certificates ---'
    docker run --rm -v \$(dirname $DATA_DIR):/p alpine:3 sh -c 'mkdir -p /p/\$(basename $DATA_DIR)/traefik && cp -a /p/certs-keep/. /p/\$(basename $DATA_DIR)/traefik/ 2>/dev/null; rm -rf /p/certs-keep' || true
  fi

  # Overlay networks and services go with the swarm; a prune cannot touch them.
  echo '--- leaving the swarm ---'
  docker swarm leave --force >/dev/null 2>&1 || true

  # Last, so it also takes the alpine image the steps above pulled.
  echo '--- pruning docker ---'
  docker system prune -a --volumes -f | tail -1
  # Named volumes survive the prune above.
  docker volume prune -a -f | tail -1
  # The prune leaves a network that still had a container attached.
  docker network rm stkr stackr >/dev/null 2>&1 || true

  echo '--- left on the box ---'
  docker ps -a --format '  {{.Names}}' | grep . || echo '  no containers'
  docker images -q | wc -l | sed 's/^/  images: /'
"
echo "--- wiped, run scripts/dev/deploy-test.sh to rebuild ---"
[ -n "$KEEP_CONNECTORS" ] && echo "--- then scripts/dev/connector-keep.sh restore <org-slug> after the org exists ---"
exit 0

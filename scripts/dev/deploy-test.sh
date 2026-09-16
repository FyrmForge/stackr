#!/usr/bin/env bash
# Deploy stackr to the test VM: build locally, rsync artifacts, build the
# image there, swap the service. Configuration is supplied through environment
# variables; the product installer is `scripts/install.sh`.
set -euo pipefail

HOST="${STACKR_DEPLOY_HOST:?set STACKR_DEPLOY_HOST to the test manager}"
IMAGE_DIR="${STACKR_DEPLOY_DIR:-stackr-image}"
DATA_DIR="${STACKR_DATA_DIR:-$HOME/stackr/data}"
ENV_FILE="${STACKR_ENV_FILE:-$HOME/stackr/.env}"
# STACKR_DEPLOY_BIND is gone: the panel is a swarm service and a swarm
# published port has no host IP to bind to, in host mode or otherwise. The
# port is on every interface of the manager, so keep the VM off the internet.

cd "$(dirname "$0")/../.."

echo "--- building ---"
templ generate >/dev/null
(cd frontend && npm run --silent css:build) >/dev/null
hamr gen static >/dev/null
# Own output path: hamr dev's watcher rebuilds bin/stackrd (dynamically linked)
# on file changes and would race us.
# STACKR_VERSION=v0.0.9 fakes an old release, to test the panel upgrade.
CGO_ENABLED=0 go build -ldflags "-X main.version=${STACKR_VERSION:-$(git rev-parse --short HEAD)}" -o bin/stackrd-deploy ./cmd/stackrd
CGO_ENABLED=0 go build -ldflags "-s -w" -o bin/proxyrelay ./cmd/proxyrelay
./bin/stackrd-deploy --generate >/dev/null

echo "--- shipping to $HOST ---"
# -R keeps the cmd/*/Dockerfile paths; the bin/./ marker still flattens the
# binaries to the image-dir root where the runtime Dockerfiles expect them.
rsync -azR --delete --exclude=node_modules cmd/stackrd/Dockerfile.runtime cmd/proxyrelay/Dockerfile.runtime bin/./stackrd-deploy bin/./proxyrelay frontend generated "$HOST:$IMAGE_DIR/"

# The manager needs the same insecure-registries entry the join script writes
# on every worker. Its own registry answers on localhost:5000, which docker
# trusts without one, but nothing else in the swarm can use that name: service
# specs reference the registry by the manager's address, and the manager has
# to be able to pull those refs too. Its own ssh call with a quoted heredoc so
# nothing here needs escaping through two shells.
echo "--- manager registry trust ---"
ssh "$HOST" 'sh -s' <<'MGRREG'
set -e
MGR_IP=$(hostname -I | awk '{print $1}')
# The live daemon config, not the file. A daemon.json written and never
# loaded looks identical here and is worth nothing: dockerd only reads it at
# startup, so the check has to ask the running daemon.
if docker info 2>/dev/null | grep -q "$MGR_IP:5000"; then
  echo "  already trusts $MGR_IP:5000"
  exit 0
fi
# /etc/docker is root-owned and ssh has no TTY for sudo; the docker group
# gives us root inside a container, which is enough to write one file.
#
# The address goes in as an environment variable and the JSON is built by the
# container's own shell. Interpolating it into the sh -c string instead sends
# the quotes through three shells and they do not survive: the file comes out
# as {insecure-registries: [...]} with none at all, which is not JSON, so
# dockerd ignores it and the failure looks like the entry was never written.
docker run --rm -e IP="$MGR_IP" -v /etc/docker:/etc/docker alpine:3 \
  sh -c 'mkdir -p /etc/docker && printf "{\"insecure-registries\": [\"%s:5000\"]}\n" "$IP" > /etc/docker/daemon.json'
echo "  wrote $(cat /etc/docker/daemon.json), restarting docker"
# Loud on failure. sudo over ssh has no TTY, so on a box without passwordless
# sudo this cannot restart anything, and a swallowed failure here looks
# exactly like success: the file is on disk, the daemon has never read it, and
# the first symptom is a service that cannot pull from the registry.
if ! sudo -n systemctl restart docker 2>/dev/null; then
  cat >&2 <<MSG
  cannot restart docker: sudo needs a password over ssh.
  The entry is written but the daemon has not read it. Run this on the box:
    sudo systemctl restart docker
  Or give the registry a TLS domain in the panel (Settings > Registry), which
  is the path that needs no insecure-registries entry at all.
MSG
  exit 1
fi
i=0
while [ $i -lt 30 ]; do docker info >/dev/null 2>&1 && break; i=$((i + 1)); sleep 1; done
MGRREG

echo "--- building image + swapping container ---"
ssh "$HOST" "
  set -e
  cd ~/$IMAGE_DIR
  docker build -q -f cmd/stackrd/Dockerfile.runtime -t stkr:local . >/dev/null
  docker build -q -f cmd/proxyrelay/Dockerfile.runtime -t stkr-proxyrelay:local . >/dev/null
  # Every stackr install is a swarm (docs/plans/30-docker-swarm.md). The pool is
  # only settable at init; install.sh probes for a free one, a dev VM can assume.
  if [ \"\$(docker info -f '{{.Swarm.ControlAvailable}}')\" != true ]; then
    docker swarm init --advertise-addr \$(hostname -I | awk '{print \$1}') \
      --default-addr-pool 10.250.0.0/15 --default-addr-pool-mask-length 24 >/dev/null
  fi
  # The panel creates this network at boot, but it has to join it to start,
  # so on a wiped box somebody has to make it first. Existing is fine.
  # Overlay, not bridge: traefik and the registry are swarm services now, and
  # a service cannot join a bridge network at all.
  # Plain VXLAN, no encryption, matching what the panel creates afterwards.
  # Traffic between nodes is in the clear, so the rig's nodes are joined over
  # Tailscale (docs/plans/31-node-agent-open-questions.md, overlay encryption).
  docker network create -d overlay --attachable stkr >/dev/null 2>&1 || true
  # The panel is a swarm service now, like everything else stackr runs
  # (docs/plans/31-node-agent-open-questions.md, the panel as a service).
  docker rm -f stackr >/dev/null 2>&1 || true
  docker service rm stackr >/dev/null 2>&1 || true

  # A swarm bind mount will not create a missing source the way docker run
  # did: the task is rejected with "bind source path does not exist" and
  # retried forever. After a wipe the data dir is exactly that.
  mkdir -p $DATA_DIR

  # Unlike install.sh, the test VM keeps a published port. install.sh's
  # replacement for it is the admin CLI, which is a separate later job, and
  # the VMs have no domain for traefik to route the panel on anyway
  # (proxy.routablePanel refuses a bare IP), without this there would be no
  # way into the box at all.
  #
  # mode=host, not the routing mesh: swarm published ports have no host IP,
  # and host mode at least keeps the port on this node's own interface.
  ENVARGS=\$(awk -F= '/^[A-Za-z_][A-Za-z0-9_]*=/ {printf \"--env %s \", \$0}' $ENV_FILE)
  docker service create \
    --name stackr \
    --network stkr \
    --hostname stkr-panel \
    --constraint 'node.role == manager' \
    --replicas 1 \
    --publish mode=host,published=8080,target=8080 \
    --mount type=bind,src=/var/run/docker.sock,dst=/var/run/docker.sock \
    --mount type=bind,src=/proc,dst=/host/proc,ro \
    --mount type=bind,src=$DATA_DIR,dst=$DATA_DIR \
    --env HOST_PROC=/host/proc \
    --env STACKR_IMAGE=stkr:local \
    \$ENVARGS \
    --stop-grace-period 120s \
    stkr:local >/dev/null
  sleep 4
  docker service logs stackr 2>&1 | tail -3
"
echo "--- deployed ---"

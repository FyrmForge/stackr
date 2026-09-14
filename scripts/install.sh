#!/usr/bin/env bash
# Install stackr on a fresh Docker host.
#
# Asks the four things nothing can infer and creates the panel as a swarm
# service. Re-running upgrades in place: the same `docker service update` with
# a newer image is the whole operation.
#
# With a hostname the panel publishes no port at all
# (docs/plans/31-node-agent-open-questions.md, the panel as a service). Swarm
# published ports have no host IP, so the old loopback-only 127.0.0.1:8080
# cannot exist on a service; users reach the panel through traefik. When
# traefik or DNS is broken the way in is ssh to this box plus the `stackr`
# CLI, which install.sh drops a wrapper for below.
#
# With localhost or a bare IP traefik has nothing to route (proxy.routablePanel
# refuses both), so the panel publishes 8080 in host mode instead, the same
# as scripts/dev/deploy-test.sh.
#
# Not tested end to end, the images below are not published yet. See
# docs/plans/16-install-scripts.md.
set -euo pipefail

IMAGE="${STACKR_IMAGE:-ghcr.io/fyrmforge/stackr:latest}"
RELAY_IMAGE="${STACKR_RELAY_IMAGE:-ghcr.io/fyrmforge/stackr-proxyrelay:latest}"
# runtime.ProxyRelayImage is a hardcoded local tag, so the pulled image has to
# be retagged to match. Wart, tracked in the plan.
RELAY_LOCAL="stkr-proxyrelay:local"

die() { echo "error: $*" >&2; exit 1; }

# ask <var> <prompt> <default>: re-asks until valid_<var> accepts the answer.
ask() {
  local var=$1 prompt=$2 def=${3:-} ans
  while true; do
    if [ -n "$def" ]; then read -r -p "$prompt [$def]: " ans; ans=${ans:-$def}
    else read -r -p "$prompt: " ans
    fi
    if "valid_$var" "$ans"; then printf -v "$var" '%s' "$REPLY_CLEAN"; return; fi
  done
}

# Each validator sets REPLY_CLEAN to the normalised value it accepted.
valid_host() {
  # A pasted browser URL is the most common paste, so take it rather than
  # rejecting it: scheme, any path, trailing dot, and case all go.
  local h=${1#http://}; h=${h#https://}; h=${h%%/*}; h=${h%.}
  h=$(printf '%s' "$h" | tr '[:upper:]' '[:lower:]')
  [ -n "$h" ] || { echo "  a hostname is required"; return 1; }
  case "$h" in
    *[!a-z0-9.-]*) echo "  '$h' has characters a hostname cannot contain"; return 1 ;;
    -*|*-|.*|*.) echo "  '$h' starts or ends with a dot or dash"; return 1 ;;
    *..*) echo "  '$h' has an empty label"; return 1 ;;
  esac
  # localhost and a bare IP are the no-TLS cases; anything else needs a dot,
  # or Let's Encrypt has nothing it can issue for.
  if [ "$h" != localhost ] && ! is_ip "$h" && [ "$h" = "${h%.*}" ]; then
    echo "  '$h' is a single label; use a full name like panel.example.com"; return 1
  fi
  REPLY_CLEAN=$h
}

valid_email() {
  case "$1" in
    *[[:space:]]*|"") echo "  an email address is required"; return 1 ;;
    *@*.*) REPLY_CLEAN=$1 ;;
    *) echo "  '$1' is not an email address"; return 1 ;;
  esac
}

valid_data_dir() {
  case "$1" in
    /*) REPLY_CLEAN=${1%/} ;;
    *) echo "  needs an absolute path"; return 1 ;;
  esac
}

valid_http_port() { valid_port "$1"; }
valid_https_port() { valid_port "$1"; }
valid_port() {
  case "$1" in
    ''|*[!0-9]*) echo "  '$1' is not a number"; return 1 ;;
  esac
  [ "$1" -ge 1 ] && [ "$1" -le 65535 ] || { echo "  '$1' is outside 1-65535"; return 1; }
  # Traefik will fail to start on a taken port, an hour after the installer
  # said everything was fine. Catch it here.
  if command -v ss >/dev/null && ss -ltn "sport = :$1" 2>/dev/null | grep -q LISTEN; then
    echo "  something is already listening on $1"; return 1
  fi
  REPLY_CLEAN=$1
}

is_ip() { printf '%s' "$1" | grep -Eq '^[0-9]+(\.[0-9]+){3}$'; }

yesno() { # yesno <prompt> <default y|n>
  local ans def=$2
  read -r -p "$1 [$([ "$def" = y ] && echo 'Y/n' || echo 'y/N')] " ans
  ans=${ans:-$def}
  case "$ans" in [Yy]*) return 0 ;; *) return 1 ;; esac
}

# --- swarm ----------------------------------------------------------------

# Every stackr install is a Swarm, one node or many (docs/plans/30-docker-swarm.md).
# The overlay address pool is only settable at init and cannot be changed after,
# so it is picked here and picked carefully. The default 10.0.0.0/8 collides with
# too many LANs, and anything inside 172.16.0.0/12 collides with docker's own
# bridge pool (the fourth bridge network on a box is 172.20.0.0/16).
POOL_CANDIDATES="10.250.0.0/15 10.252.0.0/15 10.254.0.0/15"

# cidr_overlaps <a/m> <b/n>: true when either range contains the other.
# Both directions matter: an existing 10.0.0.0/8 route is a supernet of every
# candidate, and testing only the candidate's own prefix length would read that
# as free and ship a colliding pool. Arithmetic, not and(): mawk has no bitops.
cidr_overlaps() {
  awk -v a="$1" -v b="$2" '
    function ip2int(s,  p) { split(s, p, "."); return ((p[1]*256+p[2])*256+p[3])*256+p[4] }
    function top(v, m) { return int(v / 2^(32-m)) }
    BEGIN {
      split(a, x, "/"); split(b, y, "/")
      m = (x[2] < y[2]) ? x[2] : y[2]
      exit (top(ip2int(x[1]), m) == top(ip2int(y[1]), m)) ? 0 : 1
    }'
}

# Every subnet this box already routes to or has handed to a docker network.
used_subnets() {
  {
    ip -4 route show 2>/dev/null | awk '{print $1}'
    docker network ls -q 2>/dev/null | xargs -r docker network inspect \
      -f '{{range .IPAM.Config}}{{println .Subnet}}{{end}}' 2>/dev/null
  } | grep -E '^[0-9]+(\.[0-9]+){3}/[0-9]+$' | sort -u
}

# Sets POOL to the first candidate that overlaps nothing, or fails.
pick_pool() {
  local used c u clash
  used=$(used_subnets)
  for c in $POOL_CANDIDATES; do
    clash=""
    for u in $used; do
      if cidr_overlaps "$c" "$u"; then clash=$u; break; fi
    done
    [ -z "$clash" ] && { POOL=$c; return 0; }
    echo "  $c overlaps $clash"
  done
  return 1
}

# The overlay data plane is plain VXLAN, so a private address is the one to
# advertise. Docker's own interfaces are skipped or docker0 wins the pick.
advertise_addr() {
  local ip
  ip=$(ip -4 -o addr show scope global 2>/dev/null \
    | awk '$2 !~ /^(docker|br-|veth|stkr)/ {print $4}' | cut -d/ -f1 \
    | grep -E '^(10\.|192\.168\.|172\.(1[6-9]|2[0-9]|3[01])\.)' | head -1)
  [ -n "$ip" ] || ip=$(ip -4 route get 1.1.1.1 2>/dev/null \
    | awk '{for (i = 1; i <= NF; i++) if ($i == "src") print $(i + 1)}' | head -1)
  printf '%s' "$ip"
}

swarm_init() {
  if [ "$(docker info -f '{{.Swarm.ControlAvailable}}' 2>/dev/null)" = true ]; then
    echo "  already a swarm manager; the address pool is fixed at init and stays as it is"
    return
  fi
  pick_pool || die "no free overlay address pool; every candidate ($POOL_CANDIDATES) overlaps a subnet this box already uses"
  local adv; adv=$(advertise_addr)
  [ -n "$adv" ] || die "no address to advertise; set one up or run: docker swarm init --advertise-addr <ip>"
  echo "  swarm init on $adv, overlay pool $POOL"
  docker swarm init --advertise-addr "$adv" \
    --default-addr-pool "$POOL" --default-addr-pool-mask-length 24 >/dev/null
}

# trust_own_registry writes the same insecure-registries entry the join script
# writes on every worker, for this box.
#
# The manager is a node like any other: the agent's service spec names the
# registry by this address, so the manager has to be able to pull that ref too
# or its own agent task is rejected. A registry with a TLS domain goes through
# traefik and needs none of this.
trust_own_registry() {
  local addr="$1:${REGISTRY_PORT:-5000}"
  grep -q "$addr" /etc/docker/daemon.json 2>/dev/null && return
  echo "  trusting the panel's own registry at $addr"
  mkdir -p /etc/docker
  # Merge rather than overwrite: a daemon.json already here holds settings
  # that are not ours to drop. Without python3 there is no way to merge, so
  # the fallback only ever writes a file that is not there. Clobbering an
  # existing daemon.json would silently drop the box's log driver, its DNS,
  # its data-root, whatever an operator had put in it.
  if ! command -v python3 >/dev/null 2>&1 && [ -s /etc/docker/daemon.json ]; then
    die "/etc/docker/daemon.json exists and python3 is not installed, so it cannot be merged. Add \"$addr\" to its insecure-registries by hand and run this again"
  fi
  python3 - "$addr" <<'REGPY' 2>/dev/null || printf '{\n  "insecure-registries": ["%s"]\n}\n' "$addr" > /etc/docker/daemon.json
import json, os, sys
p = "/etc/docker/daemon.json"
cfg = {}
if os.path.exists(p):
    try:
        cfg = json.load(open(p))
    except Exception:
        cfg = {}
regs = cfg.get("insecure-registries", [])
if sys.argv[1] not in regs:
    regs.append(sys.argv[1])
cfg["insecure-registries"] = regs
json.dump(cfg, open(p, "w"), indent=2)
REGPY
  systemctl restart docker 2>/dev/null || service docker restart 2>/dev/null || \
    die "could not restart docker to pick up /etc/docker/daemon.json; restart it and run this again"
  # dockerd takes a moment to come back and everything below needs it.
  for _ in $(seq 30); do docker info >/dev/null 2>&1 && return; sleep 1; done
  die "docker did not come back after the restart"
}

# --- checks ---------------------------------------------------------------

[ "$(id -u)" -eq 0 ] || die "run as root (docker, /var/lib and port 80 all need it)"
command -v docker >/dev/null || die "docker is not installed; https://docs.docker.com/engine/install/"
docker info >/dev/null 2>&1 || die "cannot talk to the docker daemon; is it running?"

swarm_init
# Before anything is created: this restarts dockerd.
trust_own_registry "$(advertise_addr)"

# --- prompts --------------------------------------------------------------

echo "stackr install"
echo

ask host "Panel hostname"
ask data_dir "Data directory" /var/lib/stackr

tls=on
acme_email=""
publish=""
if [ "$host" = localhost ] || is_ip "$host"; then
  # proxy.routablePanel refuses to write a route for these, so there is no
  # name to certify and nothing to ask, and no traefik route in either: the
  # panel has to publish its own port.
  echo "  no domain, so no HTTPS; the panel will serve plain HTTP on port 8080"
  tls=off
  publish="--publish mode=host,published=8080,target=8080"
elif ! yesno "Is $host reachable from the public internet?" y; then
  # A tailnet or LAN name: dotted, so it looks public, but the ACME http
  # challenge can never reach it.
  echo "  private name, so no HTTPS; certificates cannot be issued for it"
  tls=off
fi

if [ "$tls" = on ]; then
  ask acme_email "Email for Let's Encrypt"
fi

ask http_port "HTTP port" 80
ask https_port "HTTPS port" 443


# --- write ----------------------------------------------------------------

install -d -m 755 "$data_dir"

base_url="http://$host"
[ "$tls" = on ] && base_url="https://$host"
[ "$tls" = off ] && [ "$http_port" != 80 ] && base_url="$base_url:$http_port"
[ -n "$publish" ] && base_url="http://$host:8080"


# Overlay: every stackr piece (panel, agent, traefik, registry) is a swarm
# service, and a service cannot join a bridge network at all. Attachable
# because the forward relays are still plain containers.
#
# An install from before this ran has stkr as a bridge, and a network's driver
# cannot be changed. Refusing here is deliberate: carrying on would give a
# panel that starts, looks healthy, and has no proxy, because the traefik
# service fails to create with "network stkr not manageable" and nothing in
# the panel's own log says the site is down.
driver=$(docker network inspect -f '{{.Driver}}' stkr 2>/dev/null || true)
if [ -n "$driver" ] && [ "$driver" != overlay ]; then
  cat >&2 <<STKRNET
stkr already exists as a "$driver" network and swarm services cannot join it.
Docker cannot change a network's driver, so it has to be recreated:

  docker service rm stackr
  docker network rm stkr
  $0

Everything on it is recreated by the panel at boot; no data is on the network.
STKRNET
  die "stkr network is $driver, not overlay"
fi
# Plain VXLAN, like every network the panel creates afterwards. Traffic
# between nodes crosses the wire in the clear, so join nodes over a VPN or a
# private network (docs/plans/31-node-agent-open-questions.md, overlay
# encryption).
docker network create -d overlay --attachable stkr >/dev/null 2>&1 || true

echo
echo "--- pulling images ---"
docker pull "$IMAGE"
docker pull "$RELAY_IMAGE"
# Port forwarding looks the image up by this exact local tag.
docker tag "$RELAY_IMAGE" "$RELAY_LOCAL"

echo "--- starting ---"
# The panel is a swarm service like everything else stackr runs, so the whole
# install is one `docker service ls`. Pinned to the manager: it holds the
# docker socket and the data dir, both of which are this machine's.
#
# $publish is empty unless the host is localhost or an IP. Reaching the panel
# is otherwise traefik's job; see the header.
docker service rm stackr >/dev/null 2>&1 || true
# shellcheck disable=SC2086
docker service create \
  --name stackr \
  --network stkr \
  --hostname stkr-panel \
  --constraint 'node.role == manager' \
  --replicas 1 \
  $publish \
  --mount type=bind,src=/var/run/docker.sock,dst=/var/run/docker.sock \
  --mount type=bind,src=/proc,dst=/host/proc,ro \
  --mount "type=bind,src=$data_dir,dst=$data_dir" \
  --env "BASE_URL=$base_url" \
  --env "ACME_EMAIL=$acme_email" \
  --env "STACKR_TLS=$tls" \
  --env "DATA_DIR=$data_dir" \
  --env "DATABASE_PATH=$data_dir/stackr.db" \
  --env "TRAEFIK_HTTP_PORT=$http_port" \
  --env "TRAEFIK_HTTPS_PORT=$https_port" \
  --env "HOST_PROC=/host/proc" \
  --env "STACKR_IMAGE=$IMAGE" \
  --stop-grace-period 120s \
  "$IMAGE" >/dev/null

# The panel has no published port, so the recovery path when traefik or DNS is
# broken is the admin CLI inside the container, where the panel is always
# localhost:8080. One wrapper on the host so the operator just types `stackr`.
cat > /usr/local/bin/stackr <<'WRAP'
#!/bin/sh
# stackr admin CLI: runs inside the panel container on this manager.
exec docker exec -i "$(docker ps -q -f label=com.docker.swarm.service.name=stackr | head -1)" /app/stackr "$@"
WRAP
chmod 755 /usr/local/bin/stackr

# --- done -----------------------------------------------------------------

cat <<DONE

stackr is running.

  panel     $base_url
  data      $data_dir
  admin     stackr <command>          (works even when the panel is unreachable)
  upgrade   docker service update --image $IMAGE stackr
DONE

if [ "$tls" = on ]; then
  cat <<DNS

DNS: two records, both pointing at this server:

  $host          A    <this server's public IP>
  *.$host        A    <this server's public IP>

The second is not optional. Stacks get hostnames underneath your panel's name,
and each one needs to resolve before a certificate can be issued for it.

Until DNS is live there is no browser route in: the panel publishes no port,
so everything arrives through Traefik. Use the admin CLI on this box instead:

  stackr --help
DNS
fi

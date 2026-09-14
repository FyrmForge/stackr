package server

import (
	"fmt"
)

// The join script. Curled by a machine that is not in the swarm yet, with a
// one-time key bound to its address standing in for a login
// (docs/plans/32-multi-node-ui.md, Add node modal).
//
// Three jobs, in this order:
//
//  1. install docker if it is missing
//  2. write the panel's registry into daemon.json and restart dockerd,
//     without this a worker cannot pull anything stackr built, which is
//     finding 1 from the worker-2 test and looks like a task that never starts
//  3. join the swarm as a worker
//
// It probes 2377 first and stops with the port list if it is closed. 7946,
// 4789 and ESP cannot be probed before joining; a node that joins and then
// reads down on the servers list is one of those being blocked.

type joinScriptData struct {
	ServerName   string
	ManagerAddr  string
	Token        string
	RegistryAddr string
	// RegistryTLS is whether RegistryAddr is the registry's TLS domain, which
	// traefik serves and a node needs no configuration for. It is asked of the
	// registry, never guessed from the address: the guess used to be "does it
	// contain a dot", and every address on the no-TLS path is an IP, which is
	// nothing but dots. The worker was told there was nothing to configure,
	// never wrote the insecure-registries entry, and its agent task was
	// rejected forever with "failed to resolve reference".
	RegistryTLS bool
	// PanelURL and Key are the callback the node makes once it has joined,
	// reporting the swarm node id it was given. Without it the row is matched
	// on the address the operator typed against the one the daemon advertises,
	// which routinely differ, and the row reads "pending" for ever.
	PanelURL string
	Key      string
}

func joinScript(d joinScriptData) string {
	registry := ""
	if d.RegistryAddr != "" && !d.RegistryTLS {
		// A bare host:port has no name a certificate can be issued for, so
		// the node has to be told to accept it without TLS. A registry on a
		// real domain goes through traefik and needs nothing here.
		registry = fmt.Sprintf(`
say "pointing docker at the stackr registry"
mkdir -p /etc/docker
# Merge rather than overwrite: a daemon.json already on this box holds
# settings that are not ours to drop.
python3 - <<'PY' 2>/dev/null || fallback_daemon_json
import json, os
p = "/etc/docker/daemon.json"
cfg = {}
if os.path.exists(p):
    try:
        cfg = json.load(open(p))
    except Exception:
        cfg = {}
regs = cfg.get("insecure-registries", [])
if %[1]q not in regs:
    regs.append(%[1]q)
cfg["insecure-registries"] = regs
json.dump(cfg, open(p, "w"), indent=2)
PY
restart_docker
`, d.RegistryAddr)
	} else if d.RegistryAddr != "" {
		registry = fmt.Sprintf("\nsay \"registry is %s, reached over https through the panel, nothing to configure\"\n", d.RegistryAddr)
	}

	return fmt.Sprintf(`#!/bin/sh
# stackr: join %[1]s to the swarm.
# Generated for one node and one use; the key in the URL is already spent.
set -e

say() { printf '  %%s\n' "$*"; }
die() { printf 'error: %%s\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "run this as root (docker and /etc/docker both need it)"

MANAGER=%[2]q

# 2377 is the only port that can be checked before joining. The rest only
# show up as a node that joins and then goes down.
say "checking $MANAGER:2377"
if command -v nc >/dev/null 2>&1; then
  nc -z -w 5 "$MANAGER" 2377 || die "cannot reach $MANAGER:2377; open 2377/tcp, 7946/tcp+udp, 4789/udp and ESP (IP protocol 50) between these machines"
else
  # No nc: /dev/tcp is a bash builtin, so this is skipped on a plain sh.
  say "nc is not installed, skipping the port check"
fi

restart_docker() {
  if command -v systemctl >/dev/null 2>&1; then
    systemctl restart docker
  else
    service docker restart
  fi
  # dockerd takes a moment to accept connections again.
  i=0
  while [ $i -lt 30 ]; do
    docker info >/dev/null 2>&1 && return 0
    i=$((i + 1))
    sleep 1
  done
  die "docker did not come back after the restart"
}

fallback_daemon_json() {
  # No python3. Only safe when there is no existing file to merge into.
  [ -s /etc/docker/daemon.json ] && die "python3 is missing and /etc/docker/daemon.json already exists; add %%s to insecure-registries by hand, restart docker, and run this again"
  printf '{\n  "insecure-registries": ["%%s"]\n}\n' "$REGISTRY" > /etc/docker/daemon.json
}
REGISTRY=%[4]q

if ! command -v docker >/dev/null 2>&1; then
  say "installing docker"
  curl -fsSL https://get.docker.com | sh
fi
docker info >/dev/null 2>&1 || die "cannot talk to the docker daemon; is it running?"
%[5]s
say "joining the swarm"
docker swarm join --token %[3]q "$MANAGER:2377"

# Report the id swarm just gave this node, so the panel keys its row on that
# rather than guessing from an address. Non-fatal: the address match is still
# the fallback, and a node that joined has joined.
NODE_ID=$(docker info -f '{{.Swarm.NodeID}}' 2>/dev/null || true)
if [ -n "$NODE_ID" ]; then
  curl -fsS -X POST %[6]q --data-urlencode "node_id=$NODE_ID" >/dev/null 2>&1 ||
    say "could not tell the panel this node's id; it will match on address instead"
fi

cat <<'DONE'

Joined. The node appears on the servers page within a few seconds, and the
stackr agent starts on it by itself.

If it shows "down" there instead, one of 7946/tcp, 7946/udp, 4789/udp or ESP
(IP protocol 50) is blocked between this machine and the manager.
DONE
`, d.ServerName, d.ManagerAddr, d.Token, d.RegistryAddr, registry,
		d.PanelURL+"/join/"+d.Key+"/node")
}

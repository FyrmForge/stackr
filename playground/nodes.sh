#!/usr/bin/env bash
# shellcheck disable=SC2016
# Playground nodes: ctrl, w1, ext as Incus containers. Runs ON the playground VM as root.
#
#   nodes.sh up [--version vX.Y.Z | --local DIR]   create, install stackr on ctrl, join w1 and ext
#   nodes.sh upgrade [--local DIR]                 load the bundle on all nodes, re-run stackr-install on ctrl
#   nodes.sh reset [--fakes] [w1|ext|ctrl]         nodes only; w1 or ext alone; ctrl = everything. --fakes also
#                                                  wipes Namecheap, GitHub, S3 and share contents
#   nodes.sh trust                                 put Pebble's current issuing root into every container
#   nodes.sh disk add NODE NAME [2G] | rm NODE NAME   an ext4 loop disk, at /mnt/NAME in the node
#   nodes.sh stk ARGS | x NODE CMD... | logs NODE [CONTAINER] [LINES]
#   nodes.sh hosts                                 /etc/hosts lines for the laptop
#   nodes.sh pr REPO N ACTION HEAD [BASE]          send a pull_request event from the fake GitHub
#   nodes.sh status                                one ok|FAIL line per check; exit 1 on any FAIL
#
# --local DIR holds the output of `make release RELEASE=vX.Y.Z-dev.N`:
# images.tar (docker save of the stackr and stackr-observe images) and
# stackr-install-linux-amd64. Without flags `up` installs the latest release.
# Needs PLAYGROUND_ADMIN_PASSWORD in the environment for the first install.
# Without flags `up`, `upgrade` and `reset` use /root/out when it holds a bundle.
# Safe to rerun: what exists is skipped.
set -euo pipefail

DOMAIN=stackr.test
PANEL=panel.$DOMAIN
ADMIN_EMAIL=admin@test.com
PEBBLE=/var/lib/playground/pebble
# name:bridge:address:gateway
NODES=(ctrl:lan0:10.77.0.10:10.77.0.1 w1:lan0:10.77.0.11:10.77.0.1 ext:wan0:203.0.113.20:203.0.113.1)
REPO=https://github.com/FyrmForge/stackr/releases
DISKS=/var/lib/playground/disks
KEYFILE=/var/lib/playground/recovery.key
VERSION="" LOCAL=""

x() { local n=$1; shift; incus exec "$n" -- "$@"; }
say() { echo "== $*"; }
die() { echo "$*" >&2; exit 1; }

exists() { incus info "$1" >/dev/null 2>&1; }

launch() {
	local n=$1 br=$2 ip=$3 gw=$4
	exists "$n" && return
	say "create $n"
	incus init -q images:debian/13 "$n" -p stackr
	incus config device add "$n" eth0 nic nictype=bridged parent="$br" name=eth0
	incus start "$n"
	until x "$n" true 2>/dev/null; do sleep 1; done
	x "$n" sh -c "cat >/etc/systemd/network/10-eth0.network" <<NET
[Match]
Name=eth0
[Network]
Address=$ip/24
Gateway=$gw
DNS=$gw
NET
	x "$n" sh -c "systemctl enable --now systemd-networkd; systemctl restart systemd-networkd; rm -f /etc/resolv.conf; echo nameserver $gw >/etc/resolv.conf"
	for _ in $(seq 60); do x "$n" getent hosts deb.debian.org >/dev/null 2>&1 && break; sleep 1; done
	x "$n" getent hosts deb.debian.org >/dev/null 2>&1 || { echo "$n: no network after 60s" >&2; exit 1; }
	# trust() needs the cert store to exist before docker is installed
	x "$n" bash -euc 'export DEBIAN_FRONTEND=noninteractive
		apt-get update -qq && apt-get install -y -qq ca-certificates curl >/dev/null
		mkdir -p /usr/local/share/ca-certificates'
}

# Pebble's issuing root, live; it changes on every Pebble restart.
pebble_root() { curl -fsS --cacert $PEBBLE/api-root.pem "https://acme.$DOMAIN:15000/roots/0" -o "$1"; }

# stkr runs inside the panel container, whose own store lacks Pebble's root: give Go a file.
stk() {
	x ctrl sh -c 'exec docker exec -i -e SSL_CERT_FILE=/var/lib/stackr/pebble-issuing-root.pem -e STACKR_CONFIG=/var/lib/stackr/cli.json "$(docker ps -q --filter label=stackr.role=panel | head -n1)" /stackr "$@"' stk "$@"
}

# One file per root, so older roots keep working. A node that holds the current
# root is left alone; the others restart Docker to read the store.
trust() {
	local tmp sha; tmp=$(mktemp)
	pebble_root "$tmp"
	sha=$(sha256sum "$tmp" | cut -c1-12)
	for e in "${NODES[@]}"; do
		local n=${e%%:*}
		exists "$n" || continue
		# the containers may still be booting when this runs at VM start
		for _ in $(seq 60); do x "$n" true 2>/dev/null && break; sleep 1; done
		x "$n" true 2>/dev/null || { echo "$n is not running, skipped" >&2; continue; }
		x "$n" test -f "/usr/local/share/ca-certificates/pebble-root-$sha.crt" && continue
		incus file push -q "$tmp" "$n/usr/local/share/ca-certificates/pebble-root-$sha.crt"
		x "$n" update-ca-certificates >/dev/null
		x "$n" sh -c 'command -v docker >/dev/null && systemctl restart docker' || true
		say "$n trusts the Pebble root"
	done
	if exists ctrl && x ctrl true 2>/dev/null; then
		incus file push -q "$tmp" ctrl/var/lib/stackr/pebble-issuing-root.pem --create-dirs
	fi
	rm -f "$tmp"
}

# The panel answers over https with a Pebble certificate.
wait_panel() {
	local root; root=$(mktemp)
	pebble_root "$root"
	say "wait for the panel"
	for _ in $(seq 90); do
		if curl -fs --cacert "$root" -o /dev/null "https://$PANEL/api/health"; then rm -f "$root"; return; fi
		sleep 2
	done
	rm -f "$root"; echo "panel not healthy at https://$PANEL after 180s" >&2; exit 1
}

docker_in() {
	local n=$1
	x "$n" sh -c 'command -v docker' >/dev/null 2>&1 && return
	say "docker on $n"
	x "$n" bash -euc '
		export DEBIAN_FRONTEND=noninteractive
		apt-get update -qq && apt-get install -y -qq ca-certificates curl iptables >/dev/null
		install -m 0755 -d /etc/apt/keyrings
		curl -fsSL https://download.docker.com/linux/debian/gpg -o /etc/apt/keyrings/docker.asc
		. /etc/os-release
		echo "deb [arch=amd64 signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/debian $VERSION_CODENAME stable" >/etc/apt/sources.list.d/docker.list
		apt-get update -qq && apt-get install -y -qq docker-ce docker-ce-cli containerd.io docker-buildx-plugin >/dev/null'
}

# installer puts /usr/local/bin/stackr-install on a container: from DIR, or the release.
installer_in() {
	local n=$1
	if [[ -n $LOCAL ]]; then
		incus file push -q "$LOCAL/stackr-install-linux-amd64" "$n/usr/local/bin/stackr-install" --mode 0755
		x "$n" sh -c 'docker load' <"$LOCAL/images.tar" >/dev/null
		return
	fi
	local base=$REPO/latest/download
	[[ -z $VERSION ]] || base=$REPO/download/$VERSION
	x "$n" bash -euc "
		t=\$(mktemp -d); cd \$t
		curl -fsSL -O $base/stackr-install-linux-amd64 -O $base/SHA256SUMS
		grep ' stackr-install-linux-amd64\$' SHA256SUMS | sha256sum -c - >/dev/null
		install -m 0755 stackr-install-linux-amd64 /usr/local/bin/stackr-install"
}

install_ctrl() {
	# install.json is written before the image pull; a running panel is the proof
	[[ -n $(x ctrl docker ps -q --filter label=stackr.role=panel --filter status=running 2>/dev/null) ]] && return
	[[ -n ${PLAYGROUND_ADMIN_PASSWORD:-} ]] || { echo "set PLAYGROUND_ADMIN_PASSWORD" >&2; exit 1; }
	say "install stackr on ctrl"
	installer_in ctrl
	local ver=()
	[[ -z $VERSION ]] || ver=(--version "${VERSION}")
	# the password goes in on stdin, never on a command line. The recovery key line is
	# filtered into a mode 600 file and never printed.
	(
		umask 077
		printf '%s\n' "$PLAYGROUND_ADMIN_PASSWORD" | x ctrl bash -c '
			read -r STACKR_ADMIN_PASSWORD; export STACKR_ADMIN_PASSWORD
			exec stackr-install --yes --domain "$0" --panel-host "$1" --email "$2" --admin-email "$2" "${@:3}"' \
			$DOMAIN $PANEL $ADMIN_EMAIL "${ver[@]}" 2>&1 | awk -v f=$KEYFILE '/AGE-SECRET-KEY-/ { print > f; next } 1'
	)
	[[ ! -s $KEYFILE ]] || say "recovery key saved in $KEYFILE"
}

# Same answers as the install, no password: the installer's upgrade path moves the panel.
upgrade() {
	src "$@"
	[[ -n $LOCAL ]] || die "upgrade needs a bundle: --local DIR (or /root/out)"
	[[ -f $LOCAL/images.tar ]] || die "$LOCAL/images.tar missing"
	for e in "${NODES[@]}"; do
		local n=${e%%:*}
		exists "$n" || continue
		say "load the bundle on $n"
		installer_in "$n"
	done
	say "upgrade stackr on ctrl"
	x ctrl stackr-install --yes --domain $DOMAIN --panel-host $PANEL --email $ADMIN_EMAIL
	wait_panel
}

# dev.env and the Pebble API root, put in place before the install so the
# panel and proxy start with them (the installer accepts an existing data dir).
dev_env() {
	incus file push -q $PEBBLE/api-root.pem ctrl/var/lib/stackr/proxy/pebble-api-root.pem --create-dirs
	x ctrl sh -c 'cat >/var/lib/stackr/dev.env' <<ENV
ACME_CA=https://acme.$DOMAIN:14000/dir
ACME_CA_ROOT=pebble-api-root.pem
NAMECHEAP_API=http://203.0.113.1:8080/xml.response
GITHUB_URL=https://github.$DOMAIN:8443
GITHUB_CA=/var/lib/stackr/proxy/pebble-api-root.pem
ENV
}

# capability tags a node asks for when it joins; they apply to new joins only
tags_of() {
	case $1 in
	w1) echo "--tag builder" ;;
	ext) echo "--tag ingress" ;;
	esac
}

join() {
	local n=$1 cmd
	x "$n" test -f /var/lib/stackr-worker/node.name && return
	say "join $n"
	docker_in "$n"
	installer_in "$n"
	incus file push -q $PEBBLE/api-root.pem "$n/var/lib/stackr-worker/proxy/pebble-api-root.pem" --create-dirs
	# the panel makes the whole `stackr-install join ...` line; the on-box CLI is stkr
	# shellcheck disable=SC2046
	cmd=$(stk node add "$n" $(tags_of "$n") --json | jq -r .command)
	x "$n" sh -c "$cmd --yes" &
	local pid=$!
	# the node shows as pending once its request is in; confirm it
	for _ in $(seq 120); do stk node ls --pending 2>/dev/null | grep -qw "$n" && break; sleep 2; done
	stk node confirm "$n" --yes
	wait "$pid"
}

# src parses [--version vX.Y.Z | --local DIR]; with neither, a bundle in /root/out is used.
src() {
	while [[ $# -gt 0 ]]; do
		case $1 in
		--version) VERSION=$2; shift 2 ;;
		--local) LOCAL=$(realpath "$2"); shift 2 ;;
		*) die "usage: [--version vX.Y.Z | --local DIR]" ;;
		esac
	done
	[[ -n $VERSION || -n $LOCAL || ! -f /root/out/images.tar ]] || LOCAL=/root/out
}

up() {
	src "$@"
	for f in stackr-install-linux-amd64 images.tar; do
		[[ -z $LOCAL || -f $LOCAL/$f ]] || { echo "$LOCAL/$f missing" >&2; exit 1; }
	done
	for e in "${NODES[@]}"; do IFS=: read -r n br ip gw <<<"$e"; launch "$n" "$br" "$ip" "$gw"; done
	trust
	for e in "${NODES[@]}"; do docker_in "${e%%:*}"; done
	dev_env
	install_ctrl
	wait_panel
	# DNS connectors write tile records at the node's public IP: the VM's wan0 address
	stk node set ctrl --public-ip 203.0.113.1 >/dev/null
	join w1
	join ext
	say "done; run: $0 hosts"
}

hosts() {
	local ip; ip=$(ip -4 route get 1.1.1.1 | sed -n 's/.* src \([0-9.]*\).*/\1/p')
	for h in $PANEL acme.$DOMAIN github.$DOMAIN app.$DOMAIN web.$DOMAIN api.$DOMAIN; do echo "$ip $h"; done
}

# pr REPO N ACTION HEAD [BASE]: the fake GitHub sends a pull_request hook.
pr() {
	[[ $# -ge 4 ]] || { echo "pr REPO N ACTION HEAD [BASE]" >&2; exit 1; }
	curl -fsS --cacert $PEBBLE/api-root.pem -X POST --get \
		--data-urlencode "repo=$1" --data-urlencode "number=$2" --data-urlencode "action=$3" \
		--data-urlencode "head=$4" --data-urlencode "base=${5:-main}" \
		"https://github.$DOMAIN:8443/_dev/pr"
}

# ---- disks ----

disk() {
	local op=${1:-} n=${2:-} name=${3:-} size=${4:-2G} img mnt
	[[ -n $n && -n $name ]] || die "disk add NODE NAME [2G] | disk rm NODE NAME"
	# both end up in paths that rm -rf runs on
	[[ $n =~ ^[a-z0-9][a-z0-9-]*$ && $name =~ ^[a-z0-9][a-z0-9-]*$ ]] || die "node and disk names: a-z 0-9 -"
	img=$DISKS/$n-$name.img mnt=$DISKS/$n-$name
	case $op in
	add)
		exists "$n" || die "no node $n"
		[[ ! -e $img ]] || die "disk $name exists on $n"
		mkdir -p "$DISKS" "$mnt"
		truncate -s "${size#size=}" "$img"
		mkfs.ext4 -q -F "$img"
		mountpoint -q "$mnt" || mount -o loop "$img" "$mnt"
		incus config device add "$n" "$name" disk source="$mnt" path="/mnt/$name"
		say "$n has /mnt/$name"
		;;
	rm)
		incus config device remove "$n" "$name" 2>/dev/null || true
		mountpoint -q "$mnt" && umount "$mnt"
		rm -rf "$img" "$mnt"
		;;
	*) die "disk add NODE NAME [2G] | disk rm NODE NAME" ;;
	esac
}

# every disk of one node, with the node
drop_disks() {
	local img
	for img in "$DISKS/$1"-*.img; do
		[[ -f $img ]] || continue
		mountpoint -q "${img%.img}" && umount "${img%.img}"
		rm -rf "$img" "${img%.img}"
	done
}

# ---- reset ----

# What the fakes remember. Credentials, the Pebble CA and the GitHub cert stay.
wipe_fakes() {
	local u units=()
	for u in fake-namecheap fake-github podman-rustfs; do
		systemctl cat "$u" >/dev/null 2>&1 && units+=("$u")
	done
	say "wipe the fakes"
	((${#units[@]} == 0)) || systemctl stop "${units[@]}"
	rm -f /var/lib/playground/namecheap/zones.json
	[[ ! -f /var/lib/playground/namecheap/dnsmasq.conf ]] || : >/var/lib/playground/namecheap/dnsmasq.conf
	rm -rf /var/lib/playground/github/state.json /var/lib/playground/github/repos
	[[ ! -d /var/lib/playground/rustfs ]] || find /var/lib/playground/rustfs -mindepth 1 -delete
	find /srv/shares -mindepth 2 -delete 2>/dev/null || true
	((${#units[@]} == 0)) || systemctl start "${units[@]}"
	if systemctl cat rustfs-bucket >/dev/null 2>&1; then systemctl restart rustfs-bucket; fi
}

reset() {
	local fakes=0 target="" rest=() a
	for a in "$@"; do
		case $a in
		--fakes) fakes=1 ;;
		w1 | ext | ctrl) target=$a ;;
		*) rest+=("$a") ;;
		esac
	done
	src "${rest[@]}"
	((fakes == 0)) || wipe_fakes
	case $target in
	w1 | ext)
		exists ctrl || die "no ctrl; reset ctrl first"
		say "remove $target from the cluster"
		stk node rm "$target" --yes || echo "$target was not removed from the panel" >&2
		incus delete -f "$target" 2>/dev/null || true
		drop_disks "$target"
		for e in "${NODES[@]}"; do
			IFS=: read -r n br ip gw <<<"$e"
			[[ $n != "$target" ]] || launch "$n" "$br" "$ip" "$gw"
		done
		trust
		docker_in "$target"
		join "$target"
		;;
	*)
		for e in "${NODES[@]}"; do incus delete -f "${e%%:*}" 2>/dev/null || true; drop_disks "${e%%:*}"; done
		up "${rest[@]}"
		;;
	esac
}

# ---- status ----

# One line per check; any FAIL makes the exit code 1.
FAILS=0
ok() { echo "ok   $1 $2"; }
fail() { echo "FAIL $1 $2"; FAILS=$((FAILS + 1)); }
# check NAME CMD...: ok when CMD succeeds, its output (one line) is the detail
check() {
	local name=$1 out; shift
	if out=$("$@" 2>&1); then ok "$name" "$(tr '\n' ' ' <<<"$out")"; else fail "$name" "$(tr '\n' ' ' <<<"$out")"; fi
}

status() {
	local u n root sha v keys k img
	for u in dnsmasq incus pebble playground-disks playground-trust fake-namecheap fake-github podman-rustfs; do
		case $u in dnsmasq | incus | pebble | playground-*) ;; *) systemctl cat "$u" >/dev/null 2>&1 || continue ;; esac
		check "unit $u" systemctl is-enabled "$u"
	done
	for e in "${NODES[@]}"; do
		n=${e%%:*}
		check "container $n" bash -c '[[ $(incus list "^$0$" -f csv -c s) == RUNNING ]] && echo running' "$n"
	done
	check "dns $PANEL" bash -c '[[ $(dig +short +time=2 +tries=1 @203.0.113.1 "$0") == 203.0.113.1 ]] && echo 203.0.113.1' "$PANEL"
	root=$(mktemp)
	if pebble_root "$root"; then
		sha=$(sha256sum "$root" | cut -c1-12)
		check "panel health" curl -fsS --cacert "$root" -o /dev/null -w "https %{http_code}" "https://$PANEL/api/health"
		for e in "${NODES[@]}"; do
			n=${e%%:*}
			check "root $n" bash -c 'incus exec "$0" -- test -f "/usr/local/share/ca-certificates/pebble-root-$1.crt" && echo "$1"' "$n" "$sha"
		done
	else
		fail "pebble root" "cannot fetch https://acme.$DOMAIN:15000/roots/0"
	fi
	rm -f "$root"
	img=$(x ctrl sh -c 'docker inspect -f "{{.Config.Image}}" "$(docker ps -q --filter label=stackr.role=panel | head -n1)"' 2>&1) || img=""
	if [[ -n $img ]]; then ok "panel image" "$img"; else fail "panel image" "no panel container on ctrl"; fi
	check "nodes" stk node ls
	keys=$(x ctrl sh -c 'grep -v "^#" /var/lib/stackr/dev.env | cut -d= -f1' 2>/dev/null | tr '\n' ' ') || keys=""
	v=""
	for k in ACME_CA ACME_CA_ROOT NAMECHEAP_API GITHUB_URL GITHUB_CA; do [[ " $keys" == *" $k "* ]] || v+="$k "; done
	if [[ -z $v ]]; then ok "dev.env" "$keys"; else fail "dev.env" "missing: $v"; fi
	for img in "$DISKS"/*.img; do
		[[ -f $img ]] || continue
		check "disk $(basename "$img" .img)" mountpoint -q "${img%.img}"
	done
	((FAILS == 0))
}

# ---- entry points ----

# logs NODE [CONTAINER] [LINES]; with no container, the containers on that node
logs() {
	local n=${1:-} c=${2:-} lines=${3:-100}
	[[ -n $n ]] || die "logs NODE [CONTAINER] [LINES]"
	if [[ -z $c ]]; then x "$n" docker ps -a --format '{{.Names}}\t{{.Status}}'; return; fi
	x "$n" docker logs --tail "$lines" "$c" 2>&1
}

case ${1:-} in
up) shift; up "$@" ;;
upgrade) shift; upgrade "$@" ;;
reset) shift; reset "$@" ;;
trust) trust ;;
disk) shift; disk "$@" ;;
hosts) hosts ;;
pr) shift; pr "$@" ;;
stk) shift; stk "$@" ;;
x) shift; x "$@" ;;
logs) shift; logs "$@" ;;
status) status ;;
*) sed -n '3,16p' "$0"; exit 1 ;;
esac

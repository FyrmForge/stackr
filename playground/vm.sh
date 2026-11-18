#!/usr/bin/env bash
# Laptop side: the playground VM in libvirt.
#
#   playground/vm.sh bundle                build this checkout into $PLAYGROUND_OUT (vX.Y.Z-dev.<stamp>)
#   playground/vm.sh create                build the NixOS image in Docker, boot it, run nodes.sh up
#   playground/vm.sh deploy                push the bundle and upgrade the running cluster to it
#   playground/vm.sh sync                  push playground/ to the VM and nixos-rebuild switch
#   playground/vm.sh nodes [args]          run nodes.sh on the VM (default: up --local /root/out)
#   playground/vm.sh ssh [cmd]             a shell (or one command) on the VM
#   playground/vm.sh start | stop          boot it, or shut it down cleanly
#   playground/vm.sh status | ip           state, and its address on the default network
#   playground/vm.sh console               serial console (leave with Ctrl+])
#   playground/vm.sh delete --yes          remove the VM and its disk
#
# Env: PLAYGROUND_OUT (bundle dir, default ../stackr-playground-out next to the repo),
# PLAYGROUND_ENV (.env holding UNIVERSAL_STACKR_PASSWORD, default the repo's .env), PLAYGROUND_KEY (public key).
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NAME="${PLAYGROUND_VM:-playground}"
KEY="${PLAYGROUND_KEY:-$HOME/.ssh/id_ed25519.pub}"
ROOT="$(dirname "$HERE")"
OUT="${PLAYGROUND_OUT:-$(dirname "$ROOT")/stackr-playground-out}"
ENVFILE="${PLAYGROUND_ENV:-$ROOT/.env}"
export LIBVIRT_DEFAULT_URI="${LIBVIRT_DEFAULT_URI:-qemu:///system}"
SSH=(-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o ConnectTimeout=5)

CACHE="${XDG_CACHE_HOME:-$HOME/.cache}/stackr-playground"

ip() { virsh domifaddr "$NAME" --source lease 2>/dev/null | awk '/ipv4/ {split($4, a, "/"); print a[1]; exit}'; }
# shellcheck disable=SC2029
vssh() { ssh "${SSH[@]}" "root@$(ip)" "$@"; }

# playground/ with the laptop key appended to ssh-keys; the repo copy stays a placeholder.
stage() {
	[[ -f $KEY ]] || { echo "no public key at $KEY" >&2; exit 1; }
	mkdir -p "$CACHE"
	STAGE="$(mktemp -d "$CACHE/stage.XXXXXX")"
	trap 'rm -rf "$STAGE"' EXIT
	cp -r "$HERE" "$STAGE/playground"
	cat "$KEY" >>"$STAGE/playground/ssh-keys"
}

push_config() {
	vssh 'rm -rf /etc/playground && mkdir -p /etc/playground'
	tar -C "$STAGE/playground" -c . | vssh 'tar -x -C /etc/playground'
}

wait_ssh() {
	echo "waiting for ssh"
	for _ in $(seq 180); do
		[[ -n $(ip) ]] && vssh true 2>/dev/null && return
		sleep 2
	done
	echo "no ssh after 6 minutes; try: $0 console" >&2
	exit 1
}

# Highest release tag with its patch plus one, and a UTC stamp: unreleased builds are always -dev.N.
bundle() {
	local last ver old
	last="$(git -C "$ROOT" tag --sort=-v:refname | grep -m1 -E '^v[0-9]+\.[0-9]+\.[0-9]+$')"
	ver="v${last#v}"
	ver="${ver%.*}.$((${ver##*.} + 1))-dev.$(date -u +%y%m%d%H%M)"
	mkdir -p "$OUT"
	nice -n 10 make -C "$ROOT" release RELEASE="$ver"
	docker save "ghcr.io/fyrmforge/stackr:${ver#v}" "ghcr.io/fyrmforge/stackr-observe:${ver#v}" -o "$OUT/images.tar"
	cp "$ROOT/bin/stackr-install-linux-amd64" "$OUT/"
	# the previous dev images are in the new tar's place; drop their tags
	old="$(cat "$OUT/VERSION" 2>/dev/null || true)"
	if [[ -n $old && $old != "$ver" ]]; then
		docker rmi "ghcr.io/fyrmforge/stackr:${old#v}" "ghcr.io/fyrmforge/stackr-observe:${old#v}" >/dev/null 2>&1 || true
	fi
	echo "$ver" >"$OUT/VERSION"
	echo "bundle $ver in $OUT"
}

push_bundle() {
	[[ -f $OUT/images.tar && -f $OUT/stackr-install-linux-amd64 ]] || { echo "bundle missing in $OUT; run: $0 bundle" >&2; exit 1; }
	tar -C "$OUT" -c . | vssh 'rm -rf /root/out && mkdir -p /root/out && tar -x -C /root/out'
}

# The admin password goes over ssh stdin into a mode 600 file on the VM, only for up and reset,
# and is removed afterwards. Arguments are quoted for the VM's shell.
nodes() {
	[[ $# -gt 0 ]] || set -- up
	local pw args rc=0 load=""
	args="$(printf '%q ' "$@")"
	case $1 in
	up | reset)
		pw="$(grep -m1 '^UNIVERSAL_STACKR_PASSWORD=' "$ENVFILE" | cut -d= -f2- | sed -e 's/^["'\'']//' -e 's/["'\'']$//' || true)"
		[[ -n $pw ]] || { echo "UNIVERSAL_STACKR_PASSWORD not found in $ENVFILE" >&2; exit 1; }
		printf 'PLAYGROUND_ADMIN_PASSWORD=%q\n' "$pw" | vssh 'umask 077; cat >/root/.playground.env'
		load='set -a; . /root/.playground.env; set +a; '
		;;
	esac
	vssh "${load}/etc/playground/nodes.sh $args" || rc=$?
	vssh 'rm -f /root/.playground.env'
	return $rc
}

case "${1:-}" in
create)
	command -v docker >/dev/null || { echo "docker is required" >&2; exit 1; }
	[[ -f $OUT/images.tar && -f $OUT/stackr-install-linux-amd64 ]] || bundle
	virsh dominfo "$NAME" >/dev/null 2>&1 && { echo "VM $NAME exists; run: $0 delete" >&2; exit 1; }
	stage
	img="$STAGE/nixos.qcow2"
	# /nix lives in a named volume, so rebuilds are cached.
	docker run --rm --device /dev/kvm -v playground-nix:/nix -v "$STAGE/playground:/src:ro" -v "$STAGE:/out" \
		nixos/nix sh -euc '
			nix --extra-experimental-features "nix-command flakes" build --no-link --print-out-paths \
				/src#nixosConfigurations.playground.config.system.build.images.qemu-efi >/out/path
			cp -L "$(cat /out/path)"/*.qcow2 /out/nixos.qcow2
			chown '"$(id -u):$(id -g)"' /out/nixos.qcow2'
	# QEMU cannot read the home folder: upload into libvirt's own pool, then grow.
	vol="$NAME.qcow2"
	virsh vol-delete "$vol" --pool default >/dev/null 2>&1 || true
	virsh vol-create-as default "$vol" "$(stat -c %s "$img")" --format qcow2 >/dev/null
	virsh vol-upload "$vol" "$img" --pool default
	virsh vol-resize "$vol" 60G --pool default
	virt-install --name "$NAME" --vcpus 6 --memory 8192 --import \
		--disk "vol=default/$vol,bus=virtio" --os-variant nixos-unstable --boot uefi \
		--network network=default,model=virtio,mac=52:54:00:77:00:01 --noautoconsole
	wait_ssh
	push_config
	push_bundle
	nodes up
	echo "VM $NAME at $(ip); run: $0 nodes hosts"
	;;
bundle) bundle ;;
deploy) wait_ssh; push_bundle; nodes upgrade --local /root/out ;;
ssh) shift; vssh "$@" ;;
sync)
	stage
	wait_ssh
	push_config
	vssh 'nixos-rebuild switch --flake /etc/playground#playground'
	;;
nodes) shift; nodes "$@" ;;
start) virsh start "$NAME" ;;
stop)
	virsh shutdown "$NAME"
	for _ in $(seq 60); do
		[[ $(virsh domstate "$NAME") == "shut off" ]] && { echo "stopped"; exit 0; }
		sleep 1
	done
	echo "still running after 60s; force it with: virsh destroy $NAME" >&2
	exit 1
	;;
status) echo "$(virsh domstate "$NAME") $(ip)" ;;
ip) ip ;;
console) virsh console "$NAME" ;;
delete)
	if [[ ${2:-} != --yes ]]; then
		read -rp "delete VM $NAME and its disk? [y/N] " a
		[[ $a == y ]] || exit 1
	fi
	virsh destroy "$NAME" 2>/dev/null || true
	virsh undefine "$NAME" --nvram --remove-all-storage
	;;
*) sed -n '2,15p' "$0"; exit 1 ;;
esac

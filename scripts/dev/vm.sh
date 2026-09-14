#!/usr/bin/env bash
# Throwaway local test VM for stackr on libvirt: Debian cloud image +
# cloud-init that sets up what scripts/dev/deploy-test.sh expects (your ssh key,
# docker, the `stackr` network, ~/stackr/.env with TLS off).
#
#   scripts/dev/vm.sh up      create + boot, prints the IP
#   scripts/dev/vm.sh ip      print the IP
#   scripts/dev/vm.sh down    destroy VM and disk
#   scripts/dev/vm.sh ssh     shell in
#
# Then: STACKR_DEPLOY_HOST=$(scripts/dev/vm.sh ip) scripts/dev/deploy-test.sh
# This helper intentionally owns one fixed throwaway VM.
set -euo pipefail
export LIBVIRT_DEFAULT_URI=qemu:///system

NAME="${STACKR_VM_NAME:-stackr-test}"
CACHE="${XDG_CACHE_HOME:-$HOME/.cache}/stackr-vm"
BASE_URL="https://cloud.debian.org/images/cloud/trixie/latest/debian-13-genericcloud-amd64.qcow2"
BASE="$CACHE/debian-13-genericcloud-amd64.qcow2"
# Disks live in libvirt's default pool: qemu runs as libvirt-qemu and can't
# read $HOME, and vol-* commands need no sudo for members of the libvirt group.
POOL=default
VMUSER="$USER"
PUBKEY="$(cat "${STACKR_VM_PUBKEY:-$HOME/.ssh/id_ed25519.pub}")"

ip() {
  virsh -q domifaddr "$NAME" 2>/dev/null | awk '/ipv4/ {sub(/\/.*/, "", $4); print $4; exit}'
}

case "${1:-}" in
up)
  mkdir -p "$CACHE"
  [ -f "$BASE" ] || curl -fL# "$BASE_URL" -o "$BASE"
  if ! virsh -q vol-info --pool "$POOL" base-debian-13 >/dev/null 2>&1; then
    virsh -q vol-create-as "$POOL" base-debian-13 "$(stat -c %s "$BASE")" --format qcow2 >/dev/null
    virsh -q vol-upload --pool "$POOL" base-debian-13 "$BASE" >/dev/null
  fi
  virsh -q vol-create-as "$POOL" "$NAME.qcow2" 20G --format qcow2 \
    --backing-vol base-debian-13 --backing-vol-format qcow2 >/dev/null

  USERDATA="$(mktemp)"
  cat > "$USERDATA" <<CI
#cloud-config
hostname: $NAME
users:
  - name: $VMUSER
    groups: [sudo, docker]
    shell: /bin/bash
    sudo: ALL=(ALL) NOPASSWD:ALL
    ssh_authorized_keys:
      - $PUBKEY
packages: [docker.io, git, rsync]
runcmd:
  - docker network create stackr
  - mkdir -p /home/$VMUSER/stackr/data
  - |
    cat > /home/$VMUSER/stackr/.env <<ENV
    PORT=8080
    DEV_MODE=true
    BASE_URL=http://\$(hostname -I | cut -d' ' -f1):8080
    DATA_DIR=/home/$VMUSER/stackr/data
    DATABASE_PATH=/home/$VMUSER/stackr/data/stackr.db
    # empty = proxy serves plain HTTP, no certificates
    ACME_EMAIL=
    ENV
  - chown -R $VMUSER:$VMUSER /home/$VMUSER/stackr
CI

  virt-install --name "$NAME" --memory 4096 --vcpus 2 \
    --disk "vol=$POOL/$NAME.qcow2" --import --osinfo debian13 \
    --network network=default --graphics none --video vga --noautoconsole \
    --cloud-init "user-data=$USERDATA" >/dev/null
  rm -f "$USERDATA"

  echo -n "waiting for IP"
  for _ in $(seq 60); do
    IP="$(ip)"; [ -n "$IP" ] && break
    echo -n .; sleep 2
  done
  echo
  [ -n "${IP:-}" ] || { echo "no IP after 2min, check: virsh console $NAME"; exit 1; }
  echo "$NAME has an IP at $IP, waiting for cloud-init"

  # A DHCP lease is not a working box. This used to print "up" and the deploy
  # command the moment the lease appeared, so a cloud-init that had failed
  # rather than merely being slow looked identical to one that worked, and the
  # first sign of trouble was deploy-test.sh failing much later with something
  # unrelated-looking.
  for _ in $(seq 60); do
    ssh -o StrictHostKeyChecking=no -o ConnectTimeout=5 -o BatchMode=yes \
      "$VMUSER@$IP" true >/dev/null 2>&1 && break
    echo -n .; sleep 5
  done
  echo
  STATUS="$(ssh -o StrictHostKeyChecking=no -o ConnectTimeout=10 -o BatchMode=yes \
    "$VMUSER@$IP" 'cloud-init status --wait 2>/dev/null || true' 2>/dev/null | tr -d '\r')"
  case "$STATUS" in
    *done*)
      if ssh -o StrictHostKeyChecking=no -o BatchMode=yes "$VMUSER@$IP" 'command -v docker' >/dev/null 2>&1; then
        echo "$NAME up at $IP"
        echo "deploy: STACKR_DEPLOY_HOST=$IP scripts/dev/deploy-test.sh"
        echo "panel:  http://$IP:8080"
      else
        echo "$NAME booted at $IP but has no docker, so cloud-init did not do its job."
        echo "read it with: $0 ssh -- sudo cat /var/log/cloud-init-output.log"
        exit 1
      fi
      ;;
    *)
      echo "$NAME booted at $IP but cloud-init reports: ${STATUS:-no answer over ssh}"
      echo "the box is NOT ready; deploying to it will fail later and confusingly."
      echo "read it with: $0 ssh -- sudo cat /var/log/cloud-init-output.log"
      exit 1
      ;;
  esac
  ;;
ip) ip ;;
ssh) exec ssh "$VMUSER@$(ip)" "${@:2}" ;;
down)
  virsh destroy "$NAME" >/dev/null 2>&1 || true
  virsh undefine "$NAME" --remove-all-storage >/dev/null 2>&1 || true
  virsh vol-delete --pool "$POOL" "$NAME.qcow2" >/dev/null 2>&1 || true
  echo "$NAME gone"
  ;;
*) sed -n 2,12p "$0"; exit 1 ;;
esac

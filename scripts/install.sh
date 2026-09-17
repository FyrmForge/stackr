#!/usr/bin/env bash
# Install stackr on a fresh Docker host.
#
# Downloads stackr-install for this CPU from the same release, checks it
# against the release's checksums.txt, and runs it. The installer is the form
# and the whole install (docs/plans/49-installer-binary.md); every argument
# is passed to it:
#
#   curl -fsSL https://github.com/FyrmForge/stackr/releases/latest/download/install.sh | sudo bash
#   curl -fsSL .../install.sh | sudo bash -s -- --version 0.1.4
#   curl -fsSL .../install.sh | bash -s -- --dry-run
#   curl -fsSL .../install.sh | bash -s -- --help
#
# The body sits in one { } block so a download cut off halfway runs nothing.
set -euo pipefail

{
die() { echo "error: $*" >&2; exit 1; }

# --version picks the release the installer comes from, as well as the one it
# installs.
releases=https://github.com/FyrmForge/stackr/releases
base=$releases/latest/download
prev=""
for a in "$@"; do
  case "$a" in --version=*) base=$releases/download/v${a#--version=}; base=${base/\/vv/\/v} ;; esac
  if [ "$prev" = --version ]; then base=$releases/download/v${a#v}; fi
  prev=$a
done

case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) die "no installer for $(uname -m); stackr runs on amd64 and arm64" ;;
esac
command -v curl >/dev/null || die "curl is needed"
command -v sha256sum >/dev/null || die "sha256sum is needed"

bin=stackr-install-linux-$arch
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
curl -fsSL -o "$tmp/$bin" "$base/$bin" || die "could not download $base/$bin"
curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt" || die "could not download $base/checksums.txt"
(cd "$tmp" && grep " $bin\$" checksums.txt | sha256sum -c --quiet -) ||
  die "$bin does not match the release's checksums.txt; not running it"
chmod 755 "$tmp/$bin"

# Piped from curl, stdin is this script, so the form reads the terminal.
if { : </dev/tty; } 2>/dev/null; then
  "$tmp/$bin" "$@" </dev/tty
else
  "$tmp/$bin" "$@"
fi
}

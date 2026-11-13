#!/bin/sh
# stackr: install a panel on this machine.
#
#   curl -fsSL https://github.com/FyrmForge/stackr/releases/latest/download/install.sh | sudo sh
#
# Flags go to the installer: ... | sudo sh -s -- --domain apps.example.com
# It downloads stackr-install from the release, checks it against the
# release's SHA256SUMS, keeps it as /usr/local/bin/stackr-install and runs it.
set -eu

# The release workflow stamps the version; empty means the latest release.
VERSION=""

fail() { echo "stackr: $*" >&2; exit 1; }

# The body sits in a function so a cut-off download runs nothing.
main() {
	[ "$(uname -s)" = Linux ] || fail "the panel runs on Linux only"
	case "$(uname -m)" in
	x86_64 | amd64) ;;
	*) fail "the panel needs amd64, this machine is $(uname -m)" ;;
	esac
	[ "$(id -u)" = 0 ] || fail "run as root: pipe it to sudo sh"
	command -v curl >/dev/null 2>&1 || fail "curl is missing"
	command -v sha256sum >/dev/null 2>&1 || fail "sha256sum is missing"

	base=https://github.com/FyrmForge/stackr/releases/latest/download
	[ -z "$VERSION" ] || base=https://github.com/FyrmForge/stackr/releases/download/v$VERSION

	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	trap 'exit 1' HUP INT TERM
	curl -fsSL -o "$tmp/stackr-install-linux-amd64" "$base/stackr-install-linux-amd64"
	curl -fsSL -o "$tmp/SHA256SUMS" "$base/SHA256SUMS"
	(cd "$tmp" && grep ' stackr-install-linux-amd64$' SHA256SUMS | sha256sum -c - >/dev/null 2>&1) ||
		fail "the installer does not match the release's SHA256SUMS"
	install -m 0755 "$tmp/stackr-install-linux-amd64" /usr/local/bin/stackr-install

	# Piped to sh, stdin is this script: the installer's questions need the
	# terminal. With none (CI, cloud-init) it runs on its flags alone.
	if (: </dev/tty) 2>/dev/null; then
		/usr/local/bin/stackr-install "$@" </dev/tty
	else
		/usr/local/bin/stackr-install "$@" </dev/null
	fi
}

main "$@"

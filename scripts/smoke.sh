#!/usr/bin/env bash
# Install-and-upgrade smoke test. Run as root on a clean Linux box with Docker.
#
#   scripts/smoke.sh <new-tag> [<released-tag>]
#
#   <new-tag>       the build under test, e.g. v0.8.11-dev.7. Its images
#                   (ghcr.io/fyrmforge/stackr:<tag w/o v> and stackr-observe)
#                   must already be loaded; `make release RELEASE=<new-tag>`.
#   <released-tag>  the release to install first (default: the latest GitHub release).
#   SMOKE_INSTALLER an installer binary to use instead of downloading it.
#
# 1. install the released version over plain HTTP, 2. deploy nginx and reach it
# through the proxy, 3. upgrade to <new-tag> through the panel's own API, 4. try
# an upgrade to a deliberately broken build and expect a rollback. One line per
# check, exit 1 on any failure. Needs ports 80 and 443 and wipes nothing: run it
# on a throwaway box.
set -uo pipefail

NEW="${1:?usage: smoke.sh <new-tag> [<released-tag>]}"
OLD="${2:-}"
[[ "$NEW" =~ ^v([0-9]+\.[0-9]+\.[0-9]+)-dev\.([0-9]+)$ ]] || { echo "new tag must look like v0.8.11-dev.7" >&2; exit 2; }
BROKEN="v${BASH_REMATCH[1]}-dev.$((BASH_REMATCH[2] + 1))" # higher, so the panel accepts it as an upgrade
IMG=ghcr.io/fyrmforge/stackr
ADMIN=smoke@smoke.test
ROOT=smoke.localhost # *.localhost: curl resolves it to loopback and sends Secure cookies over HTTP
PANEL=panel.$ROOT
APPHOST=web.$ROOT
BASE=http://$PANEL
T="$(mktemp -d)"
PW="$(head -c 24 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 24)-Aa1"
KEY=""
FAILED=0

[[ $EUID -eq 0 ]] || { echo "run as root" >&2; exit 2; }
for c in docker curl; do command -v $c >/dev/null || { echo "$c is required" >&2; exit 2; }; done
command -v jq >/dev/null || { apt-get update -qq >/dev/null && apt-get install -y -qq jq >/dev/null; } || { echo "jq is required" >&2; exit 2; }

cleanup() {
	local rc=$?
	if [[ $rc -ne 0 || $FAILED -ne 0 ]]; then
		echo "--- containers"; docker ps -a --format '{{.Names}}  {{.Image}}  {{.Status}}' | grep -i stackr
		echo "--- panel log (tail)"; docker logs --tail 25 stackr 2>&1 | tail -25
		echo "--- install log (tail, lines naming keys left out)"; grep -vi 'key' "$T/install.log" 2>/dev/null | tail -15
	fi
	rm -rf "$T"
}
trap cleanup EXIT

ok() { printf 'ok    %s\n' "$1"; }
bad() { printf 'FAIL  %s\n' "$1"; FAILED=1; }
# check "<what>" cmd...   non-fatal
check() { local d="$1"; shift; if "$@"; then ok "$d"; else bad "$d"; fi; }
# die "<what>"            fatal: nothing after it can mean anything
die() { printf 'FAIL  %s\n' "$1"; FAILED=1; exit 1; }
# product "<id: what>"    a check whose failure is a known product bug, not a smoke problem
product() { printf 'FAIL  PRODUCT BUG %s\n' "$1"; FAILED=1; }

# api METHOD PATH [JSON]: a bearer-key call; prints the body, returns non-zero on a non-2xx
api() {
	local m="$1" p="$2" body="${3:-}" args=(-sS -o "$T/out" -w '%{http_code}' -X "$1" -H "Authorization: Bearer $KEY")
	[[ -n "$body" ]] && args+=(-H 'Content-Type: application/json' -d "$body")
	local code
	code="$(curl "${args[@]}" "$BASE/api/v1$p")" || return 1
	cat "$T/out"
	[[ "$code" =~ ^2 ]] || { echo "$m $p -> $code $(head -c 300 "$T/out")" >&2; return 1; }
}

healthy() { [[ "$(curl -s "$BASE/api/health" | jq -r .status 2>/dev/null)" == healthy ]]; }
wait_healthy() { local i; for ((i = 0; i < ${1:-90}; i++)); do healthy && return 0; sleep 2; done; return 1; }
version() { api GET /admin/version 2>/dev/null | jq -r .version; }
proxy_image() { docker inspect -f '{{.Config.Image}}' stackr-proxy 2>/dev/null; }
app_answers() { curl -s -o /dev/null -w '%{http_code}' --max-time 5 "http://$APPHOST/" | grep -q '^200$'; }
wait_app() { local i; for ((i = 0; i < ${1:-30}; i++)); do app_answers && return 0; sleep 2; done; return 1; }
# job_state ID: the job's state through the admin route (survives the panel restart)
job_state() { api GET "/admin/jobs/$1" 2>/dev/null | jq -r .state; }
# job_log ID: the job's own log tail, for a failed job (lines naming keys left out)
job_log() {
	local out; out="$(api GET "/admin/jobs/$1/log" 2>/dev/null)"
	echo "--- job $1 error: $(jq -r .job.error <<<"$out" | grep -viE 'key|token|password|secret')"
	echo "--- job $1 log (tail)"; jq -r .log <<<"$out" | grep -viE 'key|token|password|secret' | tail -60
	echo "--- panel warnings and errors"; docker logs stackr 2>&1 | grep -E '"level":"(WARN|ERROR)"' | grep -viE 'key|token|password|secret' | tail -30
	echo "--- docker"; docker version --format 'server {{.Server.Version}}' 2>&1; docker info --format 'driver {{.Driver}} {{.DriverStatus}}' 2>&1 | head -3
}
wait_job() { # ID SECONDS -> prints the final state
	local i s=""
	for ((i = 0; i < $2 / 3; i++)); do
		s="$(job_state "$1")"
		case "$s" in done | failed | canceled | cancelled) echo "$s"; return 0 ;; esac
		sleep 3
	done
	echo "${s:-unreachable}"; return 1
}

# ---- 1. install the released version
if [[ -z "$OLD" ]]; then
	OLD="$(curl -fsS ${GH_TOKEN:+-H "Authorization: Bearer $GH_TOKEN"} https://api.github.com/repos/FyrmForge/stackr/releases/latest | jq -r .tag_name)" || die "could not read the latest release"
fi
INST="${SMOKE_INSTALLER:-$T/stackr-install}"
if [[ -z "${SMOKE_INSTALLER:-}" ]]; then
	curl -fsSL -o "$INST" "https://github.com/FyrmForge/stackr/releases/download/$OLD/stackr-install-linux-amd64" || die "download $OLD installer"
	chmod +x "$INST"
fi
docker image inspect "$IMG:${NEW#v}" "$IMG-observe:${NEW#v}" >/dev/null 2>&1 || die "images for $NEW are not loaded (make release RELEASE=$NEW)"

printf '%s\n' "installing $OLD, upgrading to $NEW, then to broken $BROKEN"
( umask 077; STACKR_ADMIN_PASSWORD="$PW" "$INST" --domain "$ROOT" --panel-host "$PANEL" --https=false \
	--admin-email "$ADMIN" --yes >"$T/install.log" 2>&1 ) || die "install $OLD (installer exited non-zero)"
wait_healthy 60 || die "panel of $OLD never became healthy"
ok "installed $OLD, /api/health healthy"

# ---- the admin API key: the installer logs this box's CLI in as the admin
KEY="$(jq -r .key /var/lib/stackr/cli.json 2>/dev/null)"
[[ -n "$KEY" && "$KEY" != null ]] || die "no admin key in /var/lib/stackr/cli.json"
api GET /admin/version >/dev/null || die "the installer's admin key is refused"
ok "admin API key (the CLI's login)"

check "panel reports $OLD" test "$(version)" = "$OLD"
check "stackr-proxy runs $IMG:${OLD#v}" test "$(proxy_image)" = "$IMG:${OLD#v}"

# ---- 2. deploy one app and reach it through the proxy
ORG="$(api POST /orgs '{"name":"smoke"}' | jq -r .slug)"
STACK="$(api POST "/orgs/$ORG/stacks" '{"name":"web","description":""}' | jq -r .slug)"
ENV="$(api POST "/orgs/$ORG/stacks/$STACK/envs" '{"name":"prod","type":"static","color":"","from_kind":"branch","from_branch":"main","auto":false,"placement":""}' | jq -r .slug)"
TP="/orgs/$ORG/stacks/$STACK/envs/$ENV/tiles"
[[ -n "$ORG$STACK$ENV" && "$ORG" != null && "$STACK" != null && "$ENV" != null ]] || die "create org, stack and env"
TILE="$(api POST "$TP" '{"name":"web","kind":"image","image_ref":"nginx:alpine","container_port":80}' | jq -r .slug)" || die "create the nginx tile"
api POST "$TP/$TILE/domains" "{\"host\":\"$APPHOST\",\"path\":\"\",\"port\":80,\"https\":false,\"force_https\":false,\"redirect_to\":\"\",\"proxy\":{}}" >/dev/null || die "attach $APPHOST to the tile"
JOB="$(api POST "$TP/$TILE/deploy" | jq -r .id)"
s="$(
	for ((i = 0; i < 100; i++)); do
		s="$(api GET "/orgs/$ORG/jobs/$JOB" 2>/dev/null | jq -r .state)"
		case "$s" in done | failed | canceled) echo "$s"; break ;; esac
		sleep 3
	done
)"
[[ "$s" == done ]] || { job_log "$JOB"; die "deploy job ended '$s'"; }
check "nginx answers on http://$APPHOST/ through the proxy" wait_app

# ---- 3. upgrade through the panel's own path
JOB="$(api POST /admin/upgrade "{\"tag\":\"$NEW\"}" | jq -r .id)" || die "POST /admin/upgrade $NEW was refused"
[[ -n "$JOB" && "$JOB" != null ]] || die "the upgrade returned no job"
s="$(wait_job "$JOB" 420)"
[[ "$s" == done ]] || job_log "$JOB"
check "upgrade job to $NEW finished 'done' (got '$s')" test "$s" = done
check "panel healthy after the upgrade" wait_healthy 60
check "panel reports $NEW" test "$(version)" = "$NEW"
check "stackr-proxy runs $IMG:${NEW#v}" test "$(proxy_image)" = "$IMG:${NEW#v}"
check "nginx still answers" wait_app

# ---- 4. a build whose panel never gets healthy must roll back
mkdir "$T/broken"
cat >"$T/broken/Dockerfile" <<EOF
FROM $IMG:${NEW#v}
# the swap helper (upgrade-swap) works, the panel itself never serves
RUN mv /stackrd /stackrd.real && printf '#!/bin/sh\n[ "\$1" = upgrade-swap ] && exec /stackrd.real "\$@"\nexec sleep 100000\n' >/stackrd && chmod +x /stackrd
EOF
docker build -q -t "$IMG:${BROKEN#v}" "$T/broken" >/dev/null || die "build the broken image"
docker tag "$IMG-observe:${NEW#v}" "$IMG-observe:${BROKEN#v}"

JOB="$(api POST /admin/upgrade "{\"tag\":\"$BROKEN\"}" | jq -r .id)" || die "POST /admin/upgrade $BROKEN was refused"
echo "      waiting for the trial of $BROKEN to fail and roll back (about 4 minutes)"
s="$(wait_job "$JOB" 600)"
wait_healthy 120 || bad "panel did not come back healthy after the failed upgrade"
if [[ "$s" == failed ]]; then ok "failed upgrade job reads 'failed'"; else product "U1: the failed upgrade's job reads '$s', not 'failed' (internal/service/internal/flow/upgrade, internal/service/admin.go)"; fi
check "panel rolled back to $NEW" test "$(version)" = "$NEW"
if [[ "$(proxy_image)" == "$IMG:${NEW#v}" ]]; then ok "stackr-proxy back on $IMG:${NEW#v}"; else product "U2: stackr-proxy runs '$(proxy_image)' after the rollback, not $IMG:${NEW#v} (internal/service/internal/flow/upgrade/upgrade.go EnsureProxy)"; fi
check "nginx still answers after the rollback" wait_app

echo
if [[ $FAILED -eq 0 ]]; then echo "smoke: all checks passed"; else echo "smoke: FAILED"; exit 1; fi

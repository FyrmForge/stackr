#!/usr/bin/env bash
# Rig QA for the migration blitz: routes, shares, files, forward auth, host
# grants (docs/rewrite/tasks/migration-blitz.md). Run it against the rig, after
# `scripts/rig.sh upgrade <version>`:
#
#   scripts/qa/blitz.sh [check ...]    checks: login route http share files fwdauth hostgrant
#
# Prints PASS or FAIL per check with the evidence line, cleans up everything
# it made (also on failure), exits 1 when any check failed. It makes its own
# stack (qa-blitz) and throwaway containers (qa-*) and never touches the
# stacks shop, web, infra and byhand: a guard refuses those names.
#
# `files` needs a config repo and a connector (BLITZ_CONFIG_REPO, BLITZ_CONFIG_CONNECTOR)
# and the gh CLI to push; the repo needs stackr-compose.yml, conf/app.txt and conf/t.txt.
#
# Flags and paths were checked against cmd/stackr and internal/api/routes.go;
# the few lines still marked `# verify:` depend on rig behaviour only a run shows.
set -euo pipefail

RIG="${RIG:-stackr-test.inanga-degree.ts.net}"
DOMAIN="${RIG_DOMAIN:-stackr-test.vulpe.dev}"
SERVER="${BLITZ_SERVER:-https://$DOMAIN}"
ORG="${BLITZ_ORG:-smoke}"
EMAIL="${RIG_ADMIN_EMAIL:-admin@test.com}"
PASSWORD="${RIG_ADMIN_PASSWORD:-Test1234!}"
# Where the proxy listens, for curl --resolve and openssl (no DNS needed for
# the qa hosts). The domain itself is fine on the rig.
ADDR="${BLITZ_ADDR:-$DOMAIN}"
# The proxy is a bridge container: the panel listens on the docker0 gateway
# :8080 and the proxy reaches it (and any host-network container) as `stackr`
# (extra_hosts stackr:host-gateway, installspec.Proxy). Targets the proxy dials
# therefore say `stackr`, never 127.0.0.1.
PANEL="${BLITZ_PANEL_ADDR:-stackr:8080}"
UP_PORT="${BLITZ_UPSTREAM_PORT:-9443}"
AUTH_PORT="${BLITZ_AUTH_PORT:-9080}"
# tile exec forwards stdin and waits for its EOF: every call below closes it.
# files check: a config repo holding stackr-compose.yml (see files_check).
CONFIG_REPO="${BLITZ_CONFIG_REPO:-}"
CONFIG_CONNECTOR="${BLITZ_CONFIG_CONNECTOR:-}"

QA="qa-blitz"
ENV="dev"
PROTECTED="shop web infra byhand"
HOST_PASS="qa-pass.$DOMAIN"
HOST_HTTP="qa-http.$DOMAIN"
HOST_FA="qa-fa.$DOMAIN"
SHARE="qa-nfs"
T="--stack $QA --env $ENV"

pass=0 fail=0
rig() { ssh "$RIG" "$@"; }
sr() { stackr "$@"; }
ok() { pass=$((pass + 1)); printf 'PASS %s: %s\n' "$1" "$2"; }
bad() { fail=$((fail + 1)); printf 'FAIL %s: %s\n' "$1" "$2"; }
# expect NAME EVIDENCE CONDITION: PASS when the condition command succeeds.
expect() {
	local name="$1" evidence="$2"
	shift 2
	if "$@"; then ok "$name" "$evidence"; else bad "$name" "$evidence"; fi
}

# Never act on a protected stack, whatever the variables say.
for s in $PROTECTED; do
	[[ "$QA" != "$s" ]] || { echo "refusing: $QA is protected" >&2; exit 2; }
done

# poll N CMD...: retry CMD once a second, up to N tries.
poll() {
	local n="$1" i
	shift
	for ((i = 0; i < n; i++)); do
		"$@" && return 0
		sleep 1
	done
	return 1
}

# status TILE: the last job line of the tile ("deploy done <id>").
status() { sr tile status "$1" $T 2>&1 | grep -i 'last_job' || true; }
deploy() { sr tile deploy "$1" $T --no-wait >/dev/null; }
jobstate() { status "$1" | grep -qi "$2"; }
mktile() { # NAME ARGS...: a sleeping alpine tile unless --image is given
	local n="$1"
	shift
	sr tile create "$n" $T --image alpine:3 --command "sleep 3600" "$@" >/dev/null
}

cleanup() {
	set +e
	echo "-- cleanup"
	sr admin route rm "$HOST_PASS" -y >/dev/null 2>&1
	sr admin route rm "$HOST_HTTP" -y >/dev/null 2>&1
	# Tiles go first and synchronously: the share and the env wait on them.
	for t in qa-share qa-files qa-fa qa-hg; do
		sr tile rm "$t" $T -y >/dev/null 2>&1
	done
	sr share rm "$SHARE" -y >/dev/null 2>&1
	sr host-grant revoke --stack "$QA" -y >/dev/null 2>&1
	sr env rm $T -y >/dev/null 2>&1
	sr stack rm --stack "$QA" -y >/dev/null 2>&1
	rig "docker rm -f qa-tls-upstream qa-nfs qa-auth >/dev/null 2>&1; docker volume rm qa-nfs-data >/dev/null 2>&1; rm -rf /tmp/qa-blitz"
	echo "-- done: $pass passed, $fail failed"
}
trap cleanup EXIT

sel=("$@")
run() { [[ ${#sel[@]} -eq 0 ]] || [[ " ${sel[*]} " == *" $1 "* ]]; }

# ---- 1. login, and the QA stack
login_check() {
	# The CLI logs in through the browser or an API key; the script takes the
	# key (made in the panel as $EMAIL, password $PASSWORD) or an existing login.
	local out=""
	if [[ -n "${STACKR_KEY:-}" ]]; then
		out="$(sr login "$SERVER" --with-key "$STACKR_KEY" --org "$ORG" 2>&1)" || true
	fi
	if sr status 2>&1 | grep -q "$EMAIL"; then
		ok login "$(sr status 2>&1 | grep -m1 "$EMAIL")"
	else
		bad login "${out:-not logged in}; set STACKR_KEY (a key of $EMAIL, password $PASSWORD) or run: stackr login $SERVER --org $ORG"
		exit 1
	fi
	sr stack create "$QA" >/dev/null 2>&1 || true
	sr env create "$ENV" --stack "$QA" --from-kind branch --from-branch main >/dev/null 2>&1 || true
}

# ---- 2. passthrough route: the cert through the proxy is the upstream's
route_check() {
	rig "docker rm -f qa-tls-upstream >/dev/null 2>&1; mkdir -p /tmp/qa-blitz; printf '%s\n' \
		'{ admin off' ' default_sni $HOST_PASS' ' auto_https disable_redirects' '}' \
		'$HOST_PASS:$UP_PORT {' ' tls internal' ' respond \"upstream\"' '}' > /tmp/qa-blitz/Caddyfile; \
		docker run -d --name qa-tls-upstream --network host -v /tmp/qa-blitz:/c caddy:2 \
		caddy run --config /c/Caddyfile --adapter caddyfile >/dev/null"
	sleep 3
	local up via
	up="$(echo | rig "docker run -i --rm --network host alpine/openssl s_client -connect 127.0.0.1:$UP_PORT -servername $HOST_PASS 2>/dev/null" | openssl x509 -noout -serial 2>/dev/null || true)"
	sr admin route add "$HOST_PASS" --to "stackr:$UP_PORT" --mode passthrough >/dev/null
	via="$(poll_serial)"
	expect route-passthrough "upstream $up, through 443 $via" test -n "$up" -a "$up" = "$via"
	sr admin route rm "$HOST_PASS" -y >/dev/null
	sleep 2
	local after
	after="$(echo | openssl s_client -connect "$ADDR:443" -servername "$HOST_PASS" 2>/dev/null | openssl x509 -noout -serial 2>/dev/null || true)"
	expect route-removed "after rm: ${after:-no cert for $HOST_PASS} (upstream was $up)" test "$after" != "$up"
}
poll_serial() {
	local i s
	for i in 1 2 3 4 5 6 7 8 9 10; do
		s="$(echo | openssl s_client -connect "$ADDR:443" -servername "$HOST_PASS" 2>/dev/null | openssl x509 -noout -serial 2>/dev/null || true)"
		[[ -n "$s" ]] && break
		sleep 1
	done
	echo "$s"
}

# ---- 3. http route to the panel's own address
http_check() {
	sr admin route add "$HOST_HTTP" --to "$PANEL" --mode http >/dev/null
	local code redir
	poll 30 curl -ksf -o /dev/null --resolve "$HOST_HTTP:443:$(getent hosts "$ADDR" | awk '{print $1; exit}')" "https://$HOST_HTTP/login" || true
	code="$(curl -ks -o /dev/null -w '%{http_code}' --resolve "$HOST_HTTP:443:$(getent hosts "$ADDR" | awk '{print $1; exit}')" "https://$HOST_HTTP/login")"
	expect route-http "GET https://$HOST_HTTP/login -> $code" test "$code" = 200
	redir="$(curl -s -o /dev/null -w '%{http_code}' --resolve "$HOST_HTTP:80:$(getent hosts "$ADDR" | awk '{print $1; exit}')" "http://$HOST_HTTP/")"
	expect route-http-force "GET http://$HOST_HTTP/ -> $redir" test "$redir" = 308 -o "$redir" = 301 -o "$redir" = 302
	sr admin route rm "$HOST_HTTP" -y >/dev/null
}

# ---- 4. nfs share: write in a tile, read on the server
share_check() {
	rig "docker rm -f qa-nfs >/dev/null 2>&1; docker volume create qa-nfs-data >/dev/null; \
		docker run -d --name qa-nfs --privileged -p 2049:2049 -e SHARED_DIRECTORY=/data \
		-v qa-nfs-data:/data itsthenetwork/nfs-server-alpine:12 >/dev/null"
	# A fresh NFSv4 server refuses writes for its 90s grace period (they block).
	sleep 95
	rig "docker exec qa-nfs mkdir -p /data/sub"
	# The kernel on the rig mounts it, so the server is the rig itself.
	sr share add "$SHARE" --kind nfs --source "127.0.0.1:/" --options "nfsvers=4,port=2049" >/dev/null
	mktile qa-share --volumes "share:$SHARE/sub:/data"
	deploy qa-share
	poll 60 jobstate qa-share done || true
	local msg got
	msg="blitz-$(date +%s)"
	sr tile exec qa-share $T -- sh -c "echo $msg > /data/f.txt" </dev/null || true
	got="$(rig "docker exec qa-nfs cat /data/sub/f.txt" 2>/dev/null || true)"
	expect share-nfs "server /data/sub/f.txt = '$got' (wrote '$msg')" test "$got" = "$msg"
	# A managed tile on a share is refused (databases corrupt on network shares).
	local refused
	refused="$(sr managed create qa-db $T --engine postgres --volumes "share:$SHARE/db:/var/lib/postgresql/data" 2>&1 || true)"
	expect share-managed-refused "$(echo "$refused" | head -n1)" grep -qiE "share|volumes" <<<"$refused"
}

# ---- 5. files: from the stack's config repo, one plain, one :template
files_check() {
	if [[ -z "$CONFIG_REPO" || -z "$CONFIG_CONNECTOR" ]]; then
		echo "SKIP files: set BLITZ_CONFIG_REPO and BLITZ_CONFIG_CONNECTOR."
		echo "  Manual step: a repo with stackr-compose.yml declaring tile qa-files (image alpine:3,"
		echo "  command sleep 3600, files: 'conf/app.txt:/etc/app.txt' and 'conf/t.txt:/etc/t.txt:template',"
		echo "  conf/t.txt containing \${{ params.qa.greeting }}), plus the param qa.greeting=hello."
		echo "  The repo must not be the one a protected stack uses."
		return 0
	fi
	sr stack config-repo --repo "$CONFIG_REPO" --connector "$CONFIG_CONNECTOR" --stack "$QA" >/dev/null
	sr params set qa.greeting=hello --stack "$QA" >/dev/null
	# Binding makes no release: a push does (the connector's webhook). Touch
	# conf/app.txt through the GitHub API, then wait for a new release and
	# promote the newest (a re-run of the script is not release 1).
	local sha before rel
	before="$(sr release ls --stack "$QA" 2>/dev/null | grep -c . || true)"
	sha="$(gh api "repos/$CONFIG_REPO/contents/conf/app.txt" --jq .sha)"
	gh api -X PUT "repos/$CONFIG_REPO/contents/conf/app.txt" -f message=qa-blitz -f sha="$sha" \
		-f content="$(printf 'plain-file\n' | base64 -w0)" >/dev/null
	poll 90 test "$(sr release ls --stack "$QA" 2>/dev/null | grep -c . || true)" -gt "$before" || true
	rel="$(sr release ls --stack "$QA" 2>/dev/null | head -n1 | awk '{print $1}')"
	local prom
	prom="$(sr promote "$rel" $T -y 2>&1)" || echo "  promote $rel: $(tail -n 3 <<<"$prom" | tr '\n' '|')"
	local plain tmpl
	plain="$(sr tile exec qa-files $T -- cat /etc/app.txt </dev/null 2>&1 || true)"
	tmpl="$(sr tile exec qa-files $T -- cat /etc/t.txt </dev/null 2>&1 || true)"
	expect files-plain "cat /etc/app.txt -> $(echo "$plain" | head -n1)" test "$plain" = plain-file
	expect files-template "cat /etc/t.txt -> $(echo "$tmpl" | head -n1)" test "$tmpl" = hello
}

# ---- 6. forward auth: the app sees the Remote-User the provider set
fwdauth_check() {
	rig "docker rm -f qa-auth >/dev/null 2>&1; mkdir -p /tmp/qa-blitz; printf '%s\n' \
		'{ admin off' ' auto_https off' '}' ':$AUTH_PORT {' ' header Remote-User qa-user' ' respond 200' '}' > /tmp/qa-blitz/Auth; \
		docker run -d --name qa-auth --network host -v /tmp/qa-blitz:/c caddy:2 \
		caddy run --config /c/Auth --adapter caddyfile >/dev/null"
	sleep 2
	sr tile create qa-fa $T --image traefik/whoami:latest --port 80 >/dev/null
	# `tile domain add` has no forward-auth flag (the tile drawer and the stack
	# file have one): the domain API takes the extras in `proxy`.
	local key kid api ip kout
	kout="$(sr key add qa-blitz 2>&1)"
	key="$(sed -n 's/.*token[^:]*: //p' <<<"$kout" | head -n1)"
	kid="$(sed -n 's/^key id: //p' <<<"$kout" | head -n1)"
	api="$SERVER/api/v1/orgs/$ORG/stacks/$QA/envs/$ENV/tiles/qa-fa"
	curl -sf -H "Authorization: Bearer $key" -H 'Content-Type: application/json' \
		-d "{\"host\":\"$HOST_FA\",\"proxy\":{\"forward_auth\":{\"url\":\"http://stackr:$AUTH_PORT\"}}}" \
		"$api/domains" >/dev/null || bad fwdauth-attach "domain attach with forward_auth failed"
	deploy qa-fa
	poll 60 jobstate qa-fa done || true
	ip="$(getent hosts "$ADDR" | awk '{print $1; exit}')"
	local body
	poll 30 curl -ksf -o /dev/null --resolve "$HOST_FA:443:$ip" "https://$HOST_FA/" || true
	body="$(curl -ks --resolve "$HOST_FA:443:$ip" "https://$HOST_FA/" || true)"
	expect fwdauth "whoami sees: $(grep -i 'remote-user' <<<"$body" | head -n1)" grep -qi 'remote-user: qa-user' <<<"$body"
	rig "docker rm -f qa-auth >/dev/null 2>&1"
	sleep 1
	local denied
	denied="$(curl -ks -o /dev/null -w '%{http_code}' --resolve "$HOST_FA:443:$ip" "https://$HOST_FA/")"
	expect fwdauth-down "provider gone: GET / -> $denied" test "$denied" != 200
	[[ -z "$kid" ]] || sr key rm "$kid" -y >/dev/null 2>&1 || true
}

# ---- 7. host grant: parks, approve, deploys, a changed line parks again
hostgrant_check() {
	mktile qa-hg --volumes "host:/var/run/docker.sock:/sock:ro"
	deploy qa-hg
	# The queue may be busy (two workers): give the park time to come.
	poll 90 jobstate qa-hg waiting || true
	local log
	log="$(sr job log "$(sr tile status qa-hg $T 2>&1 | grep -i last_job | awk '{print $NF}')" 2>&1 || true)"
	expect hostgrant-parks "$(grep -i -m1 'host access' <<<"$log" || status qa-hg)" grep -qi "waiting: host access" <<<"$log"
	sr host-grant approve --stack "$QA" -y >/dev/null
	poll 90 jobstate qa-hg done || true
	expect hostgrant-deploys "$(status qa-hg)" jobstate qa-hg done
	sr tile set qa-hg $T --volumes "host:/etc:/hostetc:ro" >/dev/null 2>&1 || true
	poll 30 jobstate qa-hg waiting || true
	expect hostgrant-reparks "$(status qa-hg)" jobstate qa-hg waiting
	sr host-grant show --stack "$QA" 2>&1 | sed 's/^/  grant: /'
}

login_check # always: the other checks need the qa stack
run route && route_check
run http && http_check
run share && share_check
run files && files_check
run fwdauth && fwdauth_check
run hostgrant && hostgrant_check
[[ $fail -eq 0 ]]

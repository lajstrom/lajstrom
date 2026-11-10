#!/usr/bin/env bash
# The end-to-end harness (#34, docs/testing.md): builds bin/lajstrom, serves it on a free port of
# 127.0.0.1 with a configuration of its own, runs the scenarios of test/e2e/ against it and
# prints a summary line per scenario. make e2e runs it.
#
#   test/e2e/run.sh [<scenario>...]     e.g. test/e2e/run.sh raw oras; make e2e E2E='raw oras'
#
# Without arguments it runs every test/e2e/<name>.sh but lib.sh and run.sh: those of $order
# below in that order, then any other by name, then ha.sh. A scenario gets the base URL as its
# only argument, so its defaults name the repos, and the environment:
#
#   LAJSTROM_CONFIG        the configuration the server runs with (lajstrom admin uses it too)
#   LAJSTROM_BIN           bin/lajstrom, absolute
#   LAJSTROM_MASTER_KEY    the server's master key: apt repos are signed; lajstrom admin key needs it
#   LAJSTROM_SERVER_LOG    the server's log file
#   LAJSTROM_E2E_REGISTRY  host:port of the registry `local`
#   LAJSTROM_E2E_USER      a user with the admin binding, made for the run with a random password,
#   LAJSTROM_E2E_PASSWORD  for the scenarios that need no user of their own (lib.sh, e2e_credentials)
#   LAJSTROM_E2E_FORWARDED 1: the server trusts the X-Forwarded-For of 127.0.0.1
#                          (server.trustedProxies), so that the per-address throttles are on
#   LAJSTROM_E2E_REGISTRY_USER, LAJSTROM_E2E_REGISTRY_PASSWORD_FILE, LAJSTROM_E2E_REGISTRY_PREFIX
#                          as the caller set them, and LAJSTROM_E2E_REGISTRY_PASSWORD (below)
#
# The registry `local` may need credentials and an instance prefix (#353), as Gitea's does
# (deploy/compose/gitea): LAJSTROM_E2E_REGISTRY_USER names the user, and
# LAJSTROM_E2E_REGISTRY_PASSWORD_FILE a file that holds its password or token, which run.sh
# reads into LAJSTROM_E2E_REGISTRY_PASSWORD, the passwordEnv of registries[].auth, for the
# server and lajstrom admin; LAJSTROM_E2E_REGISTRY_PREFIX is registries[].prefix, ending in /.
#
# Everything lives in a temporary directory: the configuration, the SQLite index, the server's
# log and the output of each scenario. The configuration has one registry, `local`, at
# $LAJSTROM_E2E_REGISTRY (default localhost:5000, the Zot of make dev-up), with the capabilities
# of $LAJSTROM_E2E_CAPABILITIES when it is set, a YAML flow mapping such as '{catalog: false}'
# for a registry without a catalog (#37), and the repos
# raw/scratch, apt/ubuntu, apt/e2e and pypi/e2e on it, none readable anonymously; the upstream
# npmjs at $LAJSTROM_E2E_NPM_UPSTREAM (default https://registry.npmjs.org/), which npm.sh makes
# a proxy repo of; and the user
# $LAJSTROM_E2E_USER with the admin binding; auth.anonymousRead is true, so that a repo's own
# anonymousRead decides (rbac.sh turns it on for a repo of its own); no metrics listener, no
# reconcile loop. The LAJSTROM_SERVER_*, LAJSTROM_LOG_*
# and LAJSTROM_DATABASE_* overrides of the caller's environment are unset, so that the server and
# lajstrom admin both read the file. The port is one of free_port (lib.sh), below the ephemeral
# range. The server must answer /-/readyz within 30 s; when it does not, or exits before, run.sh
# prints the last 50 lines of its log, whether it runs or its exit status, and what /-/readyz
# answers, its status and body, or why it does not answer. A scenario that fails is reported
# with its FAIL lines (the assertion or the failing command, from lib.sh) and the last 50 lines
# of the server log; the others still run. On exit the server is stopped and the directory
# removed, unless LAJSTROM_E2E_KEEP=1. The exit status is 1 when a scenario failed, 2 when the
# harness could not start.
#
# ha.sh is the exception: it runs against the ha profile of the dev stack (two replicas behind
# nginx, make dev-up PROFILES=ha), at http://localhost:$LAJSTROM_HA_PORT (8090), not against
# the server of this run, and only when docker compose shows that profile's ha-nginx running;
# otherwise the summary has a SKIP line for it, which is not a failure.
set -euo pipefail
. "$(dirname "$0")/lib.sh"

order="raw oras auth rbac oci-token cosign tokens apt pypi maven npm" # the quick ones first; pypi and npm install their clients, maven builds with mvn and gradle
here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/../.." && pwd)"
registry="${LAJSTROM_E2E_REGISTRY:-localhost:5000}"
registry_user="${LAJSTROM_E2E_REGISTRY_USER:-}"
registry_prefix="${LAJSTROM_E2E_REGISTRY_PREFIX:-}"
if [ -n "$registry_user" ]; then
	secret="${LAJSTROM_E2E_REGISTRY_PASSWORD_FILE:-}"
	[ -n "$secret" ] && [ -s "$secret" ] ||
		{ echo "run.sh: LAJSTROM_E2E_REGISTRY_USER needs LAJSTROM_E2E_REGISTRY_PASSWORD_FILE, a file with the password" >&2; exit 2; }
	LAJSTROM_E2E_REGISTRY_PASSWORD_FILE="$(cd "$(dirname "$secret")" && pwd)/$(basename "$secret")"
	LAJSTROM_E2E_REGISTRY_PASSWORD="$(tr -d '\r\n' <"$secret")"
	export LAJSTROM_E2E_REGISTRY_USER LAJSTROM_E2E_REGISTRY_PASSWORD_FILE LAJSTROM_E2E_REGISTRY_PASSWORD
fi
[ -z "$registry_prefix" ] || export LAJSTROM_E2E_REGISTRY_PREFIX
for tool in curl jq make go; do
	command -v "$tool" >/dev/null || { echo "run.sh: $tool is required" >&2; exit 2; }
done

# The scenarios: the arguments, or every script of the directory.
names=()
if [ $# -gt 0 ]; then
	for n in "$@"; do
		n="${n%.sh}"
		case "$n" in lib | run) echo "run.sh: $n.sh is not a scenario" >&2; exit 2 ;; esac
		[ -f "$here/$n.sh" ] || { echo "run.sh: no scenario test/e2e/$n.sh" >&2; exit 2; }
		names+=("$n")
	done
else
	for n in $order; do
		[ ! -f "$here/$n.sh" ] || names+=("$n")
	done
	for f in "$here"/*.sh; do
		n="$(basename "$f" .sh)"
		case " lib run ha $order " in *" $n "*) continue ;; esac
		names+=("$n")
	done
	[ ! -f "$here/ha.sh" ] || names+=(ha)
fi

# ha_up: the ha profile of the dev stack runs (its edge, ha-nginx).
ha_up() {
	command -v docker >/dev/null &&
		[ -n "$(docker compose -f "$root/deploy/compose/compose.yaml" ps -q --status running ha-nginx 2>/dev/null)" ]
}

tmp="${TMPDIR:-/tmp}"
tmp="$(mktemp -d "${tmp%/}/lajstrom-e2e.XXXXXX")"
pid=""
finish() {
	if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
		kill -TERM "$pid" 2>/dev/null || true
		wait "$pid" 2>/dev/null || true
	fi
	if [ "${LAJSTROM_E2E_KEEP:-}" = 1 ]; then
		echo "run.sh: kept $tmp (the configuration, the index, server.log, a log per scenario)"
	else
		rm -rf "$tmp"
	fi
}
trap finish EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# not_ready says why the server is not ready (#372): the end of its log, whether the process
# runs or how it exited, and what GET /-/readyz answers, or why nothing answers.
not_ready() {
	local rc=0 code why="was not ready within 30 s"
	echo "--- the last 50 lines of the server log:"
	tail -n 50 "$LAJSTROM_SERVER_LOG"
	if kill -0 "$pid" 2>/dev/null; then
		echo "--- lajstrom serve: running, pid $pid"
	else
		wait "$pid" || rc=$?
		pid=""
		why="exited with status $rc before it was ready"
		echo "--- lajstrom serve: exited with status $rc"
	fi
	code="$(curl -sS -o "$tmp/readyz" -w '%{http_code}' --max-time 2 "$base/-/readyz" 2>"$tmp/readyz.err")" || code=""
	if [ -n "$code" ]; then
		echo "--- GET /-/readyz: $code $(head -c 300 "$tmp/readyz")"
	else
		echo "--- GET /-/readyz: no answer: $(cat "$tmp/readyz.err")"
	fi
	echo "run.sh: lajstrom serve on $base $why (registry $registry)"
}

start=$SECONDS
(cd "$root" && make --no-print-directory build) >"$tmp/build.log" 2>&1 || { cat "$tmp/build.log" >&2; echo "run.sh: make build failed" >&2; exit 2; }
bin="$root/bin/lajstrom"
step "make build: $("$bin" version | head -1)"

port="$(free_port)" || { echo "run.sh: no free port found on 127.0.0.1" >&2; exit 2; }
base="http://127.0.0.1:$port"
unset LAJSTROM_SERVER_LISTEN LAJSTROM_SERVER_EXTERNAL_URL LAJSTROM_SERVER_METRICS_LISTEN \
	LAJSTROM_LOG_LEVEL LAJSTROM_LOG_FORMAT LAJSTROM_DATABASE_DRIVER LAJSTROM_DATABASE_DSN LAJSTROM_ADMIN_PASSWORD
LAJSTROM_MASTER_KEY="$(od -An -tx1 -N32 /dev/urandom | tr -d ' \n')"
LAJSTROM_CONFIG="$tmp/lajstrom.yaml"
LAJSTROM_BIN="$bin"
LAJSTROM_SERVER_LOG="$tmp/server.log"
LAJSTROM_E2E_REGISTRY="$registry"
LAJSTROM_E2E_FORWARDED=1
export LAJSTROM_MASTER_KEY LAJSTROM_CONFIG LAJSTROM_BIN LAJSTROM_SERVER_LOG LAJSTROM_E2E_REGISTRY LAJSTROM_E2E_FORWARDED
cat >"$LAJSTROM_CONFIG" <<EOF
# Written by test/e2e/run.sh for one run of the end-to-end scenarios.
server:
  listen: "127.0.0.1:$port"
  externalURL: "$base"
  insecureHTTP: true
  shutdownTimeout: 5s
  metricsListen: ""
  trustedProxies: ["127.0.0.1/32"]
log:
  level: info
  format: text
database:
  driver: sqlite
  dsn: "file:$tmp/lajstrom.db"
registries:
  - name: local
    type: generic
    url: "http://$registry"
${registry_prefix:+    prefix: $registry_prefix}
${registry_user:+    auth:
      username: $registry_user
      passwordEnv: LAJSTROM_E2E_REGISTRY_PASSWORD}
${LAJSTROM_E2E_CAPABILITIES:+    capabilities: $LAJSTROM_E2E_CAPABILITIES}
upstreams:
  - name: npmjs
    format: npm
    url: "${LAJSTROM_E2E_NPM_UPSTREAM:-https://registry.npmjs.org/}"
reconcile:
  interval: 0s
keys:
  masterKeyEnv: LAJSTROM_MASTER_KEY
auth:
  anonymousRead: true
repos:
  - {name: scratch, format: raw, registry: local}
  - {name: ubuntu, format: apt, registry: local}
  - {name: e2e, format: apt, registry: local}
  - {name: e2e, format: pypi, registry: local}
EOF

"$bin" migrate --config "$LAJSTROM_CONFIG" >"$tmp/migrate.log" 2>&1 || { cat "$tmp/migrate.log" >&2; echo "run.sh: lajstrom migrate failed" >&2; exit 2; }
LAJSTROM_E2E_USER="e2e-admin"
LAJSTROM_E2E_PASSWORD="$(head -c 24 /dev/urandom | base64 | tr -d '+/=\n')"
export LAJSTROM_E2E_USER LAJSTROM_E2E_PASSWORD
printf '%s' "$LAJSTROM_E2E_PASSWORD" | "$bin" admin user create --name "$LAJSTROM_E2E_USER" --admin --password-stdin \
	--config "$LAJSTROM_CONFIG" >"$tmp/admin.log" 2>&1 || { cat "$tmp/admin.log" >&2; echo "run.sh: lajstrom admin user create failed" >&2; exit 2; }
"$bin" serve --config "$LAJSTROM_CONFIG" >"$LAJSTROM_SERVER_LOG" 2>&1 </dev/null &
pid=$!
served=$SECONDS
deadline=$((SECONDS + 30))
until curl -fsS -o /dev/null --max-time 2 "$base/-/readyz" 2>/dev/null; do
	if ! kill -0 "$pid" 2>/dev/null || [ "$SECONDS" -ge "$deadline" ]; then
		not_ready >&2
		exit 2
	fi
	sleep 0.5
done
step "lajstrom serve on $base, index $tmp/lajstrom.db, registry local at $registry${registry_prefix:+, prefix $registry_prefix}${registry_user:+, user $registry_user}${LAJSTROM_E2E_CAPABILITIES:+ $LAJSTROM_E2E_CAPABILITIES}: ready after $((SECONDS - served)) s"

results=()
failed=0
skipped=0
for n in "${names[@]}"; do
	log="$tmp/$n.log"
	echo "=== $n.sh"
	target="$base"
	if [ "$n" = ha ]; then
		if ! ha_up; then
			echo "run.sh: the ha profile is not up (make dev-up PROFILES=ha): ha.sh skipped"
			results+=("$(printf 'SKIP %-14s         (the ha profile is not up)' "$n.sh")")
			skipped=$((skipped + 1))
			continue
		fi
		target="http://localhost:${LAJSTROM_HA_PORT:-8090}"
	fi
	t0=$SECONDS
	if (cd "$root" && "$here/$n.sh" "$target") </dev/null 2>&1 | tee "$log"; then rc=0; else rc=$?; fi
	took=$((SECONDS - t0))
	if [ "$rc" = 0 ]; then
		results+=("$(printf 'PASS %-14s %4d s' "$n.sh" "$took")")
		continue
	fi
	failed=$((failed + 1))
	results+=("$(printf 'FAIL %-14s %4d s  (exit %d)' "$n.sh" "$took" "$rc")")
	{
		echo "--- $n.sh failed with exit $rc after $took s:"
		grep -E '^(FAIL |  at |  in )' "$log" || echo "(no FAIL line: the last lines of its output are above)"
		if [ "$n" = ha ]; then
			echo "--- the logs of the ha profile: docker compose -f deploy/compose/compose.yaml logs lajstrom-a lajstrom-b ha-nginx"
		else
			echo "--- the last 50 lines of the server log:"
			tail -n 50 "$LAJSTROM_SERVER_LOG"
		fi
		echo "---"
	} >&2
	if ! kill -0 "$pid" 2>/dev/null; then
		echo "run.sh: lajstrom serve exited during $n.sh; the scenarios after it do not run" >&2
		break
	fi
done

echo "=== summary"
printf '%s\n' "${results[@]}"
echo "e2e: $((${#results[@]} - skipped)) of ${#names[@]} scenarios ran, $failed failed, $skipped skipped, in $((SECONDS - start)) s against $base"
[ "$failed" = 0 ] || exit 1

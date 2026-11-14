#!/usr/bin/env bash
# End-to-end test of the bootstrap of the first admin (#440; docs/auth.md, Bootstrap), with
# servers of its own on new indexes: the server of run.sh has its admin already, and the
# bootstrap is closed there.
#
#   test/e2e/first-admin.sh [<base url>]     the base URL is not used
#
# On a new SQLite index, one serve:
#   FA-01 without auth.bootstrap.tokenFile: a warning, without a secret, says how to make the
#         first admin; there is no token, and a made-up one is 403;
#   FA-02 with LAJSTROM_BOOTSTRAP_TOKEN_FILE, a token made here: the index files hold its sha256,
#         never the token, and no log line has it;
#   FA-03 a wrong token is 403; the 11th wrong one from a client address is 429 with
#         Retry-After, the right token too;
#   FA-04 lajstrom admin bootstrap with the token makes the admin, who may read the audit log:
#         user.create with the source bootstrap;
#   FA-05 a second exchange is 404, the command says that an admin exists;
#   FA-06 a restart with the file still set warns to remove it; no log line of any start has
#         the token.
# With LAJSTROM_E2E_POSTGRES_DSN, a Postgres database where the scenario may create a schema of
# its own (the dev stack's: postgres://lajstrom:lajstrom@localhost:5432/lajstrom?sslmode=disable),
# and psql, two replicas with one token file on one new schema:
#   FA-07 started together, they store one token, made by one of them;
#   FA-08 lajstrom admin bootstrap on both at once makes one admin; the other is refused.
# Without them FA-07 and FA-08 are skipped, which is not a failure. The servers use the registry
# $LAJSTROM_E2E_REGISTRY (default localhost:5000), below LAJSTROM_E2E_REGISTRY_PREFIX and with
# the user LAJSTROM_E2E_REGISTRY_USER and the password LAJSTROM_E2E_REGISTRY_PASSWORD when run.sh
# set them (#353), and create nothing in it. The binary is
# $LAJSTROM_BIN, else bin/lajstrom. It needs curl and jq; the token goes to the server in a file,
# never on a command line, and is never printed.
set -euo pipefail
. "$(dirname "$0")/lib.sh"

for tool in curl jq; do
	command -v "$tool" >/dev/null || { echo "first-admin.sh: $tool is required" >&2; exit 2; }
done
bin="${LAJSTROM_BIN:-bin/lajstrom}"
[ -x "$bin" ] || { echo "first-admin.sh: $bin is not built (make build)" >&2; exit 2; }
registry="${LAJSTROM_E2E_REGISTRY:-localhost:5000}"
unset LAJSTROM_CONFIG LAJSTROM_SERVER_LISTEN LAJSTROM_SERVER_EXTERNAL_URL LAJSTROM_SERVER_METRICS_LISTEN \
	LAJSTROM_LOG_LEVEL LAJSTROM_LOG_FORMAT LAJSTROM_DATABASE_DRIVER LAJSTROM_DATABASE_DSN LAJSTROM_BOOTSTRAP_TOKEN_FILE

work="$(mktemp -d)"
pids=()
schema=""
cleanup() {
	for p in ${pids[@]+"${pids[@]}"}; do
		kill -TERM "$p" 2>/dev/null || true
		wait "$p" 2>/dev/null || true
	done
	[ -z "$schema" ] || psql -q "$LAJSTROM_E2E_POSTGRES_DSN" -c "DROP SCHEMA $schema CASCADE" >/dev/null 2>&1 || true
	rm -rf "$work"
}
trap cleanup EXIT

# config NAME PORT DRIVER DSN writes the configuration of a server to $work/NAME.yaml.
config() {
	cat >"$work/$1.yaml" <<EOF
server:
  listen: "127.0.0.1:$2"
  externalURL: "http://127.0.0.1:$2"
  insecureHTTP: true
  shutdownTimeout: 5s
  shutdownDelay: 0s # no edge in front to stop sending
  metricsListen: ""
  trustedProxies: ["127.0.0.1/32"]
log:
  level: info
  format: json
database:
  driver: $3
  dsn: "$4"
registries:
  - name: local
    url: "http://$registry"
${LAJSTROM_E2E_REGISTRY_PREFIX:+    prefix: $LAJSTROM_E2E_REGISTRY_PREFIX}
${LAJSTROM_E2E_REGISTRY_USER:+    auth:
      username: $LAJSTROM_E2E_REGISTRY_USER
      passwordEnv: LAJSTROM_E2E_REGISTRY_PASSWORD}
reconcile:
  interval: 0s
EOF
}

# start NAME LOG [VAR=VALUE...] starts serve with $work/NAME.yaml and the variables given,
# logging to $work/LOG.log.
start() {
	local name="$1" log="$2"
	shift 2
	env "$@" "$bin" serve --config "$work/$name.yaml" >"$work/$log.log" 2>&1 </dev/null &
	pids+=($!)
}

# ready LOG PORT waits until the server on PORT, logging to $work/LOG.log, is ready.
ready() {
	local i
	for i in $(seq 60); do
		curl -fsS -o /dev/null --max-time 1 "http://127.0.0.1:$2/-/readyz" 2>/dev/null && return 0
		sleep 0.5
	done
	tail -n 20 "$work/$1.log" >&2
	fail "serve logging to $1.log was not ready within 30 s"
}

# stop stops the server started last.
stop() {
	local p="${pids[${#pids[@]}-1]}"
	kill -TERM "$p"
	wait "$p" 2>/dev/null || true
	unset 'pids[${#pids[@]}-1]'
}

# messages LOG LEVEL prints the messages of LEVEL of the JSON log $work/LOG.log.
messages() { jq -Rr --arg l "$2" 'fromjson? | select(.level == $l) | .msg' "$work/$1.log"; }

# sha256 FILE prints the hexadecimal sha256 of the file.
sha256() { if command -v sha256sum >/dev/null; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -d' ' -f1; }

# exchange PORT FILE [CURL ARGS...] POSTs the token of FILE with the name root to the server
# and wants the status $want.
exchange() {
	local port="$1" file="$2"
	shift 2
	jq -n --rawfile t "$file" '{token: ($t | rtrimstr("\n")), name: "root", password: "root-password"}' >"$work/body.json"
	assert_status "$want" -X POST -H 'Content-Type: application/json' --data-binary "@$work/body.json" "$@" \
		"http://127.0.0.1:$port/api/v1/bootstrap"
}

# newtoken FILE writes a new token to FILE, as an operator does with openssl rand -hex 32.
newtoken() { (umask 077 && od -An -tx1 -N32 /dev/urandom | tr -d ' \n' >"$1"); }

# FA-01: no file, no token.
port="$(free_port)"
config one "$port" sqlite "file:$work/one.db"
start one one-1
ready one-1 "$port"
warning="$(messages one-1 WARN | grep 'no bootstrap token' || true)"
[ -n "$warning" ] || fail "FA-01: no warning that there is no bootstrap token"
case "$warning" in *"auth.bootstrap.tokenFile"*"lajstrom admin user create --admin"*) ;; *) fail "FA-01: the warning does not say how: $warning" ;; esac
newtoken "$work/made-up"
want=403 exchange "$port" "$work/made-up"
step "FA-01: serve without auth.bootstrap.tokenFile: WARN \"${warning:0:60}…\"; a made-up token: 403"
stop

# FA-02: the token of the file, its hash in the index only.
newtoken "$work/token"
start one one-2 LAJSTROM_BOOTSTRAP_TOKEN_FILE="$work/token"
ready one-2 "$port"
messages one-2 INFO | grep -q 'the bootstrap token of auth.bootstrap.tokenFile is valid' || fail "FA-02: no line that the token is valid"
hash="$(sha256 "$work/token")"
held="$(cat "$work"/one.db* | LC_ALL=C grep -a -c -F -f "$work/token" || true)"
assert_eq 0 "$held" "FA-02: index files that hold the token"
cat "$work"/one.db* | LC_ALL=C grep -a -q -F "$hash" || fail "FA-02: the index files do not hold the sha256 of the token"
step "FA-02: serve with LAJSTROM_BOOTSTRAP_TOKEN_FILE: the token is valid, the index holds its sha256 ${hash:0:12}…, not the token"

# FA-03: a wrong token, then the throttle of an address.
want=403 exchange "$port" "$work/made-up"
expect "FA-03: the code of a wrong token" FORBIDDEN "$(jq -r .error.code <<<"$E2E_BODY")"
for _ in $(seq 10); do want=403 exchange "$port" "$work/made-up" -H 'X-Forwarded-For: 203.0.113.40'; done
headers="$work/throttled.headers"
want=429 exchange "$port" "$work/token" -H 'X-Forwarded-For: 203.0.113.40' -D "$headers"
retry="$(tr -d '\r' <"$headers" | awk -F': ' 'tolower($1) == "retry-after" {print $2}')"
[ -n "$retry" ] || fail "FA-03: 429 without Retry-After"
step "FA-03: a wrong token: 403 FORBIDDEN; the 11th wrong token of 203.0.113.40: 429, the right token too, Retry-After $retry"

# FA-04: the admin, with the command, from another address.
out="$(printf 'root-password' | "$bin" admin bootstrap --server "http://127.0.0.1:$port" --name root --token-file "$work/token" --password-stdin)"
expect "FA-04: the admin's name" root "$(jq -r .name <<<"$out")"
(umask 077 && printf 'user = "root:root-password"\n' >"$work/root.curlrc")
assert_status 200 -K "$work/root.curlrc" "http://127.0.0.1:$port/api/v1/audit?action=user.create"
expect "FA-04: the audit event" "anonymous bootstrap true root" \
	"$(jq -r '.events[0] | "\(.principal) \(.details.source) \(.details.admin) \(.details.name)"' <<<"$E2E_BODY")"
step "FA-04: lajstrom admin bootstrap --name root --token-file …: $(jq -c '{name, kind}' <<<"$out"); audit user.create by anonymous, source bootstrap"

# FA-05: closed.
if out="$(printf 'root-password' | "$bin" admin bootstrap --server "http://127.0.0.1:$port" --name root2 --token-file "$work/token" --password-stdin 2>&1)"; then
	fail "FA-05: a second lajstrom admin bootstrap succeeded: $out"
fi
case "$out" in *"404 an admin exists already"*) ;; *) fail "FA-05: a second lajstrom admin bootstrap: $out" ;; esac
want=404 exchange "$port" "$work/token"
expect "FA-05: the code once an admin exists" NOT_FOUND "$(jq -r .error.code <<<"$E2E_BODY")"
step "FA-05: a second exchange: 404 NOT_FOUND; the command: ${out#lajstrom: }"

# FA-06: a restart with the file, and no token in any log.
stop
start one one-3 LAJSTROM_BOOTSTRAP_TOKEN_FILE="$work/token"
ready one-3 "$port"
messages one-3 WARN | grep -q "the bootstrap token is set but no longer valid" || fail "FA-06: no warning that the token of the file is no longer valid"
held="$(cat "$work"/one-*.log | LC_ALL=C grep -c -F -f "$work/token" || true)"
assert_eq 0 "$held" "FA-06: log lines with the token, in the three starts"
step "FA-06: a restart with LAJSTROM_BOOTSTRAP_TOKEN_FILE: a warning to remove it; no log line of the three starts has the token"
stop

# FA-07 and FA-08: two replicas on one new Postgres schema.
if [ -z "${LAJSTROM_E2E_POSTGRES_DSN:-}" ] || ! command -v psql >/dev/null; then
	step "FA-07, FA-08: skipped: they need LAJSTROM_E2E_POSTGRES_DSN and psql"
	echo "PASS first-admin e2e (FA-01 to FA-06)"
	exit 0
fi
schema="e2e_bootstrap_$(od -An -tx1 -N6 /dev/urandom | tr -d ' \n')"
psql -q "$LAJSTROM_E2E_POSTGRES_DSN" -c "CREATE SCHEMA $schema" >/dev/null
case "$LAJSTROM_E2E_POSTGRES_DSN" in *\?*) dsn="$LAJSTROM_E2E_POSTGRES_DSN&search_path=$schema" ;; *) dsn="$LAJSTROM_E2E_POSTGRES_DSN?search_path=$schema" ;; esac
pa="$(free_port)"
pb="$(free_port)"
config a "$pa" postgres "$dsn"
config b "$pb" postgres "$dsn"
newtoken "$work/token"
start a a LAJSTROM_BOOTSTRAP_TOKEN_FILE="$work/token"
start b b LAJSTROM_BOOTSTRAP_TOKEN_FILE="$work/token"
ready a "$pa"
ready b "$pb"
made="$(cat "$work/a.log" "$work/b.log" | jq -Rr 'fromjson? | .madeBy // empty' | sort -u)"
[ "$(printf '%s\n' "$made" | grep -c .)" = 1 ] || fail "FA-07: the replicas name the makers '$made', want one"
step "FA-07: two replicas started together on schema $schema with one token file: one token, made by $made"

printf 'root-password' | "$bin" admin bootstrap --server "http://127.0.0.1:$pa" --name root-a --token-file "$work/token" --password-stdin >"$work/a.out" 2>&1 &
ea=$!
printf 'root-password' | "$bin" admin bootstrap --server "http://127.0.0.1:$pb" --name root-b --token-file "$work/token" --password-stdin >"$work/b.out" 2>&1 &
eb=$!
ra=0 rb=0
wait "$ea" || ra=$?
wait "$eb" || rb=$?
admin="$(cat "$work/a.out" "$work/b.out" | jq -Rr 'fromjson? | .name // empty')"
[ "$(printf '%s\n' "$admin" | grep -c .)" = 1 ] || fail "FA-08: admins made: '$admin' (a: $(cat "$work/a.out"); b: $(cat "$work/b.out"))"
[ $((ra + rb)) = 1 ] || fail "FA-08: exit statuses $ra and $rb, want one 0 and one 1"
refused="$(cat "$work/a.out" "$work/b.out" | grep '^lajstrom: ' | sed 's/^lajstrom: bootstrap: POST [^ ]* //')"
(umask 077 && printf 'user = "%s:root-password"\n' "$admin" >"$work/admin.curlrc")
assert_status 200 -K "$work/admin.curlrc" "http://127.0.0.1:$pb/api/v1/users"
expect "FA-08: the users" "$admin" "$(jq -r '[.users[].name] | join(" ")' <<<"$E2E_BODY")"
step "FA-08: lajstrom admin bootstrap on both replicas at once: one admin, $admin; the other: $refused"
echo "PASS first-admin e2e (FA-01 to FA-08)"

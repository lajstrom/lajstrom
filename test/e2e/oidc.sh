#!/usr/bin/env bash
# End-to-end test of the OIDC login (#69; docs/spec/rbac.md §8) against a real IdP, Dex, with a
# server of its own: the server of run.sh has no auth.oidc.
#
#   test/e2e/oidc.sh [<base url>]     the base URL is not used
#
#   OI-01 Dex (ghcr.io/dexidp/dex:v2.45.1, by digest) in a container with a static user, alice,
#         in the groups platform and dev; serve with auth.oidc naming it, on a new SQLite index;
#         the repo raw/oidc and the binding of group:platform as writer on it, before anyone
#         logs in (lajstrom admin);
#   OI-02 the browser's login, with curl as the browser: /-/oidc/login, Dex's login form, the
#         callback's page with a personal access token, shown once;
#   OI-03 the token: whoami is alice of kind oidc with the groups dev and platform; a raw PUT and
#         GET through the group's binding; /v2/token with it as Basic grants pull and push;
#   OI-04 the audit log has user.create, user.login and token.create with createdBy oidc; no
#         log line has the token;
#   OI-05 alice leaves platform in Dex (Dex restarted with her groups dev only); the next login
#         replaces her groups: a PUT with the new token and with the first one is 404, as for a
#         repo she cannot read, and whoami shows dev only;
#   OI-06 lajstrom login, the device flow: the CLI shows the page and the code, the browser
#         confirms it and logs in, the CLI prints a token that whoami takes;
#   OI-07 a callback without the browser's cookie, or with another browser's, is 400.
#
# Dex listens on 127.0.0.1:$OIDC_E2E_DEX_PORT and serve on $OIDC_E2E_PORT (default free ports), Dex in the container
# $OIDC_E2E_CONTAINER (default lajstrom-e2e-dex-<pid>), removed with its volumes at the end. The
# binary is $LAJSTROM_BIN, else bin/lajstrom; the registry $LAJSTROM_E2E_REGISTRY (default
# localhost:5000), where the run pushes one raw file under raw/oidc. It needs curl, jq and
# docker; without docker it prints a SKIP line and exits 0. Secrets go to curl in config files
# and to the server in a file, never on a command line, and are never printed.
set -euo pipefail
. "$(dirname "$0")/lib.sh"

dex_image="ghcr.io/dexidp/dex:v2.45.1@sha256:8499afd690c437f52301efd2b05b2455da5bd2dfc20332cd697dc9937f808462"
for tool in curl jq; do
	command -v "$tool" >/dev/null || { echo "oidc.sh: $tool is required" >&2; exit 2; }
done
if ! command -v docker >/dev/null || ! docker info >/dev/null 2>&1; then
	echo "SKIP oidc.sh: docker is not available"
	exit 0
fi
bin="${LAJSTROM_BIN:-bin/lajstrom}"
[ -x "$bin" ] || { echo "oidc.sh: $bin is not built (make build)" >&2; exit 2; }
registry="${LAJSTROM_E2E_REGISTRY:-localhost:5000}"
unset LAJSTROM_CONFIG LAJSTROM_SERVER_LISTEN LAJSTROM_SERVER_EXTERNAL_URL LAJSTROM_SERVER_METRICS_LISTEN \
	LAJSTROM_LOG_LEVEL LAJSTROM_LOG_FORMAT LAJSTROM_DATABASE_DRIVER LAJSTROM_DATABASE_DSN LAJSTROM_BOOTSTRAP_TOKEN_FILE \
	LAJSTROM_AUTH_OIDC_CLIENT_SECRET_FILE

work="$(mktemp -d)"
container="${OIDC_E2E_CONTAINER:-lajstrom-e2e-dex-$$}"
pids=()
cleanup() {
	for p in ${pids[@]+"${pids[@]}"}; do
		kill -TERM "$p" 2>/dev/null || true
		wait "$p" 2>/dev/null || true
	done
	docker rm -f -v "$container" >/dev/null 2>&1 || true
	rm -rf "$work"
}
trap cleanup EXIT

dex_port="${OIDC_E2E_DEX_PORT:-$(free_port)}"
port="${OIDC_E2E_PORT:-$(free_port)}"
[ "$port" != "$dex_port" ] || port="$(free_port)"
base="http://127.0.0.1:$port"
issuer="http://127.0.0.1:$dex_port/dex"
(umask 077 && od -An -tx1 -N24 /dev/urandom | tr -d ' \n' >"$work/client-secret")

# dex GROUPS... (re)starts Dex with alice in the groups given. Her password is "password", the
# bcrypt hash of Dex's own examples.
dex() {
	local groups
	groups="$(printf '"%s",' "$@")"
	cat >"$work/dex.yaml" <<EOF
issuer: $issuer
storage: {type: memory}
web: {http: 0.0.0.0:5556}
oauth2: {skipApprovalScreen: true}
enablePasswordDB: true
staticClients:
  - id: lajstrom
    name: Lajstrom
    secret: "$(cat "$work/client-secret")"
    redirectURIs: ["$base/-/oidc/callback"]
staticPasswords:
  - email: alice@example.com
    hash: "\$2a\$10\$2b2cU8CPhOTaGrs1HRQuAueS7JTT5ZHsHSzYiFPm1leZck7Mc8T4W"
    username: alice
    preferredUsername: alice
    userID: 08a8684b-db88-4b73-90a9-3cd1661f5466
    groups: [${groups%,}]
EOF
	chmod 644 "$work/dex.yaml"
	docker rm -f -v "$container" >/dev/null 2>&1 || true
	docker run -d --name "$container" -p "127.0.0.1:$dex_port:5556" -v "$work/dex.yaml:/etc/dex/config.yaml:ro" \
		"$dex_image" dex serve /etc/dex/config.yaml >/dev/null
	local i
	for i in $(seq 60); do
		curl -fsS -o /dev/null --max-time 1 "$issuer/.well-known/openid-configuration" 2>/dev/null && return 0
		sleep 0.5
	done
	docker logs "$container" 2>&1 | tail -n 20 >&2
	fail "Dex did not answer within 30 s"
}

# OI-01: Dex, the server, the repo and the group's binding.
dex platform dev
cat >"$work/lajstrom.yaml" <<EOF
server:
  listen: "127.0.0.1:$port"
  externalURL: "$base"
  insecureHTTP: true
  shutdownTimeout: 5s
  shutdownDelay: 0s
  metricsListen: ""
log:
  level: debug
  format: json
database:
  driver: sqlite
  dsn: "file:$work/index.db"
registries:
  - name: local
    url: "http://$registry"
reconcile:
  interval: 0s
auth:
  oidc:
    issuer: "$issuer"
    clientID: lajstrom
    clientSecretFile: "$work/client-secret"
    tokenLifetime: 1h
EOF
"$bin" migrate --config "$work/lajstrom.yaml" >/dev/null 2>&1
"$bin" admin repo create raw/oidc --registry local --config "$work/lajstrom.yaml" >/dev/null
"$bin" admin binding create --principal group:platform --role writer --mask raw/oidc --config "$work/lajstrom.yaml" >/dev/null
"$bin" serve --config "$work/lajstrom.yaml" >"$work/server.log" 2>&1 </dev/null &
pids+=($!)
for i in $(seq 60); do
	curl -fsS -o /dev/null --max-time 1 "$base/-/readyz" 2>/dev/null && break
	[ "$i" -lt 60 ] || { tail -n 20 "$work/server.log" >&2; fail "serve was not ready within 30 s"; }
	sleep 0.5
done
step "OI-01: Dex at $issuer (alice in platform, dev), serve with auth.oidc, group:platform writer on raw/oidc"

# browser JAR START_URL logs alice in, as a browser does: it follows START_URL to Dex's login
# form, posts the password, follows the redirects back to the callback, and leaves the final
# page in $work/page.html and its status in $page_status.
browser() {
	local jar="$1" start="$2" form
	form="$(curl -sS -L -c "$jar" -b "$jar" -o /dev/null -w '%{url_effective}' "$start")"
	case "$form" in
	"$issuer"/auth/local/login*) ;;
	*) fail "the login did not reach Dex's form: $form" ;;
	esac
	page_status="$(curl -sS -L -c "$jar" -b "$jar" -o "$work/page.html" -w '%{http_code}' \
		--data-urlencode login=alice@example.com --data-urlencode password=password "$form")"
}

# token_of FILE prints the personal access token of the page FILE.
token_of() { grep -oE 'ljs_pat_[A-Za-z0-9]{43}' "$1" | head -n 1; }

# as TOKEN writes a curl configuration with alice and TOKEN to $work/alice.curlrc.
as() { (umask 077 && printf 'user = "alice:%s"\n' "$1" >"$work/alice.curlrc"); }

# OI-02: the browser's login.
browser "$work/jar1" "$base/-/oidc/login"
assert_eq 200 "$page_status" "the callback's status"
token1="$(token_of "$work/page.html")"
[ -n "$token1" ] || fail "no token on the callback's page: $(head -c 400 "$work/page.html")"
step "OI-02: /-/oidc/login, Dex's form, the callback's page with a personal access token"

# OI-03: the token on every route.
as "$token1"
assert_status 200 -K "$work/alice.curlrc" "$base/api/v1/whoami"
assert_eq "alice oidc" "$(jq -r '"\(.name) \(.kind)"' <<<"$E2E_BODY")" "whoami"
assert_eq "dev,platform" "$(jq -r '.groups | sort | join(",")' <<<"$E2E_BODY")" "whoami's groups"
printf 'hello from an OIDC login\n' >"$work/hello.txt"
assert_status 201 -K "$work/alice.curlrc" -T "$work/hello.txt" "$base/raw/oidc/e2e/1.0/hello-$$.txt"
assert_status 200 -K "$work/alice.curlrc" -o "$work/back.txt" "$base/raw/oidc/e2e/1.0/hello-$$.txt"
cmp -s "$work/hello.txt" "$work/back.txt" || fail "the file read back differs"
assert_status 200 -K "$work/alice.curlrc" "$base/v2/token?service=127.0.0.1:$port&scope=repository:raw/oidc/e2e:pull,push"
jwt="$(jq -r .token <<<"$E2E_BODY")"
payload="$(cut -d. -f2 <<<"$jwt" | tr '_-' '/+')"
while [ $((${#payload} % 4)) -ne 0 ]; do payload="$payload="; done
assert_eq "pull,push" "$(base64 -d <<<"$payload" 2>/dev/null | jq -r '.access[0].actions | sort | join(",")')" "the /v2 token's actions"
step "OI-03: whoami alice (oidc) in dev and platform; raw PUT 201 and GET through group:platform; /v2/token grants pull,push"

# OI-04: the audit log and the server's log.
"$bin" admin user create --name root --admin --password-stdin --config "$work/lajstrom.yaml" <<<"root-password-$$" >/dev/null
(umask 077 && printf 'user = "root:root-password-%s"\n' "$$" >"$work/root.curlrc")
for action in user.create user.login token.create; do
	assert_status 200 -K "$work/root.curlrc" "$base/api/v1/audit?action=$action"
	[ "$(jq '[.events[] | select(.details.source == "oidc")] | length' <<<"$E2E_BODY")" -ge 1 ] ||
		fail "no $action event of the source oidc: $E2E_BODY"
done
assert_eq oidc "$(jq -r '.events[0].details.createdBy' <<<"$E2E_BODY")" "token.create's createdBy"
! grep -q "$token1" "$work/server.log" || fail "the server's log has the token"
step "OI-04: audit user.create, user.login and token.create (createdBy oidc); the log has no token"

# OI-05: alice leaves platform; the next login revokes the group's binding, for every token.
dex dev
browser "$work/jar2" "$base/-/oidc/login"
assert_eq 200 "$page_status" "the second login's status"
token2="$(token_of "$work/page.html")"
as "$token2"
assert_status 200 -K "$work/alice.curlrc" "$base/api/v1/whoami"
assert_eq "dev" "$(jq -r '.groups | join(",")' <<<"$E2E_BODY")" "whoami's groups after the second login"
assert_status 404 -K "$work/alice.curlrc" -T "$work/hello.txt" "$base/raw/oidc/e2e/1.1/hello-$$.txt"
as "$token1"
assert_status 404 -K "$work/alice.curlrc" "$base/raw/oidc/e2e/1.0/hello-$$.txt"
step "OI-05: alice out of platform in Dex: the next login leaves her dev only, and raw/oidc is 404 with either token"

# OI-06: lajstrom login, the device flow.
"$bin" login --server "$base" --output token >"$work/cli.out" 2>"$work/cli.err" &
cli=$!
pids+=("$cli")
for i in $(seq 40); do
	grep -q '/-/oidc/device?user_code=' "$work/cli.err" 2>/dev/null && break
	[ "$i" -lt 40 ] || fail "lajstrom login showed no page: $(cat "$work/cli.err")"
	sleep 0.25
done
page="$(grep -oE 'http://[^ ]+/-/oidc/device\?user_code=[A-Z-]+' "$work/cli.err" | head -n 1)"
code="${page##*user_code=}"
assert_status 200 "$page"
grep -q "value=\"${code//-/}\"" <<<"$E2E_BODY" || fail "the device page does not show the code $code"
browser "$work/jar3" "$base/-/oidc/login?user_code=$code"
assert_eq 200 "$page_status" "the device login's status"
grep -q "Return to your terminal" "$work/page.html" || fail "the device login's page: $(head -c 400 "$work/page.html")"
wait "$cli" || fail "lajstrom login failed: $(cat "$work/cli.err")"
token3="$(tr -d '\n' <"$work/cli.out")"
as "$token3"
assert_status 200 -K "$work/alice.curlrc" "$base/api/v1/whoami"
assert_eq alice "$(jq -r .name <<<"$E2E_BODY")" "whoami with the CLI's token"
step "OI-06: lajstrom login: the page and the code $code, the browser's login, a token whoami takes"

# OI-07: a state used twice, and a callback without the browser's cookie.
start="$(curl -sS -c "$work/jar4" -o /dev/null -w '%{redirect_url}' "$base/-/oidc/login")"
state="$(sed -E 's/.*[?&]state=([^&]+).*/\1/' <<<"$start")"
assert_status 400 "$base/-/oidc/callback?code=x&state=$state"
grep -q "another browser" <<<"$E2E_BODY" || fail "a callback without the cookie: $E2E_BODY"
assert_status 400 -b "$work/jar1" "$base/-/oidc/callback?code=x&state=$state"
step "OI-07: a callback without the browser's cookie, or with another's, is 400"

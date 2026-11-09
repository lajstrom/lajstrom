#!/usr/bin/env bash
# End-to-end test of the docker bearer flow against a running Lajstrom (docs/auth.md, "Bearer
# tokens on /v2"; docs/acceptance.md O-01, O-12 and H-12 for the part of #26): the challenge of
# GET /v2/, a token from /v2/token with Basic credentials, its claims, the token on /v2, a
# changed token, a token without push, oras login, push and pull, then a rotation of the token
# key and a rotation that drops it.
#
#   test/e2e/oci-token.sh [<base url> [<repo>]]     default: http://localhost:8080 raw/scratch
#
# <repo> is <format>/<repo> of an existing repo; the script uses the repository <repo>/hello
# and pushes a raw artifact there with oras, so it must be a raw repo (the example configuration
# creates raw/scratch). The user and the key commands run lajstrom admin straight against the
# index database of the configuration the server runs with: $LAJSTROM_CONFIG, else
# deploy/compose/lajstrom.example.yaml, with the same LAJSTROM_DATABASE_* overrides, and the
# server and the script need the same master key in LAJSTROM_MASTER_KEY (the rotations need it).
# The binary is $LAJSTROM_BIN, else bin/lajstrom, else go run ./cmd/lajstrom. oras runs from
# ghcr.io/oras-project/oras:v1.3.6 with the host's network. Each run creates an admin user of its
# own and disables it at the end. The password goes to curl in a config file and to oras and
# lajstrom on standard input, never on a command line; tokens are never printed, their decoded
# claims are. It needs curl, jq and docker, prints one line per step and exits non-zero at the
# first failure, naming the assertion or the command (lib.sh).
set -euo pipefail
. "$(dirname "$0")/lib.sh"

base="${1:-http://localhost:8080}"
base="${base%/}"
repo="${2:-raw/scratch}"
for tool in curl jq docker; do
	command -v "$tool" >/dev/null || { echo "oci-token.sh: $tool is required" >&2; exit 2; }
done
[ -n "${LAJSTROM_MASTER_KEY:-}" ] || { echo "oci-token.sh: LAJSTROM_MASTER_KEY must be the server's master key" >&2; exit 2; }
export LAJSTROM_CONFIG="${LAJSTROM_CONFIG:-deploy/compose/lajstrom.example.yaml}"
if [ -n "${LAJSTROM_BIN:-}" ]; then
	lajstrom=("$LAJSTROM_BIN")
elif [ -x bin/lajstrom ]; then
	lajstrom=(bin/lajstrom)
else
	lajstrom=(go run ./cmd/lajstrom)
fi
oras_image=ghcr.io/oras-project/oras:v1.3.6
registry="${base#*://}" # host:port of the facade, for oras
plain=""                # --plain-http for an http:// base URL
case "$base" in http://*) plain=--plain-http ;; esac

work="$(mktemp -d)"
run="$(date -u +%Y%m%d%H%M%S)-$RANDOM"
user="e2e-token-$run"
name="$repo/hello" # the repository of the scopes
cleanup() {
	"${lajstrom[@]}" admin user disable "$user" >/dev/null 2>&1 || true
	rm -rf "$work"
}
trap cleanup EXIT
status="" # status of the last request

# sha256 prints the hex sha256 of a file.
sha256() {
	if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

# req METHOD URL [curl args...]: one request. The status goes to $status, the headers to
# $work/headers (lower-case names, no CR), the body to $work/body.
req() {
	local method="$1" url="$2"
	shift 2
	status="$(curl -sS -X "$method" -o "$work/body" -D "$work/headers.raw" -w '%{http_code}' "$@" "$url")"
	tr -d '\r' <"$work/headers.raw" | awk -F': ' 'NF > 1 { $1 = tolower($1); print }' OFS=': ' >"$work/headers"
}

# hdr NAME prints the value of the response header NAME (lower case).
hdr() { awk -F': ' -v n="$1" '$1 == n { sub(/^[^:]*: /, ""); v = $0 } END { print v }' "$work/headers"; }

# bearer FILE: the curl config of the token in FILE, for -K.
bearer() { (umask 077 && printf 'header = "Authorization: Bearer %s"\n' "$(cat "$1")" >"$1.curl"); }

# claims FILE prints the decoded header and payload of the token in FILE, one JSON line.
claims() {
	jq -Rc 'split(".") | .[0:2] | map(gsub("-"; "+") | gsub("_"; "/") | @base64d | fromjson) | {header: .[0], payload: .[1]}' "$1"
}

# token FILE SCOPE: a token for SCOPE with the user's Basic credentials into FILE, and its curl
# config into FILE.curl.
token() {
	req GET "$base/v2/token?service=$service&scope=$2" -K "$work/basic"
	expect "GET /v2/token status" 200 "$status"
	jq -r .token "$work/body" >"$1"
	bearer "$1"
}

# oras ARGS...: oras in its container, with the run's directory as /w and the registry
# configuration of the login there. It runs as the caller's user: what oras pull writes as root
# on Linux, the cleanup could not remove.
oras() {
	docker run --rm -i --network host -u "$(id -u):$(id -g)" -v "$work:/w" -w /w "$oras_image" "$@" --registry-config /w/oras-auth.json ${plain:+"$plain"}
}

# O-01, first step: the challenge.
req GET "$base/v2/"
expect "GET /v2/ status" 401 "$status"
challenge="$(hdr www-authenticate)"
case "$challenge" in 'Bearer realm="'*'/v2/token",service="'*'"') ;; *) fail "GET /v2/ challenge: $challenge" ;; esac
service="$(sed -E 's/.*service="([^"]*)".*/\1/' <<<"$challenge")"
ok "GET /v2/: 401 WWW-Authenticate: $challenge"

password="$(head -c 24 /dev/urandom | base64 | tr -d '+/=\n')"
(umask 077 && printf 'user = "%s:%s"\n' "$user" "$password" >"$work/basic")
out="$(printf '%s\n' "$password" | "${lajstrom[@]}" admin user create --name "$user" --admin --password-stdin)"
expect "admin user create name" "$user" "$(jq -r .name <<<"$out")"
ok "admin user create --name $user --admin --password-stdin"

token "$work/t1" "repository:$name:pull,push"
expect "token expires_in" 1200 "$(jq -r .expires_in "$work/body")"
expect "token Cache-Control" no-store "$(hdr cache-control)"
expect "token and access_token" "$(jq -r .token "$work/body")" "$(jq -r .access_token "$work/body")"
ok "GET /v2/token -u $user scope=repository:$name:pull,push: 200, expires_in 1200, issued_at $(jq -r .issued_at "$work/body")"
c1="$(claims "$work/t1")"
expect "token sub" "user:$user" "$(jq -r .payload.sub <<<"$c1")"
expect "token aud" "$service" "$(jq -r '.payload.aud[0]' <<<"$c1")"
expect "token access" "[{\"type\":\"repository\",\"name\":\"$name\",\"actions\":[\"pull\",\"push\"]}]" "$(jq -c .payload.access <<<"$c1")"
expect "token lifetime" 1200 "$(jq '.payload.exp - .payload.iat' <<<"$c1")"
kid1="$(jq -r .header.kid <<<"$c1")"
ok "claims: $c1"

req GET "$base/v2/$name/tags/list" -K "$work/t1.curl"
[ "$status" != 401 ] || fail "GET tags/list with the token: 401 $(cat "$work/body")"
ok "GET /v2/$name/tags/list with the token: $status"

t1="$(cat "$work/t1")"
swap=A # the last but one character of the signature, changed
[ "${t1: -2:1}" != A ] || swap=B
printf '%s%s%s' "${t1:0:${#t1}-2}" "$swap" "${t1: -1}" >"$work/changed"
bearer "$work/changed"
req GET "$base/v2/$name/tags/list" -K "$work/changed.curl"
expect "a changed token status" 401 "$status"
expect "a changed token code" UNAUTHORIZED "$(jq -r '.errors[0].code' "$work/body")"
ok "the token with one byte changed: 401, WWW-Authenticate: $(hdr www-authenticate)"

# O-12 as far as it goes before #45: a token asked for pull only does not push.
token "$work/pull" "repository:$name:pull"
expect "the pull token's access" '["pull"]' "$(claims "$work/pull" | jq -c '.payload.access[0].actions')"
req POST "$base/v2/$name/blobs/uploads/" -K "$work/pull.curl"
expect "POST uploads with a pull token status" 403 "$status"
expect "POST uploads with a pull token code" DENIED "$(jq -r '.errors[0].code' "$work/body")"
ok "POST /v2/$name/blobs/uploads/ with a pull-only token: 403 DENIED: $(jq -r '.errors[0].message' "$work/body")"

printf '%s\n' "$password" | oras login -u "$user" --password-stdin "$registry" >"$work/oras.log" 2>&1 ||
	{ cat "$work/oras.log" >&2; fail "oras login"; }
grep -q 'Login Succeeded' "$work/oras.log" || fail "oras login: $(cat "$work/oras.log")"
ok "oras login $registry -u $user: Login Succeeded"

# The raw file at the path hello: the layer's title is the path, lajstrom.name.
printf 'bearer e2e %s\n' "$run" >"$work/hello"
version="$(sha256 "$work/hello" | cut -c1-16)" # the raw model: the first 16 hex of the content's sha256
oras push --artifact-type application/vnd.lajstrom.raw.v1 \
	--annotation lajstrom.schema=1 --annotation lajstrom.format=raw --annotation "lajstrom.repo=${repo#*/}" \
	--annotation lajstrom.name=hello --annotation "lajstrom.version=$version" --annotation lajstrom.raw.path=hello \
	--annotation "org.opencontainers.image.created=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
	"$registry/$name:$version" hello:application/vnd.lajstrom.raw.file.v1 >"$work/oras.log" 2>&1 ||
	{ cat "$work/oras.log" >&2; fail "oras push"; }
ok "oras push $registry/$name:$version: $(grep '^Digest:' "$work/oras.log")"
mkdir -p "$work/out"
oras pull -o out "$registry/$name:$version" >"$work/oras.log" 2>&1 || { cat "$work/oras.log" >&2; fail "oras pull"; }
cmp -s "$work/hello" "$work/out/hello" || fail "oras pull: the file differs"
ok "oras pull $registry/$name:$version: the same bytes"

# A rotation: the old key verifies for the grace, a new token has another kid.
kid2="$("${lajstrom[@]}" --log-level warn admin key rotate --token)"
[ "$kid2" != "$kid1" ] || fail "admin key rotate --token printed the old kid"
req GET "$base/v2/$name/tags/list" -K "$work/t1.curl"
expect "the old token after a rotation status" 200 "$status"
token "$work/t2" "repository:$name:pull"
expect "the new token's kid" "$kid2" "$(claims "$work/t2" | jq -r .header.kid)"
ok "admin key rotate --token: $kid2; the old token (kid ${kid1:0:16}…) still 200; a new token has kid ${kid2:0:16}…"

# A rotation with drop: the token of the dropped key dies. The replicas reload their keys at
# most once a second.
kid3="$("${lajstrom[@]}" --log-level warn admin key rotate --token --drop)"
sleep 1.2
req GET "$base/v2/$name/tags/list" -K "$work/t2.curl"
expect "the token of a dropped key status" 401 "$status"
token "$work/t3" "repository:$name:pull"
expect "the token after the drop's kid" "$kid3" "$(claims "$work/t3" | jq -r .header.kid)"
req GET "$base/v2/$name/tags/list" -K "$work/t3.curl"
expect "the token after the drop status" 200 "$status"
ok "admin key rotate --token --drop: $kid3; the token of ${kid2:0:16}… is 401, a new one 200"
echo "PASS oci-token e2e against $base as $user"

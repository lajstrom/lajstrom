#!/usr/bin/env bash
# End-to-end test of local users and HTTP Basic against a running Lajstrom (docs/auth.md,
# docs/acceptance.md S-01 for the part of #43): the bootstrap command, whoami, Basic on a raw PUT,
# disable, enable and set-password.
#
#   test/e2e/auth.sh [<base url> [<raw repo>]]     default: http://localhost:8080 scratch
#
# The users are created with lajstrom admin user, straight against the index database of the
# configuration the server runs with: $LAJSTROM_CONFIG, else deploy/compose/lajstrom.example.yaml,
# with the same LAJSTROM_DATABASE_* overrides (the example's SQLite path is relative: run the
# script from the server's directory). The binary is $LAJSTROM_BIN, else bin/lajstrom, else go
# run ./cmd/lajstrom. The repo must exist and be writable by the user (every repo is, until RBAC,
# #45). Each run creates a user of its own, named after the run, and disables it at the end;
# users are not deleted in this version. Passwords go to curl in a config file, never on a
# command line, and are never printed. It needs curl and jq, prints one line per step and exits
# non-zero at the first failure.
set -euo pipefail

base="${1:-http://localhost:8080}"
repo="${2:-scratch}"
for tool in curl jq; do
	command -v "$tool" >/dev/null || { echo "auth.sh: $tool is required" >&2; exit 2; }
done
export LAJSTROM_CONFIG="${LAJSTROM_CONFIG:-deploy/compose/lajstrom.example.yaml}"
if [ -n "${LAJSTROM_BIN:-}" ]; then
	lajstrom=("$LAJSTROM_BIN")
elif [ -x bin/lajstrom ]; then
	lajstrom=(bin/lajstrom)
else
	lajstrom=(go run ./cmd/lajstrom)
fi

work="$(mktemp -d)"
user="e2e-$(date -u +%Y%m%d%H%M%S)-$RANDOM"
path="$user/file.txt"
uploaded=""
cleanup() {
	if [ -n "$uploaded" ]; then
		curl -sS -o /dev/null -K "$work/new" -X DELETE "$base/raw/$repo/$path" || true
	fi
	"${lajstrom[@]}" admin user disable "$user" >/dev/null 2>&1 || true
	rm -rf "$work"
}
trap cleanup EXIT
status="" # status of the last request

fail() { echo "FAIL $*" >&2; exit 1; }
ok() { echo "ok   $*"; }
expect() { [ "$2" = "$3" ] || fail "$1: got '$3', want '$2'"; }

# password prints a new random password: 24 bytes of urandom in base64 without +, / and =.
password() { head -c 24 /dev/urandom | base64 | tr -d '+/=\n'; }

# credentials NAME USER PASSWORD writes a curl config with the Basic credentials to $work/NAME.
credentials() {
	(umask 077 && printf 'user = "%s:%s"\n' "$2" "$3" >"$work/$1")
}

# req CRED METHOD URL [curl args...]: one request, with the credentials of $work/CRED, or none
# when CRED is -. The status goes to $status, the headers to $work/headers (lower-case names, no
# CR), the body to $work/body.
req() {
	local cred="$1" method="$2" url="$3"
	shift 3
	local how=(-X "$method") # never empty: bash 3.2 takes an empty array for unset under set -u
	[ "$cred" = - ] || how+=(-K "$work/$cred")
	status="$(curl -sS "${how[@]}" -o "$work/body" -D "$work/headers.raw" -w '%{http_code}' "$@" "$url")"
	tr -d '\r' <"$work/headers.raw" | awk -F': ' 'NF > 1 { $1 = tolower($1); print }' OFS=': ' >"$work/headers"
}

# hdr NAME prints the value of the response header NAME (lower case).
hdr() { awk -F': ' -v n="$1" '$1 == n { sub(/^[^:]*: /, ""); v = $0 } END { print v }' "$work/headers"; }

# put CRED: PUT a file to $path in the repo with the credentials of $work/CRED.
put() { req "$1" PUT "$base/raw/$repo/$path" -T "$work/file" -H 'Content-Type: text/plain'; }

pw1="$(password)"
pw2="$(password)"
credentials old "$user" "$pw1"
credentials new "$user" "$pw2"
credentials wrong "$user" "$pw1-wrong"
printf 'auth e2e %s\n' "$user" >"$work/file"

# The bootstrap command: a user from LAJSTROM_ADMIN_PASSWORD, printed as JSON without the hash.
out="$(LAJSTROM_ADMIN_PASSWORD="$pw1" "${lajstrom[@]}" admin user create --name "$user")"
expect "admin user create name" "$user" "$(jq -r .name <<<"$out")"
expect "admin user create disabled" false "$(jq -r .disabled <<<"$out")"
case "$out" in *argon2*) fail "admin user create printed the hash" ;; esac
ok "admin user create --name $user: $(jq -c 'del(.createdAt)' <<<"$out")"

req - GET "$base/api/v1/whoami"
expect "whoami without credentials status" 401 "$status"
expect "whoami without credentials WWW-Authenticate" 'Basic realm="lajstrom"' "$(hdr www-authenticate)"
expect "whoami without credentials code" FORBIDDEN "$(jq -r .error.code "$work/body")"
ok "GET /api/v1/whoami without credentials: 401 with Basic realm=\"lajstrom\""

req old GET "$base/api/v1/whoami"
expect "whoami status" 200 "$status"
expect "whoami principal" "user:$user" "$(jq -r .principal "$work/body")"
ok "GET /api/v1/whoami as $user: 200 $(jq -c . "$work/body")"

req wrong GET "$base/api/v1/whoami"
expect "whoami with a wrong password status" 401 "$status"
expect "whoami with a wrong password WWW-Authenticate" 'Basic realm="lajstrom"' "$(hdr www-authenticate)"
ok "GET /api/v1/whoami with a wrong password: 401"

put old
expect "PUT as $user status" 201 "$status"
uploaded=yes
ok "curl -u $user -T PUT /raw/$repo/$path: 201 $(hdr docker-content-digest)"

"${lajstrom[@]}" admin user disable "$user"
put old
expect "PUT after disable status" 401 "$status"
expect "PUT after disable WWW-Authenticate" 'Basic realm="lajstrom"' "$(hdr www-authenticate)"
ok "admin user disable $user; the same PUT: 401"

"${lajstrom[@]}" admin user enable "$user"
put old
expect "PUT after enable status" 201 "$status"
ok "admin user enable $user; the same PUT: 201"

LAJSTROM_ADMIN_PASSWORD="$pw2" "${lajstrom[@]}" admin user set-password "$user"
put old
expect "PUT with the old password status" 401 "$status"
ok "admin user set-password $user; the old password: 401"
put new
expect "PUT with the new password status" 201 "$status"
req new GET "$base/api/v1/whoami"
expect "whoami with the new password" "user:$user" "$(jq -r .principal "$work/body")"
ok "the new password: PUT 201, whoami user:$user"

req new DELETE "$base/raw/$repo/$path"
expect "DELETE status" 204 "$status"
uploaded=""
ok "cleanup: DELETE /raw/$repo/$path: 204; $user is disabled on exit"
echo "PASS auth e2e against $base as $user"

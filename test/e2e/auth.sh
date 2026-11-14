#!/usr/bin/env bash
# End-to-end test of local users and HTTP Basic against a running Lajstrom (docs/auth.md,
# docs/acceptance.md S-01 for the part of #43): the bootstrap command, whoami, Basic on a raw PUT,
# disable, enable and set-password; the throttle of failed logins, X-04 (#130), and with
# LAJSTROM_E2E_FORWARDED=1 (the server trusts this host's X-Forwarded-For, as run.sh sets it up)
# the throttle of /v2/token per client address. Its requests name made-up clients in
# X-Forwarded-For, so that the budget of this host is left to the other scenarios.
#
#   test/e2e/auth.sh [<base url> [<raw repo>]]     default: http://localhost:8080 scratch
#
# The users are created with lajstrom admin user, straight against the index database of the
# configuration the server runs with: $LAJSTROM_CONFIG, else deploy/compose/lajstrom.example.yaml,
# with the same LAJSTROM_DATABASE_* overrides (the example's SQLite path is relative: run the
# script from the server's directory). The binary is $LAJSTROM_BIN, else bin/lajstrom, else go
# run ./cmd/lajstrom. The repo must exist; the user gets the maintainer role on it (lajstrom
# admin binding create), to put and delete. Each run creates a user of its own, named after the
# run, and disables it at the end; users are not deleted in this version. Passwords go to curl in
# a config file, never on a command line, and are never printed. It needs curl and jq, prints one line per step and exits
# non-zero at the first failure, naming the assertion or the command (lib.sh). Where the registry
# deletes no manifests, LAJSTROM_E2E_DELETE_MANIFEST=false (run.sh, #800), the maintainer's DELETE
# is 501, which it gets past the permission check, and the file stays in the registry.
set -euo pipefail
. "$(dirname "$0")/lib.sh"

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
x04="" # the user of X-04
cleanup() {
	if [ -n "$uploaded" ]; then
		curl -sS -o /dev/null -K "$work/new" -X DELETE "$base/raw/$repo/$path" || true
	fi
	"${lajstrom[@]}" admin user disable "$user" >/dev/null 2>&1 || true
	[ -z "$x04" ] || "${lajstrom[@]}" admin user disable "$x04" >/dev/null 2>&1 || true
	rm -rf "$work"
}
trap cleanup EXIT
status="" # status of the last request

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

# The command against the database: a user with the password of standard input, printed as JSON
# without the hash.
out="$(printf '%s' "$pw1" | "${lajstrom[@]}" admin user create --name "$user" --password-stdin)"
expect "admin user create name" "$user" "$(jq -r .name <<<"$out")"
expect "admin user create disabled" false "$(jq -r .disabled <<<"$out")"
case "$out" in *argon2*) fail "admin user create printed the hash" ;; esac
uid="$(jq -r .uid <<<"$out")"
ok "admin user create --name $user: $(jq -c 'del(.createdAt)' <<<"$out")"
out="$("${lajstrom[@]}" admin binding create --principal "user:$user" --role maintainer --mask "raw/$repo")"
expect "admin binding create" "user:$uid $user maintainer raw/$repo" "$(jq -r '[.principal, .name, .role, .repoMask] | join(" ")' <<<"$out")"
ok "admin binding create --principal user:$user --role maintainer --mask raw/$repo: id $(jq -r .id <<<"$out")"

req - GET "$base/api/v1/whoami"
expect "whoami without credentials status" 401 "$status"
expect "whoami without credentials WWW-Authenticate" 'Basic realm="lajstrom"' "$(hdr www-authenticate)"
expect "whoami without credentials code" UNAUTHENTICATED "$(jq -r .error.code "$work/body")"
ok "GET /api/v1/whoami without credentials: 401 UNAUTHENTICATED with Basic realm=\"lajstrom\""

req old GET "$base/api/v1/whoami"
expect "whoami status" 200 "$status"
expect "whoami principal" "user:$uid $user" "$(jq -r '"\(.principal) \(.name)"' "$work/body")"
ok "GET /api/v1/whoami as $user: 200 $(jq -c . "$work/body")"

req wrong GET "$base/api/v1/whoami"
expect "whoami with a wrong password status" 401 "$status"
expect "whoami with a wrong password WWW-Authenticate" 'Basic realm="lajstrom"' "$(hdr www-authenticate)"
ok "GET /api/v1/whoami with a wrong password: 401"

# X-04 (#130): 20 wrong passwords in a row for one user, the 11th and later 429 with Retry-After,
# while another user's password works from the same client; the right password of the first is
# 429 too within the minute (a correct password after it is the unit tests': it takes a minute).
x04="$user-x"
client=(-H "X-Forwarded-For: 198.51.100.$((RANDOM % 100))")
pwx="$(password)"
out="$(printf '%s' "$pwx" | "${lajstrom[@]}" admin user create --name "$x04" --password-stdin)"
uidx="$(jq -r .uid <<<"$out")"
credentials x04 "$x04" "$pwx"
credentials x04wrong "$x04" "$pwx-wrong"
codes=""
for i in $(seq 20); do
	req x04wrong GET "$base/api/v1/whoami" "${client[@]}"
	codes="$codes $status"
	if [ "$i" -gt 10 ]; then
		retry="$(hdr retry-after)"
		case "$retry" in '' | *[!0-9]*) fail "wrong password $i: Retry-After '$retry'" ;; esac
		[ "$retry" -ge 1 ] && [ "$retry" -le 60 ] || fail "wrong password $i: Retry-After $retry, want 1 to 60"
	fi
done
expect "20 wrong passwords" "$(printf ' 401%.0s' $(seq 10))$(printf ' 429%.0s' $(seq 10))" "$codes"
expect "the 429 body" UNAVAILABLE "$(jq -r .error.code "$work/body")"
req old GET "$base/api/v1/whoami" "${client[@]}"
expect "another user's password after the 20" 200 "$status"
req x04 GET "$base/api/v1/whoami" "${client[@]}"
expect "the right password under throttle" 429 "$status"
if [ -n "${LAJSTROM_SERVER_LOG:-}" ]; then
	grep -q "msg=throttled throttle=account account=user:$uidx address=" "$LAJSTROM_SERVER_LOG" ||
		fail "no throttled line for user:$uidx in the server log"
	! grep -qF -e "$pwx" "$LAJSTROM_SERVER_LOG" || fail "the server log has the password of $x04"
fi
ok "20 wrong passwords for $x04:$codes (Retry-After $retry); $user from the same client: 200; the right password: 429"

if [ "${LAJSTROM_E2E_FORWARDED:-}" = 1 ]; then
	# 200 requests to /v2/token from one client in well under a second, over one connection: the
	# first 100 have a token, the later ones are 429 with Retry-After; another client behind the
	# same proxy is not throttled.
	flood="198.51.100.$((100 + RANDOM % 100))"
	args=()
	for _ in $(seq 200); do args+=(-o /dev/null "$base/v2/token?scope=repository:raw/$repo:pull"); done
	out="$(curl -sS -H "X-Forwarded-For: $flood" -w '%{http_code}/%header{retry-after} ' "${args[@]}")"
	expect "200 token requests" "$(printf '200/ %.0s' $(seq 100))$(printf '429/1 %.0s' $(seq 100))" "$out"
	req - GET "$base/v2/token?scope=repository:raw/$repo:pull" -H "X-Forwarded-For: 198.51.100.$((RANDOM % 100))"
	expect "a token for another client behind the proxy" 200 "$status"
	if [ -n "${LAJSTROM_SERVER_LOG:-}" ]; then
		grep -q "msg=throttled throttle=token address=$flood " "$LAJSTROM_SERVER_LOG" ||
			fail "no throttled line for $flood in the server log"
	fi
	ok "200 GET /v2/token from $flood: 100 tokens, then 100 x 429 Retry-After: 1; another client: 200"
fi

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

printf '%s' "$pw2" | "${lajstrom[@]}" admin user set-password "$user" --password-stdin
put old
expect "PUT with the old password status" 401 "$status"
ok "admin user set-password $user; the old password: 401"
put new
expect "PUT with the new password status" 201 "$status"
req new GET "$base/api/v1/whoami"
expect "whoami with the new password" "user:$uid" "$(jq -r .principal "$work/body")"
ok "the new password: PUT 201, whoami user:$uid"

# A user is its uid (#182): renamed, it keeps its uid and its binding; the old name is gone.
was="$user"
user="$user-renamed"
"${lajstrom[@]}" admin user rename "$was" "$user" >/dev/null
put new
expect "PUT under the old name after the rename" 401 "$status"
credentials new "$user" "$pw2"
req new GET "$base/api/v1/whoami"
expect "whoami after the rename" "user:$uid $user" "$(jq -r '"\(.principal) \(.name)"' "$work/body")"
ok "admin user rename $was $user: the old name 401, whoami user:$uid $user"

deleted=204 # 501 where the registry deletes no manifests: refused after the permission check
[ "${LAJSTROM_E2E_DELETE_MANIFEST:-}" != false ] || deleted=501
req new DELETE "$base/raw/$repo/$path"
expect "DELETE status" "$deleted" "$status"
uploaded=""
ok "DELETE /raw/$repo/$path as $user: $deleted, the maintainer binding holds across the rename"

# Deleted, the user is refused; its name is free again, for a user with another uid.
"${lajstrom[@]}" admin user delete "$user" >/dev/null
req new GET "$base/api/v1/whoami"
expect "whoami after the delete" 401 "$status"
out="$(printf '%s' "$pw1" | "${lajstrom[@]}" admin user create --name "$user" --password-stdin)"
[ "$(jq -r .uid <<<"$out")" != "$uid" ] || fail "the name $user came back with the deleted user's uid $uid"
ok "admin user delete $user: whoami 401; admin user create --name $user: a new uid $(jq -r .uid <<<"$out"), disabled on exit"
echo "PASS auth e2e against $base as $was"

#!/usr/bin/env bash
# End-to-end test of RBAC against a running Lajstrom (docs/spec/rbac.md §3 and §4;
# docs/acceptance.md S-02, S-07, S-08 and X-05 for #45):
#
#   - the S-02 matrix on the raw frontend: GET, PUT and DELETE on a repo, and GET on a repo that
#     does not exist, as the anonymous principal, a reader, a writer, a maintainer, the admin and
#     a user whose only binding is reader on pypi/* (S-08);
#   - the same matrix on /v2, each principal with a token from /v2/token asked for pull, push and
#     delete on the repo and on the one that does not exist: the token carries what the role
#     allows, and the same empty actions on a repo the principal may not read and on one that does
#     not exist (X-05); a manifest DELETE that passes the authorization is 405 until #61;
#   - a binding created and deleted with lajstrom admin binding counts from the next request, and
#     so does the repo's anonymousRead, changed through the API (S-07);
#   - creating a repo with a taken name is 403 to a reader, before the name matters, and 409 to
#     the admin only (rbac.md §3).
#
#   test/e2e/rbac.sh [<base url>]     default: http://localhost:8080
#
# It runs as $LAJSTROM_E2E_USER, a user with the admin binding (lib.sh, e2e_credentials;
# run.sh makes one), and creates a raw repo of its own, rbac-<run>, through the API. The other
# users and their bindings are made with lajstrom admin, straight against the index database of
# the configuration the server runs with: $LAJSTROM_CONFIG, else
# deploy/compose/lajstrom.example.yaml, with the same LAJSTROM_DATABASE_* overrides. The binary
# is $LAJSTROM_BIN, else bin/lajstrom, else go run ./cmd/lajstrom. The server must have
# auth.anonymousRead: true (run.sh sets it), else S-07 fails and says so. On exit the users are
# disabled (users are not deleted in this version) and the repo deleted. Passwords go to curl in
# config files and to lajstrom on standard input, never on a command line; tokens are never
# printed. It needs curl and jq, prints one line per step and exits non-zero at the first
# failure, naming the assertion or the command (lib.sh).
set -euo pipefail
. "$(dirname "$0")/lib.sh"

base="${1:-http://localhost:8080}"
base="${base%/}"
for tool in curl jq; do
	command -v "$tool" >/dev/null || { echo "rbac.sh: $tool is required" >&2; exit 2; }
done
export LAJSTROM_CONFIG="${LAJSTROM_CONFIG:-deploy/compose/lajstrom.example.yaml}"
if [ -n "${LAJSTROM_BIN:-}" ]; then
	lajstrom=("$LAJSTROM_BIN")
elif [ -x bin/lajstrom ]; then
	lajstrom=(bin/lajstrom)
else
	lajstrom=(go run ./cmd/lajstrom)
fi
service="${base#*://}" # the service of the tokens: the host of the base URL

work="$(mktemp -d)"
e2e_credentials "$work"
cp "$E2E_CURLRC" "$work/admin"
run="$(date -u +%Y%m%d%H%M%S)-$RANDOM"
repo="rbac-$run"
missing="rbac-missing-$run"
users="" # the users this run made, disabled on exit
created=""
cleanup() {
	local u
	for u in $users; do
		"${lajstrom[@]}" admin user disable "$u" >/dev/null 2>&1 || true
	done
	if [ -n "$created" ]; then
		curl -sS -o /dev/null -K "$work/admin" -X DELETE "$base/api/v1/repos/raw/$repo?force=true" || true
	fi
	rm -rf "$work"
}
trap cleanup EXIT
status="" # status of the last request

# req CRED METHOD URL [curl args...]: one request, with the curl config $work/CRED (Basic
# credentials or a bearer), or none when CRED is -. The status goes to $status, the headers to
# $work/headers (lower-case names, no CR), the body to $work/body.
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

# answer prints the status of the last request, and the code of its distribution error when it
# has one: 404/NAME_UNKNOWN.
answer() {
	local code
	code="$(jq -r '.errors[0].code // empty' "$work/body" 2>/dev/null || true)"
	echo "$status${code:+/$code}"
}

# mkuser NAME ROLE MASK: a user with a random password, its Basic credentials in $work/NAME, and
# the binding ROLE on MASK.
mkuser() {
	local pw
	pw="$(head -c 24 /dev/urandom | base64 | tr -d '+/=\n')"
	printf '%s' "$pw" | "${lajstrom[@]}" admin user create --name "$1" --password-stdin >/dev/null
	users="$users $1"
	(umask 077 && printf 'user = "%s:%s"\n' "$1" "$pw" >"$work/$1")
	"${lajstrom[@]}" admin binding create --principal "user:$1" --role "$2" --mask "$3" >/dev/null
}

# cred WHO prints the credentials of a principal of the matrix for req: - for anonymous.
cred() {
	case "$1" in
	anonymous) echo - ;;
	*) echo "$1" ;;
	esac
}

printf 'rbac e2e %s\n' "$run" >"$work/file"
req admin POST "$base/api/v1/repos" -H 'Content-Type: application/json' \
	-d "{\"format\":\"raw\",\"name\":\"$repo\",\"registry\":\"local\"}"
expect "POST /api/v1/repos raw/$repo as the admin" 201 "$status"
created=yes
step "POST /api/v1/repos raw/$repo as $E2E_USER (admin): 201"

# The principals of the matrix: the admin of the run is "admin"; the others are users of their
# own, named after the role and the run.
for role in reader writer maintainer; do
	mkuser "$role-$run" "$role" "raw/$repo"
	ln -s "$work/$role-$run" "$work/$role"
done
mkuser "outsider-$run" reader "pypi/*"
ln -s "$work/outsider-$run" "$work/outsider"
step "users reader, writer, maintainer with their role on raw/$repo; outsider with reader on pypi/* only"

for f in seed.txt del-anonymous.txt del-reader.txt del-writer.txt del-maintainer.txt del-admin.txt del-outsider.txt; do
	req admin PUT "$base/raw/$repo/$f" -T "$work/file" -H 'Content-Type: text/plain'
	expect "seed PUT $f" 201 "$status"
done
step "the admin put seed.txt and a file to delete for each principal"

# S-02 on the raw frontend (docs/acceptance.md §3.8): GET, PUT and DELETE on raw/$repo, GET on
# raw/$missing. The outsider's row is S-08: 404 alike for a repo it may not read and one that
# does not exist.
while read -r who get put del miss; do
	[ -n "$who" ] || continue
	c="$(cred "$who")"
	req "$c" GET "$base/raw/$repo/seed.txt"
	expect "$who GET raw/$repo" "$get" "$status"
	req "$c" PUT "$base/raw/$repo/put-$who.txt" -T "$work/file" -H 'Content-Type: text/plain'
	expect "$who PUT raw/$repo" "$put" "$status"
	req "$c" DELETE "$base/raw/$repo/del-$who.txt"
	expect "$who DELETE raw/$repo" "$del" "$status"
	req "$c" GET "$base/raw/$missing/seed.txt"
	expect "$who GET raw/$missing" "$miss" "$status"
	step "raw frontend, $who: GET $get, PUT $put, DELETE $del, GET of a repo that does not exist $miss"
done <<'EOF'
anonymous  401 401 401 401
reader     200 403 403 404
writer     200 201 403 404
maintainer 200 201 204 404
admin      200 201 204 404
outsider   404 404 404 404
EOF

# S-02 on /v2: a token asked for everything on both repositories, then a pull (tags/list), a
# push (an upload session, cancelled), a manifest DELETE (405 until #61 once authorized) and a
# pull of the repository that does not exist. The token's access on the repo that does not exist
# is [] for everybody, and the same [] on the repo for a principal that may not read it.
name="raw/$repo/seed.txt"
gone="raw/$missing/seed.txt"
zero="sha256:$(printf '0%.0s' $(seq 64))"
while read -r who actions pull push del miss; do
	[ -n "$who" ] || continue
	req "$(cred "$who")" GET "$base/v2/token?service=$service&scope=repository:$name:pull,push,delete&scope=repository:$gone:pull,push,delete"
	expect "$who GET /v2/token" 200 "$status"
	(umask 077 && printf 'header = "Authorization: Bearer %s"\n' "$(jq -r .token "$work/body")" >"$work/bearer")
	got="$(jq -r '.token | split(".")[1] | gsub("-"; "+") | gsub("_"; "/") | @base64d | fromjson | .access' "$work/body")"
	expect "$who: the token's actions on $name" "$actions" "$(jq -c --arg n "$name" 'map(select(.name == $n))[0].actions' <<<"$got")"
	expect "$who: the token's actions on $gone (X-05)" '[]' "$(jq -c --arg n "$gone" 'map(select(.name == $n))[0].actions' <<<"$got")"
	req bearer GET "$base/v2/$name/tags/list"
	expect "$who /v2 pull" "$pull" "$(answer)"
	req bearer POST "$base/v2/$name/blobs/uploads/"
	expect "$who /v2 push" "$push" "$(answer)"
	if [ "$status" = 202 ]; then
		req bearer DELETE "$base$(hdr location)"
		expect "$who /v2 cancel of the upload" 204 "$status"
	fi
	req bearer DELETE "$base/v2/$name/manifests/$zero"
	expect "$who /v2 delete" "$del" "$(answer)"
	req bearer GET "$base/v2/$gone/tags/list"
	expect "$who /v2 pull of a repo that does not exist" "$miss" "$(answer)"
	step "/v2, $who: token $actions; pull $pull, push $push, delete $del, a repo that does not exist $miss"
done <<'EOF'
anonymous  []                        401/UNAUTHORIZED 401/UNAUTHORIZED 401/UNAUTHORIZED 401/UNAUTHORIZED
reader     ["pull"]                  200              403/DENIED       403/DENIED       404/NAME_UNKNOWN
writer     ["pull","push"]           200              202              403/DENIED       404/NAME_UNKNOWN
maintainer ["pull","push","delete"]  200              202              405/UNSUPPORTED  404/NAME_UNKNOWN
admin      ["pull","push","delete"]  200              202              405/UNSUPPORTED  404/NAME_UNKNOWN
outsider   []                        404/NAME_UNKNOWN 404/NAME_UNKNOWN 404/NAME_UNKNOWN 404/NAME_UNKNOWN
EOF

# A binding counts from the next request, on the server, though lajstrom admin wrote it to the
# database behind the server's back (auth_version; rbac.md §3).
out="$("${lajstrom[@]}" admin binding create --principal "user:outsider-$run" --role reader --mask "raw/$repo")"
id="$(jq -r .id <<<"$out")"
req outsider GET "$base/raw/$repo/seed.txt"
expect "outsider GET once it is a reader" 200 "$status"
"${lajstrom[@]}" admin binding delete "$id"
req outsider GET "$base/raw/$repo/seed.txt"
expect "outsider GET once the binding is gone" 404 "$status"
step "admin binding create reader on raw/$repo for outsider: the next GET 200; admin binding delete $id: the next GET 404"

# S-07: the repo's anonymousRead, changed through the API, counts from the next request.
req admin PATCH "$base/api/v1/repos/raw/$repo" -H 'Content-Type: application/json' -d '{"anonymousRead": true}'
expect "PATCH anonymousRead true" 200 "$status"
req - GET "$base/raw/$repo/seed.txt"
[ "$status" = 200 ] || fail "anonymous GET with anonymousRead on the repo: $status, want 200; the server needs auth.anonymousRead: true"
req - PUT "$base/raw/$repo/anonymous.txt" -T "$work/file" -H 'Content-Type: text/plain'
expect "anonymous PUT with anonymousRead" 401 "$status"
expect "anonymous PUT WWW-Authenticate" 'Basic realm="lajstrom"' "$(hdr www-authenticate)"
req - GET "$base/raw/$missing/seed.txt"
expect "anonymous GET of a repo that does not exist" 401 "$status"
req admin PATCH "$base/api/v1/repos/raw/$repo" -H 'Content-Type: application/json' -d '{"anonymousRead": false}'
expect "PATCH anonymousRead false" 200 "$status"
req - GET "$base/raw/$repo/seed.txt"
expect "anonymous GET once anonymousRead is off" 401 "$status"
step "S-07: anonymousRead on: anonymous GET 200, PUT 401, a repo that does not exist 401; off: the next GET 401"

# Creating a repo is the admin's, decided before the body names a repo: a taken name tells a
# reader nothing.
for who in reader admin; do
	req "$who" POST "$base/api/v1/repos" -H 'Content-Type: application/json' \
		-d "{\"format\":\"raw\",\"name\":\"$repo\",\"registry\":\"local\"}"
	case "$who" in
	reader) expect "reader POST /api/v1/repos with a taken name" 403 "$status" ;;
	admin) expect "admin POST /api/v1/repos with a taken name" 409 "$status" ;;
	esac
done
step "POST /api/v1/repos with the taken name raw/$repo: reader 403, admin 409"
echo "PASS rbac e2e against $base, repo raw/$repo"

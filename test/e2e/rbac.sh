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
#     not exist (X-05); a manifest DELETE that passes the authorization reaches the delete (#61),
#     404 MANIFEST_UNKNOWN for a digest that names nothing;
#   - oras: the reader's oras push is refused, the writer's goes, and the file is on the frontend;
#   - a deploy token with read and write on the repo: PUT 201 and DELETE 403 there, 404 on
#     another repo and on one that does not exist (S-04);
#   - a binding created and deleted with lajstrom admin binding counts from the next request, and
#     so does the repo's anonymousRead, changed through the API (S-07);
#   - creating a repo with a taken name is 403 to a reader, before the name matters, and 409 to
#     the admin only (rbac.md §3).
#
# Where the registry deletes no manifests, LAJSTROM_E2E_DELETE_MANIFEST=false (run.sh, #800), a
# delete that passes the authorization is refused after it: 501 on the raw frontend, 405
# UNSUPPORTED on /v2, where the matrices want 204 and 404 MANIFEST_UNKNOWN.
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
# printed. oras runs from ghcr.io/oras-project/oras:v1.3.6 with the host's network, or is the oras
# on PATH. It needs curl, jq and oras or docker, prints one line per step and exits non-zero at
# the first failure, naming the assertion or the command (lib.sh).
set -euo pipefail
. "$(dirname "$0")/lib.sh"

base="${1:-http://localhost:8080}"
base="${base%/}"
for tool in curl jq; do
	command -v "$tool" >/dev/null || { echo "rbac.sh: $tool is required" >&2; exit 2; }
done
command -v oras >/dev/null || command -v docker >/dev/null || { echo "rbac.sh: oras or docker is required" >&2; exit 2; }
export LAJSTROM_CONFIG="${LAJSTROM_CONFIG:-deploy/compose/lajstrom.example.yaml}"
if [ -n "${LAJSTROM_BIN:-}" ]; then
	lajstrom=("$LAJSTROM_BIN")
elif [ -x bin/lajstrom ]; then
	lajstrom=(bin/lajstrom)
else
	lajstrom=(go run ./cmd/lajstrom)
fi
service="${base#*://}" # the service of the tokens, and host:port of the facade for oras
plain=""               # --plain-http for an http:// base URL
case "$base" in http://*) plain=--plain-http ;; esac

work="$(mktemp -d)"
e2e_credentials "$work"
cp "$E2E_CURLRC" "$work/admin"
run="$(date -u +%Y%m%d%H%M%S)-$RANDOM"
repo="rbac-$run"
other="rbac-other-$run" # a repo of the run that the deploy token is not for
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
		curl -sS -o /dev/null -K "$work/admin" -X DELETE "$base/api/v1/repos/raw/$other?force=true" || true
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

# mkuser NAME ROLE MASK: a user with a random password, its Basic credentials in $work/NAME and
# the password alone in $work/NAME.pw, and the binding ROLE on MASK.
mkuser() {
	local pw
	pw="$(head -c 24 /dev/urandom | base64 | tr -d '+/=\n')"
	printf '%s' "$pw" | "${lajstrom[@]}" admin user create --name "$1" --password-stdin >/dev/null
	users="$users $1"
	(umask 077 && printf 'user = "%s:%s"\n' "$1" "$pw" >"$work/$1" && printf '%s\n' "$pw" >"$work/$1.pw")
	"${lajstrom[@]}" admin binding create --principal "user:$1" --role "$2" --mask "$3" >/dev/null
}

# cred WHO prints the credentials of a principal of the matrix for req: - for anonymous.
cred() {
	case "$1" in
	anonymous) echo - ;;
	*) echo "$1" ;;
	esac
}

# oras ARGS...: oras in the run's directory as /w, with the registry configuration $work/$ORAS_AS.json.
if command -v oras >/dev/null; then
	oras() { (cd "$work" && command oras "$@" --registry-config "$work/$ORAS_AS.json" ${plain:+"$plain"}); }
else
	oras() { docker run --rm -i --network host -u "$(id -u):$(id -g)" -v "$work:/w" -w /w ghcr.io/oras-project/oras:v1.3.6 "$@" --registry-config "/w/$ORAS_AS.json" ${plain:+"$plain"}; }
fi

# oras_push WHO: oras login as WHO, then oras push of $work/oras-WHO.txt to raw/$repo with the raw
# profile; the output goes to $work/oras.log and oras's status is returned.
oras_push() {
	local path="oras-$1.txt" version # the raw model: the version is the first 16 hex of the content's sha256
	ORAS_AS="$1"
	printf 'rbac oras e2e %s %s\n' "$1" "$run" >"$work/$path"
	if command -v sha256sum >/dev/null; then version="$(sha256sum "$work/$path" | cut -c1-16)"; else version="$(shasum -a 256 "$work/$path" | cut -c1-16)"; fi
	oras login -u "$1-$run" --password-stdin "$service" <"$work/$1-$run.pw" >"$work/oras.log" 2>&1 || return 1
	oras push --artifact-type application/vnd.lajstrom.raw.v1 \
		--annotation lajstrom.schema=1 --annotation lajstrom.format=raw --annotation "lajstrom.repo=$repo" \
		--annotation "lajstrom.name=$path" --annotation "lajstrom.version=$version" \
		--annotation "lajstrom.raw.path=$path" --annotation "org.opencontainers.image.created=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
		"$service/raw/$repo/$path:$version" "$path:application/vnd.lajstrom.raw.file.v1" >"$work/oras.log" 2>&1
}

printf 'rbac e2e %s\n' "$run" >"$work/file"
for r in "$repo" "$other"; do
	req admin POST "$base/api/v1/repos" -H 'Content-Type: application/json' \
		-d "{\"format\":\"raw\",\"name\":\"$r\",\"registry\":\"local\"}"
	expect "POST /api/v1/repos raw/$r as the admin" 201 "$status"
	created=yes
done
step "POST /api/v1/repos raw/$repo and raw/$other as $E2E_USER (admin): 201"

# The principals of the matrix: the admin of the run is "admin"; the others are users of their
# own, named after the role and the run.
for role in reader writer maintainer; do
	mkuser "$role-$run" "$role" "raw/$repo"
	ln -s "$work/$role-$run" "$work/$role"
done
mkuser "outsider-$run" reader "pypi/*"
ln -s "$work/outsider-$run" "$work/outsider"
step "users reader, writer, maintainer with their role on raw/$repo; outsider with reader on pypi/* only"

deletes=yes # the registry deletes manifests; no: a delete is refused after the authorization (#800)
[ "${LAJSTROM_E2E_DELETE_MANIFEST:-}" != false ] || deletes=no
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
	[ "$deletes" = yes ] || [ "$del" != 204 ] || del=501
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
# push (an upload session, cancelled), a manifest DELETE (of a digest that names nothing: 404
# MANIFEST_UNKNOWN once authorized, #61) and a
# pull of the repository that does not exist. The token's access on the repo that does not exist
# is [] for everybody, and the same [] on the repo for a principal that may not read it.
name="raw/$repo/seed.txt"
gone="raw/$missing/seed.txt"
zero="sha256:$(printf '0%.0s' $(seq 64))"
while read -r who actions pull push del miss; do
	[ -n "$who" ] || continue
	[ "$deletes" = yes ] || [ "$del" != 404/MANIFEST_UNKNOWN ] || del=405/UNSUPPORTED
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
maintainer ["pull","push","delete"]  200              202              404/MANIFEST_UNKNOWN 404/NAME_UNKNOWN
admin      ["pull","push","delete"]  200              202              404/MANIFEST_UNKNOWN 404/NAME_UNKNOWN
outsider   []                        404/NAME_UNKNOWN 404/NAME_UNKNOWN 404/NAME_UNKNOWN 404/NAME_UNKNOWN
EOF

# oras, a real client of /v2: the reader's push is refused (its token has pull only), the
# writer's goes through ingest, and the file is on the frontend.
if oras_push reader; then fail "oras push as the reader succeeded: $(cat "$work/oras.log")"; fi
grep -qiE 'denied|403' "$work/oras.log" || fail "oras push as the reader: not a denial: $(cat "$work/oras.log")"
denial="$(grep -iE -m1 'denied|403' "$work/oras.log")"
oras_push writer || { cat "$work/oras.log" >&2; fail "oras push as the writer"; }
digest="$(awk '$1 == "Digest:" { print $2 }' "$work/oras.log")"
req reader GET "$base/raw/$repo/oras-writer.txt"
expect "the reader GETs the writer's oras push" 200 "$status"
step "oras push as the reader: refused, $denial; as the writer: $digest, and the reader GETs it on the frontend"

# S-04: a deploy token with read and write on raw/$repo, made by the admin through the API, as
# Basic deploy:<token>.
req admin POST "$base/api/v1/repos/raw/$repo/deploy-tokens" -H 'Content-Type: application/json' \
	-d "{\"name\": \"rbac $run\", \"scopes\": [\"read\", \"write\"]}"
expect "POST deploy-tokens" 201 "$status"
(umask 077 && printf 'user = "deploy:%s"\n' "$(jq -r .secret "$work/body")" >"$work/deploy")
dep="$(jq -r .id "$work/body")"
req deploy PUT "$base/raw/$repo/deploy.txt" -T "$work/file" -H 'Content-Type: text/plain'
expect "deploy token PUT on its repo" 201 "$status"
req deploy DELETE "$base/raw/$repo/deploy.txt"
expect "deploy token DELETE on its repo" 403 "$status"
req deploy PUT "$base/raw/$other/deploy.txt" -T "$work/file" -H 'Content-Type: text/plain'
expect "deploy token PUT on another repo" 404 "$status"
req deploy GET "$base/raw/$missing/seed.txt"
expect "deploy token GET of a repo that does not exist" 404 "$status"
step "deploy token $dep (read, write on raw/$repo): PUT 201, DELETE 403; raw/$other 404, a repo that does not exist 404"

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

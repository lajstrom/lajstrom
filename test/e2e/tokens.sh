#!/usr/bin/env bash
# End-to-end test of personal access tokens and deploy tokens against a running Lajstrom
# (docs/auth.md, "Tokens"; docs/acceptance.md S-03, S-04, S-05, S-06, X-03 and X-14 as far as
# #44 goes, and the apt half of #44's e2e criterion: apt with user:token in auth.conf; the
# twine half waits for the pypi frontend, #31):
#
#   - a personal access token (PAT) through POST /api/v1/users/<user>/tokens with the admin's
#     password, its secret shown once, the list with the hash prefix only;
#   - the PAT as Basic on a raw PUT and as a bearer on whoami;
#   - an apt repo with a .deb built in ubuntu:24.04 and uploaded with the PAT; a PAT and a deploy
#     token with read for apt, each in /etc/apt/auth.conf.d (login <user> and login deploy),
#     apt-get update and install in ubuntu:24.04 ([trusted=yes] when the server has no master
#     key, signed-by key.asc when it has one), the token's lastUsedAt set by apt's requests, and
#     a token that does not exist refused by apt with 401;
#   - the deploy token with read: a raw PUT 404 (another repo), an apt upload 403 (no write); a
#     deploy token with write on the raw repo: PUT 201, DELETE 403, PATCH of the repo 403, a PUT
#     to the apt repo 404;
#   - oras login with the PAT (Basic on /v2/token) and oras push; a PAT with scopes read gets a
#     /v2 token with pull only, whose push is 403 DENIED, and its raw PUT is 403;
#   - the PAT revoked: the next request is 401; a PAT whose expiresAt is 2 s ahead: 200, then
#     401 once it has passed; an expiresAt 400 days ahead: 400.
#
#   test/e2e/tokens.sh [<base url> [<raw repo>]]     default: http://localhost:8080 scratch
#
# The raw repo must exist (the example configuration creates raw/scratch); the apt repo apt/e2e
# is created with lajstrom admin repo create --registry local when it does not exist. lajstrom
# admin runs straight against the index database of the configuration the server runs with:
# $LAJSTROM_CONFIG, else deploy/compose/lajstrom.example.yaml, with the same LAJSTROM_DATABASE_*
# overrides. The binary is $LAJSTROM_BIN, else bin/lajstrom, else go run ./cmd/lajstrom. With
# $LAJSTROM_SERVER_LOG, the server's log file, the script also checks that its access log names
# the principals, user:<uid> for the PAT and token:<id> for the deploy token, and holds no
# secret. Each run creates an admin user of its own and, at the end, revokes its tokens,
# deletes its raw file and disables the user; the apt package stays (apt versions cannot be
# deleted yet). Secrets go to curl in config files, to oras and lajstrom on standard input, and
# into the apt containers in a file, never on a command line, and are never printed: the script
# prints token ids and hash prefixes. It needs curl, jq and docker (ubuntu:24.04 for
# linux/amd64, or $APT_E2E_IMAGE, and ghcr.io/oras-project/oras:v1.3.6, with --network host),
# prints one line per step and exits non-zero at the first failure, naming the assertion or the
# command (lib.sh).
set -euo pipefail
. "$(dirname "$0")/lib.sh"
umask 077

base="${1:-http://localhost:8080}"
base="${base%/}"
raw="${2:-scratch}"
for tool in curl jq docker; do
	command -v "$tool" >/dev/null || { echo "tokens.sh: $tool is required" >&2; exit 2; }
done
export LAJSTROM_CONFIG="${LAJSTROM_CONFIG:-deploy/compose/lajstrom.example.yaml}"
unset LAJSTROM_ADMIN_PASSWORD # the password goes on standard input
if [ -n "${LAJSTROM_BIN:-}" ]; then
	lajstrom=("$LAJSTROM_BIN")
elif [ -x bin/lajstrom ]; then
	lajstrom=(bin/lajstrom)
else
	lajstrom=(go run ./cmd/lajstrom)
fi
image="${APT_E2E_IMAGE:-ubuntu:24.04}"
oras_image=ghcr.io/oras-project/oras:v1.3.6
registry="${base#*://}" # host:port of the facade, for oras
plain=""                # --plain-http for an http:// base URL
case "$base" in http://*) plain=--plain-http ;; esac
apt_repo=e2e
# apt sends auth.conf credentials over http only to a machine written with http://.
case "$base" in http://*) machine="$base/apt/$apt_repo" ;; *) machine="${base#https://}/apt/$apt_repo" ;; esac

work="$(mktemp -d)"
run="$(date -u +%Y%m%d%H%M%S)-$RANDOM"
user="e2e-tokens-$run"
pkg="e2e-tokens-$run" # the apt package
version=1.0.0-1
rawpath="e2e-tokens-$run/hello.txt"
status="" # status of the last request
cleanup() {
	local id
	if [ -f "$work/basic" ]; then
		curl -sS -o /dev/null -K "$work/basic" -X DELETE "$base/raw/$raw/$rawpath" || true
		for id in $(cat "$work/pats" 2>/dev/null); do
			curl -sS -o /dev/null -K "$work/basic" -X DELETE "$base/api/v1/users/$user/tokens/$id" || true
		done
		while read -r repo id; do
			curl -sS -o /dev/null -K "$work/basic" -X DELETE "$base/api/v1/repos/$repo/deploy-tokens/$id" || true
		done <"$work/deploys"
	fi
	"${lajstrom[@]}" admin user disable "$user" >/dev/null 2>&1 || true
	rm -rf "$work"
}
trap cleanup EXIT
: >"$work/pats"
: >"$work/deploys"

# sha256 prints the hex sha256 of a file.
sha256() {
	if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

# req CRED METHOD URL [curl args...]: one request with the curl config $work/CRED (Basic
# credentials or a bearer header), or none when CRED is -. The status goes to $status, the
# headers to $work/headers (lower-case names, no CR), the body to $work/body.
req() {
	local cred="$1" method="$2" url="$3"
	shift 3
	local k=()
	[ "$cred" = - ] || k=(-K "$work/$cred")
	status="$(curl -sS -X "$method" -o "$work/body" -D "$work/headers.raw" -w '%{http_code}' ${k[@]+"${k[@]}"} "$@" "$url")"
	tr -d '\r' <"$work/headers.raw" | awk -F': ' 'NF > 1 { $1 = tolower($1); print }' OFS=': ' >"$work/headers"
}

# hdr NAME prints the value of the response header NAME (lower case).
hdr() { awk -F': ' -v n="$1" '$1 == n { sub(/^[^:]*: /, ""); v = $0 } END { print v }' "$work/headers"; }

# basic NAME LOGIN SECRETFILE: the curl config $work/NAME with the Basic credentials LOGIN and
# the secret in SECRETFILE.
basic() { printf 'user = "%s:%s"\n' "$2" "$(cat "$3")" >"$work/$1"; }

# bearer NAME SECRETFILE: the curl config $work/NAME with Authorization: Bearer and the secret.
bearer() { printf 'header = "Authorization: Bearer %s"\n' "$(cat "$2")" >"$work/$1"; }

# create NAME COLLECTION BODY CRED: POST BODY to COLLECTION (a path below /api/v1) with CRED and
# expect 201 with its Location; the secret goes to $work/NAME, never printed, the token without
# it to $work/NAME.json, and its id to the list cleanup revokes.
create() {
	req "$4" POST "$base/api/v1/$2" -H 'Content-Type: application/json' -d "$3"
	expect "POST $2 status ($(jq -c 'del(.secret)' "$work/body" 2>/dev/null))" 201 "$status"
	jq -r .secret "$work/body" >"$work/$1"
	jq -c 'del(.secret)' "$work/body" >"$work/$1.json"
	rm -f "$work/body"
	local id
	id="$(jq -r .id "$work/$1.json")"
	expect "POST $2 Location" "/api/v1/$2/$id" "$(hdr location)"
	case "$2" in
	users/*) echo "$id" >>"$work/pats" ;;
	repos/*) echo "${2#repos/} $id" | sed 's|/deploy-tokens||' >>"$work/deploys" ;;
	esac
}

# info NAME prints the id, hash prefix, scopes and expiry of the token in $work/NAME.json.
info() { jq -r '"id \(.id), hashPrefix \(.hashPrefix), scopes \(.scopes | join(",") | if . == "" then "all" else . end), expiresAt \(.expiresAt // "never")"' "$work/$1.json"; }

# no_secret FILE WHAT: fail when FILE holds any secret made so far.
no_secret() {
	local s
	for s in "$work"/pat "$work"/pat-* "$work"/dep "$work"/dep-*; do
		[ -f "$s" ] && [ "${s%.json}" = "$s" ] && [ -s "$s" ] || continue
		case "$s" in *-basic | *-bearer) continue ;; esac
		! grep -qF -f "$s" "$1" || fail "$2 holds the secret of $(basename "$s")"
	done
}

# build: a .deb of $pkg in $work/deb, built with dpkg-deb in $image; prints its path.
build() {
	mkdir -p "$work/deb"
	docker run --rm --platform linux/amd64 -u "$(id -u):$(id -g)" -v "$work/deb:/w" -w /w "$image" bash -ec "
		mkdir -p pkg/DEBIAN pkg/usr/share/$pkg && echo 'hello from $pkg' > pkg/usr/share/$pkg/greeting
		printf 'Package: $pkg\nVersion: $version\nArchitecture: amd64\nMaintainer: Lajstrom e2e <e2e@lajstrom.dev>\nDescription: says hello\n' > pkg/DEBIAN/control
		dpkg-deb --root-owner-group --build pkg ${pkg}_${version}_amd64.deb >/dev/null && rm -r pkg" >"$work/build.log" 2>&1 ||
		{ cat "$work/build.log" >&2; fail "dpkg-deb --build $pkg"; }
	echo "$work/deb/${pkg}_${version}_amd64.deb"
}

# apt_get LOGIN SECRETFILE: apt-get update and install of $pkg in $image with
# /etc/apt/auth.conf.d/lajstrom.conf holding LOGIN and the secret of SECRETFILE; the log goes to
# $work/apt.log, and the status of the container is returned. Each run mounts a directory of its
# own: a directory removed and made again at the same path may be stale in the container.
apt_get() {
	local dir
	dir="$(mktemp -d "$work/apt.XXXXXX")"
	printf 'machine %s login %s password %s\n' "$machine" "$1" "$(cat "$2")" >"$dir/auth.conf"
	[ ! -f "$work/key.asc" ] || cp "$work/key.asc" "$dir/"
	local rc=0
	docker run --rm --network host --platform linux/amd64 -v "$dir:/w" "$image" bash -ec "
		rm -f /etc/apt/sources.list /etc/apt/sources.list.d/*
		install -m 0600 /w/auth.conf /etc/apt/auth.conf.d/lajstrom.conf
		rm /w/auth.conf
		if [ -f /w/key.asc ]; then install -D -m 0644 /w/key.asc /etc/apt/keyrings/$apt_repo.asc; fi
		echo 'deb [$source_opts] $base/apt/$apt_repo noble main' > /etc/apt/sources.list.d/$apt_repo.list
		apt-get update
		apt-get install -y --no-install-recommends $pkg
		echo \"greeting: \$(cat /usr/share/$pkg/greeting)\"" >"$work/apt.log" 2>&1 || rc=$?
	rm -rf "$dir"
	no_secret "$work/apt.log" "the apt log"
	return "$rc"
}

# claims FILE prints the payload of the JWT in FILE, one JSON line.
claims() { jq -Rc 'split(".") | .[1] | gsub("-"; "+") | gsub("_"; "/") | @base64d | fromjson' "$1"; }

# oras ARGS...: oras in its container, with the run's directory as /w and the registry
# configuration of the login there.
oras() {
	docker run --rm -i --network host -v "$work:/w" -w /w "$oras_image" "$@" --registry-config /w/oras-auth.json ${plain:+"$plain"}
}

# The admin user, with a password nobody sees.
password="$(head -c 24 /dev/urandom | base64 | tr -d '+/=\n')"
printf 'user = "%s:%s"\n' "$user" "$password" >"$work/basic"
out="$(printf '%s\n' "$password" | "${lajstrom[@]}" admin user create --name "$user" --admin --password-stdin)"
expect "admin user create name" "$user" "$(jq -r .name <<<"$out")"
uid="$(jq -r .uid <<<"$out")"
ok "admin user create --name $user --admin --password-stdin"

# A PAT, shown once (X-03).
create pat "users/$user/tokens" "{\"name\": \"e2e $run\"}" basic
pat_id="$(jq -r .id "$work/pat.json")"
expect "the PAT's prefix and length" "ljs_pat_ 51" "$(cut -c1-8 "$work/pat") $(tr -d '\n' <"$work/pat" | wc -c | tr -d ' ')"
ok "POST /api/v1/users/$user/tokens -u $user: 201, Location /api/v1/users/$user/tokens/$pat_id, $(info pat), the secret ljs_pat_… shown once"
req basic GET "$base/api/v1/users/$user/tokens"
expect "GET tokens status" 200 "$status"
expect "GET tokens: the hash prefix, no secret" "$(jq -r .hashPrefix "$work/pat.json") false" "$(jq -r '.tokens[0] | "\(.hashPrefix) \(has("secret"))"' "$work/body")"
no_secret "$work/body" "GET /api/v1/users/$user/tokens"
ok "GET /api/v1/users/$user/tokens: hashPrefix $(jq -r .hashPrefix "$work/pat.json"), no secret"
basic pat-basic "$user" "$work/pat"
bearer pat-bearer "$work/pat"

# The PAT as Basic on a frontend and as a bearer on the API.
printf 'tokens e2e %s\n' "$run" >"$work/hello.txt"
req pat-basic PUT "$base/raw/$raw/$rawpath" -T "$work/hello.txt"
expect "PUT /raw/$raw/$rawpath with the PAT status ($(cat "$work/body"))" 201 "$status"
ok "PUT /raw/$raw/$rawpath -u $user:<PAT>: 201 $(jq -r .digest "$work/body")"
req pat-bearer GET "$base/api/v1/whoami"
expect "whoami with the PAT as a bearer" "200 user:$uid $user" "$status $(jq -r '"\(.principal) \(.name)"' "$work/body")"
ok "GET /api/v1/whoami with Authorization: Bearer <PAT>: 200 {\"principal\":\"user:$uid\",\"name\":\"$user\"}"

# The apt repo and a package uploaded with the PAT.
req pat-bearer GET "$base/api/v1/repos/apt/$apt_repo"
case "$status" in
200) ok "apt/$apt_repo exists" ;;
404)
	"${lajstrom[@]}" admin repo create "apt/$apt_repo" --registry local >/dev/null
	ok "admin repo create apt/$apt_repo --registry local"
	;;
*) fail "GET /api/v1/repos/apt/$apt_repo: status $status" ;;
esac
deb="$(build)"
req pat-basic PUT "$base/apt/$apt_repo/upload?suite=noble&component=main" -T "$deb"
expect "PUT $(basename "$deb") with the PAT status ($(cat "$work/body"))" 201 "$status"
ok "dpkg-deb --build in $image, PUT /apt/$apt_repo/upload?suite=noble -u $user:<PAT>: 201 $(jq -r .digest "$work/body")"

# key.asc: signed-by when the server signs, [trusted=yes] when it has no master key.
status="$(curl -sS -o "$work/key.asc" -w '%{http_code}' -K "$work/basic" "$base/apt/$apt_repo/key.asc")"
case "$status" in
200) source_opts="signed-by=/etc/apt/keyrings/$apt_repo.asc"; ok "GET /apt/$apt_repo/key.asc: 200, the source is signed-by it" ;;
404) rm -f "$work/key.asc"; source_opts="trusted=yes"; ok "GET /apt/$apt_repo/key.asc: 404, the server does not sign: [trusted=yes]" ;;
*) fail "GET key.asc: status $status" ;;
esac

# apt with a PAT of its own (login <user>) and with a deploy token with read (login deploy);
# lastUsedAt, null before, shows that apt's requests carried the token.
create pat-apt "users/$user/tokens" "{\"name\": \"apt $run\", \"scopes\": [\"read\"]}" pat-bearer
create dep "repos/apt/$apt_repo/deploy-tokens" "{\"name\": \"e2e $run\", \"scopes\": [\"read\"]}" pat-bearer
dep_id="$(jq -r .id "$work/dep.json")"
expect "the deploy token's prefix" ljs_dep_ "$(cut -c1-8 "$work/dep")"
ok "POST /api/v1/repos/apt/$apt_repo/deploy-tokens with the PAT: 201, $(info dep)"
for login in "$user pat-apt users/$user/tokens" "deploy dep repos/apt/$apt_repo/deploy-tokens"; do
	read -r alogin afile acollection <<<"$login"
	apt_get "$alogin" "$work/$afile" || { cat "$work/apt.log" >&2; fail "apt-get with login $alogin"; }
	! grep -E '^(E|W): ' "$work/apt.log" >&2 || fail "apt reported errors or warnings with login $alogin"
	grep -qxF "greeting: hello from $pkg" "$work/apt.log" || { cat "$work/apt.log" >&2; fail "the installed file with login $alogin"; }
	req basic GET "$base/api/v1/$acollection"
	used="$(jq -r --argjson id "$(jq .id "$work/$afile.json")" '.tokens[] | select(.id == $id) | .lastUsedAt // "never"' "$work/body")"
	[ "$used" != never ] || fail "lastUsedAt of $afile is not set after apt-get with login $alogin"
	ok "apt-get update && apt-get install $pkg in $image [$source_opts], auth.conf login $alogin password <$(jq -r '"\(.kind) \(.id), scopes \(.scopes | join(","))"' "$work/$afile.json")>: installed; lastUsedAt $used"
done
printf 'ljs_dep_%s\n' "$(head -c 64 /dev/urandom | base64 | tr -dc '0-9A-Za-z' | cut -c1-43)" >"$work/dep-unknown"
if apt_get deploy "$work/dep-unknown"; then fail "apt-get with a deploy token that does not exist succeeded"; fi
grep -q '401' "$work/apt.log" || { cat "$work/apt.log" >&2; fail "apt-get with an unknown token: no 401"; }
ok "apt-get update with a deploy token that does not exist: fails, $(grep -m1 -o '401 *Unauthorized' "$work/apt.log" || echo 401)"

# S-04 and X-14: the deploy token with read reaches its repo only and does not write; one with
# write on the raw repo pushes there, never deletes or manages, and gets nothing elsewhere.
basic dep-basic deploy "$work/dep"
req dep-basic PUT "$base/raw/$raw/$rawpath.dep" -T "$work/hello.txt"
expect "PUT to raw/$raw with the apt deploy token" 404 "$status"
ok "PUT /raw/$raw/… -u deploy:<deploy token $dep_id, read on apt/$apt_repo>: 404 (no access at all): $(tr -d '\n' <"$work/body")"
req dep-basic PUT "$base/apt/$apt_repo/upload?suite=noble&component=main" -T "$deb"
expect "PUT to apt/$apt_repo with a read deploy token" 403 "$status"
ok "PUT /apt/$apt_repo/upload -u deploy:<deploy token $dep_id, read>: 403: $(tr -d '\n' <"$work/body")"
create dep-raw "repos/raw/$raw/deploy-tokens" "{\"name\": \"e2e $run\", \"scopes\": [\"write\"]}" basic
basic dep-raw-basic gitlab-ci-token "$work/dep-raw"
rawid="$(jq -r .id "$work/dep-raw.json")"
req dep-raw-basic PUT "$base/raw/$raw/$rawpath.ci" -T "$work/hello.txt"
expect "PUT with a write deploy token status ($(cat "$work/body"))" 201 "$status"
req dep-raw-basic DELETE "$base/raw/$raw/$rawpath.ci"
expect "DELETE with a write deploy token" 403 "$status"
req dep-raw-basic PATCH "$base/api/v1/repos/raw/$raw" -H 'Content-Type: application/json' -d '{"anonymousRead": true}'
expect "PATCH of its repo with a deploy token" "403 FORBIDDEN" "$status $(jq -r .error.code "$work/body")"
req dep-raw-basic PUT "$base/apt/$apt_repo/upload?suite=noble&component=main" -T "$deb"
expect "PUT to apt/$apt_repo with the raw deploy token" 404 "$status"
ok "deploy token $rawid with write on raw/$raw, as gitlab-ci-token:<token>: PUT 201, DELETE 403, PATCH /api/v1/repos/raw/$raw 403 FORBIDDEN, PUT to apt/$apt_repo 404"
curl -sS -o /dev/null -K "$work/basic" -X DELETE "$base/raw/$raw/$rawpath.ci" || true

# oras with the PAT: login through /v2/token with Basic <user>:<PAT>, then a push.
req - GET "$base/v2/"
expect "GET /v2/ status" 401 "$status"
service="$(hdr www-authenticate | sed -E 's/.*service="([^"]*)".*/\1/')"
oras login -u "$user" --password-stdin "$registry" <"$work/pat" >"$work/oras.log" 2>&1 || { cat "$work/oras.log" >&2; fail "oras login"; }
grep -q 'Login Succeeded' "$work/oras.log" || fail "oras login: $(cat "$work/oras.log")"
ok "oras login $registry -u $user --password-stdin <PAT>: Login Succeeded (Basic on /v2/token)"
name="raw/$raw/t"
printf 'tokens e2e oras %s\n' "$run" >"$work/t"
tver="$(sha256 "$work/t" | cut -c1-16)"
oras push --artifact-type application/vnd.lajstrom.raw.v1 \
	--annotation lajstrom.schema=1 --annotation lajstrom.format=raw --annotation "lajstrom.repo=$raw" \
	--annotation lajstrom.name=t --annotation "lajstrom.version=$tver" --annotation lajstrom.raw.path=t \
	--annotation "org.opencontainers.image.created=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
	"$registry/$name:$tver" t:application/vnd.lajstrom.raw.file.v1 >"$work/oras.log" 2>&1 ||
	{ cat "$work/oras.log" >&2; fail "oras push"; }
ok "oras push $registry/$name:$tver: $(grep '^Digest:' "$work/oras.log")"
req pat-basic GET "$base/v2/token?service=$service&scope=repository:$name:pull,push"
expect "GET /v2/token with the PAT status" 200 "$status"
jq -r .token "$work/body" >"$work/jwt-pat"
expect "the PAT's /v2 token" "user:$uid [\"pull\",\"push\"]" "$(claims "$work/jwt-pat" | jq -r '"\(.sub) \(.access[0].actions | tojson)"')"
ok "GET /v2/token -u $user:<PAT> scope=repository:$name:pull,push: sub user:$uid, actions pull,push"

# S-03: a PAT with read only pushes nothing, on the frontends and through its /v2 token.
create pat-read "users/$user/tokens" "{\"name\": \"read $run\", \"scopes\": [\"read\"]}" basic
basic pat-read-basic "$user" "$work/pat-read"
req pat-read-basic PUT "$base/raw/$raw/$rawpath.read" -T "$work/hello.txt"
expect "PUT with a read PAT" 403 "$status"
ok "PUT /raw/$raw/… -u $user:<PAT $(jq -r .id "$work/pat-read.json"), scopes read>: 403: $(tr -d '\n' <"$work/body")"
req pat-read-basic GET "$base/v2/token?service=$service&scope=repository:$name:pull,push"
expect "GET /v2/token with a read PAT status" 200 "$status"
jq -r .token "$work/body" >"$work/jwt-read"
expect "the read PAT's /v2 token actions" '["pull"]' "$(claims "$work/jwt-read" | jq -c '.access[0].actions')"
bearer jwt-read-bearer "$work/jwt-read"
req jwt-read-bearer POST "$base/v2/$name/blobs/uploads/"
expect "POST uploads with the read PAT's token" "403 DENIED" "$status $(jq -r '.errors[0].code' "$work/body")"
ok "GET /v2/token -u $user:<read PAT> scope=…:pull,push: actions [pull]; POST /v2/$name/blobs/uploads/ with it: 403 DENIED: $(jq -r '.errors[0].message' "$work/body")"

# S-05: a revoked PAT is refused at the next request.
req basic DELETE "$base/api/v1/users/$user/tokens/$pat_id"
expect "DELETE the PAT" 204 "$status"
req pat-bearer GET "$base/api/v1/whoami"
expect "whoami with the revoked PAT" 401 "$status"
req pat-basic PUT "$base/raw/$raw/$rawpath.revoked" -T "$work/hello.txt"
expect "PUT with the revoked PAT" 401 "$status"
req basic GET "$base/api/v1/users/$user/tokens"
ok "DELETE /api/v1/users/$user/tokens/$pat_id: 204; the next requests with it, bearer and Basic: 401; revokedAt $(jq -r --argjson id "$pat_id" '.tokens[] | select(.id == $id) | .revokedAt' "$work/body")"

# S-06: a PAT that expires 2 seconds from now works until then and is refused after.
now="$(date -u +%s)"
at="$(date -u -r "$((now + 2))" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -d "@$((now + 2))" +%Y-%m-%dT%H:%M:%SZ)"
create pat-exp "users/$user/tokens" "{\"name\": \"expiring $run\", \"expiresAt\": \"$at\"}" basic
bearer pat-exp-bearer "$work/pat-exp"
req pat-exp-bearer GET "$base/api/v1/whoami"
expect "whoami before the expiry" 200 "$status"
sleep 3
req pat-exp-bearer GET "$base/api/v1/whoami"
expect "whoami after the expiry" 401 "$status"
ok "a PAT with expiresAt $at: 200 at once, 401 three seconds later"
far="$(date -u -r "$((now + 400 * 86400))" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -d "@$((now + 400 * 86400))" +%Y-%m-%dT%H:%M:%SZ)"
req basic POST "$base/api/v1/users/$user/tokens" -H 'Content-Type: application/json' -d "{\"name\": \"far $run\", \"expiresAt\": \"$far\"}"
expect "a PAT 400 days ahead" "400 INVALID" "$status $(jq -r .error.code "$work/body")"
ok "a PAT with expiresAt $far: 400 $(jq -r .error.message "$work/body")"

# The access log names the principals and holds no secret.
if [ -n "${LAJSTROM_SERVER_LOG:-}" ]; then
	sleep 0.2
	grep -qE "principal=user:$uid|\"principal\":\"user:$uid\"" "$LAJSTROM_SERVER_LOG" || fail "the access log has no principal user:$uid"
	grep -qE "principal=token:$dep_id|\"principal\":\"token:$dep_id\"" "$LAJSTROM_SERVER_LOG" || fail "the access log has no principal token:$dep_id"
	no_secret "$LAJSTROM_SERVER_LOG" "the server log"
	ok "server log: principal=user:$uid ($(grep -cE "principal=user:$uid|\"principal\":\"user:$uid\"" "$LAJSTROM_SERVER_LOG") lines), principal=token:$dep_id ($(grep -cE "principal=token:$dep_id|\"principal\":\"token:$dep_id\"" "$LAJSTROM_SERVER_LOG") lines), no secret"
fi
echo "PASS tokens e2e against $base as $user"

#!/usr/bin/env bash
# End-to-end test of a raw repo with curl against a running Lajstrom (docs/acceptance.md §3.2,
# docs/quickstart/raw.md): R-01, R-02, R-03, a single range, R-04, the rollback of the quick
# start (a version deleted through the management API, the path's other versions still served),
# R-09, R-10, the size cap of a repo made with --policy '{"maxFileSize": 1048576}' (a larger
# upload is 413), then R-07: DELETE and a 404.
#
#   test/e2e/raw.sh [<base url> [<repo>]]     default: http://localhost:8080 scratch
#
# The repo must exist (the example configuration creates raw/scratch), and the user of
# LAJSTROM_E2E_USER and LAJSTROM_E2E_PASSWORD must write and delete in it (run.sh makes an admin;
# lib.sh, e2e_credentials). Every request carries those credentials, from a curl configuration
# file. Every path starts with a directory of its own for the run, so the script can run again
# against the same repo, and it deletes what it uploaded at the end. The size cap's repo,
# raw/<run>, is made and deleted with lajstrom admin repo, as the quick start makes one: straight
# against the index database of $LAJSTROM_CONFIG, else deploy/compose/lajstrom.example.yaml, with
# the binary $LAJSTROM_BIN, else bin/lajstrom, else go run ./cmd/lajstrom; the user writes and
# deletes in it too (an admin may). It needs curl and jq, prints one line per step and exits
# non-zero at the first failure, naming the assertion or the command (lib.sh). make e2e runs it
# (test/e2e/run.sh).
set -euo pipefail
. "$(dirname "$0")/lib.sh"

base="${1:-http://localhost:8080}"
repo="${2:-scratch}"
for tool in curl jq; do
	command -v "$tool" >/dev/null || { echo "raw.sh: $tool is required" >&2; exit 2; }
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
limited="" # the size cap's repo while it exists
cleanup() {
	if [ -n "$limited" ]; then
		curl -sS -o /dev/null -K "$E2E_CURLRC" -X DELETE "$base/raw/$limited/at-limit.bin" || true
		"${lajstrom[@]}" admin repo delete "raw/$limited" >/dev/null 2>&1 || true
	fi
	rm -rf "$work"
}
trap cleanup EXIT
e2e_credentials "$work"
run="e2e-$(date -u +%Y%m%d%H%M%S)-$RANDOM"
created=()  # every path uploaded, for the cleanup
status=""   # status of the last request
digest=""   # manifest digest of the last put

# sha256 prints the hex sha256 of a file.
sha256() {
	if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

# urlpath percent-encodes each component of a path and keeps the slashes.
urlpath() { jq -rn --arg p "$1" '$p | split("/") | map(@uri) | join("/")'; }

# req METHOD PATH [curl args...]: one request to the path below the repo, already encoded. The
# status goes to $status, the headers to $work/headers (lower-case names, no CR), the body to
# $work/body (empty for HEAD).
req() {
	local method="$1" path="$2" out="$work/body"
	shift 2
	local how=(-X "$method")
	if [ "$method" = HEAD ]; then
		how=(-I) out=/dev/null
		: >"$work/body"
	fi
	status="$(curl -sS --globoff -K "$E2E_CURLRC" "${how[@]}" -o "$out" -D "$work/headers.raw" -w '%{http_code}' "$@" "$base/raw/$repo/$path")"
	tr -d '\r' <"$work/headers.raw" | awk -F': ' 'NF > 1 { $1 = tolower($1); print }' OFS=': ' >"$work/headers"
}

# hdr NAME prints the value of the response header NAME (lower case), the last one sent.
hdr() { awk -F': ' -v n="$1" '$1 == n { sub(/^[^:]*: /, ""); v = $0 } END { print v }' "$work/headers"; }

# put FILE PATH: PUT FILE to PATH, check 201, Location and the body; the digest goes to $digest.
put() {
	local file="$1" path="$2" encoded
	encoded="$(urlpath "$path")"
	req PUT "$encoded" -T "$file" -H 'Content-Type: text/plain'
	expect "PUT $path status" 201 "$status"
	expect "PUT $path Location" "/raw/$repo/$encoded" "$(hdr location)"
	expect "PUT $path body path" "$path" "$(jq -r .path "$work/body")"
	digest="$(hdr docker-content-digest)"
	expect "PUT $path Docker-Content-Digest" "$(jq -r .digest "$work/body")" "$digest"
	created+=("$path")
}

# get PATH [curl args...]: GET PATH into $work/body and check 200.
get() {
	local path="$1"
	shift
	req GET "$(urlpath "$path")" "$@"
	expect "GET $path status" 200 "$status"
}

# listing PREFIX: GET the listing of PREFIX into $work/body, check 200, and write its paths, one
# per line, to $work/paths.
listing() {
	req GET "$(urlpath "$1")"
	expect "GET listing '$1' status" 200 "$status"
	jq -r '.entries[].path' "$work/body" >"$work/paths"
}

# count PATH: how often PATH is in $work/paths.
count() { grep -cxF -- "$1" "$work/paths" || true; }

file="$run/docs/file.txt"
head -c 4000 /dev/urandom >"$work/v1"
head -c 3000 /dev/urandom >"$work/v2"

# R-01: PUT, GET returns the exact bytes, HEAD has the same headers.
put "$work/v1" "$file"
d1="$digest"
get "$file"
cmp -s "$work/v1" "$work/body" || fail "R-01 GET: the bytes differ from the file"
expect "R-01 GET Content-Length" 4000 "$(hdr content-length)"
expect "R-01 GET Content-Type" text/plain "$(hdr content-type)"
expect "R-01 GET ETag" "\"sha256:$(sha256 "$work/v1")\"" "$(hdr etag)"
# Accept-Ranges is the backend's (http-routing.md §2, §3): distribution sends it on a GET, Zot
# only on a HEAD. The range below is 206 either way.
case "$(hdr accept-ranges)" in "" | bytes) ;; *) fail "R-01 GET Accept-Ranges: '$(hdr accept-ranges)', want 'bytes' or none" ;; esac
expect "R-01 GET Docker-Content-Digest" "$d1" "$(hdr docker-content-digest)"
lastmod="$(hdr last-modified)"
[ -n "$lastmod" ] || fail "R-01 GET: no Last-Modified"
etag="$(hdr etag)"
req HEAD "$(urlpath "$file")"
expect "R-01 HEAD status" 200 "$status"
expect "R-01 HEAD Content-Length" 4000 "$(hdr content-length)"
expect "R-01 HEAD Content-Type" text/plain "$(hdr content-type)"
expect "R-01 HEAD ETag" "$etag" "$(hdr etag)"
expect "R-01 HEAD Last-Modified" "$lastmod" "$(hdr last-modified)"
req GET "$(urlpath "$file")" -H "If-None-Match: $etag"
expect "R-01 GET with If-None-Match status" 304 "$status"
ok "R-01 PUT $file: 201 with $d1; GET returns the bytes; HEAD has the headers; If-None-Match is 304"

# R-02: the same bytes again are 201 with the same digest.
put "$work/v1" "$file"
expect "R-02 digest of the repeat" "$d1" "$digest"
ok "R-02 PUT of the same bytes: 201, the same digest"

# R-03: new bytes are a new version; ?digest= pins the old one.
put "$work/v2" "$file"
d2="$digest"
[ "$d2" != "$d1" ] || fail "R-03: new bytes kept the digest $d1"
get "$file"
cmp -s "$work/v2" "$work/body" || fail "R-03 GET: not the new bytes"
get "$file" -G --data-urlencode "digest=$d1"
cmp -s "$work/v1" "$work/body" || fail "R-03 GET ?digest=: not the old bytes"
ok "R-03 new bytes: $d2; GET returns them, ?digest=$d1 the old ones"

# A single range is 206 with exactly those bytes.
req GET "$(urlpath "$file")" -r 1000-1999
expect "range status" 206 "$status"
expect "range Content-Range" "bytes 1000-1999/3000" "$(hdr content-range)"
tail -c +1001 "$work/v2" | head -c 1000 >"$work/range"
cmp -s "$work/range" "$work/body" || fail "range: the body is not bytes 1000-1999"
ok "range 1000-1999: 206 with those 1000 bytes"

# R-04: the listing of the root and of a prefix shows both versions, sorted.
put "$work/v1" "$run/docs/a.txt"
listing ""
expect "R-04 $run/docs/a.txt in the root listing" 1 "$(count "$run/docs/a.txt")"
expect "R-04 versions of $file in the root listing" 2 "$(count "$file")"
listing "$run/docs/"
expect "R-04 listing of $run/docs/" "$(printf '%s\n' "$run/docs/a.txt" "$file" "$file")" "$(cat "$work/paths")"
expect "R-04 digests of $file" "$(printf '%s\n' "$d1" "$d2")" \
	"$(jq -r --arg p "$file" '.entries[] | select(.path == $p) | .digest' "$work/body")"
ok "R-04 the listings of / and $run/docs/ show both versions, sorted, with the manifest digests"

# The rollback of the quick start (§4): one version deleted through the management API, its
# version the first 16 hex digits of the sha256 of its bytes and the path's '/' escaped as %2F;
# the path's other versions are still served, the newest of them by GET.
head -c 2000 /dev/urandom >"$work/v3"
put "$work/v3" "$file"
d3="$digest"
v3="$(jq -r .version "$work/body")"
expect "the version of the third file" "$(sha256 "$work/v3" | cut -c1-16)" "$v3"
assert_status 204 -K "$E2E_CURLRC" -X DELETE \
	"$base/api/v1/repos/raw/$repo/packages/$(jq -rn --arg p "$file" '$p | @uri')/versions/$v3"
listing "$run/docs/"
expect "the digests of $file after the version delete" "$(printf '%s\n' "$d1" "$d2")" \
	"$(jq -r --arg p "$file" '.entries[] | select(.path == $p) | .digest' "$work/body")"
get "$file"
cmp -s "$work/v2" "$work/body" || fail "GET after the version delete: not the bytes of $d2"
get "$file" -G --data-urlencode "digest=$d1"
cmp -s "$work/v1" "$work/body" || fail "GET ?digest=$d1 after the version delete: not its bytes"
req GET "$(urlpath "$file")" -G --data-urlencode "digest=$d3"
expect "GET ?digest=$d3 of the deleted version status" 404 "$status"
ok "DELETE /api/v1/repos/raw/$repo/packages/…/versions/$v3, the newest of three versions: 204; the other two listed and served, GET the second; ?digest= of the deleted one 404"

# R-09: a raw path is unique without regard to case, so after Docs/Readme.MD the path
# docs/readme.md is 409 naming it; paths that differ in script and length are versions of their
# own.
long="$run/long/$(printf 'x%.0s' $(seq 1 300)).txt"
r09=("$run/Docs/Readme.MD" "$run/с пробелом/файл.txt" "$long")
twin="$run/docs/readme.md"
for i in "${!r09[@]}"; do
	printf 'R-09 file %d\n' "$i" >"$work/r09-$i"
	put "$work/r09-$i" "${r09[$i]}"
	[ "$i" = 0 ] || continue
	req PUT "$(urlpath "$twin")" -T "$work/r09-$i"
	expect "R-09 PUT $twin status" 409 "$status"
	grep -qF "exists as \"${r09[0]}\"" "$work/body" || fail "R-09 PUT $twin: the 409 does not name ${r09[0]}: $(cat "$work/body")"
done
for i in "${!r09[@]}"; do
	get "${r09[$i]}"
	cmp -s "$work/r09-$i" "$work/body" || fail "R-09 GET ${r09[$i]}: not its bytes"
done
listing "$run/"
for p in "${r09[@]}"; do
	expect "R-09 '$p' in the listing" 1 "$(count "$p")"
done
expect "R-09 '$twin' in the listing" 0 "$(count "$twin")"
ok "R-09 $twin after ${r09[0]}: 409 naming it; three paths, three versions; the listing shows the original paths"

# R-10: an empty file.
: >"$work/empty"
put "$work/empty" "$run/empty"
get "$run/empty"
expect "R-10 Content-Length" 0 "$(hdr content-length)"
[ ! -s "$work/body" ] || fail "R-10: GET of the empty file has a body"
ok "R-10 an empty file: 201; GET has Content-Length 0"

# The size cap of the quick start (§1): a repo made with the policy maxFileSize takes a file of
# that size and refuses a larger one with 413, keeping nothing of it.
limited="$run"
"${lajstrom[@]}" admin repo create "raw/$limited" --registry local --policy '{"maxFileSize": 1048576}' >/dev/null
head -c 1048576 /dev/urandom >"$work/limit"
head -c 1048577 /dev/urandom >"$work/over"
assert_status 201 -K "$E2E_CURLRC" -o /dev/null -T "$work/limit" "$base/raw/$limited/at-limit.bin"
assert_status 413 -K "$E2E_CURLRC" -T "$work/over" "$base/raw/$limited/over-limit.bin"
assert_status 404 -K "$E2E_CURLRC" -o /dev/null "$base/raw/$limited/over-limit.bin"
assert_status 204 -K "$E2E_CURLRC" -X DELETE "$base/raw/$limited/at-limit.bin"
"${lajstrom[@]}" admin repo delete "raw/$limited" >/dev/null
limited=""
ok "raw/$run with --policy '{\"maxFileSize\": 1048576}': PUT of 1048576 bytes 201, of 1048577 bytes 413 and not stored; the repo deleted"

# R-07, the delete by a principal that may: DELETE removes every version of the path; GET is
# 404 after it.
req DELETE "$(urlpath "$file")"
expect "R-07 DELETE $file status" 204 "$status"
req GET "$(urlpath "$file")"
expect "R-07 GET $file after DELETE status" 404 "$status"
ok "R-07 DELETE $file: 204; GET is 404 after it"

# Cleanup: the other paths of the run.
n=0
while IFS= read -r p; do
	[ "$p" = "$file" ] && continue
	req DELETE "$(urlpath "$p")"
	expect "DELETE $p status" 204 "$status"
	n=$((n + 1))
done < <(printf '%s\n' "${created[@]}" | sort -u)
listing ""
[ "$(grep -c "^$run/" "$work/paths" || true)" = 0 ] || fail "cleanup: paths of the run are still listed"
ok "cleanup: deleted the other $n paths of the run"
echo "PASS raw e2e against $base/raw/$repo/"

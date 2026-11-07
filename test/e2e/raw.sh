#!/usr/bin/env bash
# End-to-end test of a raw repo with curl against a running Lajstrom (docs/acceptance.md §3.2):
# R-01, R-02, R-03, R-04, a single range, R-09, R-10, then DELETE and a 404.
#
#   test/e2e/raw.sh [<base url> [<repo>]]     default: http://localhost:8080 scratch
#
# The repo must exist (the example configuration creates raw/scratch) and be writable without
# credentials, as on the dev stack. Every path starts with a directory of its own for the run, so
# the script can run again against the same repo, and it deletes what it uploaded at the end. It
# needs curl and jq, prints one line per step and exits non-zero at the first failure. make e2e
# runs it once #34 lands; until then run it by hand.
set -euo pipefail

base="${1:-http://localhost:8080}"
repo="${2:-scratch}"
for tool in curl jq; do
	command -v "$tool" >/dev/null || { echo "raw.sh: $tool is required" >&2; exit 2; }
done

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
run="e2e-$(date -u +%Y%m%d%H%M%S)-$RANDOM"
created=()  # every path uploaded, for the cleanup
status=""   # status of the last request
digest=""   # manifest digest of the last put

fail() { echo "FAIL $*" >&2; exit 1; }
ok() { echo "ok   $*"; }

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
	status="$(curl -sS --globoff "${how[@]}" -o "$out" -D "$work/headers.raw" -w '%{http_code}' "$@" "$base/raw/$repo/$path")"
	tr -d '\r' <"$work/headers.raw" | awk -F': ' 'NF > 1 { $1 = tolower($1); print }' OFS=': ' >"$work/headers"
}

# hdr NAME prints the value of the response header NAME (lower case), the last one sent.
hdr() { awk -F': ' -v n="$1" '$1 == n { sub(/^[^:]*: /, ""); v = $0 } END { print v }' "$work/headers"; }

# expect WHAT WANT GOT
expect() { [ "$2" = "$3" ] || fail "$1: got '$3', want '$2'"; }

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
expect "R-01 GET Accept-Ranges" bytes "$(hdr accept-ranges)"
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

# R-09: four paths that differ in case, script and length are four versions.
long="$run/long/$(printf 'x%.0s' $(seq 1 300)).txt"
r09=("$run/Docs/Readme.MD" "$run/docs/readme.md" "$run/с пробелом/файл.txt" "$long")
for i in "${!r09[@]}"; do
	printf 'R-09 file %d\n' "$i" >"$work/r09-$i"
	put "$work/r09-$i" "${r09[$i]}"
done
for i in "${!r09[@]}"; do
	get "${r09[$i]}"
	cmp -s "$work/r09-$i" "$work/body" || fail "R-09 GET ${r09[$i]}: not its bytes"
done
listing "$run/"
for p in "${r09[@]}"; do
	expect "R-09 '$p' in the listing" 1 "$(count "$p")"
done
ok "R-09 four paths, four versions; the listing shows the original paths"

# R-10: an empty file.
: >"$work/empty"
put "$work/empty" "$run/empty"
get "$run/empty"
expect "R-10 Content-Length" 0 "$(hdr content-length)"
[ ! -s "$work/body" ] || fail "R-10: GET of the empty file has a body"
ok "R-10 an empty file: 201; GET has Content-Length 0"

# DELETE removes every version of the path; GET is 404 after it.
req DELETE "$(urlpath "$file")"
expect "DELETE $file status" 204 "$status"
req GET "$(urlpath "$file")"
expect "GET $file after DELETE status" 404 "$status"
ok "DELETE $file: 204; GET is 404 after it"

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

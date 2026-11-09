#!/usr/bin/env bash
# End-to-end test of an apt repo with real clients against a running Lajstrom (#42, #48;
# docs/acceptance.md A-01, A-02, A-03, A-04, A-06, A-07, E-07):
#
#   - two packages built with dpkg-deb, one uploaded with PUT …/upload, the other pushed with oras
#     through the OCI facade with the deb profile (its metadata layer from lajstrom admin deb
#     metadata);
#   - the repo's key.asc into /etc/apt/keyrings, the source signed-by it ([trusted=yes] when the
#     server has no master key and key.asc is 404);
#   - anonymousRead on, through PATCH /api/v1/repos/apt/<repo>: apt-get update without
#     credentials, the Packages fetched by-hash as the Release says, apt-get install of both and
#     apt-cache policy;
#   - anonymousRead off: apt-get update without credentials fails with apt's 401; with the
#     credentials in /etc/apt/auth.conf.d, apt-get update by-hash, install of both again, apt-get
#     download of both and their sha256 against the uploaded files, apt-cache policy;
#   - a second apt-get update, with Acquire::By-Hash=no: the Packages fetched by name;
#   - a copy of the suite whose Release has the SHA256 of each index one digit off, read from a
#     file: source: apt-get update fails, and its own "Hash Sum mismatch" error is printed, as a
#     broken Release from the server would make the steps above fail with it;
#   - the same bytes again (201, the same digest); other bytes under the same name, version and
#     architecture in the same suite (409, the tag) and in another suite (409, the pool file name);
#   - both versions deleted through DELETE /api/v1/repos/apt/<repo>/packages/<name>/versions/<v>
#     (#61): the next Packages and apt-get update have neither.
#
#   test/e2e/apt.sh [<base url> [<repo>]]     default: http://localhost:8080 ubuntu
#
# The apt repo must exist (lajstrom admin repo create apt/<repo> --registry local), and the user
# of LAJSTROM_E2E_USER and LAJSTROM_E2E_PASSWORD must write to it and manage it, to change its
# anonymousRead (run.sh makes an admin; lib.sh, e2e_credentials): curl sends them from a curl
# configuration file, oras logs in with them, and apt-get has them in /etc/apt/auth.conf.d. The
# server must have auth.anonymousRead: true (run.sh sets it), else the anonymous step fails and
# says so; on exit the repo's anonymousRead is set back to what it was. The packages are named
# after the run, so the script can run again against the same repo; the last step deletes them
# through the API (#61), which needs delete on the repo too, and a run that fails before it
# leaves them. With $LAJSTROM_SERVER_LOG, the server's log file (run.sh sets it),
# the script reads in its access log how apt fetched the Packages (by-hash or by name) and as
# whom; without it those checks are left out and the steps say so.
#
# It needs curl, jq and docker: the image ubuntu:24.04 for linux/amd64, emulated on an arm64 host,
# or $APT_E2E_IMAGE (ubuntu:22.04 and debian:12 pass too), which builds the packages and is the
# apt client: one container that runs the apt steps in turn and is removed on exit; and
# ghcr.io/oras-project/oras:v1.3.6. The containers use --network host, which reaches the base URL
# on Linux and under OrbStack. lajstrom admin deb metadata runs from $LAJSTROM_BIN, else
# bin/lajstrom, else go run ./cmd/lajstrom. It prints one line per step and exits non-zero at the
# first failure, naming the assertion or the command (lib.sh). make e2e runs it
# (test/e2e/run.sh).
set -euo pipefail
. "$(dirname "$0")/lib.sh"

base="${1:-http://localhost:8080}"
base="${base%/}"
repo="${2:-ubuntu}"
for tool in curl jq docker; do
	command -v "$tool" >/dev/null || { echo "apt.sh: $tool is required" >&2; exit 2; }
done
root="$(cd "$(dirname "$0")/../.." && pwd)"
image="${APT_E2E_IMAGE:-ubuntu:24.04}"
oras_image=ghcr.io/oras-project/oras:v1.3.6
registry="${base#*://}" # host:port of the OCI facade, for oras
plain="" # --plain-http for an http:// base URL
case "$base" in http://*) plain=--plain-http ;; esac

work="$(mktemp -d)"
started=""   # set once the client container runs
anonymous="" # the repo's anonymousRead before the run, set back on exit
cleanup() {
	if [ -n "$anonymous" ]; then
		curl -sS -o /dev/null -K "$E2E_CURLRC" -X PATCH -H 'Content-Type: application/json' \
			-d "{\"anonymousRead\": $anonymous}" "$base/api/v1/repos/apt/$repo" || true
	fi
	if [ -n "$started" ]; then
		docker exec "$client" chown -R "$(id -u):$(id -g)" /w >/dev/null 2>&1 || true
		docker rm -f "$client" >/dev/null 2>&1 || true
	fi
	rm -rf "$work"
}
trap cleanup EXIT
e2e_credentials "$work"
# apt sends auth.conf credentials over http only to a machine written with http://.
case "$base" in http://*) machine="$base/apt/$repo" ;; *) machine="${base#https://}/apt/$repo" ;; esac
run="$(date -u +%Y%m%d%H%M%S)-$RANDOM"
one="e2e-put-$run"   # uploaded with PUT
two="e2e-oras-$run"  # pushed with oras
version=1.0.0-1      # needs no escaping in a tag: digits, dots and a hyphen
status=""            # status of the last request
server_log=""
if [ -n "${LAJSTROM_SERVER_LOG:-}" ] && [ -f "$LAJSTROM_SERVER_LOG" ]; then server_log="$LAJSTROM_SERVER_LOG"; fi
client="lajstrom-e2e-apt-$run" # the container of the apt steps, with $work/client as /w

# sha256 prints the hex sha256 of a file.
sha256() {
	if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

# deb_metadata FILE prints the deb metadata layer of FILE, an absolute path: lajstrom admin deb
# metadata of $LAJSTROM_BIN, bin/lajstrom, or go run in the repository.
deb_metadata() {
	if [ -n "${LAJSTROM_BIN:-}" ]; then
		"$LAJSTROM_BIN" admin deb metadata "$1"
	elif [ -x "$root/bin/lajstrom" ]; then
		"$root/bin/lajstrom" admin deb metadata "$1"
	else
		(cd "$root" && go run ./cmd/lajstrom admin deb metadata "$1")
	fi
}

# hdr NAME prints the value of the response header NAME (lower case) of the last request.
hdr() {
	tr -d '\r' <"$work/headers" | awk -v n="$1" 'index(tolower($0), n ": ") == 1 { sub(/^[^:]*: /, ""); v = $0 } END { print v }'
}

# put FILE SUITE: PUT FILE to upload?suite=SUITE&component=main; the status goes to $status, the
# headers to $work/headers, the body to $work/body.
put() {
	status="$(curl -sS -K "$E2E_CURLRC" -o "$work/body" -D "$work/headers" -w '%{http_code}' -T "$1" \
		"$base/apt/$repo/upload?suite=$2&component=main")"
}

# build DIR NAME GREETING: build NAME_1.0.0-1_amd64.deb in $work/DIR/ with dpkg-deb in a
# container, and print its path: one file, /usr/share/NAME/greeting, that says GREETING.
build() {
	local dir="$work/$1" name="$2" greeting="$3"
	mkdir -p "$dir"
	docker run --rm --platform linux/amd64 -u "$(id -u):$(id -g)" -v "$dir:/w" -w /w "$image" bash -ec "
		mkdir -p pkg/DEBIAN pkg/usr/share/$name && echo '$greeting' > pkg/usr/share/$name/greeting
		printf 'Package: $name\nVersion: $version\nArchitecture: amd64\nMaintainer: Lajstrom e2e <e2e@lajstrom.dev>\nDescription: says $greeting\n' > pkg/DEBIAN/control
		dpkg-deb --root-owner-group --build pkg ${name}_${version}_amd64.deb >/dev/null && rm -r pkg" >"$work/build.log" 2>&1 ||
		{ cat "$work/build.log" >&2; fail "dpkg-deb --build $name"; }
	echo "$dir/${name}_${version}_amd64.deb"
}

# anonymous_read ON: PATCH the repo's anonymousRead to ON (true or false) as $E2E_USER.
anonymous_read() {
	status="$(curl -sS -K "$E2E_CURLRC" -o "$work/body" -w '%{http_code}' -X PATCH -H 'Content-Type: application/json' \
		-d "{\"anonymousRead\": $1}" "$base/api/v1/repos/apt/$repo")"
	expect "PATCH /api/v1/repos/apt/$repo anonymousRead $1 ($(cat "$work/body"))" 200 "$status"
	expect "anonymousRead after the PATCH" "$1" "$(jq -r .anonymousRead "$work/body")"
}

# anon PATH prints the status of an anonymous GET of the repo's PATH.
anon() { curl -sS -o /dev/null -w '%{http_code}' "$base/apt/$repo/$1"; }

# in_client LOG SCRIPT: run SCRIPT with bash -e as root in the client container, its output in
# $work/LOG; the status is the script's.
in_client() { docker exec "$client" bash -ec "$2" >"$work/$1" 2>&1; }

# quiet LOG: fail when apt wrote an error or a warning to $work/LOG.
quiet() {
	if grep -E '^(E|W): ' "$work/$1" >&2; then fail "apt reported errors or warnings ($1)"; fi
}

# policy LOG: the apt-cache policy in $work/LOG has both packages, with the candidate $version
# from the repo's jammy/main.
policy() {
	local n m
	n="$(grep -cxF "  Candidate: $version" "$work/$1" || true)"
	m="$(grep -cF " 500 $base/apt/$repo jammy/main amd64 Packages" "$work/$1" || true)"
	[ "$n $m" = "2 2" ] || { cat "$work/$1" >&2; fail "apt-cache policy $one $two: $n candidates $version and $m from $base/apt/$repo jammy/main, want 2 and 2 ($1)"; }
}

# mark prints the number of lines of the server log, 0 without one.
mark() { if [ -n "$server_log" ]; then wc -l <"$server_log" | tr -d ' '; else echo 0; fi; }

# fetched FROM WHO HOW: the GETs of dists/jammy/main/binary-amd64/ in the access log after line
# FROM of the server log were all 200 to WHO (anonymous, user:<name>), and asked for the Packages
# by-hash (HOW by-hash) or by name (HOW name). Prints them, "GET by-hash/SHA256/0cf0486af20a… 200
# as anonymous", or, without a server log, that it did not look.
fetched() {
	local dir="/apt/$repo/dists/jammy/main/binary-amd64/" got
	if [ -z "$server_log" ]; then echo "how apt fetched the Packages is not checked without a server log"; return 0; fi
	got="$(tail -n "+$(($1 + 1))" "$server_log" | awk -v dir="$dir" '/ msg=request / && / method=GET / {
		p = s = u = ""
		for (i = 1; i <= NF; i++) {
			if (index($i, "path=") == 1) p = substr($i, 6)
			else if (index($i, "status=") == 1) s = substr($i, 8)
			else if (index($i, "principal=") == 1) u = substr($i, 11)
		}
		if (index(p, dir) == 1) print substr(p, length(dir) + 1), s, u
	}')"
	[ -n "$got" ] || fail "no GET of $dir in the server log after line $1"
	if printf '%s\n' "$got" | awk -v who="$2" '$2 != 200 || $3 != who' | grep -q .; then
		fail "the GETs of $dir were not all 200 to $2: $got"
	fi
	case "$3" in
	by-hash) if printf '%s\n' "$got" | grep -qv '^by-hash/SHA256/'; then fail "apt got a Packages by name, by-hash on: $got"; fi ;;
	name) if printf '%s\n' "$got" | grep -q '^by-hash/'; then fail "apt got a Packages by-hash with Acquire::By-Hash=no: $got"; fi ;;
	esac
	printf '%s\n' "$got" | awk '{ p = $1; if (length(p) > 27) p = substr(p, 1, 27) "…"; printf "%sGET %s %s", (NR > 1 ? ", " : ""), p, $2 } END { print " as " who }' who="$2"
}

deb1="$(build one "$one" "hello from $one")"
deb2="$(build two "$two" "hello from $two")"
other="$(build other "$one" "other bytes")" # the name, version and architecture of the first
cmp -s "$deb1" "$other" && fail "the two builds of $one have the same bytes"
ok "dpkg-deb --build: $(basename "$deb1"), $(basename "$deb2"), and other bytes under the first name"

# A-01: PUT the first package to jammy/main.
put "$deb1" jammy
expect "PUT $one status ($(cat "$work/body"))" 201 "$status"
expect "PUT $one Location" "/apt/$repo/pool/main/e/$one/${one}_${version}_amd64.deb" "$(hdr location)"
digest1="$(jq -r .digest "$work/body")"
expect "PUT $one Docker-Content-Digest" "$digest1" "$(hdr docker-content-digest)"
expect "PUT $one body" "$one $version amd64 jammy main ${one}_${version}_amd64.deb ${version}__jammy_2fmain_2famd64" \
	"$(jq -r '[.package, .version, .architecture, .suite, .component, .filename, .tag] | join(" ")' "$work/body")"
ok "PUT $(basename "$deb1") to jammy/main: 201 with $digest1, Location in the pool"

# A-02: oras push of the second package with the deb profile, the metadata layer from
# lajstrom admin deb metadata.
deb_metadata "$deb2" >"$work/two/metadata.json" || fail "lajstrom admin deb metadata $deb2"
expect "metadata filename" "${two}_${version}_amd64.deb" "$(jq -r .filename "$work/two/metadata.json")"
expect "metadata sha256" "$(sha256 "$deb2")" "$(jq -r .sha256 "$work/two/metadata.json")"
docker run --rm -i --network host -u "$(id -u):$(id -g)" -v "$work/two:/w" -w /w "$oras_image" login ${plain:+"$plain"} \
	--registry-config /w/oras-auth.json -u "$E2E_USER" --password-stdin "$registry" <<<"$E2E_PASSWORD" >"$work/oras.log" 2>&1 ||
	{ cat "$work/oras.log" >&2; fail "oras login"; }
docker run --rm --network host -u "$(id -u):$(id -g)" -v "$work/two:/w" -w /w "$oras_image" push ${plain:+"$plain"} \
	--registry-config /w/oras-auth.json \
	--artifact-type application/vnd.lajstrom.deb.v1 \
	--annotation lajstrom.schema=1 --annotation lajstrom.format=apt --annotation "lajstrom.repo=$repo" \
	--annotation "lajstrom.name=$two" --annotation "lajstrom.version=$version" \
	--annotation lajstrom.variant=jammy/main/amd64 --annotation lajstrom.deb.suite=jammy \
	--annotation lajstrom.deb.component=main --annotation lajstrom.deb.arch=amd64 \
	--annotation "org.opencontainers.image.created=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
	"$registry/apt/$repo/$two:${version}__jammy_2fmain_2famd64" \
	"${two}_${version}_amd64.deb:application/vnd.lajstrom.deb.package.v1" \
	metadata.json:application/vnd.lajstrom.deb.metadata.v1+json >"$work/oras.log" 2>&1 ||
	{ cat "$work/oras.log" >&2; fail "oras push $two"; }
ok "oras push $(basename "$deb2") to jammy/main: $(grep '^Digest:' "$work/oras.log")"

# The repo readable anonymously, through the API; the anonymousRead it had comes back on exit.
status="$(curl -sS -K "$E2E_CURLRC" -o "$work/body" -w '%{http_code}' "$base/api/v1/repos/apt/$repo")"
expect "GET /api/v1/repos/apt/$repo ($(cat "$work/body"))" 200 "$status"
was="$(jq -r .anonymousRead "$work/body")"
anonymous="$was"
anonymous_read true
status="$(anon dists/jammy/Release)"
[ "$status" = 200 ] || fail "anonymous GET of dists/jammy/Release with anonymousRead on the repo: $status, want 200; the server needs auth.anonymousRead: true"
ok "PATCH /api/v1/repos/apt/$repo anonymousRead true (it was $was): anonymous GET of dists/jammy/Release 200"

# A-04: key.asc, fetched without credentials, into /etc/apt/keyrings: signed-by when the server
# signs, [trusted=yes] when it has no master key.
mkdir -p "$work/client"
status="$(curl -sS -o "$work/client/key.asc" -w '%{http_code}' "$base/apt/$repo/key.asc")"
case "$status" in
200) source_opts="signed-by=/etc/apt/keyrings/$repo.asc"; ok "anonymous GET key.asc: 200, the source is signed-by it" ;;
404) rm -f "$work/client/key.asc"; source_opts="trusted=yes"; ok "anonymous GET key.asc: 404, the server does not sign: [trusted=yes]" ;;
*) fail "anonymous GET key.asc: status $status" ;;
esac

# The client: one container for every apt step, as root, the run's directory as /w.
started=1
docker run -d --name "$client" --network host --platform linux/amd64 -v "$work/client:/w" "$image" sleep infinity >/dev/null
apt_version="$(docker exec "$client" apt-get --version | sed -n 1p)"
ok "client container $image (linux/amd64): $apt_version"

# A-03, A-04: anonymousRead on, no credentials: apt-get update fetches the Packages by-hash, as
# the Release says; install of both; apt-cache policy.
from="$(mark)"
in_client anon.log "
	rm -f /etc/apt/sources.list /etc/apt/sources.list.d/*
	if [ -f /w/key.asc ]; then install -D -m 0644 /w/key.asc /etc/apt/keyrings/$repo.asc; fi
	echo 'deb [$source_opts] $base/apt/$repo jammy main' > /etc/apt/sources.list.d/$repo.list
	apt-get update
	apt-get install -y --no-install-recommends $one $two
	echo \"greeting: \$(cat /usr/share/$one/greeting) / \$(cat /usr/share/$two/greeting)\"
	apt-cache policy $one $two" || { cat "$work/anon.log" >&2; fail "anonymous apt-get update, install in $image"; }
quiet anon.log
grep -qxF "greeting: hello from $one / hello from $two" "$work/anon.log" || { cat "$work/anon.log" >&2; fail "the installed files"; }
policy anon.log
seen="$(fetched "$from" anonymous by-hash)"
ok "anonymous apt-get update && apt-get install $one $two [$source_opts]: both installed, their files there, apt-cache policy candidate $version from jammy/main; $seen"

# anonymousRead off: without credentials apt-get update fails with apt's 401.
anonymous_read false
expect "anonymous GET of dists/jammy/Release with anonymousRead off" 401 "$(anon dists/jammy/Release)"
if in_client noauth.log "rm -rf /var/lib/apt/lists/*; apt-get update"; then
	cat "$work/noauth.log" >&2
	fail "anonymous apt-get update succeeded with anonymousRead off"
fi
grep -E '^E: .*401' "$work/noauth.log" >"$work/noauth.err" || { cat "$work/noauth.log" >&2; fail "anonymous apt-get update failed without a 401"; }
ok "PATCH anonymousRead false: anonymous GET 401, anonymous apt-get update fails: $(head -1 "$work/noauth.err")"

# A-04, A-07: with the credentials in /etc/apt/auth.conf.d: apt-get update by-hash, install of
# both again (purged, the archives cleaned), download of both and their sha256, apt-cache policy.
(umask 077 && printf 'machine %s login %s password %s\n' "$machine" "$E2E_USER" "$E2E_PASSWORD" >"$work/client/auth.conf")
# The access log names a user by its uid (#182): whoami says which.
principal="$(curl -fsS -K "$E2E_CURLRC" "$base/api/v1/whoami" | jq -r .principal)"
from="$(mark)"
in_client auth.log "
	install -m 0600 /w/auth.conf /etc/apt/auth.conf.d/lajstrom.conf && rm /w/auth.conf
	apt-get update
	apt-get purge -y $one $two && apt-get clean
	apt-get install -y --no-install-recommends $one $two
	echo \"greeting: \$(cat /usr/share/$one/greeting) / \$(cat /usr/share/$two/greeting)\"
	mkdir -p /w/download && cd /w/download && apt-get -o APT::Sandbox::User=root download $one $two
	chown -R $(id -u):$(id -g) /w/download
	apt-cache policy $one $two" || { cat "$work/auth.log" >&2; fail "apt-get update, install, download with auth.conf in $image"; }
quiet auth.log
grep -qxF "greeting: hello from $one / hello from $two" "$work/auth.log" || { cat "$work/auth.log" >&2; fail "the installed files"; }
policy auth.log
seen="$(fetched "$from" "$principal" by-hash)"
ok "apt-get update && apt-get install $one $two, auth.conf login $E2E_USER: both installed again, apt-cache policy candidate $version; $seen"
for f in "$deb1" "$deb2"; do
	got="$work/client/download/$(basename "$f")"
	[ -f "$got" ] || fail "apt-get download wrote no $(basename "$f")"
	expect "sha256 of $(basename "$f") from apt-get download" "$(sha256 "$f")" "$(sha256 "$got")"
done
ok "apt-get download $one $two: sha256 $(sha256 "$deb1") and $(sha256 "$deb2"), those of the uploaded files"

# A-03: a second apt-get update, the lists removed, with Acquire::By-Hash=no: the Packages by name.
from="$(mark)"
in_client nohash.log "
	rm -rf /var/lib/apt/lists/*
	apt-get -o Acquire::By-Hash=no update
	apt-cache policy $one $two" || { cat "$work/nohash.log" >&2; fail "apt-get -o Acquire::By-Hash=no update in $image"; }
quiet nohash.log
policy nohash.log
seen="$(fetched "$from" "$principal" name)"
ok "apt-get -o Acquire::By-Hash=no update: apt-cache policy candidate $version; $seen"

# A broken Release: a copy of the suite (Release and the indexes it lists, the same bytes apt got
# from the server) whose Release has the SHA256 of every index one digit off, read from a file:
# source, [trusted=yes] as the copy is not signed. Every index, not one: apt falls back to the
# next compression when one does not match. apt-get update must fail with its own error, the one
# the steps above would print if the server's Release were wrong.
mirror="$work/client/mirror/dists/jammy"
mkdir -p "$mirror"
curl -fsS -K "$E2E_CURLRC" -o "$mirror/Release.good" "$base/apt/$repo/dists/jammy/Release"
for f in $(awk '/^SHA256:/ { s = 1; next } /^[^ ]/ { s = 0 } s { print $3 }' "$mirror/Release.good"); do
	mkdir -p "$mirror/$(dirname "$f")"
	curl -fsS -K "$E2E_CURLRC" -o "$mirror/$f" "$base/apt/$repo/dists/jammy/$f"
done
awk '/^SHA256:/ { s = 1; print; next } /^[^ ]/ { s = 0 }
	s { d = substr($1, 1, 1); $1 = (d == "0" ? "1" : "0") substr($1, 2); print " " $0; next } { print }' \
	"$mirror/Release.good" >"$mirror/Release"
rm "$mirror/Release.good"
if in_client broken.log "
	rm -rf /var/lib/apt/lists/* /etc/apt/sources.list.d/*
	echo 'deb [trusted=yes] file:/w/mirror jammy main' > /etc/apt/sources.list.d/broken.list
	apt-get update"; then
	cat "$work/broken.log" >&2
	fail "apt-get update succeeded with a broken Release"
fi
grep -E '^E: .*Hash Sum mismatch' "$work/broken.log" >"$work/broken.err" ||
	{ cat "$work/broken.log" >&2; fail "apt-get update with a broken Release failed without a Hash Sum mismatch"; }
ok "a copy of the suite whose Release has every SHA256 one digit off: apt-get update fails: $(head -1 "$work/broken.err" | tr -s ' ')"

# A-06: the same bytes again are the repeat.
put "$deb1" jammy
expect "PUT $one again status ($(cat "$work/body"))" 201 "$status"
expect "PUT $one again digest" "$digest1" "$(jq -r .digest "$work/body")"
ok "PUT of the same bytes again: 201, the same digest $digest1"

# A-06: other bytes under the same name, version and architecture in the same suite: the tag.
put "$other" jammy
expect "PUT other bytes to jammy status ($(cat "$work/body"))" 409 "$status"
grep -qF "${version}__jammy_2fmain_2famd64" "$work/body" || fail "the 409 does not name the tag: $(cat "$work/body")"
ok "PUT of other bytes as $one $version amd64 to jammy/main: 409, $(tr -d '\n' <"$work/body")"

# E-07: the same in another suite: the pool file name.
put "$other" noble
expect "PUT other bytes to noble status ($(cat "$work/body"))" 409 "$status"
grep -qF "pool file name ${one}_${version}_amd64.deb is held by version $version in jammy/main" "$work/body" ||
	fail "the 409 does not name the pool file name: $(cat "$work/body")"
ok "PUT of other bytes as $one $version amd64 to noble/main: 409, $(tr -d '\n' <"$work/body")"

# #61: both versions deleted through the API are gone from the next Packages and from apt.
for p in "$one" "$two"; do
	status="$(curl -sS -K "$E2E_CURLRC" -o "$work/body" -w '%{http_code}' -X DELETE \
		"$base/api/v1/repos/apt/$repo/packages/$p/versions/$version")"
	expect "DELETE of $p $version status ($(cat "$work/body"))" 204 "$status"
done
# A suite left without versions is 404 (http-routing.md §3), as on a server of its own (run.sh).
status="$(curl -sS -K "$E2E_CURLRC" -o "$work/Packages.after" -w '%{http_code}' "$base/apt/$repo/dists/jammy/main/binary-amd64/Packages")"
case "$status" in
200) if grep -qE "^Package: ($one|$two)\$" "$work/Packages.after"; then fail "the Packages lists $one or $two after the delete"; fi
	after="the Packages lists neither" ;;
404) after="jammy has no version left: its Packages is 404" ;;
*) fail "GET of the Packages after the delete: $status" ;;
esac
in_client gone.log "
	apt-get purge -y $one $two
	rm -rf /var/lib/apt/lists/* /etc/apt/sources.list.d/*
	echo 'deb [$source_opts] $base/apt/$repo jammy main' > /etc/apt/sources.list.d/$repo.list
	apt-get update || echo 'apt-get update failed'
	if apt-cache show $one $two; then echo 'still there'; exit 1; fi" ||
	{ cat "$work/gone.log" >&2; fail "apt-get after the delete still has $one or $two"; }
ok "DELETE /api/v1/repos/apt/$repo/packages/{$one,$two}/versions/$version: 204; $after; purged, apt-get update, apt-cache show finds neither"
echo "PASS apt e2e against $base/apt/$repo/"

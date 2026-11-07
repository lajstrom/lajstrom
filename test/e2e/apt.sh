#!/usr/bin/env bash
# End-to-end test of an apt repo with real clients against a running Lajstrom (#42; docs/acceptance.md
# A-01, A-02, A-04, A-06, A-07, E-07): two packages built with dpkg-deb, one uploaded with
# PUT …/upload, the other pushed with oras through the OCI facade with the deb profile (its
# metadata layer from lajstrom admin deb metadata); apt-get update and apt-get install of both in
# ubuntu:24.04, signed-by the repo's key.asc ([trusted=yes] when the server has no master key and
# key.asc is 404); apt-get download of both and their sha256 against the uploaded files; the same
# bytes again (201, the same digest); other bytes under the same name, version and architecture in
# the same suite (409, the tag) and in another suite (409, the pool file name).
#
#   test/e2e/apt.sh [<base url> [<repo>]]     default: http://localhost:8080 ubuntu
#
# The apt repo must exist (lajstrom admin repo create apt/<repo> --registry local) and be writable
# without credentials, as on the dev stack. The packages are named after the run, so the script
# can run again against the same repo; apt versions cannot be deleted yet, so they stay. It needs
# curl, jq and docker (the images ubuntu:24.04 for linux/amd64, emulated on an arm64 host, or
# $APT_E2E_IMAGE, which builds and installs, and ghcr.io/oras-project/oras:v1.3.6; both
# containers use --network host, which reaches the base URL on Linux and under OrbStack).
# lajstrom admin deb metadata runs from $LAJSTROM_BIN, else bin/lajstrom, else go run
# ./cmd/lajstrom. It prints one line per step and exits non-zero at the first failure. #48
# wires it into make e2e and CI.
set -euo pipefail

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
trap 'rm -rf "$work"' EXIT
run="$(date -u +%Y%m%d%H%M%S)-$RANDOM"
one="e2e-put-$run"   # uploaded with PUT
two="e2e-oras-$run"  # pushed with oras
version=1.0.0-1      # needs no escaping in a tag: digits, dots and a hyphen
status=""            # status of the last request

fail() { echo "FAIL $*" >&2; exit 1; }
ok() { echo "ok   $*"; }

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

# expect WHAT WANT GOT
expect() { [ "$2" = "$3" ] || fail "$1: got '$3', want '$2'"; }

# put FILE SUITE: PUT FILE to upload?suite=SUITE&component=main; the status goes to $status, the
# headers to $work/headers, the body to $work/body.
put() {
	status="$(curl -sS -o "$work/body" -D "$work/headers" -w '%{http_code}' -T "$1" \
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
docker run --rm --network host -v "$work/two:/w" -w /w "$oras_image" push ${plain:+"$plain"} \
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

# key.asc: signed-by when the server signs, [trusted=yes] when it has no master key.
status="$(curl -sS -o "$work/key.asc" -w '%{http_code}' "$base/apt/$repo/key.asc")"
case "$status" in
200) source_opts="signed-by=/etc/apt/keyrings/$repo.asc"; ok "GET key.asc: 200, the source is signed-by it" ;;
404) rm -f "$work/key.asc"; source_opts="trusted=yes"; ok "GET key.asc: 404, the server does not sign: [trusted=yes]" ;;
*) fail "GET key.asc: status $status" ;;
esac

# A-04, A-07: apt-get update, install and download in ubuntu:24.04.
mkdir -p "$work/download"
cp "$work/key.asc" "$work/download/" 2>/dev/null || true
docker run --rm --network host --platform linux/amd64 -v "$work/download:/w" "$image" bash -ec "
	rm -f /etc/apt/sources.list /etc/apt/sources.list.d/*
	if [ -f /w/key.asc ]; then install -D -m 0644 /w/key.asc /etc/apt/keyrings/$repo.asc && rm /w/key.asc; fi
	echo 'deb [$source_opts] $base/apt/$repo jammy main' > /etc/apt/sources.list.d/$repo.list
	apt-get update
	apt-get install -y --no-install-recommends $one $two
	echo \"greeting: \$(cat /usr/share/$one/greeting) / \$(cat /usr/share/$two/greeting)\"
	cd /w && apt-get -o APT::Sandbox::User=root download $one $two
	chown -R $(id -u):$(id -g) /w" >"$work/apt.log" 2>&1 || { cat "$work/apt.log" >&2; fail "apt-get in $image"; }
grep -E '^(E|W): ' "$work/apt.log" >&2 && fail "apt reported errors or warnings"
grep -qxF "greeting: hello from $one / hello from $two" "$work/apt.log" || { cat "$work/apt.log" >&2; fail "the installed files"; }
ok "apt-get update && apt-get install $one $two in $image [$source_opts]: both installed, their files there"
for f in "$deb1" "$deb2"; do
	got="$work/download/$(basename "$f")"
	[ -f "$got" ] || fail "apt-get download wrote no $(basename "$f")"
	expect "sha256 of $(basename "$f") from apt-get download" "$(sha256 "$f")" "$(sha256 "$got")"
done
ok "apt-get download $one $two: sha256 $(sha256 "$deb1") and $(sha256 "$deb2"), those of the uploaded files"

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
echo "PASS apt e2e against $base/apt/$repo/"

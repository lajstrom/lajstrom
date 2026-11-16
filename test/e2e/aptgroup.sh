#!/usr/bin/env bash
# End-to-end test of an apt group repo with a real apt client against a running Lajstrom (#68;
# docs/spec/proxy-and-group.md §8.6, docs/spec/http-routing.md §3, apt, Group):
#
#   - two hosted apt repos and a group of both, in that order, made through the API;
#   - a package built with dpkg-deb uploaded to each member under the same name, version and
#     architecture with other bytes, and a second package to the second member alone;
#   - a PUT through the group of two members: 405 with Allow: GET, HEAD;
#   - the group's key.asc, not a member's, into /etc/apt/keyrings, the source signed-by it
#     ([trusted=yes] when the server has no master key), the credentials in /etc/apt/auth.conf.d:
#     apt-get update and install of both through the group, the first package the first member's
#     bytes, and apt-cache policy naming the group;
#   - a third package published to the second member right after: the next apt-get update
#     through the group has it, and it installs;
#   - the group and its members deleted through the API.
#
#   test/e2e/aptgroup.sh [<base url>]     default: http://localhost:8080
#
# The user of LAJSTROM_E2E_USER and LAJSTROM_E2E_PASSWORD must have the admin role, to create the
# repos (run.sh makes one; lib.sh, e2e_credentials), and the server a registry named local. The
# repos and the packages are named after the run, so the script can run again against the same
# server; a run that fails before its last step leaves its repos. It needs curl, jq and docker:
# the image ubuntu:24.04 for linux/amd64, emulated on an arm64 host, or $APT_E2E_IMAGE, which
# builds the packages and is the apt client, one container named
# $E2E_CONTAINER_PREFIX-aptgroup-<run> (lajstrom-e2e by default), removed on exit, with
# --network host, which reaches the base URL on Linux and under OrbStack. It prints one line per
# step and exits non-zero at the first failure (lib.sh). make e2e runs it (test/e2e/run.sh).
set -euo pipefail
. "$(dirname "$0")/lib.sh"

base="${1:-http://localhost:8080}"
base="${base%/}"
for tool in curl jq docker; do
	command -v "$tool" >/dev/null || { echo "aptgroup.sh: $tool is required" >&2; exit 2; }
done
image="${APT_E2E_IMAGE:-ubuntu:24.04}"
work="$(mktemp -d)"
run="$(date -u +%Y%m%d%H%M%S)-$RANDOM"
first="g1-$run" second="g2-$run" group="all-$run" # the members, in order, and the group
one="e2e-g1-$run" two="e2e-g2-$run" three="e2e-g3-$run"
version=1.0.0-1
client="${E2E_CONTAINER_PREFIX:-lajstrom-e2e}-aptgroup-$run"
started=""
cleanup() {
	if [ -n "$started" ]; then docker rm -f "$client" >/dev/null 2>&1 || true; fi
	rm -rf "$work"
}
trap cleanup EXIT
e2e_credentials "$work"

# api METHOD PATH [BODY]: the API as $E2E_USER; the status goes to $status, the body to $work/body.
api() {
	status="$(curl -sS -K "$E2E_CURLRC" -o "$work/body" -D "$work/headers" -w '%{http_code}' -X "$1" \
		-H 'Content-Type: application/json' ${3:+-d "$3"} "$base$2")"
}

# put FILE REPO: PUT FILE to upload?suite=jammy&component=main of apt/REPO.
put() {
	status="$(curl -sS -K "$E2E_CURLRC" -o "$work/body" -D "$work/headers" -w '%{http_code}' -T "$1" \
		"$base/apt/$2/upload?suite=jammy&component=main")"
}

# build DIR NAME GREETING: build NAME_$version_amd64.deb in $work/DIR/ with dpkg-deb in a
# container, of one file, /usr/share/NAME/greeting, and print its path.
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

# in_client LOG SCRIPT: run SCRIPT with bash -e as root in the client container, its output in
# $work/LOG.
in_client() { docker exec "$client" bash -ec "$2" >"$work/$1" 2>&1; }

for r in "$first" "$second"; do
	api POST /api/v1/repos "{\"format\": \"apt\", \"name\": \"$r\", \"registry\": \"local\"}"
	expect "POST /api/v1/repos apt/$r ($(cat "$work/body"))" 201 "$status"
done
api POST /api/v1/repos "{\"format\": \"apt\", \"name\": \"$group\", \"kind\": \"group\", \"group\": {\"members\": [\"$first\", \"$second\"]}}"
expect "POST /api/v1/repos apt/$group ($(cat "$work/body"))" 201 "$status"
expect "the group's members" "$first $second" "$(jq -r '.group.members | join(" ")' "$work/body")"
step "POST /api/v1/repos: apt/$first, apt/$second and the group apt/$group of both"

deb1="$(build one "$one" "from $first")"
other="$(build other "$one" "from $second")" # the name, version and architecture of the first
deb2="$(build two "$two" "hello from $two")"
deb3="$(build three "$three" "hello from $three")"
put "$deb1" "$first"
expect "PUT $one to apt/$first ($(cat "$work/body"))" 201 "$status"
put "$other" "$second"
expect "PUT $one to apt/$second ($(cat "$work/body"))" 201 "$status"
put "$deb2" "$second"
expect "PUT $two to apt/$second ($(cat "$work/body"))" 201 "$status"
step "dpkg-deb --build and PUT: $one to both members with other bytes, $two to apt/$second"

put "$deb3" "$group"
expect "PUT through the group of two members ($(cat "$work/body"))" 405 "$status"
expect "Allow of the 405" "GET, HEAD" "$(tr -d '\r' <"$work/headers" | awk 'tolower($1) == "allow:" { sub(/^[^:]*: /, ""); print }')"
step "PUT through apt/$group: 405, Allow: GET, HEAD"

mkdir -p "$work/client"
status="$(curl -sS -K "$E2E_CURLRC" -o "$work/client/key.asc" -w '%{http_code}' "$base/apt/$group/key.asc")"
case "$status" in
200)
	curl -sS -K "$E2E_CURLRC" -o "$work/member.asc" "$base/apt/$first/key.asc"
	cmp -s "$work/client/key.asc" "$work/member.asc" && fail "the group's key.asc is its first member's"
	source_opts="signed-by=/etc/apt/keyrings/$group.asc"
	step "GET key.asc of apt/$group: 200, its own key, the source is signed-by it" ;;
404) rm -f "$work/client/key.asc"; source_opts="trusted=yes"; step "GET key.asc of apt/$group: 404, the server does not sign: [trusted=yes]" ;;
*) fail "GET key.asc of apt/$group: status $status" ;;
esac

# apt sends auth.conf credentials over http only to a machine written with http://.
case "$base" in http://*) machine="$base/apt/$group" ;; *) machine="${base#https://}/apt/$group" ;; esac
printf 'machine %s\nlogin %s\npassword %s\n' "$machine" "$E2E_USER" "$E2E_PASSWORD" >"$work/client/auth.conf"
started=1
docker run -d --name "$client" --network host --platform linux/amd64 -v "$work/client:/w" "$image" sleep infinity >/dev/null
in_client update.log "
	rm -f /etc/apt/sources.list /etc/apt/sources.list.d/*
	install -m 0600 /w/auth.conf /etc/apt/auth.conf.d/lajstrom.conf
	if [ -f /w/key.asc ]; then install -D -m 0644 /w/key.asc /etc/apt/keyrings/$group.asc; fi
	echo 'deb [$source_opts] $base/apt/$group jammy main' > /etc/apt/sources.list.d/$group.list
	apt-get update
	apt-get install -y --no-install-recommends $one $two
	echo \"greeting: \$(cat /usr/share/$one/greeting) / \$(cat /usr/share/$two/greeting)\"
	apt-cache policy $one" || { cat "$work/update.log" >&2; fail "apt-get update, install through apt/$group in $image"; }
if grep -E '^(E|W): ' "$work/update.log" >&2; then fail "apt reported errors or warnings"; fi
grep -qxF "greeting: from $first / hello from $two" "$work/update.log" || { cat "$work/update.log" >&2; fail "the installed files"; }
grep -qF " 500 $base/apt/$group jammy/main amd64 Packages" "$work/update.log" || { cat "$work/update.log" >&2; fail "apt-cache policy does not name the group"; }
step "apt-get update && apt-get install $one $two through apt/$group [$source_opts]: $one is the first member's, apt-cache policy names the group"

# Published at once: on a fast host the next render falls within the second of the one apt holds
# and has its Last-Modified, and apt, which asks with If-Modified-Since alone, gets it (#1255).
put "$deb3" "$second"
expect "PUT $(basename "$deb3") to apt/$second ($(cat "$work/body"))" 201 "$status"
in_client late.log "
	apt-get update
	apt-get install -y --no-install-recommends $three
	cat /usr/share/$three/greeting" || { cat "$work/late.log" >&2; fail "apt-get update, install $three through apt/$group"; }
grep -qxF "hello from $three" "$work/late.log" || { cat "$work/late.log" >&2; fail "the file of $three"; }
step "PUT $three to apt/$second, then apt-get update && apt-get install $three through apt/$group"

api DELETE "/api/v1/repos/apt/$group"
expect "DELETE apt/$group ($(cat "$work/body"))" 204 "$status"
for r in "$first" "$second"; do
	api DELETE "/api/v1/repos/apt/$r?force=true"
	expect "DELETE apt/$r ($(cat "$work/body"))" 204 "$status"
done
step "DELETE the group, then its members with force=true"
echo "PASS aptgroup e2e against $base/apt/$group/"

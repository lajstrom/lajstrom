#!/usr/bin/env bash
# docker and crane pull a multi-arch image through an oci proxy repo (#442, ADR 0018,
# docs/spec/proxy-and-group.md §11). A manual scenario: make e2e does not run it (run.sh runs
# test/e2e/*.sh only; test/e2e/ociproxy.sh is its crane part there, with containerd, #873), and
# with OCI_PROXY_DOCKERHUB=1 it asks Docker Hub, whose anonymous pulls are limited (100 per hour
# per address, ratelimit-limit says).
#
#   test/e2e/manual/ociproxy.sh
#
# Like run.sh it builds bin/lajstrom and serves it on a free port of 127.0.0.1 with a
# configuration of its own in a temporary directory: a fresh SQLite index; the registry `local`,
# the backend, at $LAJSTROM_E2E_REGISTRY (localhost:5000, the Zot of make dev-up); the upstream
# `up` of format oci at $OCI_PROXY_UPSTREAM (the same registry by default), seeded with crane
# with an image of two platforms under oci-proxy-up-<run>/img:1.0; the proxy repo oci/up-<run>,
# readable anonymously; and with OCI_PROXY_DOCKERHUB=1 the upstream Docker Hub and the proxy repo
# oci/hub-<run>. Then:
#
#   1. crane pull --platform linux/arm64: the platform's two blobs are fetched, and its manifest
#      is in the backend under its keep-tag, not the tag, since the index is not complete;
#   2. crane pull --platform linux/amd64: two more blobs, and the tag 1.0 in the backend;
#   3. both again, and docker pull when docker is on PATH: no blob is fetched again;
#   4. with OCI_PROXY_DOCKERHUB=1: crane pull of hello-world for linux/arm64 (two counted pulls:
#      the index and the platform's manifest), the same as library/hello-world (no request), an
#      unknown repository (a 404 from HEADs, which Docker Hub does not count), and the gauges
#      lajstrom_proxy_upstream_ratelimit_*.
#
# It needs curl, crane and jq; it exits 1 at the first failure, 2 when it could not start. What it
# pushed stays in the registries; the temporary directory is removed unless LAJSTROM_E2E_KEEP=1.
set -euo pipefail
. "$(dirname "$0")/../lib.sh"

here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/../../.." && pwd)"
registry="${LAJSTROM_E2E_REGISTRY:-localhost:5000}"
upstream="${OCI_PROXY_UPSTREAM:-$registry}"
for tool in curl crane jq make go; do
	command -v "$tool" >/dev/null || { echo "ociproxy.sh: $tool is required" >&2; exit 2; }
done
run="$(date +%Y%m%d%H%M%S)-$RANDOM"
tmp="${TMPDIR:-/tmp}"
tmp="$(mktemp -d "${tmp%/}/lajstrom-ociproxy.XXXXXX")"
pid=""
finish() {
	[ -n "$pid" ] && kill -TERM "$pid" 2>/dev/null && wait "$pid" 2>/dev/null || true
	if [ "${LAJSTROM_E2E_KEEP:-}" = 1 ]; then echo "ociproxy.sh: kept $tmp"; else rm -rf "$tmp"; fi
}
trap finish EXIT

(cd "$root" && make --no-print-directory build) >"$tmp/build.log" 2>&1 || { cat "$tmp/build.log" >&2; exit 2; }
port="$(free_port)" && mport="$(free_port)" || { echo "ociproxy.sh: no free port" >&2; exit 2; }
hub=""
if [ "${OCI_PROXY_DOCKERHUB:-}" = 1 ]; then
	hub="  - {name: dockerhub, format: oci, url: \"https://registry-1.docker.io\"}"
fi
cat >"$tmp/lajstrom.yaml" <<EOF
# Written by test/e2e/manual/ociproxy.sh for one run.
server: {listen: "127.0.0.1:$port", externalURL: "http://127.0.0.1:$port", insecureHTTP: true, metricsListen: "127.0.0.1:$mport"}
log: {level: info, format: text}
database: {driver: sqlite, dsn: "file:$tmp/lajstrom.db"}
registries:
  - {name: local, url: "http://$registry"}
upstreams:
  - {name: up, format: oci, url: "http://$upstream"}
$hub
keys: {masterKeyEnv: LAJSTROM_MASTER_KEY}
auth: {anonymousRead: true}
reconcile: {interval: 0s}
EOF
LAJSTROM_MASTER_KEY="$(od -An -tx1 -N32 /dev/urandom | tr -d ' \n')"
export LAJSTROM_MASTER_KEY LAJSTROM_CONFIG="$tmp/lajstrom.yaml"
bin="$root/bin/lajstrom"
"$bin" migrate >"$tmp/migrate.log" 2>&1 || { cat "$tmp/migrate.log" >&2; exit 2; }
"$bin" serve >"$tmp/server.log" 2>&1 &
pid=$!
for _ in $(seq 1 100); do curl -fsS "http://127.0.0.1:$port/-/readyz" >/dev/null 2>&1 && break; sleep 0.2; done
"$bin" admin repo create oci/up-"$run" --registry local --kind proxy --upstream '{"name": "up"}' --anonymous-read >/dev/null ||
	{ tail -20 "$tmp/server.log" >&2; exit 2; }

# The upstream's image: two platforms of one layer each, OCI media types (Zot refuses Docker's).
up="$upstream/oci-proxy-up-$run/img"
for arch in amd64 arm64; do
	mkdir -p "$tmp/$arch" && head -c 300000 /dev/urandom >"$tmp/$arch/data" && tar -C "$tmp/$arch" -cf "$tmp/$arch.tar" data
	crane append --insecure --oci-empty-base -f "$tmp/$arch.tar" -t "$up:$arch-base" 2>/dev/null >/dev/null
	crane mutate --insecure --set-platform "linux/$arch" -t "$up:$arch" "$up:$arch-base" 2>/dev/null >/dev/null
done
crane index append --insecure -t "$up:1.0" -m "$up:amd64" -m "$up:arm64" 2>/dev/null >/dev/null
index="$(crane digest --insecure "$up:1.0")"

lj="127.0.0.1:$port/oci/up-$run/oci-proxy-up-$run/img"
back="$registry/oci/up-$run/oci-proxy-up-$run/img"
fetched() { grep -c "a blob fetched from the upstream" "$tmp/server.log" || true; }
# eventually waits up to 5 s for a command to succeed: the client's last byte comes with the
# commit of the blob, before the push of what it completed (§11.8).
eventually() { for _ in $(seq 1 50); do "$@" && return 0; sleep 0.1; done; return 1; }
keep_tag() { crane ls --insecure "$back" | grep -q '^_keep\.sha256\.'; }
tag_is() { [ "$(crane digest --insecure "$back:1.0" 2>/dev/null)" = "$1" ]; }
crane pull --insecure --platform linux/arm64 "$lj:1.0" "$tmp/arm64.oci" 2>/dev/null
expect "blobs fetched by the first platform" 2 "$(fetched)"
eventually keep_tag || fail "no keep-tag in the backend"
crane ls --insecure "$back" | grep -qx '1.0' && fail "1.0 is in the backend before its index is complete"
ok "linux/arm64: two blobs fetched, its manifest kept, the tag waits for the index"
crane pull --insecure --platform linux/amd64 "$lj:1.0" "$tmp/amd64.oci" 2>/dev/null
expect "blobs fetched by both platforms" 4 "$(fetched)"
eventually tag_is "$index" || fail "1.0 does not name the index $index in the backend"
ok "linux/amd64: two more blobs, and 1.0 names the index in the backend"
crane pull --insecure --platform linux/arm64 "$lj:1.0" "$tmp/arm64b.oci" 2>/dev/null
crane pull --insecure --platform linux/amd64 "$lj:1.0" "$tmp/amd64b.oci" 2>/dev/null
if command -v docker >/dev/null; then
	docker pull -q --platform linux/arm64 "$lj:1.0" >/dev/null && docker rmi -f "$lj:1.0" >/dev/null
fi
expect "blobs fetched after the second pulls" 4 "$(fetched)"
expect "tags/list without the keep-tags" "1.0" "$(crane ls --insecure "$lj")"
ok "a second pull, by crane and docker, fetches no blob"

if [ -n "$hub" ]; then
	"$bin" admin repo create oci/hub-"$run" --registry local --kind proxy --upstream '{"name": "dockerhub"}' --anonymous-read >/dev/null
	h="127.0.0.1:$port/oci/hub-$run"
	crane pull --insecure --platform linux/arm64 "$h/hello-world:latest" "$tmp/hello.oci" 2>/dev/null
	expect "library/hello-world" "$(crane digest --insecure "$h/hello-world:latest")" "$(crane digest --insecure "$h/library/hello-world:latest")"
	crane digest --insecure "$h/library/no-such-image-$run:latest" >/dev/null 2>&1 && fail "an unknown repository answered"
	curl -fsS "http://127.0.0.1:$mport/metrics" | grep 'ratelimit.*dockerhub'
	ok "Docker Hub: hello-world as library/hello-world, an unknown repository 404, the rate-limit gauges"
fi

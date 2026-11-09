#!/usr/bin/env bash
# The load test of #62 (docs/operations.md, Performance): one object of 1 GiB pushed and pulled
# through a raw repo and through /v2, with the memory of the server sampled the whole time, to
# show that Lajstrom streams bodies and does not buffer them. make load runs it; it is not part
# of make e2e, because it moves gigabytes and needs the disk for them.
#
#   test/load/run.sh
#
# It builds bin/lajstrom and serves it on a free port of 127.0.0.1 with a configuration of its
# own (a fresh SQLite index in a temporary directory, the registry `local` at
# $LAJSTROM_LOAD_REGISTRY, an admin user with a random password, a metrics listener) and the
# repos raw/<run> and oci/<run>, <run> a name of this run. Then, one phase after the other:
#
#   raw PUT      curl -T of the object to /raw/<run>/big.bin (spooled to a temporary file, then
#                pushed to the registry)
#   raw GET      $LAJSTROM_LOAD_PARALLEL curl downloads of it at once, each piped into sha256
#   v2 push      oras push of the same object to oci/<run>/big:v1 (streamed to the registry)
#   v2 GET       $LAJSTROM_LOAD_PARALLEL oras blob fetch --output - of its layer at once
#
# Every 0.2 s it samples the server's resident set (ps), its Go heap in use (the metrics
# listener) and, in the upload phases, the temporary files it holds open (lsof: the upload spool
# is unlinked as soon as it is made, so only an open descriptor shows it), and prints per
# phase the time, the throughput and the peaks. It fails when a digest differs, a request fails, or the peak
# resident set of a phase exceeds $LAJSTROM_LOAD_MAX_RSS_MIB.
#
# Environment:
#
#   LAJSTROM_LOAD_REGISTRY      host:port of the registry, plain HTTP without credentials
#                               (default localhost:5000, the Zot of make dev-up)
#   LAJSTROM_LOAD_SIZE_MIB      the size of the object in MiB (default 1024)
#   LAJSTROM_LOAD_PARALLEL      downloads at once in the GET phases (default 8)
#   LAJSTROM_LOAD_MAX_RSS_MIB   the bound on the server's resident set (default 256, #62)
#   LAJSTROM_LOAD_KEEP=1        keep the temporary directory (the configuration, the server log,
#                               the samples); the object itself is always removed
#
# Disk: the object and the spool of the raw PUT live in $TMPDIR at once, and the registry keeps
# one copy (Zot deduplicates the second push of the same bytes) plus, while the v2 push runs, the
# upload it is receiving. With Docker on the same disk (Docker Desktop, OrbStack) that is
# 3 x the size at the peak; the script wants that plus 1 GiB free on the filesystem of $TMPDIR and
# refuses to start otherwise. At the end it deletes what it pushed from the registry (the
# manifests and blobs of the repositories of <run>, nothing else) and removes its files.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/../.." && pwd)"
registry="${LAJSTROM_LOAD_REGISTRY:-localhost:5000}"
size_mib="${LAJSTROM_LOAD_SIZE_MIB:-1024}"
parallel="${LAJSTROM_LOAD_PARALLEL:-8}"
max_rss_mib="${LAJSTROM_LOAD_MAX_RSS_MIB:-256}"
size=$((size_mib * 1048576))

say() { printf '%s\n' "$*"; }
die() { printf 'load: %s\n' "$*" >&2; exit 2; }

for tool in curl jq oras go make lsof ps df awk; do
	command -v "$tool" >/dev/null || die "$tool is required"
done
if command -v sha256sum >/dev/null; then sha=(sha256sum); else sha=(shasum -a 256); fi
curl -fsS -o /dev/null --max-time 5 "http://$registry/v2/" || die "no registry answers at http://$registry/v2/: make dev-up, or set LAJSTROM_LOAD_REGISTRY"

# The disk, before anything is written.
tmp="${TMPDIR:-/tmp}"
need_mib=$((3 * size_mib + 1024))
free_mib=$(($(df -Pk "$tmp" | awk 'NR==2 {print $4}') / 1024))
say "disk: $free_mib MiB free in $tmp; the run needs $need_mib MiB (3 x $size_mib MiB at the peak, 1 GiB to spare)"
[ "$free_mib" -ge "$need_mib" ] || die "not enough free disk: $free_mib MiB free, $need_mib MiB needed; lower LAJSTROM_LOAD_SIZE_MIB or free space"

tmp="$(mktemp -d "${tmp%/}/lajstrom-load.XXXXXX")"
run="load-$(date +%s)-$((RANDOM % 10000))"
pid=""
sampler=""
finish() {
	set +e
	[ -z "$sampler" ] || kill "$sampler" 2>/dev/null
	if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
		kill -TERM "$pid" 2>/dev/null
		wait "$pid" 2>/dev/null
	fi
	cleanup_registry
	rm -f "$tmp/data/big.bin"
	if [ "${LAJSTROM_LOAD_KEEP:-}" = 1 ]; then
		say "load: kept $tmp"
	else
		rm -rf "$tmp"
	fi
}

# cleanup_registry deletes, in the registry, every manifest and blob of the repositories whose
# name starts with raw/<run>/ or oci/<run>/: what this run pushed, nothing else; the manifest
# first, then its blobs. Zot frees the space of a blob deleted in every repository that has it.
cleanup_registry() {
	local repos repo tags tag m d oci_manifest=application/vnd.oci.image.manifest.v1+json
	repos="$(curl -fsS "http://$registry/v2/_catalog?n=100000" 2>/dev/null | jq -r --arg r "$run" '.repositories[]? | select(startswith("raw/" + $r + "/") or startswith("oci/" + $r + "/"))')" || return 0
	for repo in $repos; do
		tags="$(curl -fsS "http://$registry/v2/$repo/tags/list" | jq -r '.tags[]?')" || continue
		for tag in $tags; do
			m="$(curl -fsS -D "$tmp/manifest.hdr" -H "Accept: $oci_manifest" "http://$registry/v2/$repo/manifests/$tag")" || continue
			d="$(tr -d '\r' <"$tmp/manifest.hdr" | awk -F': ' 'tolower($1) == "docker-content-digest" {print $2}')"
			[ -z "$d" ] || curl -fsS -o /dev/null -X DELETE "http://$registry/v2/$repo/manifests/$d" || true
			for d in $(jq -r '[.config.digest] + [.layers[].digest] | .[]' <<<"$m"); do
				curl -fsS -o /dev/null -X DELETE "http://$registry/v2/$repo/blobs/$d" || true
			done
		done
		say "load: deleted $repo from the registry"
	done
}
trap finish EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

free_port() {
	local port rc _
	for _ in 1 2 3 4 5 6 7 8 9 10; do
		port=$((20000 + RANDOM % 40000))
		rc=0
		curl -s -o /dev/null --max-time 1 "http://127.0.0.1:$port/" || rc=$?
		[ "$rc" != 7 ] || { echo "$port"; return 0; }
	done
	return 1
}

(cd "$root" && make --no-print-directory build) >"$tmp/build.log" 2>&1 || { cat "$tmp/build.log" >&2; die "make build failed"; }
bin="$root/bin/lajstrom"
port="$(free_port)" || die "no free port on 127.0.0.1"
mport="$(free_port)" || die "no free port on 127.0.0.1"
host="127.0.0.1:$port"
base="http://$host"
unset LAJSTROM_SERVER_LISTEN LAJSTROM_SERVER_EXTERNAL_URL LAJSTROM_SERVER_METRICS_LISTEN \
	LAJSTROM_LOG_LEVEL LAJSTROM_LOG_FORMAT LAJSTROM_DATABASE_DRIVER LAJSTROM_DATABASE_DSN LAJSTROM_ADMIN_PASSWORD
config="$tmp/lajstrom.yaml"
cat >"$config" <<EOF
# Written by test/load/run.sh for one run of the load test.
server:
  listen: "$host"
  externalURL: "$base"
  insecureHTTP: true
  shutdownTimeout: 5s
  metricsListen: "127.0.0.1:$mport"
log:
  level: warn
  format: text
database:
  driver: sqlite
  dsn: "file:$tmp/lajstrom.db"
registries:
  - name: local
    type: generic
    url: "http://$registry"
reconcile:
  interval: 0s
repos:
  - {name: $run, format: raw, registry: local}
  - {name: $run, format: oci, registry: local}
EOF
"$bin" migrate --config "$config" >"$tmp/migrate.log" 2>&1 || { cat "$tmp/migrate.log" >&2; die "lajstrom migrate failed"; }
user=load-admin
password="$(head -c 24 /dev/urandom | base64 | tr -d '+/=\n')"
printf '%s' "$password" | "$bin" admin user create --name "$user" --admin --password-stdin --config "$config" >"$tmp/admin.log" 2>&1 ||
	{ cat "$tmp/admin.log" >&2; die "lajstrom admin user create failed"; }
# The credentials stay in files, out of the process list.
printf 'user = "%s:%s"\n' "$user" "$password" >"$tmp/curlrc"
mkdir -p "$tmp/server-tmp" "$tmp/data"
TMPDIR="$tmp/server-tmp" "$bin" serve --config "$config" >"$tmp/server.log" 2>&1 </dev/null &
pid=$!
deadline=$((SECONDS + 30))
until curl -fsS -o /dev/null --max-time 2 "$base/-/readyz" 2>/dev/null; do
	if ! kill -0 "$pid" 2>/dev/null || [ "$SECONDS" -ge "$deadline" ]; then
		tail -n 30 "$tmp/server.log" >&2
		die "lajstrom serve on $base was not ready within 30 s"
	fi
	sleep 0.5
done
printf '%s' "$password" | oras login --registry-config "$tmp/oras.json" --plain-http -u "$user" --password-stdin "$host" >/dev/null
say "$("$bin" version | head -1) on $base, registry $registry, repos raw/$run and oci/$run"

# The sampler: one line per 0.2 s, "<phase> <rss KiB> <heap in use bytes> <open temp bytes>";
# lsof, the slow one, runs in the upload phases only.
set_phase() { echo "$1" >"$tmp/phase.new" && mv "$tmp/phase.new" "$tmp/phase"; } # atomic: the sampler never reads it empty
set_phase idle
sample() {
	local phase rss heap spool
	while kill -0 "$pid" 2>/dev/null; do
		phase="$(cat "$tmp/phase")"
		rss="$(ps -o rss= -p "$pid" | tr -d ' ')" || rss=0
		heap="$(curl -fsS --max-time 1 "http://127.0.0.1:$mport/metrics" | awk '$1 == "go_memstats_heap_inuse_bytes" {printf "%d", $2}')" || heap=0
		spool=0
		case "$phase" in
		*-put | *-push)
			spool="$(lsof -n -P -p "$pid" -Fsn 2>/dev/null | awk '/^f/ {s = 0} /^s/ {s = substr($0, 2)} /^n.*lajstrom-(upload|session|pypi)-/ {t += s} END {print t + 0}')" || spool=0
			;;
		esac
		printf '%s %s %s %s\n' "$phase" "${rss:-0}" "${heap:-0}" "${spool:-0}"
		sleep 0.2
	done >>"$tmp/samples"
}
sample &
sampler=$!
sleep 2

# timed PHASE CMD...: runs CMD as the phase PHASE of the samples and leaves its wall time, in
# seconds, in $secs; fails when CMD fails.
timed() {
	local phase="$1" rc=0
	shift
	set_phase "$phase"
	TIMEFORMAT=%3R
	{ time "$@" 2>>"$tmp/client.log"; } 2>"$tmp/time" || rc=$?
	set_phase between
	secs="$(tail -n 1 "$tmp/time")"
	[ "$rc" = 0 ] || { tail -n 20 "$tmp/client.log" >&2; die "$phase failed (exit $rc)"; }
}

results=()
# record PHASE BYTES: one line of the results, from $secs and the samples of PHASE.
record() {
	local phase="$1" bytes="$2" peaks
	peaks="$(awk -v p="$phase" '$1 == p {n++; if ($2 > r) r = $2; if ($3 > h) h = $3; if ($4 > s) s = $4}
		END {printf "%d %d %d %d", r / 1024, h / 1048576, s / 1048576, n}' "$tmp/samples")"
	set -- "$phase" "$bytes" $peaks
	results+=("$(awk -v p="$1" -v b="$2" -v t="$secs" -v r="$3" -v h="$4" -v s="$5" -v n="$6" 'BEGIN {
		printf "%-10s %8d %8.2f %8.0f", p, b / 1048576, t, (t > 0 ? b / 1048576 / t : 0)
		if (n > 0) printf " %8d %8d %8d %6d", r, h, s, n; else printf " %8s %8s %8s %6d", "-", "-", "-", n }')")
	if [ "$3" -gt "$max_rss_mib" ]; then
		over+=("$phase: $3 MiB")
	fi
}
over=()

# The object: random bytes, so that nothing in the registry has them yet.
head -c "$size" /dev/urandom >"$tmp/data/big.bin"
want="$("${sha[@]}" <"$tmp/data/big.bin" | cut -d' ' -f1)"
say "object: $size_mib MiB, sha256:$want"

# check_digests PREFIX: every $tmp/PREFIX.* holds the sha256 of the object.
check_digests() {
	local f got
	for f in "$tmp/$1".*; do
		got="$(cut -d' ' -f1 <"$f")"
		[ "$got" = "$want" ] || die "$1: $(basename "$f") has sha256:$got, want sha256:$want"
	done
}

raw_put() {
	local code
	code="$(curl -sS -K "$tmp/curlrc" -T "$tmp/data/big.bin" -o "$tmp/raw-put.json" -w '%{http_code}' "$base/raw/$run/big.bin")"
	[ "$code" = 201 ] || { cat "$tmp/raw-put.json" >&2; return 1; }
}
# raw_get and v2_get wait for their own downloads only: a bare wait would wait for the sampler.
raw_get() {
	local i pids=()
	for i in $(seq 1 "$parallel"); do
		{ curl -fsS -K "$tmp/curlrc" "$base/raw/$run/big.bin" | "${sha[@]}" >"$tmp/raw-get.$i"; } &
		pids+=($!)
	done
	wait "${pids[@]}"
}
v2_push() {
	(cd "$tmp/data" && oras push --plain-http --registry-config "$tmp/oras.json" --no-tty \
		"$host/oci/$run/big:v1" big.bin:application/octet-stream >"$tmp/v2-push.log")
}
v2_get() {
	local i pids=()
	for i in $(seq 1 "$parallel"); do
		{ oras blob fetch --plain-http --registry-config "$tmp/oras.json" --output - "$host/oci/$run/big@sha256:$want" |
			"${sha[@]}" >"$tmp/v2-get.$i"; } &
		pids+=($!)
	done
	wait "${pids[@]}"
}

secs=0
record idle 0
timed raw-put raw_put
record raw-put "$size"
timed raw-get raw_get
check_digests raw-get
record raw-get $((size * parallel))
timed v2-push v2_push
record v2-push "$size"
timed v2-get v2_get
check_digests v2-get
record v2-get $((size * parallel))
rm -f "$tmp/data/big.bin"
sleep 1
kill "$sampler" 2>/dev/null || true
wait "$sampler" 2>/dev/null || true
sampler=""

say ""
say "$("$bin" version | head -1), $(uname -sm), $(getconf _NPROCESSORS_ONLN 2>/dev/null || echo '?') CPUs; object $size_mib MiB, $parallel downloads at once; registry $registry"
printf '%-10s %8s %8s %8s %8s %8s %8s %6s\n' phase MiB s MiB/s "RSS MiB" "heap MiB" "tmp MiB" samples
printf '%s\n' "${results[@]}"
if [ "${#over[@]}" -gt 0 ]; then
	printf 'load: the resident set exceeded %d MiB: %s\n' "$max_rss_mib" "${over[*]}" >&2
	exit 1
fi
say "load: every digest matched; the peak resident set stayed under $max_rss_mib MiB"

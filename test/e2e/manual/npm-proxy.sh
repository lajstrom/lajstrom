#!/usr/bin/env bash
# npm install through a proxy repo of registry.npmjs.org (#310, docs/testing.md, "A proxy of
# npmjs.org"). A manual scenario, with a tree of hundreds of packages and the counts of the
# metrics, so make e2e does not run it (run.sh runs test/e2e/*.sh only); test/e2e/npm.sh installs
# a small tree through a proxy of the same upstream in make e2e (#312).
#
#   test/e2e/manual/npm-proxy.sh [<package spec>...]     default: jest@29.7.0
#
# Like run.sh it builds bin/lajstrom and serves it on a free port of 127.0.0.1 with a
# configuration of its own in a temporary directory: a fresh SQLite index, the registry `local`
# at $LAJSTROM_E2E_REGISTRY (localhost:5000, the Zot of make dev-up), the upstream `npmjs` at
# $NPM_PROXY_UPSTREAM (https://registry.npmjs.org/), the proxy repo npm/npmjs-<run> readable
# anonymously, the metrics listener on another free port, debug logs. Then, with npm on PATH:
#
#   1. npm install of the packages into an empty project with an empty npm cache: every tarball
#      of the tree is fetched by the proxy from the upstream, checked and pushed to the registry;
#   2. the same again with another empty cache and without node_modules or the lockfile: npm
#      asks Lajstrom for every packument and tarball again, and the proxy asks the upstream for
#      no tarball.
#
# It prints, for each install, the packages npm installed, the tarball requests Lajstrom
# answered and the tarballs they named (npm asks for some more than once), the requests to the
# upstream (tarballs and packuments) and the fetches the proxy made, from the metrics and the
# access log. It exits 1 when an install fails, when the first one did not fetch each tarball it
# served once, or when the second asked the upstream for a tarball; 2 when it could not start. What it pushed stays in the registry under npm/npmjs-<run>/; the temporary
# directory is removed on exit unless LAJSTROM_E2E_KEEP=1.
set -euo pipefail
. "$(dirname "$0")/../lib.sh"

here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/../../.." && pwd)"
registry="${LAJSTROM_E2E_REGISTRY:-localhost:5000}"
upstream="${NPM_PROXY_UPSTREAM:-https://registry.npmjs.org/}"
packages=("$@")
[ ${#packages[@]} -gt 0 ] || packages=(jest@29.7.0)
for tool in curl jq make go npm; do
	command -v "$tool" >/dev/null || { echo "npm-proxy.sh: $tool is required" >&2; exit 2; }
done
run="$(date +%Y%m%d%H%M%S)-$RANDOM"
repo="npmjs-$run"

tmp="${TMPDIR:-/tmp}"
tmp="$(mktemp -d "${tmp%/}/lajstrom-npm-proxy.XXXXXX")"
pid=""
finish() {
	if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
		kill -TERM "$pid" 2>/dev/null || true
		wait "$pid" 2>/dev/null || true
	fi
	if [ "${LAJSTROM_E2E_KEEP:-}" = 1 ]; then
		echo "npm-proxy.sh: kept $tmp"
	else
		rm -rf "$tmp"
	fi
}
trap finish EXIT

(cd "$root" && make --no-print-directory build) >"$tmp/build.log" 2>&1 || { cat "$tmp/build.log" >&2; exit 2; }
port="$(free_port)" && mport="$(free_port)" || { echo "npm-proxy.sh: no free port" >&2; exit 2; }
base="http://127.0.0.1:$port"
log="$tmp/server.log"
unset LAJSTROM_SERVER_LISTEN LAJSTROM_SERVER_EXTERNAL_URL LAJSTROM_SERVER_METRICS_LISTEN \
	LAJSTROM_LOG_LEVEL LAJSTROM_LOG_FORMAT LAJSTROM_DATABASE_DRIVER LAJSTROM_DATABASE_DSN
cat >"$tmp/lajstrom.yaml" <<EOF
# Written by test/e2e/manual/npm-proxy.sh for one run.
server:
  listen: "127.0.0.1:$port"
  externalURL: "$base"
  insecureHTTP: true
  metricsListen: "127.0.0.1:$mport"
log:
  level: debug
  format: json
database:
  driver: sqlite
  dsn: "file:$tmp/lajstrom.db"
registries:
  - name: local
    type: generic
    url: "http://$registry"
upstreams:
  - name: npmjs
    format: npm
    url: "$upstream"
reconcile:
  interval: 0s
auth:
  anonymousRead: true
repos:
  - {name: $repo, format: npm, registry: local, kind: proxy, anonymousRead: true, upstream: {name: npmjs}}
EOF
"$root/bin/lajstrom" migrate --config "$tmp/lajstrom.yaml" >"$tmp/migrate.log" 2>&1 || { cat "$tmp/migrate.log" >&2; exit 2; }
"$root/bin/lajstrom" serve --config "$tmp/lajstrom.yaml" >"$log" 2>&1 </dev/null &
pid=$!
deadline=$((SECONDS + 30))
until curl -fsS -o /dev/null --max-time 2 "$base/-/readyz" 2>/dev/null; do
	if ! kill -0 "$pid" 2>/dev/null || [ "$SECONDS" -ge "$deadline" ]; then
		tail -n 50 "$log" >&2
		echo "npm-proxy.sh: lajstrom serve on $base was not ready within 30 s (registry $registry)" >&2
		exit 2
	fi
	sleep 0.5
done
step "lajstrom serve on $base, the proxy repo npm/$repo of $upstream, registry $registry"

# metric NAME LABEL=VALUE... prints the sum of the samples of NAME with those labels.
metric() {
	local name="$1"
	shift
	curl -fsS "http://127.0.0.1:$mport/metrics" | awk -v name="$name" -v labels="$*" '
		BEGIN { n = split(labels, want, " ") }
		index($0, name) == 1 && substr($0, length(name) + 1, 1) ~ /[{ ]/ {
			for (i = 1; i <= n; i++) if (index($0, want[i]) == 0) next
			sum += $NF
		}
		END { printf "%d\n", sum }'
}

# served FROM prints the paths of the tarballs Lajstrom answered 200 or 307, from line FROM of
# the access log on, one per request.
served() {
	tail -n +"$1" "$log" | jq -r --arg p "/npm/$repo/" 'select(.msg == "request" and .method == "GET" and (.path | startswith($p))
		and (.path | contains("/-/")) and (.status == 200 or .status == 307)) | .path' 2>/dev/null
}

# install N: npm install of the packages into a new project with a new cache; it sets
# installed, requests and tarballs (the tarball requests Lajstrom answered, and the tarballs
# they named), upstream_files, upstream_docs and fetched, each for this install alone, and took
# (seconds).
install() {
	local dir="$tmp/install$1" from before_files before_docs before_fetched t0
	mkdir -p "$dir/project" "$dir/cache"
	printf '{"name": "npm-proxy-e2e", "version": "1.0.0", "private": true}\n' >"$dir/project/package.json"
	printf 'registry=%s/npm/%s/\ncache=%s\naudit=false\nfund=false\nupdate-notifier=false\n' "$base" "$repo" "$dir/cache" >"$dir/project/.npmrc"
	from=$(($(wc -l <"$log") + 1))
	before_files="$(metric lajstrom_proxy_upstream_request_duration_seconds_count 'operation="file"')"
	before_docs="$(metric lajstrom_proxy_upstream_request_duration_seconds_count 'operation="document"')"
	before_fetched="$(metric lajstrom_proxy_fetches_total 'result="fetched"')"
	t0=$SECONDS
	(cd "$dir/project" && npm install --no-audit --no-fund --loglevel=warn "${packages[@]}") >"$dir/npm.log" 2>&1 ||
		{ tail -n 30 "$dir/npm.log" >&2; tail -n 30 "$log" >&2; fail "install $1: npm install ${packages[*]} failed"; }
	took=$((SECONDS - t0))
	installed="$(cd "$dir/project" && npm ls --all --parseable 2>/dev/null | tail -n +2 | wc -l | tr -d ' ')"
	requests="$(served "$from" | wc -l | tr -d ' ')"
	tarballs="$(served "$from" | sort -u | wc -l | tr -d ' ')"
	upstream_files=$(($(metric lajstrom_proxy_upstream_request_duration_seconds_count 'operation="file"') - before_files))
	upstream_docs=$(($(metric lajstrom_proxy_upstream_request_duration_seconds_count 'operation="document"') - before_docs))
	fetched=$(($(metric lajstrom_proxy_fetches_total 'result="fetched"') - before_fetched))
	echo "install $1: ${packages[*]}: $installed packages in $took s; Lajstrom answered $requests tarball requests for $tarballs tarballs; the upstream was asked for $upstream_files tarballs and $upstream_docs packuments; the proxy fetched $fetched"
}

install 1
[ "$installed" -gt 0 ] || fail "install 1: npm installed nothing"
expect "install 1: tarballs fetched from the upstream, one per tarball served" "$tarballs" "$fetched"
step "install 1 from a clean cache: each of the $tarballs tarballs fetched once by the proxy"
install 2
expect "install 2: upstream requests for tarballs" 0 "$upstream_files"
[ "$tarballs" -gt 0 ] || fail "install 2: Lajstrom served no tarball; did npm read its own cache?"
step "install 2 from a clean cache: $tarballs tarballs served by Lajstrom, none asked of the upstream"
echo "PASS npm-proxy e2e against $base (upstream $upstream)"

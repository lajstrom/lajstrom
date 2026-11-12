#!/usr/bin/env bash
# End-to-end test of moving npm packages from Verdaccio into Lajstrom (#441; docs/migrating.md,
# docs/spec/proxy-and-group.md §4.4 and §5.1): a proxy repo whose upstream is the old registry, a
# prefetch, and the upstream switched off.
#
#   1. Three private packages of a scope of the run are published to the Verdaccio of the
#      verdaccio profile, with its user's token, as npm publish sends them (the tarball, its
#      sha512 and sha1): @mig-<run>/a 1.0.0, 1.1.0 and 2.0.0-rc.1 (dist-tag next), latest moved
#      back to 1.0.0, below the highest; b 1.0.0 and 1.0.1, which depend on a ^1.0.0; c 0.1.0 and
#      0.2.0, 0.1.0 deprecated. Verdaccio serves none of them without a token.
#   2. A proxy repo legacy-<run> of the upstream verdaccio (run.sh configures it with that
#      token as auth.tokenFile), and a user with the maintainer role on it and a token.
#   3. lajstrom admin repo prefetch with the user's token: without --all-versions it fetches the
#      versions the dist-tags name (4), then with --all-versions the others (3) and finds the 4
#      held, then all 7 held, each run exit 0 with its report; 7 proxy.fetch rows by the user.
#   4. Each package's dist-tags and deprecations as Verdaccio has them, each version's
#      dist.integrity, and each tarball byte for byte (sha512 of the bytes both serve).
#   5. The switch: upstream.offline true. A prefetch is now refused (409, exit 1). When
#      $MIGRATE_E2E_VERDACCIO_CONTAINER names the Verdaccio's container, it is stopped here
#      (and started again on exit); otherwise Verdaccio keeps running and offline mode alone keeps
#      the server from it.
#   6. npm install of b, a@next and c@0.1.0 into an empty project with an empty cache, from the
#      proxy repo with the user's token: the versions resolved through the dist-tags (b's a
#      ^1.0.0 is latest, 1.0.0, not the highest), the integrity of the lockfile Verdaccio's, the
#      deprecation warned.
#   7. The conversion (§2.6): PATCH {"kind": "hosted"} as the admin; repo.convert plans 5 journal
#      ops (4 dist-tags, a deprecation) and skips nothing; every packument has Verdaccio's
#      dist-tags, deprecations, integrity, and publish times to the second.
#   8. The repo deleted (force), made again empty and reindexed: the same, from the registry.
#   9. A publish to the converted repo: latest moves to it, next stays.
#
#   test/e2e/migrate-npm.sh [<base url>]     default: http://localhost:8080
#
# It needs LAJSTROM_E2E_VERDACCIO and LAJSTROM_E2E_VERDACCIO_TOKEN_FILE, which run.sh sets when
# the Verdaccio answers (make dev-up PROFILES=verdaccio), and the upstream verdaccio in the
# server's configuration. It runs as $LAJSTROM_E2E_USER (lib.sh, e2e_credentials), makes its user
# and token with lajstrom admin ($LAJSTROM_BIN, the configuration of $LAJSTROM_CONFIG), and on
# exit deletes the repo (force), revokes the token and disables the user; the packages stay in
# Verdaccio under their scope of the run. npm runs in node:26.13.0-slim ($NPM_E2E_IMAGE) with
# --network host. The tokens go into curl configurations and .npmrc, mode 600, never on a command
# line. It needs curl, jq, docker, shasum or sha1sum, and openssl, prints one line per step and
# exits non-zero at the first failure (lib.sh).
set -euo pipefail
. "$(dirname "$0")/lib.sh"
umask 077

base="${1:-http://localhost:8080}"
base="${base%/}"
for tool in curl jq docker openssl; do
	command -v "$tool" >/dev/null || { echo "migrate-npm.sh: $tool is required" >&2; exit 2; }
done
verdaccio="${LAJSTROM_E2E_VERDACCIO:-}"
vtoken="${LAJSTROM_E2E_VERDACCIO_TOKEN_FILE:-}"
if [ -z "$verdaccio" ] || [ ! -s "$vtoken" ]; then
	echo "migrate-npm.sh: needs LAJSTROM_E2E_VERDACCIO and LAJSTROM_E2E_VERDACCIO_TOKEN_FILE (run.sh sets them)" >&2
	exit 2
fi
verdaccio="${verdaccio%/}"
if [ -n "${LAJSTROM_BIN:-}" ]; then
	lajstrom=("$LAJSTROM_BIN")
else
	lajstrom=(bin/lajstrom)
fi
image="${NPM_E2E_IMAGE:-node:26.13.0-slim}"
container="${MIGRATE_E2E_VERDACCIO_CONTAINER:-}"
ids="$(id -u):$(id -g)"

work="$(mktemp -d)"
e2e_credentials "$work"
printf 'header = "Authorization: Bearer %s"\n' "$(cat "$vtoken")" >"$work/verdaccio.curlrc"
run="$(date -u +%Y%m%d%H%M%S)-$RANDOM"
scope="@mig-$run"
repo="legacy-$run"
user="e2e-migrate-$run"
names=("$scope/a" "$scope/b" "$scope/c")
token_id=""
stopped=""
cleanup() {
	[ -z "$stopped" ] || docker start "$container" >/dev/null 2>&1 || true
	[ -z "$token_id" ] || "${lajstrom[@]}" admin token revoke "$token_id" >/dev/null 2>&1 || true
	curl -sS -o /dev/null -K "$E2E_CURLRC" -X DELETE "$base/api/v1/repos/npm/$repo?force=true" || true
	"${lajstrom[@]}" admin user disable "$user" >/dev/null 2>&1 || true
	rm -rf "$work"
}
trap cleanup EXIT

# enc NAME: NAME as a segment of a registry URL, the scope's / as %2f.
enc() { printf '%s' "${1/\//%2f}"; }

# vpublish NAME VERSION TAG [DEPENDENCY RANGE]: VERSION of NAME published to Verdaccio under
# TAG (publish).
vpublish() { publish "$verdaccio" "$work/verdaccio.curlrc" "$@"; }

# publish REGISTRY CURLRC NAME VERSION TAG [DEPENDENCY RANGE]: VERSION of NAME published to the
# registry at REGISTRY with the curl configuration CURLRC under TAG, as npm publish sends it: the
# packument of the version with dist.integrity (sha512) and dist.shasum (sha1) of the tarball,
# and the tarball as its attachment.
publish() {
	local registry="$1" curlrc="$2"
	shift 2
	local name="$1" v="$2" tag="$3" dep="${4:-}" range="${5:-}" dir="$work/pub/$1/$2"
	mkdir -p "$dir/package"
	if [ -n "$dep" ]; then
		jq -n --arg n "$name" --arg v "$v" --arg d "$dep" --arg r "$range" \
			'{name: $n, version: $v, main: "index.js", license: "MIT", dependencies: {($d): $r}}' >"$dir/package/package.json"
	else
		jq -n --arg n "$name" --arg v "$v" '{name: $n, version: $v, main: "index.js", license: "MIT"}' >"$dir/package/package.json"
	fi
	printf 'module.exports = "%s@%s";\n' "$name" "$v" >"$dir/package/index.js"
	(cd "$dir" && COPYFILE_DISABLE=1 tar -cf - package | gzip -n >"$dir/pkg.tgz")
	local integrity shasum
	integrity="sha512-$(openssl dgst -sha512 -binary "$dir/pkg.tgz" | base64 | tr -d '\n')"
	shasum="$(openssl dgst -sha1 -r "$dir/pkg.tgz" | cut -d' ' -f1)"
	jq -n --slurpfile pj "$dir/package/package.json" --arg tag "$tag" --arg v "$v" --arg tarball "$registry/$name/-/$name-$v.tgz" \
		--arg integrity "$integrity" --arg shasum "$shasum" --arg data "$(base64 <"$dir/pkg.tgz" | tr -d '\n')" \
		--argjson length "$(wc -c <"$dir/pkg.tgz")" \
		'$pj[0] as $p | {_id: $p.name, name: $p.name, "dist-tags": {($tag): $v},
		  versions: {($v): ($p + {_id: "\($p.name)@\($v)", dist: {tarball: $tarball, integrity: $integrity, shasum: $shasum}})},
		  _attachments: {("\($p.name)-\($v).tgz"): {content_type: "application/octet-stream", data: $data, length: $length}}}' >"$dir/body.json"
	assert_status 201 -K "$curlrc" -X PUT -H 'Content-Type: application/json' --data-binary "@$dir/body.json" "$registry/$(enc "$name")"
}

# 1. The packages in Verdaccio.
vpublish "$scope/a" 1.0.0 latest
vpublish "$scope/a" 1.1.0 latest
vpublish "$scope/a" 2.0.0-rc.1 next
assert_status 201 -K "$work/verdaccio.curlrc" -X PUT -H 'Content-Type: application/json' -d '"1.0.0"' \
	"$verdaccio/-/package/$(enc "$scope/a")/dist-tags/latest"
vpublish "$scope/b" 1.0.0 latest "$scope/a" "^1.0.0"
vpublish "$scope/b" 1.0.1 latest "$scope/a" "^1.0.0"
vpublish "$scope/c" 0.1.0 latest
vpublish "$scope/c" 0.2.0 latest
assert_status 200 -K "$work/verdaccio.curlrc" "$verdaccio/$(enc "$scope/c")"
rev="$(jq -r ._rev <<<"$E2E_BODY")"
jq '.versions["0.1.0"].deprecated = "use 0.2.0"' <<<"$E2E_BODY" >"$work/deprecate.json"
assert_status 201 -K "$work/verdaccio.curlrc" -X PUT -H 'Content-Type: application/json' --data-binary "@$work/deprecate.json" \
	"$verdaccio/$(enc "$scope/c")/-rev/$rev"
assert_status 401 "$verdaccio/$(enc "$scope/a")"
assert_status 200 -K "$work/verdaccio.curlrc" "$verdaccio/$(enc "$scope/a")"
expect "Verdaccio's dist-tags of $scope/a" '{"latest":"1.0.0","next":"2.0.0-rc.1"}' "$(jq -S -c '.["dist-tags"]' <<<"$E2E_BODY")"
step "Verdaccio $verdaccio: $scope/a 1.0.0 1.1.0 2.0.0-rc.1 (latest 1.0.0, next), b 1.0.0 1.0.1, c 0.1.0 (deprecated) 0.2.0; 401 without a token"

# 2. The proxy repo, its maintainer and the maintainer's token.
assert_status 201 -K "$E2E_CURLRC" -X POST "$base/api/v1/repos" \
	-d "{\"format\": \"npm\", \"name\": \"$repo\", \"registry\": \"local\", \"kind\": \"proxy\", \"upstream\": {\"name\": \"verdaccio\"}}"
head -c 24 /dev/urandom | base64 | tr -d '+/=\n' | "${lajstrom[@]}" admin user create --name "$user" --password-stdin >/dev/null
"${lajstrom[@]}" admin binding create --principal "user:$user" --role maintainer --mask "npm/$repo" >/dev/null
"${lajstrom[@]}" admin token create --user "$user" --name "migrate $run" >"$work/token.json"
token_id="$(jq -r .id "$work/token.json")"
jq -r .secret "$work/token.json" >"$work/token"
rm "$work/token.json"
printf 'header = "Authorization: Bearer %s"\n' "$(cat "$work/token")" >"$work/bearer.curlrc"
step "POST /api/v1/repos npm/$repo, kind proxy, upstream verdaccio; user $user, maintainer of npm/$repo, token:$token_id"

# 3. The prefetch.
printf '# the private scope of the run\n%s\n%s\n%s\n' "${names[@]}" >"$work/names"

# prefetch ARGS...: lajstrom admin repo prefetch npm/$repo --from-file names with ARGS and the
# user's token; its output in $work/out, its report in $work/report.json, its status in $rc.
prefetch() {
	rc=0
	LAJSTROM_URL="$base" LAJSTROM_TOKEN_FILE="$work/token" "${lajstrom[@]}" admin repo prefetch "npm/$repo" \
		--from-file "$work/names" --report "$work/report.json" "$@" >"$work/out" 2>"$work/err" || rc=$?
}

# counts: the counts of the last report, fetched held shared failed.
counts() { jq -r '"\(.counts.fetched // 0) \(.counts.held // 0) \(.counts.shared // 0) \(.counts.failed // 0)"' "$work/report.json"; }

prefetch
expect "the first prefetch's exit status ($(cat "$work/err") $(cat "$work/out"))" 0 "$rc"
expect "the first prefetch: fetched held shared failed" "4 0 0 0" "$(counts)"
expect "the first prefetch's versions" "a 1.0.0|a 2.0.0-rc.1|b 1.0.1|c 0.2.0" \
	"$(jq -r --arg s "$scope/" '[.results[] | "\(.name | ltrimstr($s)) \(.version)"] | join("|")' "$work/report.json")"
step "admin repo prefetch npm/$repo: the dist-tags' versions fetched, a 1.0.0 and 2.0.0-rc.1, b 1.0.1, c 0.2.0"
prefetch --all-versions
expect "the second prefetch's exit status ($(cat "$work/err") $(cat "$work/out"))" 0 "$rc"
expect "--all-versions: fetched held shared failed" "3 4 0 0" "$(counts)"
prefetch --all-versions
expect "--all-versions again: fetched held shared failed" "0 7 0 0" "$(counts)"
step "admin repo prefetch --all-versions: 3 fetched and 4 held, then all 7 held"
assert_status 200 -K "$work/bearer.curlrc" "$base/api/v1/whoami"
principal="$(jq -r .principal <<<"$E2E_BODY")"
assert_status 200 -K "$E2E_CURLRC" "$base/api/v1/audit?repo=npm/$repo&action=proxy.fetch&limit=100"
expect "proxy.fetch rows by $user ($principal)" "7 7" \
	"$(jq -r --arg p "$principal" '"\(.events | length) \([.events[] | select(.principal == $p)] | length)"' <<<"$E2E_BODY")"
step "audit: 7 proxy.fetch rows, by $user"

# 4. What Lajstrom serves is what Verdaccio served.
for name in "${names[@]}"; do
	assert_status 200 -K "$work/verdaccio.curlrc" "$verdaccio/$(enc "$name")"
	printf '%s' "$E2E_BODY" >"$work/v.json"
	cp "$work/v.json" "$work/v-${name##*/}.json" # Verdaccio's packument, for after the conversion
	assert_status 200 -K "$work/bearer.curlrc" "$base/npm/$repo/$(enc "$name")"
	printf '%s' "$E2E_BODY" >"$work/l.json"
	expect "the dist-tags of $name" "$(jq -S -c '.["dist-tags"]' "$work/v.json")" "$(jq -S -c '.["dist-tags"]' "$work/l.json")"
	expect "the deprecations of $name" "$(jq -S -c '[.versions[] | {version, deprecated}]' "$work/v.json")" \
		"$(jq -S -c '[.versions[] | {version, deprecated}]' "$work/l.json")"
	for v in $(jq -r '.versions | keys[]' "$work/v.json"); do
		expect "the integrity of $name@$v" "$(jq -r --arg v "$v" '.versions[$v].dist.integrity' "$work/v.json")" \
			"$(jq -r --arg v "$v" '.versions[$v].dist.integrity' "$work/l.json")"
		curl -fsS -K "$work/verdaccio.curlrc" -o "$work/v.tgz" "$(jq -r --arg v "$v" '.versions[$v].dist.tarball' "$work/v.json")"
		curl -fsS -K "$work/bearer.curlrc" -o "$work/l.tgz" "$(jq -r --arg v "$v" '.versions[$v].dist.tarball' "$work/l.json")"
		expect "the sha512 of the tarball of $name@$v" "$(openssl dgst -sha512 -r "$work/v.tgz" | cut -d' ' -f1)" \
			"$(openssl dgst -sha512 -r "$work/l.tgz" | cut -d' ' -f1)"
	done
done
step "the dist-tags, the deprecations, every dist.integrity and every tarball's bytes are Verdaccio's"

# 5. The switch.
assert_status 200 -K "$E2E_CURLRC" -X PATCH -H 'Content-Type: application/json' -d '{"upstream": {"offline": true}}' "$base/api/v1/repos/npm/$repo"
expect "upstream.offline" true "$(jq -r .upstream.offline <<<"$E2E_BODY")"
prefetch
expect "a prefetch of an offline proxy: exit status" 1 "$rc"
grep -q "409 Conflict" "$work/out" || fail "a prefetch of an offline proxy: $(cat "$work/out")"
if [ -n "$container" ]; then
	docker stop "$container" >/dev/null
	stopped=1
	! curl -fsS -o /dev/null --max-time 2 "$verdaccio/-/ping" 2>/dev/null || fail "Verdaccio still answers after docker stop $container"
	step "PATCH upstream.offline true; a prefetch refused (409, exit 1); docker stop $container: Verdaccio does not answer"
else
	step "PATCH upstream.offline true; a prefetch refused (409, exit 1); Verdaccio left running (MIGRATE_E2E_VERDACCIO_CONTAINER unset)"
fi

# 6. npm install from the proxy repo alone.
hostport="${base#*://}"
mkdir -p "$work/app" "$work/home"
printf '{"name": "app", "version": "1.0.0", "private": true}\n' >"$work/app/package.json"
printf '%s:registry=%s/npm/%s/\n//%s/npm/%s/:_authToken=%s\n' "$scope" "$base" "$repo" "$hostport" "$repo" "$(cat "$work/token")" >"$work/app/.npmrc"
rc=0
docker run --rm --network host -u "$ids" -e HOME=/w/home -e npm_config_update_notifier=false -e npm_config_fund=false \
	-e npm_config_audit=false -v "$work:/w" -w /w/app "$image" \
	npm install "$scope/b" "$scope/a@next" "$scope/c@0.1.0" >"$work/install.log" 2>&1 || rc=$?
! grep -qF -f "$work/token" "$work/install.log" || fail "npm's log holds the token"
[ "$rc" = 0 ] || { tail -n 30 "$work/install.log" >&2; fail "npm install from npm/$repo: exit $rc"; }
lock="$work/app/package-lock.json"
# b's a ^1.0.0 is 1.0.0, not the highest 1.1.0: npm takes the version of latest when it is in the range.
expect "the installed versions" "2.0.0-rc.1 1.0.1 1.0.0 0.1.0" "$(jq -r --arg s "$scope" \
	'[.packages["node_modules/\($s)/a"].version, .packages["node_modules/\($s)/b"].version,
	  .packages["node_modules/\($s)/b/node_modules/\($s)/a"].version, .packages["node_modules/\($s)/c"].version] | join(" ")' "$lock")"
for entry in a "b/node_modules/$scope/a" b c; do
	pkg="$scope/${entry##*/}"
	v="$(jq -r --arg e "node_modules/$scope/$entry" '.packages[$e].version' "$lock")"
	expect "the lockfile's integrity of $pkg@$v" "$(jq -r --arg v "$v" '.versions[$v].dist.integrity' "$work/pub/$pkg/$v/body.json")" \
		"$(jq -r --arg e "node_modules/$scope/$entry" '.packages[$e].integrity' "$lock")"
done
grep -q "deprecated" "$work/install.log" || fail "npm did not warn that $scope/c@0.1.0 is deprecated: $(cat "$work/install.log")"
step "npm install $scope/b $scope/a@next $scope/c@0.1.0 from npm/$repo with Verdaccio off: a 2.0.0-rc.1, b 1.0.1 with a 1.0.0 (latest, not the highest), c 0.1.0 (deprecated), Verdaccio's integrity"

# 7. The conversion: the proxy becomes a hosted repo, its dist-tags and deprecations written to
# its journal (proxy-and-group.md §2.6).

# same_as_verdaccio WHAT: the packuments of the repo, read as its maintainer, have Verdaccio's
# dist-tags, deprecations, integrity, and publish times to the second.
same_as_verdaccio() {
	local name v
	for name in "${names[@]}"; do
		assert_status 200 -K "$work/bearer.curlrc" "$base/npm/$repo/$(enc "$name")"
		printf '%s' "$E2E_BODY" >"$work/l.json"
		v="$work/v-${name##*/}.json"
		expect "$1: the dist-tags of $name" "$(jq -S -c '.["dist-tags"]' "$v")" "$(jq -S -c '.["dist-tags"]' "$work/l.json")"
		expect "$1: the deprecations and integrity of $name" \
			"$(jq -S -c '[.versions[] | {version, deprecated, integrity: .dist.integrity}]' "$v")" \
			"$(jq -S -c '[.versions[] | {version, deprecated, integrity: .dist.integrity}]' "$work/l.json")"
		expect "$1: the publish times of $name" \
			"$(jq -c '[.time | to_entries[] | select(.key != "created" and .key != "modified") | "\(.key) \(.value[:19])"] | sort' "$v")" \
			"$(jq -c '[.time | to_entries[] | select(.key != "created" and .key != "modified") | "\(.key) \(.value[:19])"] | sort' "$work/l.json")"
	done
}

assert_status 200 -K "$E2E_CURLRC" -X PATCH -H 'Content-Type: application/json' -d '{"kind": "hosted"}' "$base/api/v1/repos/npm/$repo"
expect "the kind after the conversion" "hosted null" "$(jq -r '"\(.kind) \(.upstream)"' <<<"$E2E_BODY")"
assert_status 200 -K "$E2E_CURLRC" "$base/api/v1/audit?repo=npm/$repo&action=repo.convert"
expect "repo.convert: packages ops skipped" "3 5 0" "$(jq -r '.events[0].details | "\(.packages) \(.ops) \(.skipped | length)"' <<<"$E2E_BODY")"
same_as_verdaccio "converted"
step "PATCH {\"kind\": \"hosted\"}: npm/$repo hosted, 5 journal ops (4 dist-tags, 1 deprecation); dist-tags, deprecations, integrity and times Verdaccio's"

# 8. The journal survives the index: the repo deleted, made again empty, and reindexed.
assert_status 204 -K "$E2E_CURLRC" -X DELETE "$base/api/v1/repos/npm/$repo?force=true"
assert_status 201 -K "$E2E_CURLRC" -X POST "$base/api/v1/repos" -d "{\"format\": \"npm\", \"name\": \"$repo\", \"registry\": \"local\"}"
assert_status 202 -K "$E2E_CURLRC" -X POST "$base/api/v1/repos/npm/$repo/reindex"
for _ in $(seq 1 120); do
	assert_status 200 -K "$E2E_CURLRC" "$base/api/v1/repos/npm/$repo/reindex"
	[ "$(jq -r .state <<<"$E2E_BODY")" = running ] || break
	sleep 0.5
done
expect "the state of the reindex ($(head -c 300 <<<"$E2E_BODY"))" done "$(jq -r .state <<<"$E2E_BODY")"
same_as_verdaccio "reindexed"
step "npm/$repo deleted (force), made again empty and reindexed: the same dist-tags, deprecations, integrity and times"

# 9. The hosted repo takes a publish; latest moves to it, next stays.
publish "$base/npm/$repo" "$work/bearer.curlrc" "$scope/a" 1.2.0 latest
assert_status 200 -K "$work/bearer.curlrc" "$base/npm/$repo/$(enc "$scope/a")"
expect "the dist-tags of $scope/a after a publish" '{"latest":"1.2.0","next":"2.0.0-rc.1"}' "$(jq -S -c '.["dist-tags"]' <<<"$E2E_BODY")"
step "a publish of $scope/a 1.2.0 to the converted repo: 201, latest 1.2.0, next kept"

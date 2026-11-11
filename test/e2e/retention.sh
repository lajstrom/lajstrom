#!/usr/bin/env bash
# End-to-end test of lajstrom admin retention against a running Lajstrom (#72; docs/operations.md,
# Retention): a client of the management API, as a CronJob runs it.
#
#   - an npm repo npm/retention-<run> through POST /api/v1/repos, and a user of its own with the
#     maintainer role on it and a personal access token, made with lajstrom admin;
#   - a package with 1.0.0, 1.1.0 and 1.2.0, published with curl as npm publishes (PUT of the
#     packument with the tarball in _attachments, the dist-tag latest moving to each), and the
#     dist-tag beta added to 1.0.0;
#   - GET …/packages and …/versions: the three, newest first, beta and latest as pointers;
#   - lajstrom admin retention --keep-last 1, a dry run: exit status 2, 1.1.0 the one version to
#     delete; 1.0.0 stays, a dist-tag names it, and 1.2.0, the newest;
#   - --apply --output json: exit status 0, 1.1.0 deleted, the other two listed still; an audit
#     row delete of 1.1.0 with the job's id and the token's (details.job, details.token);
#   - --apply again: nothing to delete, exit status 0.
#
#   test/e2e/retention.sh [<base url>]     default: http://localhost:8080
#
# It runs as $LAJSTROM_E2E_USER, a user with the admin binding (lib.sh, e2e_credentials; run.sh
# makes one), for the repo and the audit log; lajstrom admin runs against the index of
# $LAJSTROM_CONFIG (run.sh exports it), the binary is $LAJSTROM_BIN, else bin/lajstrom. On exit
# it revokes the token, deletes the repo (force) and disables its user. The token goes to curl
# and to lajstrom in files, never on a command line, and is never printed. It needs curl, jq,
# tar, gzip and base64, prints one line per step and exits non-zero at the first failure (lib.sh).
set -euo pipefail
. "$(dirname "$0")/lib.sh"
umask 077

base="${1:-http://localhost:8080}"
base="${base%/}"
for tool in curl jq tar gzip base64; do
	command -v "$tool" >/dev/null || { echo "retention.sh: $tool is required" >&2; exit 2; }
done
if [ -n "${LAJSTROM_BIN:-}" ]; then
	lajstrom=("$LAJSTROM_BIN")
else
	lajstrom=(bin/lajstrom)
fi
work="$(mktemp -d)"
e2e_credentials "$work"
run="$(date -u +%Y%m%d%H%M%S)-$RANDOM"
repo="retention-$run"
user="e2e-retention-$run"
pkg="e2e-retention-$run"
token_id=""
cleanup() {
	[ -z "$token_id" ] || "${lajstrom[@]}" admin token revoke "$token_id" >/dev/null 2>&1 || true
	curl -sS -o /dev/null -K "$E2E_CURLRC" -X DELETE "$base/api/v1/repos/npm/$repo?force=true" || true
	"${lajstrom[@]}" admin user disable "$user" >/dev/null 2>&1 || true
	rm -rf "$work"
}
trap cleanup EXIT

# The repo, the user with the maintainer role on it, and its token in a file.
assert_status 201 -K "$E2E_CURLRC" -X POST "$base/api/v1/repos" -d "{\"format\": \"npm\", \"name\": \"$repo\", \"registry\": \"local\"}"
head -c 24 /dev/urandom | base64 | tr -d '+/=\n' | "${lajstrom[@]}" admin user create --name "$user" --password-stdin >/dev/null
"${lajstrom[@]}" admin binding create --principal "user:$user" --role maintainer --mask "npm/$repo" >/dev/null
"${lajstrom[@]}" admin token create --user "$user" --name "retention $run" >"$work/token.json"
token_id="$(jq -r .id "$work/token.json")"
jq -r .secret "$work/token.json" >"$work/token"
rm "$work/token.json"
printf 'header = "Authorization: Bearer %s"\n' "$(cat "$work/token")" >"$work/bearer.curlrc"
step "POST /api/v1/repos npm/$repo; admin user create $user, binding maintainer npm/$repo, token create: token:$token_id"

# publish VERSION: PUT the packument of VERSION with its tarball, as npm publish sends it.
publish() {
	local v="$1" dir="$work/pkg-$1"
	mkdir -p "$dir/package"
	printf '{"name": "%s", "version": "%s", "main": "index.js"}\n' "$pkg" "$v" >"$dir/package/package.json"
	printf 'module.exports = "%s";\n' "$v" >"$dir/package/index.js"
	(cd "$dir" && COPYFILE_DISABLE=1 tar -cf - package | gzip -n >"$dir/pkg.tgz")
	jq -n --arg name "$pkg" --arg v "$v" --arg data "$(base64 <"$dir/pkg.tgz" | tr -d '\n')" --argjson length "$(wc -c <"$dir/pkg.tgz")" \
		--arg tarball "$base/npm/$repo/$pkg/-/$pkg-$v.tgz" \
		'{_id: $name, name: $name, "dist-tags": {latest: $v},
		  versions: {($v): {name: $name, version: $v, main: "index.js", dist: {tarball: $tarball}}},
		  _attachments: {("\($name)-\($v).tgz"): {content_type: "application/octet-stream", data: $data, length: $length}}}' >"$dir/body.json"
	assert_status 201 -K "$work/bearer.curlrc" -X PUT -H 'Content-Type: application/json' --data-binary "@$dir/body.json" "$base/npm/$repo/$pkg"
}
for v in 1.0.0 1.1.0 1.2.0; do
	publish "$v"
done
assert_status 201 -K "$work/bearer.curlrc" -X PUT -H 'Content-Type: application/json' -d '"1.0.0"' "$base/npm/$repo/-/package/$pkg/dist-tags/beta"
step "PUT /npm/$repo/$pkg of 1.0.0, 1.1.0 and 1.2.0 with the token: 201 each; dist-tag beta 1.0.0: 201"

# The lists.
assert_status 200 -K "$work/bearer.curlrc" "$base/api/v1/repos/npm/$repo/packages"
expect "GET …/packages" "$pkg" "$(jq -r '[.packages[].name] | join(" ")' <<<"$E2E_BODY")"
assert_status 200 -K "$work/bearer.curlrc" "$base/api/v1/repos/npm/$repo/packages/$pkg/versions"
expect "GET …/versions" "1.2.0 latest|1.1.0 |1.0.0 beta" \
	"$(jq -r '[.versions[] | "\(.version) \([.pointers[].name] | join(","))"] | join("|")' <<<"$E2E_BODY")"
d11="$(jq -r '.versions[] | select(.version == "1.1.0") | .variants[0].digest' <<<"$E2E_BODY")"
step "GET …/packages: $pkg; …/versions: 1.2.0 (latest), 1.1.0 ($d11), 1.0.0 (beta)"

# retention runs lajstrom admin retention --repo npm/$repo with ARGS as the job does, its output
# in $work/out and its exit status in $rc.
retention() {
	rc=0
	LAJSTROM_URL="$base" LAJSTROM_TOKEN_FILE="$work/token" "${lajstrom[@]}" admin retention --repo "npm/$repo" "$@" \
		>"$work/out" 2>"$work/err" || rc=$?
}

retention --keep-last 1
expect "the dry run's exit status ($(cat "$work/err"))" 2 "$rc"
expect "the dry run's versions to delete" "delete $pkg 1.1.0" "$(grep '^delete ' "$work/out" | cut -d' ' -f1-3)"
grep -q "rank 2 > keep-last 1" "$work/out" || fail "the dry run does not say why: $(cat "$work/out")"
step "admin retention --keep-last 1: exit status 2, 1.1.0 to delete (rank 2 > keep-last 1); 1.0.0 (beta) and 1.2.0 (newest) kept"

retention --keep-last 1 --apply --output json
expect "--apply's exit status ($(cat "$work/err"))" 0 "$rc"
expect "--apply's results" "1.1.0 deleted" "$(jq -r '[.results[] | "\(.version) \(.status)"] | join("|")' "$work/out")"
job="$(jq -r .job "$work/out")"
assert_status 200 -K "$work/bearer.curlrc" "$base/api/v1/repos/npm/$repo/packages/$pkg/versions"
expect "the versions after --apply" "1.2.0 1.0.0" "$(jq -r '[.versions[].version] | join(" ")' <<<"$E2E_BODY")"
assert_status 200 -K "$E2E_CURLRC" "$base/api/v1/audit?repo=npm/$repo&action=delete"
expect "the audit row of the delete" "$pkg 1.1.0|$d11|$job|token:$token_id" \
	"$(jq -r '[.events[] | "\(.subject)|\(.digest)|\(.details.job)|\(.details.token)"] | join(" ")' <<<"$E2E_BODY")"
step "admin retention --keep-last 1 --apply: exit status 0, 1.1.0 deleted; 1.2.0 and 1.0.0 left; audit row delete with job $job and token:$token_id"

retention --keep-last 1 --apply
expect "a second --apply's exit status ($(cat "$work/err"))" 0 "$rc"
grep -q "0 of 2 versions in 1 packages to delete" "$work/out" || fail "a second --apply: $(cat "$work/out")"
step "admin retention --keep-last 1 --apply again: nothing to delete, exit status 0"

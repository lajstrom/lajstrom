#!/usr/bin/env bash
# End-to-end test of oras through the OCI facade against a running Lajstrom
# (docs/quickstart/oras.md §3; docs/acceptance.md O-01, O-02 and O-04): the challenge of
# GET /v2/, oras login, oras push of a file to a raw repo with the raw profile, the same manifest
# again (201, the same digest), another manifest under the tag (409 TAG_INVALID naming the
# first), the repo's policy (#60: PATCH merges it; a file above maxFileSize is 413 through oras
# push and the raw PUT), oras pull and the bytes, oras repo tags, oras manifest fetch, the raw
# frontend serving the pushed file, then oras manifest delete through the facade (#61) and a 404.
#
#   test/e2e/oras.sh [<base url> [<raw repo>]]     default: http://localhost:8080 scratch
#
# The repo must exist (the example configuration creates raw/scratch), and the user of
# LAJSTROM_E2E_USER and LAJSTROM_E2E_PASSWORD must push to it and delete in it (run.sh makes an
# admin; lib.sh, e2e_credentials): oras logs in with them, and gets its tokens from /v2/token
# with Basic. Tokens of other kinds are in oci-token.sh and tokens.sh. The file is at a path of
# its own for the run, so the script can run again against the same repo, and it deletes it at
# the end. oras is the oras on PATH, else ghcr.io/oras-project/oras:v1.3.6 in docker with
# --network host (which reaches the base URL on Linux and under OrbStack); either way it gets a
# registry configuration of its own. It needs curl and jq, prints one line per step and exits
# non-zero at the first failure.
set -euo pipefail
. "$(dirname "$0")/lib.sh"

base="${1:-http://localhost:8080}"
base="${base%/}"
repo="${2:-scratch}"
for tool in curl jq; do
	command -v "$tool" >/dev/null || { echo "oras.sh: $tool is required" >&2; exit 2; }
done
oras_image=ghcr.io/oras-project/oras:v1.3.6
registry="${base#*://}" # host:port of the facade, for oras
plain=""                # --plain-http for an http:// base URL
case "$base" in http://*) plain=--plain-http ;; esac

work="$(mktemp -d)"
e2e_credentials "$work"
run="e2e-oras-$(date -u +%Y%m%d%H%M%S)-$RANDOM"
path="$run/hello.txt"  # lower case: the path is also the OCI repository below raw/<repo>/
name="raw/$repo/$path" # the OCI name of the file
pushed=""
policy_set=""
cleanup() {
	[ -z "$pushed" ] || curl -sS -o /dev/null -K "$E2E_CURLRC" -X DELETE "$base/raw/$repo/$path" || true
	[ -z "$policy_set" ] || curl -sS -o /dev/null -K "$E2E_CURLRC" -X PATCH -H 'Content-Type: application/json' \
		-d '{"policy": null}' "$base/api/v1/repos/raw/$repo" || true
	rm -rf "$work"
}
trap cleanup EXIT

# oras ARGS...: oras in the run's directory, with a registry configuration of its own there.
if command -v oras >/dev/null; then
	oras_how="$(command -v oras), $(command oras version | head -1)"
	oras() { (cd "$work" && command oras "$@" --registry-config "$work/oras-auth.json" ${plain:+"$plain"}); }
else
	command -v docker >/dev/null || { echo "oras.sh: oras or docker is required" >&2; exit 2; }
	oras_how="$oras_image"
	# As the caller's user: what oras pull writes as root on Linux, the cleanup could not remove.
	oras() { docker run --rm -i --network host -u "$(id -u):$(id -g)" -v "$work:/w" -w /w "$oras_image" "$@" --registry-config /w/oras-auth.json ${plain:+"$plain"}; }
fi

# sha256 prints the hex sha256 of a file.
sha256() {
	if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

# push CREATED: oras push of $path, with the raw annotations and CREATED as the created time,
# from the run's directory, so that the layer is titled with the path; the output goes to
# $work/oras.log and oras's status is returned.
push() {
	oras push --artifact-type application/vnd.lajstrom.raw.v1 \
		--annotation lajstrom.schema=1 --annotation lajstrom.format=raw --annotation "lajstrom.repo=$repo" \
		--annotation "lajstrom.name=$path" --annotation "lajstrom.version=$version" \
		--annotation "lajstrom.raw.path=$path" --annotation "org.opencontainers.image.created=$1" \
		"$registry/$name:$version" "$path:application/vnd.lajstrom.raw.file.v1" >"$work/oras.log" 2>&1
}

# O-01: the challenge without credentials, which oras answers with its login.
assert_status 401 -D "$work/headers" "$base/v2/"
challenge="$(tr -d '\r' <"$work/headers" | awk 'tolower($1) == "www-authenticate:" { sub(/^[^:]*: /, ""); print }')"
case "$challenge" in 'Bearer realm="'*'/v2/token",service="'*'"') ;; *) fail "GET /v2/ challenge: $challenge" ;; esac
step "GET /v2/ without credentials: 401 WWW-Authenticate: $challenge"
printf '%s\n' "$E2E_PASSWORD" | oras login -u "$E2E_USER" --password-stdin "$registry" >"$work/oras.log" 2>&1 ||
	{ cat "$work/oras.log" >&2; fail "oras login"; }
grep -q 'Login Succeeded' "$work/oras.log" || fail "oras login: $(cat "$work/oras.log")"
step "oras login $registry -u $E2E_USER: Login Succeeded"

mkdir -p "$work/$run"
printf 'oras e2e %s\n' "$run" >"$work/$path"
version="$(sha256 "$work/$path" | cut -c1-16)" # the raw model: the first 16 hex of the content's sha256
created="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
push "$created" || { cat "$work/oras.log" >&2; fail "oras push $name:$version"; }
pushed=yes
digest="$(awk '$1 == "Digest:" { print $2 }' "$work/oras.log")"
case "$digest" in sha256:*) ;; *) cat "$work/oras.log" >&2; fail "oras push printed no digest" ;; esac
step "oras push $registry/$name:$version ($oras_how, as $E2E_USER): $digest"

# The same manifest again is the idempotent repeat; another one under the tag is refused.
push "$created" || { cat "$work/oras.log" >&2; fail "oras push of the same manifest again"; }
assert_eq "$digest" "$(awk '$1 == "Digest:" { print $2 }' "$work/oras.log")" "the digest of the repeat"
step "oras push of the same manifest again: the same digest"
if push "2001-02-03T04:05:06Z"; then fail "oras push of another manifest under $version succeeded: $(cat "$work/oras.log")"; fi
grep -qF "tag $version of $name is taken by $digest" "$work/oras.log" ||
	{ cat "$work/oras.log" >&2; fail "the refusal of another manifest under $version does not name $digest"; }
step "oras push of another manifest under $version: refused, tag invalid: the tag is taken by $digest"

# #60: the repo's policy. PATCH merges it into what the repo has; a file above maxFileSize is
# 413 through /v2 (the manifest PUT, which ingest checks) and through the raw frontend. The
# policy goes again at the end of the step, or in the cleanup when the step fails.
patch_policy() {
	assert_status 200 -K "$E2E_CURLRC" -X PATCH -H 'Content-Type: application/json' \
		-d "{\"policy\": $1}" "$base/api/v1/repos/raw/$repo"
	jq -c .policy <<<"$E2E_BODY"
}
assert_eq null "$(curl -sS -K "$E2E_CURLRC" "$base/api/v1/repos/raw/$repo" | jq -c .policy)" "the policy of raw/$repo before the step"
policy_set=yes
assert_eq '{"blobRedirect":"never"}' "$(patch_policy '{"blobRedirect": "never"}')" "PATCH blobRedirect"
size="$(wc -c <"$work/$path" | tr -d ' ')"
limit=$((size - 1))
assert_eq "{\"blobRedirect\":\"never\",\"maxFileSize\":$limit}" "$(patch_policy "{\"maxFileSize\": $limit}")" "PATCH maxFileSize, merged"
step "PATCH raw/$repo policy {blobRedirect}, then {maxFileSize: $limit}: merged into both"
if push "$created"; then fail "oras push of a file of $size bytes over maxFileSize $limit succeeded: $(cat "$work/oras.log")"; fi
grep -qiE 'size[ _]invalid' "$work/oras.log" && grep -qF "larger than the maxFileSize of raw/$repo, $limit bytes" "$work/oras.log" ||
	{ cat "$work/oras.log" >&2; fail "the refusal of a file over maxFileSize is not SIZE_INVALID naming the limit"; }
step "oras push of $size bytes with maxFileSize $limit: refused, 413 SIZE_INVALID naming the limit"
assert_status 413 -K "$E2E_CURLRC" -T "$work/$path" "$base/raw/$repo/$run/large.txt"
step "PUT /raw/$repo/$run/large.txt of $size bytes: 413"
assert_eq null "$(patch_policy '{"maxFileSize": null, "blobRedirect": null}')" "PATCH removing both keys"
policy_set=""
step "PATCH raw/$repo policy {maxFileSize: null, blobRedirect: null}: no policy left"

# O-02: pull, tags and the manifest through the facade, the file through the raw frontend.
oras pull -o out "$registry/$name:$version" >"$work/oras.log" 2>&1 || { cat "$work/oras.log" >&2; fail "oras pull"; }
cmp -s "$work/$path" "$work/out/$path" || fail "oras pull: out/$path is not the pushed file"
step "oras pull $registry/$name:$version: out/$path, the same bytes"
assert_eq "$version" "$(oras repo tags "$registry/$name")" "oras repo tags $registry/$name"
step "oras repo tags $registry/$name: $version"
oras manifest fetch "$registry/$name:$version" >"$work/manifest.json"
assert_eq "$digest" "sha256:$(sha256 "$work/manifest.json")" "the digest of the fetched manifest"
assert_eq "application/vnd.lajstrom.raw.v1 $path $path" \
	"$(jq -r '[.artifactType, .annotations["lajstrom.raw.path"], .layers[0].annotations["org.opencontainers.image.title"]] | join(" ")' "$work/manifest.json")" \
	"artifactType, lajstrom.raw.path and the layer title of the fetched manifest"
step "oras manifest fetch: $digest, artifactType application/vnd.lajstrom.raw.v1, the layer titled $path"
assert_status 200 -K "$E2E_CURLRC" -o "$work/get" "$base/raw/$repo/$path"
cmp -s "$work/$path" "$work/get" || fail "GET /raw/$repo/$path: not the pushed bytes"
step "GET /raw/$repo/$path: 200, the pushed bytes"

# #61, and the cleanup: oras manifest delete through the facade (202), then the version is gone
# from the facade and from the raw frontend.
oras manifest delete --force "$registry/$name:$version" >"$work/oras.log" 2>&1 ||
	{ cat "$work/oras.log" >&2; fail "oras manifest delete $name:$version"; }
pushed=""
assert_status 404 -K "$E2E_CURLRC" -o /dev/null "$base/v2/$name/manifests/$digest"
assert_eq "" "$(oras repo tags "$registry/$name")" "oras repo tags after the delete"
assert_status 404 -K "$E2E_CURLRC" -o /dev/null "$base/raw/$repo/$path"
step "oras manifest delete $registry/$name:$version: the manifest 404, no tag, GET /raw/$repo/$path 404"
echo "PASS oras e2e against $base as $E2E_USER"

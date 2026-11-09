#!/usr/bin/env bash
# End-to-end test of cosign and oras discover through the OCI facade (#28): a raw file pushed
# with oras, signed with cosign and a key pair made for the run (no keyless, no transparency
# log), verified with cosign, its signature listed by oras discover and by the referrers API;
# cosign's legacy signature under the tag sha256-<hex>.sig verified too; another key refused, a
# reader's signature refused and its verify through, an anonymous referrers request 401. Against
# a backend with the referrers API (Zot) the signature is listed by it; against one without
# (distribution, LAJSTROM_E2E_REGISTRY=localhost:5001) the step says that the provider wrote the
# sha256-<hex> tag schema, which the script reads from the backend.
#
#   test/e2e/cosign.sh [<base url> [<raw repo>]]     default: http://localhost:8080 scratch
#
# The repo must exist (the example configuration creates raw/scratch), and the user of
# LAJSTROM_E2E_USER and LAJSTROM_E2E_PASSWORD must push to it and delete in it (run.sh makes an
# admin; lib.sh, e2e_credentials). The reader is made with lajstrom admin ($LAJSTROM_BIN or
# bin/lajstrom, on $LAJSTROM_CONFIG or the example configuration) and disabled on exit. cosign
# is ghcr.io/sigstore/cosign/cosign:v3.1.4 in docker with --network host, with HTTPS_PROXY set to
# a closed port: nothing reaches the public Sigstore (Rekor, Fulcio, TUF). oras is the oras on
# PATH, else ghcr.io/oras-project/oras:v1.3.6. The file is at a path of its own for the run,
# deleted at the end; its signatures stay in the backend, which keeps the referrers of a deleted
# manifest. It needs curl, jq and docker.
set -euo pipefail
. "$(dirname "$0")/lib.sh"

base="${1:-http://localhost:8080}"
base="${base%/}"
repo="${2:-scratch}"
for tool in curl jq docker; do
	command -v "$tool" >/dev/null || { echo "cosign.sh: $tool is required" >&2; exit 2; }
done
cosign_image=ghcr.io/sigstore/cosign/cosign:v3.1.4
oras_image=ghcr.io/oras-project/oras:v1.3.6
bundle=application/vnd.dev.sigstore.bundle.v0.3+json # what cosign v3 signs with
registry="${base#*://}"
plain=""     # --plain-http for oras on an http:// base URL
insecure=""  # --allow-http-registry for cosign
case "$base" in http://*) plain=--plain-http insecure=--allow-http-registry ;; esac
export LAJSTROM_CONFIG="${LAJSTROM_CONFIG:-deploy/compose/lajstrom.example.yaml}"
if [ -n "${LAJSTROM_BIN:-}" ]; then lajstrom=("$LAJSTROM_BIN"); else lajstrom=(bin/lajstrom); fi

work="$(mktemp -d)"
e2e_credentials "$work"
run="e2e-cosign-$(date -u +%Y%m%d%H%M%S)-$RANDOM"
path="$run/hello.txt"
name="raw/$repo/$path"
pushed=""
reader="cosign-reader-$run"
made=""
cleanup() {
	[ -z "$pushed" ] || curl -sS -o /dev/null -K "$E2E_CURLRC" -X DELETE "$base/raw/$repo/$path" || true
	[ -z "$made" ] || "${lajstrom[@]}" admin user disable "$reader" >/dev/null 2>&1 || true
	rm -rf "$work"
}
trap cleanup EXIT

# docker_config DIR USER PASSWORD: a docker configuration in $work/DIR with the credentials of
# USER for the facade, which cosign reads (DOCKER_CONFIG).
docker_config() {
	mkdir -p "$work/$1"
	(umask 077 && printf '%s:%s' "$2" "$3" | base64 | tr -d '\n' |
		jq -Rn --arg r "$registry" '{auths: {($r): {auth: input}}}' >"$work/$1/config.json")
}

# cosign AS ARGS...: cosign in the run's directory with the docker configuration AS, no
# password on the key, and no way out to the public Sigstore.
cosign() {
	local as="$1"
	shift
	docker run --rm -i --network host -u "$(id -u):$(id -g)" -v "$work:/w" -w /w -e HOME=/w \
		-e DOCKER_CONFIG="/w/$as" -e COSIGN_PASSWORD= -e HTTPS_PROXY=http://127.0.0.1:9 -e NO_PROXY="${registry%:*}" \
		"$cosign_image" "$@"
}

if command -v oras >/dev/null; then
	oras() { (cd "$work" && command oras "$@" --registry-config "$work/oras-auth.json" ${plain:+"$plain"}); }
else
	oras() { docker run --rm -i --network host -u "$(id -u):$(id -g)" -v "$work:/w" -w /w "$oras_image" "$@" --registry-config /w/oras-auth.json ${plain:+"$plain"}; }
fi

# sha256 prints the hex sha256 of a file.
sha256() {
	if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

# The subject: a raw file pushed with oras, as oras.sh does.
printf '%s\n' "$E2E_PASSWORD" | oras login -u "$E2E_USER" --password-stdin "$registry" >"$work/oras.log" 2>&1 ||
	{ cat "$work/oras.log" >&2; fail "oras login"; }
mkdir -p "$work/$run"
printf 'cosign e2e %s\n' "$run" >"$work/$path"
version="$(sha256 "$work/$path" | cut -c1-16)"
(cd "$work" && oras push --artifact-type application/vnd.lajstrom.raw.v1 \
	--annotation lajstrom.schema=1 --annotation lajstrom.format=raw --annotation "lajstrom.repo=$repo" \
	--annotation "lajstrom.name=$path" --annotation "lajstrom.version=$version" --annotation "lajstrom.raw.path=$path" \
	"$registry/$name:$version" "$path:application/vnd.lajstrom.raw.file.v1") >"$work/oras.log" 2>&1 ||
	{ cat "$work/oras.log" >&2; fail "oras push $name:$version"; }
pushed=yes
digest="$(awk '$1 == "Digest:" { print $2 }' "$work/oras.log")"
subject="$registry/$name@$digest"
step "oras push $registry/$name:$version: $digest"

# A key pair of the run, and a second one that signed nothing.
docker_config writer "$E2E_USER" "$E2E_PASSWORD"
cosign writer generate-key-pair >/dev/null 2>&1 || fail "cosign generate-key-pair"
cosign writer generate-key-pair --output-key-prefix other >/dev/null 2>&1 || fail "cosign generate-key-pair, the other pair"
step "cosign generate-key-pair ($cosign_image): cosign.key, cosign.pub, and another pair"

# cosign v3 signs with a sigstore bundle, a referrer of the subject: OCI-Subject from the facade,
# so cosign writes no tag schema itself.
cosign writer sign --key cosign.key --tlog-upload=false --use-signing-config=false -y ${insecure:+"$insecure"} "$subject" \
	>"$work/sign.log" 2>&1 || { cat "$work/sign.log" >&2; fail "cosign sign $subject"; }
step "cosign sign --key cosign.key $subject"
cosign writer verify --key cosign.pub --insecure-ignore-tlog=true ${insecure:+"$insecure"} "$subject" >"$work/verify.json" 2>"$work/verify.log" ||
	{ cat "$work/verify.log" >&2; fail "cosign verify $subject"; }
assert_eq "$digest" "$(jq -r '.[0].critical.image["docker-manifest-digest"]' "$work/verify.json")" "the digest cosign verify signed"
step "cosign verify --key cosign.pub: the signature of $digest"

oras discover --format json "$subject" >"$work/discover.json" 2>"$work/oras.log" || { cat "$work/oras.log" >&2; fail "oras discover"; }
assert_eq "$bundle" "$(jq -r '[.referrers[].artifactType] | join(" ")' "$work/discover.json")" "the referrers oras discover lists"
signature="$(jq -r '.referrers[0].digest' "$work/discover.json")"
step "oras discover $subject: $signature, $bundle"
assert_status 200 -K "$E2E_CURLRC" -D "$work/headers" --globoff "$base/v2/$name/referrers/$digest?artifactType=$(jq -rn --arg t "$bundle" '$t | @uri')"
assert_eq "$signature" "$(jq -r '[.manifests[].digest] | join(" ")' <<<"$E2E_BODY")" "the referrers API with artifactType"
grep -qi '^oci-filters-applied: artifactType' "$work/headers" || fail "the referrers API: no OCI-Filters-Applied: artifactType"
assert_status 401 "$base/v2/$name/referrers/$digest"
step "GET /v2/$name/referrers/$digest?artifactType=…: $signature, OCI-Filters-Applied; anonymous: 401"

# What the backend did: its referrers API, or the tag schema the provider wrote.
backend="${LAJSTROM_E2E_REGISTRY:-}"
if [ -n "$backend" ]; then
	code="$(curl -sS -o /dev/null -w '%{http_code}' "http://$backend/v2/$name/referrers/$digest")"
	if [ "$code" = 200 ]; then
		step "the backend $backend: its referrers API"
	else
		curl -fsS "http://$backend/v2/$name/tags/list" | jq -e --arg t "sha256-${digest#sha256:}" '.tags | index($t)' >/dev/null ||
			fail "the backend $backend has no referrers API ($code) and no tag sha256-${digest#sha256:}"
		step "the backend $backend: no referrers API ($code); the provider wrote the tag sha256-${digest#sha256:}"
	fi
fi

# cosign's legacy signature: a manifest under sha256-<hex>.sig, without a subject.
cosign writer sign --key cosign.key --tlog-upload=false --use-signing-config=false --new-bundle-format=false -y \
	${insecure:+"$insecure"} "$subject" >"$work/sign.log" 2>&1 || { cat "$work/sign.log" >&2; fail "cosign sign, the legacy signature"; }
assert_status 200 -K "$E2E_CURLRC" -o /dev/null -H 'Accept: application/vnd.oci.image.manifest.v1+json' \
	"$base/v2/$name/manifests/sha256-${digest#sha256:}.sig"
# --new-bundle-format=false reads the legacy signature only: "no signatures found" without it.
cosign writer verify --key cosign.pub --insecure-ignore-tlog=true --new-bundle-format=false ${insecure:+"$insecure"} "$subject" \
	>/dev/null 2>"$work/verify.log" || { cat "$work/verify.log" >&2; fail "cosign verify of the legacy signature"; }
hex="${digest#sha256:}"
step "cosign sign and verify --new-bundle-format=false: the tag sha256-${hex:0:12}….sig through the facade"

if cosign writer verify --key other.pub --insecure-ignore-tlog=true ${insecure:+"$insecure"} "$subject" >/dev/null 2>"$work/verify.log"; then
	fail "cosign verify with a key that signed nothing succeeded"
fi
step "cosign verify with another key: refused"

# A reader verifies, and cannot sign.
pw="$(head -c 24 /dev/urandom | base64 | tr -d '+/=\n')"
printf '%s' "$pw" | "${lajstrom[@]}" admin user create --name "$reader" --password-stdin >/dev/null
made=yes
"${lajstrom[@]}" admin binding create --principal "user:$reader" --role reader --mask "raw/$repo" >/dev/null
docker_config reader "$reader" "$pw"
cosign reader verify --key cosign.pub --insecure-ignore-tlog=true ${insecure:+"$insecure"} "$subject" >/dev/null 2>"$work/verify.log" ||
	{ cat "$work/verify.log" >&2; fail "cosign verify as a reader"; }
if cosign reader sign --key cosign.key --tlog-upload=false --use-signing-config=false -y ${insecure:+"$insecure"} "$subject" >"$work/sign.log" 2>&1; then
	fail "cosign sign as a reader succeeded"
fi
grep -q 'DENIED' "$work/sign.log" || { cat "$work/sign.log" >&2; fail "cosign sign as a reader: not refused with DENIED"; }
step "a reader of raw/$repo: cosign verify passes, cosign sign is DENIED"

assert_status 204 -K "$E2E_CURLRC" -X DELETE "$base/raw/$repo/$path"
pushed=""
step "DELETE /raw/$repo/$path: 204"
echo "PASS cosign e2e against $base/v2/raw/$repo/"

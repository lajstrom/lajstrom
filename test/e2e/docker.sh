#!/usr/bin/env bash
# End-to-end test of docker images through an oci repo (#71; docs/spec/http-routing.md §2): the
# passthrough of the images of docker, buildx and crane, with RBAC and audit, on an oci repo of
# the run's own:
#
#   D-01 docker login as the writer (the user of LAJSTROM_E2E_USER) and as a reader made for the
#        run, with the reader role on the repo;
#   D-02 two single-platform images, linux/amd64 and linux/arm64, each made from a file with
#        docker import --platform (no build, no base image), pushed with docker push as
#        app:1-amd64 and app:1-arm64, as native CI runners push them;
#   D-03 crane index append makes app:1 of them, an OCI image index, as the CI job after the
#        native builds does (docker buildx imagetools create does the same): crane manifest lists
#        both digests with their platforms, crane digest gives the digest crane pushed, crane ls
#        the three tags, and the backend holds the index under that digest with the same bytes;
#   D-04 docker pull --platform of each platform of app:1, and the file of that platform in the
#        image docker holds;
#   D-05 docker buildx build --platform linux/amd64,linux/arm64 --push with provenance: an index of
#        both platforms and their attestation manifests, pulled as in D-04; only where the builder
#        can push several platforms (the containerd image store, or a builder with another driver
#        than docker), else skipped and said;
#   D-06 cosign sign --key of app:1 by its digest, and cosign verify; the referrers API lists the
#        signature;
#   D-07 the reader pulls app:1, and its docker push and crane tag are refused; a pull without
#        credentials is refused;
#   D-08 the audit log has a push row for each manifest pushed by tag, by the writer, with its tag
#        and digest, and none by the reader.
#
# The manifests and the repo are deleted at the end, and so are the images of the run that docker
# holds.
#
#   test/e2e/docker.sh [<base url>]     default: http://localhost:8080
#
# The user of LAJSTROM_E2E_USER and LAJSTROM_E2E_PASSWORD must create and delete a repo on the
# registry local and read the audit log, which takes the admin role (run.sh makes an admin;
# lib.sh, e2e_credentials). The reader is made with lajstrom admin ($LAJSTROM_BIN or bin/lajstrom,
# on $LAJSTROM_CONFIG or the example configuration) and disabled on exit. docker is the caller's
# CLI and daemon, which must reach the base URL's host and port: on Linux and under OrbStack
# 127.0.0.1 is the host's, and a daemon takes plain HTTP from 127.0.0.0/8 without configuration.
# Every docker command runs with a configuration directory of the run's own (DOCKER_CONFIG), the
# daemon of the caller's context and the caller's CLI plugins, so that the caller's logins stay as
# they were and no credential reaches a keychain. crane is the crane on PATH, else
# gcr.io/go-containerregistry/crane:v0.22.1, and cosign ghcr.io/sigstore/cosign/cosign:v3.1.4 as
# in cosign.sh, both in docker with --network host. The backend is compared at
# LAJSTROM_E2E_REGISTRY below LAJSTROM_E2E_REGISTRY_PREFIX when it needs no credentials. The
# containers it makes are named after LAJSTROM_E2E_CONTAINER_PREFIX (default lajstrom-e2e-), for
# a machine shared with others. It needs curl, jq, tar and docker.
set -euo pipefail
. "$(dirname "$0")/lib.sh"

base="${1:-http://localhost:8080}"
base="${base%/}"
for tool in curl jq tar docker; do
	command -v "$tool" >/dev/null || { echo "docker.sh: $tool is required" >&2; exit 2; }
done
crane_image=gcr.io/go-containerregistry/crane:v0.22.1
cosign_image=ghcr.io/sigstore/cosign/cosign:v3.1.4
bundle=application/vnd.dev.sigstore.bundle.v0.3+json # what cosign v3 signs with
registry="${base#*://}"
insecure="" # --insecure for crane, --allow-http-registry for cosign, on an http:// base URL
case "$base" in http://*) insecure=yes ;; esac
cprefix="${LAJSTROM_E2E_CONTAINER_PREFIX:-lajstrom-e2e-}"
export LAJSTROM_CONFIG="${LAJSTROM_CONFIG:-deploy/compose/lajstrom.example.yaml}"
if [ -n "${LAJSTROM_BIN:-}" ]; then lajstrom=("$LAJSTROM_BIN"); else lajstrom=(bin/lajstrom); fi

work="$(mktemp -d)"
e2e_credentials "$work"
run="e2e-docker-$(date -u +%Y%m%d%H%M%S)-$RANDOM"
name="oci/$run/app" # the OCI name of the images
ref="$registry/$name"
reader="docker-reader-$run"
made=""
user_made=""
pushed=() # the digests of the manifests pushed, each index before its children
cleanup() {
	if [ -n "$made" ]; then
		# An index first, which takes its referrers (signature, attestations) along, then the
		# manifests it lists: Zot keeps a manifest that an index lists.
		for d in ${pushed[@]+"${pushed[@]}"}; do
			curl -sS -o /dev/null -K "$E2E_CURLRC" -X DELETE "$base/v2/$name/manifests/$d" || true
		done
		curl -sS -o /dev/null -K "$E2E_CURLRC" -X DELETE "$base/api/v1/repos/oci/$run?force=true" || true
	fi
	[ -z "$user_made" ] || "${lajstrom[@]}" admin user disable "$reader" >/dev/null 2>&1 || true
	local left
	left="$(docker ps -aq --filter "name=^/${cprefix}docker-$run-" 2>/dev/null || true)" # image_file's, when it failed
	[ -z "$left" ] || docker rm -f -v $left >/dev/null 2>&1 || true
	drop_images
	rm -rf "$work"
}
trap cleanup EXIT

# drop_images removes the images of the run that docker holds.
drop_images() {
	local images
	images="$(docker image ls --format '{{.Repository}}:{{.Tag}}' 2>/dev/null | grep -F "$ref:" || true)"
	[ -z "$images" ] || docker rmi -f $images >/dev/null 2>&1 || true
}

# The daemon of the caller's context, and the caller's CLI plugins (buildx), for the docker
# commands that run with a configuration directory of the run's own.
docker_host="${DOCKER_HOST:-$(docker context inspect -f '{{.Endpoints.docker.Host}}' 2>/dev/null || true)}"
plugins="${DOCKER_CONFIG:-$HOME/.docker}/cli-plugins"

# docker_config AS: the configuration directory $work/AS. Its auths names the facade already, so
# that docker login keeps the credential in config.json: a configuration without credentials
# would take the platform's helper (osxkeychain, secretservice) and write the run's password there.
docker_config() {
	mkdir -p "$work/$1"
	(umask 077 && jq -n --arg r "$registry" --arg p "$plugins" '{auths: {($r): {}}, cliPluginsExtraDirs: [$p]}' >"$work/$1/config.json")
}

# dk AS ARGS...: docker with the configuration directory of AS.
dk() {
	local as="$1"
	shift
	env ${docker_host:+DOCKER_HOST="$docker_host"} DOCKER_CONFIG="$work/$as" docker "$@"
}

# crane AS ARGS...: crane with the docker configuration of AS.
if command -v crane >/dev/null; then
	crane_how="$(command -v crane) $(command crane version)"
	crane() {
		local as="$1"
		shift
		DOCKER_CONFIG="$work/$as" command crane "$@" ${insecure:+--insecure}
	}
else
	crane_how="$crane_image"
	crane() {
		local as="$1"
		shift
		docker run --rm -i --name "${cprefix}crane-$run-$RANDOM" --network host -u "$(id -u):$(id -g)" -v "$work:/w" -w /w \
			-e DOCKER_CONFIG="/w/$as" "$crane_image" "$@" ${insecure:+--insecure}
	}
fi

# cosign AS ARGS...: cosign in the run's directory with the docker configuration of AS, no
# password on the key, and no way out to the public Sigstore.
cosign() {
	local as="$1"
	shift
	docker run --rm -i --name "${cprefix}cosign-$run-$RANDOM" --network host -u "$(id -u):$(id -g)" -v "$work:/w" -w /w -e HOME=/w \
		-e DOCKER_CONFIG="/w/$as" -e COSIGN_PASSWORD= -e HTTPS_PROXY=http://127.0.0.1:9 -e NO_PROXY="${registry%:*}" \
		"$cosign_image" "$@"
}

# sha256 prints the hex sha256 of standard input.
sha256() {
	if command -v sha256sum >/dev/null; then sha256sum | cut -d' ' -f1; else shasum -a 256 | cut -d' ' -f1; fi
}

# image_file REF PLATFORM: the file /hello.txt of the image REF for PLATFORM, as docker holds it,
# read from a container that is created and never started.
image_file() {
	local c
	c="$(dk writer create --platform "$2" --name "${cprefix}docker-$run-${2//\//-}" "$1" /hello 2>"$work/docker.log")" ||
		{ cat "$work/docker.log" >&2; fail "docker create --platform $2 $1"; }
	dk writer cp "$c:/hello.txt" - 2>"$work/docker.log" | tar -xOf - hello.txt || { cat "$work/docker.log" >&2; fail "docker cp $c:/hello.txt"; }
	dk writer rm -f -v "$c" >/dev/null
}

# pull_platforms TAG: docker pull --platform of linux/amd64 and linux/arm64 of TAG, each checked
# by its file, "hello <platform> <run>"; the images of the run are removed before, so that each
# pull asks the facade.
pull_platforms() {
	local p
	drop_images
	for p in linux/amd64 linux/arm64; do
		dk writer pull --platform "$p" "$ref:$1" >"$work/docker.log" 2>&1 || { cat "$work/docker.log" >&2; fail "docker pull --platform $p $ref:$1"; }
		assert_eq "hello $p $run" "$(image_file "$ref:$1" "$p")" "the file of $p in $ref:$1"
	done
}

# pushed_digest LOG: the digest docker push printed, "<tag>: digest: sha256:… size: N".
pushed_digest() { awk '$2 == "digest:" { print $3 }' "$1" | tail -1; }

# D-01: the repo, the reader, and both logins.
assert_status 201 -K "$E2E_CURLRC" -H 'Content-Type: application/json' \
	-d "{\"format\":\"oci\",\"name\":\"$run\",\"registry\":\"local\"}" "$base/api/v1/repos"
made=yes
pw="$(head -c 24 /dev/urandom | base64 | tr -d '+/=\n')"
printf '%s' "$pw" | "${lajstrom[@]}" admin user create --name "$reader" --password-stdin >/dev/null
user_made=yes
"${lajstrom[@]}" admin binding create --principal "user:$reader" --role reader --mask "oci/$run" >/dev/null
for as in writer reader none; do docker_config "$as"; done
printf '%s' "$E2E_PASSWORD" | dk writer login -u "$E2E_USER" --password-stdin "$registry" >"$work/docker.log" 2>&1 ||
	{ cat "$work/docker.log" >&2; fail "docker login as $E2E_USER"; }
printf '%s' "$pw" | dk reader login -u "$reader" --password-stdin "$registry" >"$work/docker.log" 2>&1 ||
	{ cat "$work/docker.log" >&2; fail "docker login as $reader"; }
step "D-01: the repo oci/$run; docker login $registry as $E2E_USER and as $reader, a reader of oci/$run ($(docker version -f '{{.Client.Version}}/{{.Server.Version}}'))"

# D-02: an image per platform, pushed with docker.
for p in linux/amd64 linux/arm64; do
	arch="${p#*/}"
	mkdir -p "$work/$arch"
	printf 'hello %s %s\n' "$p" "$run" >"$work/$arch/hello.txt"
	tar -C "$work/$arch" -cf "$work/$arch.tar" hello.txt
	dk writer import --platform "$p" "$work/$arch.tar" "$ref:1-$arch" >/dev/null 2>"$work/docker.log" ||
		{ cat "$work/docker.log" >&2; fail "docker import --platform $p"; }
	dk writer push "$ref:1-$arch" >"$work/push.log" 2>&1 || { cat "$work/push.log" >&2; fail "docker push $ref:1-$arch"; }
	d="$(pushed_digest "$work/push.log")"
	case "$d" in sha256:*) ;; *) cat "$work/push.log" >&2; fail "docker push $ref:1-$arch printed no digest" ;; esac
	pushed+=("$d")
	case "$arch" in amd64) amd64_digest="$d" ;; arm64) arm64_digest="$d" ;; esac
done
step "D-02: docker push $ref:1-amd64 $amd64_digest and :1-arm64 $arm64_digest"

# D-03: the index, made on the client with crane, and what the facade and the backend hold.
crane writer index append -m "$ref:1-amd64" -m "$ref:1-arm64" -t "$ref:1" >"$work/crane.log" 2>&1 ||
	{ cat "$work/crane.log" >&2; fail "crane index append"; }
index_digest="$(crane writer digest "$ref:1")"
pushed=("$index_digest" "${pushed[@]}")
grep -qF "$index_digest" "$work/crane.log" || { cat "$work/crane.log" >&2; fail "crane index append: not the digest $index_digest"; }
crane writer manifest "$ref:1" >"$work/index.json"
assert_eq application/vnd.oci.image.index.v1+json "$(jq -r .mediaType "$work/index.json")" "the media type of $ref:1"
assert_eq "$amd64_digest linux/amd64 $arm64_digest linux/arm64" \
	"$(jq -r '[.manifests[] | "\(.digest) \(.platform.os)/\(.platform.architecture)"] | sort_by(split(" ")[1]) | join(" ")' "$work/index.json")" \
	"the manifests of $ref:1"
assert_eq "${index_digest#sha256:}" "$(sha256 <"$work/index.json")" "the sha256 of the index crane manifest got"
assert_eq "1 1-amd64 1-arm64" "$(crane writer ls "$ref" | sort | tr '\n' ' ' | sed 's/ $//')" "crane ls $ref"
backend="${LAJSTROM_E2E_REGISTRY:-}"
if [ -n "$backend" ] && [ -z "${LAJSTROM_E2E_REGISTRY_USER:-}" ]; then
	curl -fsS -H 'Accept: application/vnd.oci.image.index.v1+json' \
		"http://$backend/v2/${LAJSTROM_E2E_REGISTRY_PREFIX:-}$name/manifests/$index_digest" >"$work/backend.json" ||
		fail "the backend $backend: GET the index $index_digest"
	cmp -s "$work/index.json" "$work/backend.json" || fail "the backend $backend holds other bytes under $index_digest"
	held="the backend $backend holds the same bytes"
else
	held="the backend not compared (LAJSTROM_E2E_REGISTRY unset, or it needs credentials)"
fi
step "D-03: crane index append ($crane_how): $ref:1 $index_digest, an OCI index of both; crane ls 1 1-amd64 1-arm64; $held"

# D-04: docker pulls each platform of the index.
pull_platforms 1
step "D-04: docker pull --platform linux/amd64 and linux/arm64 $ref:1: the file of each platform"

# D-05: buildx pushes both platforms and their provenance in one build, where the builder can.
driver="$(dk writer buildx inspect 2>/dev/null | awk '$1 == "Driver:" { print $2; exit }' || true)"
store="$(docker info -f '{{range .DriverStatus}}{{index . 1}} {{end}}' 2>/dev/null || true)"
if [ -z "$driver" ]; then
	step "D-05: skipped: no docker buildx"
elif [ "$driver" = docker ] && [[ "$store" != *io.containerd.snapshotter* ]]; then
	step "D-05: skipped: the builder's docker driver cannot push several platforms without the containerd image store"
else
	mkdir -p "$work/ctx"
	printf 'FROM scratch\nARG TARGETOS TARGETARCH\nCOPY ${TARGETOS}-${TARGETARCH}.txt /hello.txt\n' >"$work/ctx/Dockerfile"
	for p in linux/amd64 linux/arm64; do printf 'hello %s %s\n' "$p" "$run" >"$work/ctx/${p%/*}-${p#*/}.txt"; done
	dk writer buildx build --platform linux/amd64,linux/arm64 --provenance=mode=min --sbom=false -t "$ref:2" --push "$work/ctx" \
		>"$work/buildx.log" 2>&1 || { tail -30 "$work/buildx.log" >&2; fail "docker buildx build --push $ref:2"; }
	bx_digest="$(crane writer digest "$ref:2")"
	crane writer manifest "$ref:2" >"$work/bx.json"
	pushed=("$bx_digest" "${pushed[@]}")
	for d in $(jq -r '.manifests[] | select(.platform.os != "unknown") | .digest' "$work/bx.json"); do pushed+=("$d"); done
	images="$(jq -r '[.manifests[] | select(.platform.os != "unknown") | "\(.platform.os)/\(.platform.architecture)"] | sort | join(" ")' "$work/bx.json")"
	assert_eq "linux/amd64 linux/arm64" "$images" "the platforms of $ref:2"
	# buildx's attestation manifests: platform unknown/unknown, each naming an image of the index.
	assert_eq 2 "$(jq '. as $i | [.manifests[] | select(.annotations["vnd.docker.reference.type"] == "attestation-manifest")
		| select(.annotations["vnd.docker.reference.digest"] as $d | [$i.manifests[].digest] | index($d))] | length' "$work/bx.json")" \
		"the attestation manifests of $ref:2, each of an image of the index"
	pull_platforms 2
	step "D-05: docker buildx build --platform linux/amd64,linux/arm64 --push ($driver driver): $ref:2 $bx_digest, both platforms and their attestations; docker pull of each"
fi

# D-06: cosign signs the index by its digest, and verifies it.
cosign writer generate-key-pair >/dev/null 2>&1 || fail "cosign generate-key-pair"
subject="$ref@$index_digest"
cosign writer sign --key cosign.key --tlog-upload=false --use-signing-config=false -y ${insecure:+--allow-http-registry} "$subject" \
	>"$work/sign.log" 2>&1 || { cat "$work/sign.log" >&2; fail "cosign sign $subject"; }
cosign writer verify --key cosign.pub --insecure-ignore-tlog=true ${insecure:+--allow-http-registry} "$subject" >"$work/verify.json" 2>"$work/verify.log" ||
	{ cat "$work/verify.log" >&2; fail "cosign verify $subject"; }
assert_eq "$index_digest" "$(jq -r '.[0].critical.image["docker-manifest-digest"]' "$work/verify.json")" "the digest cosign verify signed"
assert_status 200 -K "$E2E_CURLRC" "$base/v2/$name/referrers/$index_digest"
assert_eq "$bundle" "$(jq -r '[.manifests[].artifactType] | join(" ")' <<<"$E2E_BODY")" "the referrers of $index_digest"
step "D-06: cosign sign and verify --key of $subject ($cosign_image); the referrers API lists the signature"

# D-07: the reader pulls and pushes nothing; nobody pulls without credentials.
drop_images
dk reader pull --platform linux/arm64 "$ref:1" >"$work/docker.log" 2>&1 || { cat "$work/docker.log" >&2; fail "docker pull as $reader"; }
assert_eq "hello linux/arm64 $run" "$(image_file "$ref:1" linux/arm64)" "the file of linux/arm64 in $ref:1, pulled by $reader"
dk reader import --platform linux/amd64 "$work/amd64.tar" "$ref:reader" >/dev/null
if dk reader push "$ref:reader" >"$work/docker.log" 2>&1; then
	fail "docker push as $reader succeeded"
fi
refusal="$(grep -Ei 'not grant push|denied|unauthorized|insufficient_scope' "$work/docker.log" | tail -1 || true)"
[ -n "$refusal" ] || { cat "$work/docker.log" >&2; fail "docker push as $reader: no refusal in what docker said"; }
assert_status 404 -K "$E2E_CURLRC" -o /dev/null -I "$base/v2/$name/manifests/reader"
# Not buildx's 2 (D-05), nor the sha256-<hex> tags of the referrers tag schema for the signature of
# D-06 and buildx's attestations: the facade writes them where the registry has no referrers API
# (#28; Nexus's docker format), and Nexus's oci format writes them itself.
assert_eq "1 1-amd64 1-arm64" "$(crane reader ls "$ref" | grep -v -e '^2$' -e '^sha256-[0-9a-f]\{64\}$' | sort | tr '\n' ' ' | sed 's/ $//')" \
	"crane ls $ref as $reader"
if crane reader tag "$ref:1" reader >"$work/crane.log" 2>&1; then
	fail "crane tag as $reader succeeded"
fi
assert_status 404 -K "$E2E_CURLRC" -o /dev/null -I "$base/v2/$name/manifests/reader"
drop_images
if dk none pull --platform linux/amd64 "$ref:1" >"$work/docker.log" 2>&1; then
	fail "docker pull without credentials succeeded"
fi
step "D-07: $reader pulls $ref:1 and crane ls; its docker push ($refusal) and crane tag refused; docker pull without credentials refused"

# D-08: the push rows: one per manifest pushed by tag, the writer's, none the reader's.
assert_status 200 -K "$E2E_CURLRC" "$base/api/v1/audit?action=push&repo=oci/$run&limit=100"
assert_eq "1 $index_digest|1-amd64 $amd64_digest|1-arm64 $arm64_digest" \
	"$(jq -r --arg s "$name" '[.events[] | select(.subject == $s and .details.tag != null and .details.tag != "2") | "\(.details.tag) \(.digest)"] | sort | join("|")' <<<"$E2E_BODY")" \
	"the push rows of the tags of $name"
assert_eq "$E2E_USER ok" "$(jq -r --arg s "$name" '[.events[] | select(.subject == $s) | "\(.principalName) \(.state)"] | unique | join(",")' <<<"$E2E_BODY")" \
	"who pushed to $name, and how it ended"
assert_eq 0 "$(jq --arg r "$reader" '[.events[] | select(.principalName == $r)] | length' <<<"$E2E_BODY")" "the push rows of $reader"
step "D-08: GET /api/v1/audit?action=push&repo=oci/$run: 1, 1-amd64 and 1-arm64 by $E2E_USER, ok; none by $reader"

echo "PASS docker e2e against $base/v2/oci/$run/"

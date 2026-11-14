#!/usr/bin/env bash
# End-to-end test of an oci proxy repo (#873; ADR 0018, docs/spec/proxy-and-group.md §11,
# docs/quickstart/oci-proxy.md) with crane and containerd against a running Lajstrom. An oci
# proxy repo of the run's own, readable anonymously, pulls through from the upstream of
# $OCI_PROXY_E2E_UPSTREAM in the server's configuration (run.sh names e2e-oci), a registry at
# $LAJSTROM_E2E_OCI_UPSTREAM, which crane seeds with two images of the run, each an index of
# linux/amd64 and linux/arm64 of one random layer:
#
#   1. crane pull of img:1.0 for linux/arm64: the platform's two blobs (its layer and config)
#      are fetched, and its manifest is in the backend under its keep-tag only, not 1.0, since
#      the index is not complete; the proxy's tags/list is empty;
#   2. crane pull for linux/amd64: two more blobs, and 1.0 in the backend, naming the index,
#      with the keep-tags of both manifests and of the index; the proxy's tags/list is 1.0;
#   3. both pulls again: the proxy serves their blobs again, and fetches none;
#   4. containerd (ctr images pull, in a container) with a hosts.toml mirror of the upstream's
#      host at <base>/v2/oci/<repo> and override_path = true pulls ctr:1.0 for its own
#      platform: three blobs are fetched through the proxy, the configs of both platforms (ctr
#      fetches the metadata of every platform unless --skip-metadata) and the layer of its own,
#      and its platform's manifest is in the backend under its keep-tag, the other's not; the
#      same pull by a new containerd, with an empty content store: the proxy serves the three
#      blobs again, and fetches none; then the same pull through a second proxy repo of the
#      upstream, without anonymous read, by a containerd with --user and a personal access token
#      of the run's user with read (#908): containerd posts the OAuth2 form to the realm, and in
#      the access log of the pull each token is one POST /v2/token, 200 for the user, and there
#      is no GET /v2/token (ctr asks for two tokens, one to resolve and one to fetch);
#   5. a group of oci repos (#848, ADR 0019, §12) of a hosted repo of the run, which serves
#      acme/*, and of the proxy, which serves the rest: crane pushes an image to the hosted repo
#      at its own URL and pulls it through the group; crane pulls img:1.0 through the group, the
#      digest of step 2 with no blob fetched, and grp:1.0 for linux/arm64, which the proxy fetches
#      through the group (two blobs, proxy.fetch rows with details.via the group); tags/list of
#      the hosted path through the group is 1.4; an upload through the group is 405 with
#      Allow: GET, HEAD.
#
#   test/e2e/ociproxy.sh [<base url>]     default: http://localhost:8080
#
# The server's configuration must name the upstream, of format oci (run.sh does), and the
# registry local; the user of LAJSTROM_E2E_USER and LAJSTROM_E2E_PASSWORD creates and deletes
# the repo (run.sh makes an admin). A blob fetched is a line "proxy: a blob fetched from the
# upstream" of the repo and the path in $LAJSTROM_SERVER_LOG, the server's log, which run.sh
# sets and the script needs; a blob served, an access line of a GET of it with status 200. The backend is $LAJSTROM_E2E_REGISTRY (localhost:5000, the Zot of
# make dev-up), with $LAJSTROM_E2E_REGISTRY_PREFIX, read without credentials; the upstream too.
#
# crane is the crane on PATH, else gcr.io/go-containerregistry/crane:v0.22.1 pinned by digest in
# docker with --network host. containerd is the static release v2.4.2 of containerd/containerd
# on GitHub, downloaded into the run's directory and checked against its sha256, run in
# debian:12 with --privileged (its overlayfs snapshotter mounts), a tmpfs as its root and
# --network host, so that it reaches the base URL and the upstream as the host does. At the end
# the repos are deleted, and what crane pushed to the upstream, to the hosted repo, and the proxy to
# the backend is deleted from them by digest, where the registry lets it. crane pushes to the
# hosted repo as the run's user, from a docker configuration in the run's directory
# (DOCKER_CONFIG), which it then removes; every other crane call is anonymous. It needs curl, jq and docker, prints
# one line per step and exits non-zero at the first failure (lib.sh).
set -euo pipefail
. "$(dirname "$0")/lib.sh"

base="${1:-http://localhost:8080}"
base="${base%/}"
for tool in curl jq docker; do
	command -v "$tool" >/dev/null || { echo "ociproxy.sh: $tool is required" >&2; exit 2; }
done
log="${LAJSTROM_SERVER_LOG:-}"
[ -n "$log" ] && [ -f "$log" ] ||
	{ echo "ociproxy.sh: set LAJSTROM_SERVER_LOG to the server's log file, where the blobs fetched are counted (run.sh sets it)" >&2; exit 2; }
registry="${LAJSTROM_E2E_REGISTRY:-localhost:5000}"
prefix="${LAJSTROM_E2E_REGISTRY_PREFIX:-}"
upstream="${LAJSTROM_E2E_OCI_UPSTREAM:-$registry}"
upname="${OCI_PROXY_E2E_UPSTREAM:-e2e-oci}"
crane_image=gcr.io/go-containerregistry/crane:v0.22.1@sha256:1f968817b95790bed063f71175aa6b8ff879fa17064020415f3e18bb6e6a36e1
containerd_image=debian:12
containerd_version=2.4.2
size=262144 # the bytes of each layer

work="$(mktemp -d)"
e2e_credentials "$work"
run="$(date -u +%Y%m%d%H%M%S)-$RANDOM"
proxy="up-$run" # the repo oci/<proxy>
# The run's repositories, img and ctr, below these: on the upstream, through the proxy and in the
# backend.
up="$upstream/e2e-ociproxy-$run"
lj="${base#*://}/oci/$proxy/e2e-ociproxy-$run"
back="$registry/${prefix}oci/$proxy/e2e-ociproxy-$run"
ctr_name="lajstrom-e2e-containerd-$run"
private="pv-$run"                  # step 4: the repo oci/<private>, without anonymous read
hosted="int-$run" group="all-$run" # step 5: the repos oci/<hosted> and oci/<group>
host="${base#*://}"
proxy_made=""
private_made=""
group_made=""
hosted_made=""
pushed=() # what crane pushed to the upstream and the proxy to the backend, host/name@digest
cleanup() {
	docker rm -f "$ctr_name" >/dev/null 2>&1 || true
	[ -z "$group_made" ] || curl -sS -o /dev/null -K "$E2E_CURLRC" -X DELETE "$base/api/v1/repos/oci/$group" || true
	[ -z "$hosted_made" ] || curl -sS -o /dev/null -K "$E2E_CURLRC" -X DELETE "$base/api/v1/repos/oci/$hosted?force=true" || true
	[ -z "$proxy_made" ] || curl -sS -o /dev/null -K "$E2E_CURLRC" -X DELETE "$base/api/v1/repos/oci/$proxy?force=true" || true
	[ -z "$private_made" ] || curl -sS -o /dev/null -K "$E2E_CURLRC" -X DELETE "$base/api/v1/repos/oci/$private?force=true" || true
	local i ref name
	for ((i = ${#pushed[@]} - 1; i >= 0; i--)); do # an index before what it lists
		ref="${pushed[$i]}" name="${pushed[$i]#*/}"
		curl -sS -o /dev/null -X DELETE "http://${ref%%/*}/v2/${name%@*}/manifests/${ref##*@}" || true
	done
	rm -rf "$work"
}
trap cleanup EXIT

# crane ARGS...: crane in the run's directory, where its file arguments are.
# Its docker configuration is the run's, empty but in step 5: anonymous.
mkdir -p "$work/docker"
if command -v crane >/dev/null; then
	crane_how="$(command -v crane) $(command crane version)"
	crane() { (cd "$work" && DOCKER_CONFIG="$work/docker" command crane "$@"); }
else
	crane_how="${crane_image%@*}"
	# As the caller's user: what crane pull writes as root on Linux, the cleanup could not remove.
	crane() { docker run --rm --network host -u "$(id -u):$(id -g)" -v "$work:/w" -w /w -e DOCKER_CONFIG=/w/docker "$crane_image" "$@"; }
fi

# sha256 prints the hex sha256 of a file.
sha256() {
	if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

# fetched PATH prints how many blobs the repo fetched from the upstream for PATH.
fetched() {
	grep -F 'msg="proxy: a blob fetched from the upstream"' "$log" | grep -F " repo=oci/$proxy " | grep -cF " path=$1 " || true
}

# served PATH prints how many GETs of a blob of PATH the proxy answered 200, from the backend or
# from the upstream's bytes as they went into it.
served() {
	grep -F ' msg=request method=GET ' "$log" | grep -F " path=/v2/oci/$proxy/$1/blobs/" | grep -cF ' status=200 ' || true
}

# eventually CMD...: CMD succeeds within 10 s. A client's last byte comes with the commit of the
# blob, before the pushes of what it completed (§11.5, step 3).
eventually() {
	local _
	for _ in $(seq 1 100); do
		"$@" && return 0
		sleep 0.1
	done
	return 1
}

# backend_tags NAME prints the tags of NAME in the backend, sorted, on one line.
backend_tags() { crane ls --insecure "$back/$1" 2>/dev/null | sort | tr '\n' ' ' | sed 's/ $//'; }
# tags_are NAME TAGS: the backend's tags of NAME are TAGS, sorted, on one line.
tags_are() { [ "$(backend_tags "$1")" = "$2" ]; }
# keep DIGEST prints the keep-tag of DIGEST (§11.6).
keep() { echo "_keep.sha256.${1#sha256:}"; }

# seed NAME: an index NAME:1.0 on the upstream of linux/amd64 and linux/arm64, one random layer
# each, with OCI media types (Zot refuses Docker's); every push is recorded for the cleanup.
seed() {
	local arch ref
	for arch in amd64 arm64; do
		mkdir -p "$work/$1-$arch"
		head -c "$size" /dev/urandom >"$work/$1-$arch/data"
		tar -C "$work/$1-$arch" -cf "$work/$1-$arch.tar" data
		ref="$(crane append --insecure --oci-empty-base -f "$1-$arch.tar" -t "$up/$1:$arch-base" 2>"$work/crane.log")" ||
			{ cat "$work/crane.log" >&2; fail "crane append to $up/$1:$arch-base"; }
		pushed+=("$ref")
		ref="$(crane mutate --insecure --set-platform "linux/$arch" -t "$up/$1:$arch" "$up/$1:$arch-base" 2>"$work/crane.log")" ||
			{ cat "$work/crane.log" >&2; fail "crane mutate --set-platform linux/$arch of $up/$1"; }
		pushed+=("$ref")
	done
	ref="$(crane index append --insecure -t "$up/$1:1.0" -m "$up/$1:amd64" -m "$up/$1:arm64" 2>"$work/crane.log")" ||
		{ cat "$work/crane.log" >&2; fail "crane index append $up/$1:1.0"; }
	pushed+=("$ref")
}

# pull NAME ARCH OUT: crane pull of NAME:1.0 for linux/ARCH through the proxy, into OUT.
pull() {
	crane pull --insecure --platform "linux/$2" "$lj/$1:1.0" "$3" 2>"$work/crane.log" ||
		{ cat "$work/crane.log" >&2; fail "crane pull --platform linux/$2 $lj/$1:1.0"; }
}

assert_status 201 -K "$E2E_CURLRC" -H 'Content-Type: application/json' \
	-d "{\"format\":\"oci\",\"name\":\"$proxy\",\"registry\":\"local\",\"kind\":\"proxy\",\"upstream\":{\"name\":\"$upname\"},\"anonymousRead\":true}" \
	"$base/api/v1/repos"
proxy_made=yes
step "POST /api/v1/repos oci/$proxy, a proxy of the upstream $upname at $upstream, anonymousRead: 201"

seed img
seed ctr
index="$(crane digest --insecure "$up/img:1.0")"
amd64="$(crane digest --insecure --platform linux/amd64 "$up/img:1.0")"
arm64="$(crane digest --insecure --platform linux/arm64 "$up/img:1.0")"
step "crane ($crane_how): $up/img:1.0 and ctr:1.0, indexes of linux/amd64 and linux/arm64; img:1.0 is $index"

# 1-3: crane, a platform at a time.
pull img arm64 arm64.tar
pushed+=("$back/img@$arm64")
assert_eq 2 "$(fetched "e2e-ociproxy-$run/img")" "the blobs fetched by the pull of linux/arm64"
eventually tags_are img "$(keep "$arm64")" || fail "the backend's tags of img after linux/arm64: '$(backend_tags img)', want its keep-tag alone, $(keep "$arm64")"
assert_eq "" "$(crane ls --insecure "$lj/img")" "the proxy's tags/list of img before the index is complete"
step "crane pull --platform linux/arm64 $lj/img:1.0: two blobs fetched, the backend has $(keep "$arm64") and no 1.0, tags/list empty"

pull img amd64 amd64.tar
pushed+=("$back/img@$amd64" "$back/img@$index")
assert_eq 4 "$(fetched "e2e-ociproxy-$run/img")" "the blobs fetched by the pulls of both platforms"
want="$(printf '%s\n' 1.0 "$(keep "$amd64")" "$(keep "$arm64")" "$(keep "$index")" | sort | tr '\n' ' ' | sed 's/ $//')"
eventually tags_are img "$want" || fail "the backend's tags of img after both platforms: '$(backend_tags img)', want '$want'"
assert_eq "$index" "$(crane digest --insecure "$back/img:1.0")" "the digest of 1.0 in the backend"
assert_eq 1.0 "$(crane ls --insecure "$lj/img")" "the proxy's tags/list of img once the index is complete"
step "crane pull --platform linux/amd64: two more blobs, 1.0 in the backend naming $index after the keep-tags, tags/list 1.0"

before="$(served "e2e-ociproxy-$run/img")"
pull img arm64 arm64-again.tar
pull img amd64 amd64-again.tar
assert_eq 4 "$(fetched "e2e-ociproxy-$run/img")" "the blobs fetched after the second pulls"
[ "$(served "e2e-ociproxy-$run/img")" -ge $((before + 4)) ] ||
	fail "the second pulls asked the proxy for $(($(served "e2e-ociproxy-$run/img") - before)) blobs, want 4 at least"
cmp -s "$work/arm64.tar" "$work/arm64-again.tar" && cmp -s "$work/amd64.tar" "$work/amd64-again.tar" ||
	fail "a second crane pull wrote other bytes than the first"
step "crane pull of both platforms again: the same images, their four blobs served again, none fetched"

# 4: containerd, through a mirror of the upstream's host.
arch="$(docker version -f '{{.Server.Arch}}')"
case "$arch" in
amd64) sum=da656542e754c699a0f438eab925e6ffdbd6346a4428e659f83dc058f04dcffe ;;
arm64) sum=37b84b9534533672528141b9d815f58621a2fe5deee2daa84317800255756689 ;;
*) fail "no containerd release is pinned for the docker server's architecture $arch" ;;
esac
tarball="containerd-static-$containerd_version-linux-$arch.tar.gz"
curl -fsSL --retry 3 -o "$work/$tarball" "https://github.com/containerd/containerd/releases/download/v$containerd_version/$tarball"
assert_eq "$sum" "$(sha256 "$work/$tarball")" "the sha256 of $tarball"
mkdir -p "$work/containerd" "$work/certs.d/$upstream"
tar -C "$work/containerd" -xzf "$work/$tarball"
cat >"$work/certs.d/$upstream/hosts.toml" <<EOF
server = "http://$upstream"

[host."$base/v2/oci/$proxy"]
  capabilities = ["pull", "resolve"]
  override_path = true
EOF
ctr() { docker exec "$ctr_name" /opt/containerd/bin/ctr "$@"; }
platform="$(crane digest --insecure --platform "linux/$arch" "$up/ctr:1.0")"
ref="$upstream/e2e-ociproxy-$run/ctr:1.0"
# ctr_pull [DIR [ARG...]]: a containerd of its own, its root a new tmpfs, pulls ref with the
# mirror of $work/DIR (certs.d when there is none) and the ARGs of ctr images pull. A second
# pull by the same containerd, in any namespace, would ask for no blob: it has them.
ctr_pull() {
	local certs="${1:-certs.d}"
	[ $# -eq 0 ] || shift
	docker rm -f "$ctr_name" >/dev/null 2>&1 || true
	docker run -d --rm --name "$ctr_name" --privileged --network host --tmpfs /var/lib/containerd \
		-v "$work/containerd:/opt/containerd:ro" -v "$work/$certs:/etc/containerd/certs.d:ro" \
		"$containerd_image" /opt/containerd/bin/containerd >/dev/null
	eventually ctr version >/dev/null 2>&1 ||
		{ docker logs "$ctr_name" 2>&1 | tail -n 30 >&2; fail "containerd did not answer ctr version within 10 s"; }
	ctr images pull --hosts-dir /etc/containerd/certs.d "$@" "$ref" >"$work/ctr.log" 2>&1 ||
		{ cat "$work/ctr.log" >&2; docker logs "$ctr_name" 2>&1 | tail -n 30 >&2; fail "ctr images pull $ref"; }
	assert_eq "$ref" "$(ctr images ls -q)" "ctr images ls -q"
}
ctr_pull
pushed+=("$back/ctr@$platform")
assert_eq 3 "$(fetched "e2e-ociproxy-$run/ctr")" "the blobs fetched by containerd's pull of linux/$arch: both configs and its layer"
eventually tags_are ctr "$(keep "$platform")" || fail "the backend's tags of ctr after containerd's pull: '$(backend_tags ctr)', want $(keep "$platform")"
step "ctr images pull $ref (containerd $containerd_version, linux/$arch) through the mirror $base/v2/oci/$proxy with override_path: both configs and its layer fetched, $(keep "$platform") alone in the backend"
before="$(served "e2e-ociproxy-$run/ctr")"
ctr_pull
assert_eq 3 "$(fetched "e2e-ociproxy-$run/ctr")" "the blobs fetched after containerd's second pull"
[ "$(served "e2e-ociproxy-$run/ctr")" -ge $((before + 3)) ] ||
	fail "containerd's second pull asked the proxy for $(($(served "e2e-ociproxy-$run/ctr") - before)) blobs, want 3 at least"
step "ctr images pull by a new containerd: the three blobs served again, none fetched"

# The same image through oci/<private>, without anonymous read, by a containerd with --user
# (#908): its token comes from POST /v2/token, the OAuth2 form, and none from GET.
assert_status 201 -K "$E2E_CURLRC" -H 'Content-Type: application/json' \
	-d "{\"format\":\"oci\",\"name\":\"$private\",\"registry\":\"local\",\"kind\":\"proxy\",\"upstream\":{\"name\":\"$upname\"}}" \
	"$base/api/v1/repos"
private_made=yes
pat="$(curl -sS -f -K "$E2E_CURLRC" -H 'Content-Type: application/json' \
	-d "{\"name\":\"ctr-$run\",\"scopes\":[\"read\"],\"expiresIn\":\"1h\"}" "$base/api/v1/users/$E2E_USER/tokens" | jq -r .secret)"
case "$pat" in ljs_pat_*) ;; *) fail "POST /api/v1/users/$E2E_USER/tokens gave no personal access token" ;; esac
mkdir -p "$work/certs-private.d/$upstream"
cat >"$work/certs-private.d/$upstream/hosts.toml" <<EOF
server = "http://$upstream"

[host."$base/v2/oci/$private"]
  capabilities = ["pull", "resolve"]
  override_path = true
EOF
mark="$(wc -l <"$log")"
ctr_pull certs-private.d --user "$E2E_USER:$pat"
pushed+=("$registry/${prefix}oci/$private/e2e-ociproxy-$run/ctr@$platform")
# token_requests prints the access lines of /v2/token since the mark.
token_requests() { tail -n +"$((mark + 1))" "$log" | grep -F ' msg=request ' | grep -F ' path=/v2/token ' || true; }
posts="$(token_requests | grep -F ' method=POST ' | grep -F ' status=200 ' | grep -cF ' principal=user:' || true)"
[ "$posts" -ge 1 ] || fail "containerd's pull with --user got no token from POST /v2/token"
assert_eq 0 "$(token_requests | grep -cF ' method=GET ' || true)" "GET /v2/token during containerd's pull with --user"
assert_eq "$posts" "$(token_requests | wc -l | tr -d ' ')" "the requests to /v2/token during containerd's pull with --user, all POST 200 for the user"
step "ctr images pull --user $E2E_USER:<read PAT> $ref through $base/v2/oci/$private, no anonymous read: $posts tokens, each one POST /v2/token 200 for user:…; no GET /v2/token"

# 5: a group of the hosted repo and the proxy, one URL for both.
assert_status 201 -K "$E2E_CURLRC" -H 'Content-Type: application/json' \
	-d "{\"format\":\"oci\",\"name\":\"$hosted\",\"registry\":\"local\",\"anonymousRead\":true}" "$base/api/v1/repos"
hosted_made=yes
assert_status 201 -K "$E2E_CURLRC" -H 'Content-Type: application/json' \
	-d "{\"format\":\"oci\",\"name\":\"$group\",\"kind\":\"group\",\"anonymousRead\":true,\"group\":{\"members\":[\"$hosted\",\"$proxy\"],\"paths\":{\"$hosted\":[\"acme/*\"],\"$proxy\":[\"*\"]}}}" \
	"$base/api/v1/repos"
group_made=yes
step "POST /api/v1/repos oci/$hosted, hosted, and oci/$group, a group of it for acme/* and of oci/$proxy for the rest: 201"

(umask 077 && printf '{"auths":{"%s":{"auth":"%s"}}}\n' "$host" "$(printf '%s:%s' "$E2E_USER" "$E2E_PASSWORD" | base64 | tr -d '\n')" >"$work/docker/config.json")
api="acme/api-$run"
head -c "$size" /dev/urandom >"$work/api.data"
tar -C "$work" -cf "$work/api.tar" api.data
ref="$(crane append --insecure --oci-empty-base -f api.tar -t "$host/oci/$hosted/$api:1.4" 2>"$work/crane.log")" ||
	{ cat "$work/crane.log" >&2; fail "crane append to $host/oci/$hosted/$api:1.4"; }
rm -f "$work/docker/config.json"
pushed+=("$registry/${prefix}oci/$hosted/$api@${ref##*@}")
assert_eq "${ref##*@}" "$(crane digest --insecure "$host/oci/$group/$api:1.4")" "the digest of $api:1.4 through the group"
crane pull --insecure "$host/oci/$group/$api:1.4" api-group.tar 2>"$work/crane.log" ||
	{ cat "$work/crane.log" >&2; fail "crane pull $host/oci/$group/$api:1.4"; }
assert_eq 1.4 "$(crane ls --insecure "$host/oci/$group/$api")" "tags/list of $api through the group"
step "crane push to $host/oci/$hosted/$api:1.4, then pulled through $host/oci/$group: ${ref##*@}, tags/list 1.4"

crane pull --insecure --platform linux/amd64 "$host/oci/$group/e2e-ociproxy-$run/img:1.0" amd64-group.tar 2>"$work/crane.log" ||
	{ cat "$work/crane.log" >&2; fail "crane pull --platform linux/amd64 $host/oci/$group/e2e-ociproxy-$run/img:1.0"; }
assert_eq "$amd64" "$(crane digest --insecure --platform linux/amd64 "$host/oci/$group/e2e-ociproxy-$run/img:1.0")" "the digest of img:1.0 for linux/amd64 through the group"
assert_eq 2 "$(grep -F ' msg=request method=GET ' "$log" | grep -F " path=/v2/oci/$group/e2e-ociproxy-$run/img/blobs/" | grep -cF ' status=200 ' || true)" \
	"the blobs of img the group served"
assert_eq 4 "$(fetched "e2e-ociproxy-$run/img")" "the blobs of img fetched after its pull through the group"
seed grp
grp_arm64="$(crane digest --insecure --platform linux/arm64 "$up/grp:1.0")"
crane pull --insecure --platform linux/arm64 "$host/oci/$group/e2e-ociproxy-$run/grp:1.0" grp.tar 2>"$work/crane.log" ||
	{ cat "$work/crane.log" >&2; fail "crane pull --platform linux/arm64 $host/oci/$group/e2e-ociproxy-$run/grp:1.0"; }
pushed+=("$back/grp@$grp_arm64")
assert_eq 2 "$(fetched "e2e-ociproxy-$run/grp")" "the blobs of grp fetched through the group"
# via_rows prints the digests of the proxy's proxy.fetch rows that name the group, completed, sorted.
via_rows() {
	curl -sS -K "$E2E_CURLRC" --globoff "$base/api/v1/audit?action=proxy.fetch&repo=oci/$proxy&limit=100" |
		jq -r --arg v "oci/$group" '[.events[] | select(.details.via == $v and .state == "ok") | .digest] | sort | join(" ")'
}
eventually test "$(via_rows)" = "$grp_arm64" || fail "proxy.fetch rows of oci/$proxy with via oci/$group: '$(via_rows)', want $grp_arm64"
step "crane pull through $host/oci/$group of img:1.0, the proxy's image with no blob fetched, and of grp:1.0 for linux/arm64: two blobs fetched, proxy.fetch via oci/$group"

assert_status 405 -K "$E2E_CURLRC" -X POST "$base/v2/oci/$group/$api/blobs/uploads/"
grep -qi '^allow: GET, HEAD' <(curl -sS -D - -o /dev/null -K "$E2E_CURLRC" -X POST "$base/v2/oci/$group/$api/blobs/uploads/") ||
	fail "the 405 of an upload through the group has no Allow: GET, HEAD"
step "POST $base/v2/oci/$group/$api/blobs/uploads/ by $E2E_USER: 405 UNSUPPORTED, Allow: GET, HEAD"

assert_status 204 -K "$E2E_CURLRC" -X DELETE "$base/api/v1/repos/oci/$group"
group_made=""
assert_status 204 -K "$E2E_CURLRC" -X DELETE "$base/api/v1/repos/oci/$hosted?force=true"
hosted_made=""
assert_status 204 -K "$E2E_CURLRC" -X DELETE "$base/api/v1/repos/oci/$proxy?force=true"
proxy_made=""
assert_status 204 -K "$E2E_CURLRC" -X DELETE "$base/api/v1/repos/oci/$private?force=true"
private_made=""
step "DELETE /api/v1/repos oci/$group, oci/$hosted, oci/$proxy and oci/$private: 204"
echo "PASS ociproxy e2e against $base as $E2E_USER"

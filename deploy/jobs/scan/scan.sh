#!/usr/bin/env bash
# The reference scan job of #89 (docs/guides/sbom-and-scans.md, A reference job): for every
# version of a repo that has no SBOM, or no scan report, it reads the version's files through
# /v2, makes a CycloneDX SBOM of them with syft and a vulnerability report of that SBOM with
# trivy, and attaches each to the version with cosign attest, signed with the job's key. It is a
# reconciliation, not an event pipeline: each run asks the server which versions still lack an
# attachment (GET …/versions?without=sbom, ?without=scan), so a missed run or a crashed one loses
# nothing, and a version it attached to is not listed again.
#
#   deploy/jobs/scan/scan.sh <format>/<repo> [<package>...]     default: every package
#
# Environment:
#   LAJSTROM_URL      the server, https://art.example.com (http only for a test server)
#   LAJSTROM_USER     the user of the credential; any name for a deploy token (default deploy)
#   LAJSTROM_TOKEN    a deploy token with the scopes read and attach: read lists the versions and
#                     reads their files, attach pushes the attestations; it cannot publish
#   COSIGN_KEY        the job's private key (cosign generate-key-pair), COSIGN_PASSWORD its
#                     password; the server trusts its public key (attachments.trust)
#   SYFT, TRIVY, COSIGN  the commands, syft, trivy and cosign by default; a container works,
#                     docker run --rm -v "$TMPDIR:$TMPDIR" anchore/syft:v1.33.0, when it sees the
#                     job's TMPDIR at the same path. The default cosign keeps its login in a
#                     directory of the job (DOCKER_CONFIG); a COSIGN container needs one of its
#                     own that outlives each call, -e DOCKER_CONFIG=<a directory of TMPDIR>, and
#                     COSIGN_KEY and COSIGN_PASSWORD
#   COSIGN_FLAGS      more flags of cosign attest, --allow-http-registry for a test server
#
# Nothing goes to the public Sigstore: cosign attest runs with --tlog-upload=false and
# --use-signing-config=false, as the guide's examples. cosign 3.1 sets no manifest annotation, so
# the report's lajstrom.attachment.input stays empty; the server reads the tool, its version and
# the database from the predicates (manifest-profile.md §6.2). It needs bash, curl and jq.
set -euo pipefail

usage() {
	echo "usage: $0 <format>/<repo> [<package>...]" >&2
	exit 2
}
[ $# -ge 1 ] && [[ "$1" == */* ]] || usage
repo="$1"
shift
: "${LAJSTROM_URL:?the server}" "${LAJSTROM_TOKEN:?a deploy token with read and attach}" "${COSIGN_KEY:?the private key of the job}"
user="${LAJSTROM_USER:-deploy}"
host="${LAJSTROM_URL#*://}"
host="${host%/}"
read -r -a syft <<<"${SYFT:-syft}"
read -r -a trivy <<<"${TRIVY:-trivy}"
read -r -a cosign_flags <<<"${COSIGN_FLAGS:-}"
work="$(mktemp -d "${TMPDIR:-/tmp}/scan.XXXXXX")"
trap 'rm -rf "$work"' EXIT
cosign=(env DOCKER_CONFIG="$work/docker" cosign) # its login is the job's own, gone with it
[ -z "${COSIGN:-}" ] || read -r -a cosign <<<"$COSIGN"
netrc="$work/netrc" # curl's credential, off the command line
sep=$'\x1f'         # the field separator of the lists below
(umask 077 && printf 'machine %s login %s password %s\n' "${host%%:*}" "$user" "$LAJSTROM_TOKEN" >"$netrc")

# api PATH prints the answer of GET /api/v1/repos/<repo>PATH.
api() { curl -fsS --netrc-file "$netrc" "$LAJSTROM_URL/api/v1/repos/$repo$1"; }

# uri S prints S percent-encoded for a path segment or a query value.
uri() { jq -rn --arg s "$1" '$s | @uri'; }

# packages prints the names of the repo's packages, one per line, every page.
packages() {
	local cursor="" page
	while :; do
		page="$(api "/packages?limit=500${cursor:+&cursor=$(uri "$cursor")}")"
		jq -r '.packages[].name' <<<"$page"
		cursor="$(jq -r '.next // empty' <<<"$page")"
		[ -n "$cursor" ] || return 0
	done
}

# lacking PACKAGE KIND prints the version, the variant and the digest of each variant of PACKAGE
# without an attachment of KIND, separated by $sep, every page. The separator is no whitespace, so
# that read keeps the empty variant of a format that has none.
lacking() {
	local cursor="" page
	while :; do
		page="$(api "/packages/$(uri "$1")/versions?limit=100&without=$2${cursor:+&cursor=$(uri "$cursor")}")"
		jq -r --arg sep "$sep" '.versions[] | .version as $v | .variants[] | [$v, .variant, .digest] | join($sep)' <<<"$page"
		cursor="$(jq -r '.next // empty' <<<"$page")"
		[ -n "$cursor" ] || return 0
	done
}

# sbom PACKAGE VERSION VARIANT writes the CycloneDX SBOM of the variant's files to $work/sbom.json
# and prints its OCI repository: the files are read by digest through /v2, which takes the
# token as Basic credentials.
sbom() {
	local detail oci files="$work/files"
	detail="$(api "/packages/$(uri "$1")/versions/$(uri "$2")?variant=$(uri "$3")")"
	oci="$(jq -r .ociRepository <<<"$detail")"
	rm -rf "$files" && mkdir -p "$files"
	while IFS="$sep" read -r name digest; do
		curl -fsSL --netrc-file "$netrc" -o "$files/$(basename "$name")" "$LAJSTROM_URL/v2/$oci/blobs/$digest"
	done < <(jq -r --arg sep "$sep" '.variants[0].files[] | [.name, .digest] | join($sep)' <<<"$detail")
	"${syft[@]}" scan "dir:$files" -o "cyclonedx-json=$work/sbom.json" -q
	echo "$oci"
}

# attest TYPE PREDICATE OCI DIGEST signs PREDICATE as an attestation of TYPE and pushes it as a
# referrer of the manifest DIGEST in OCI.
attest() {
	"${cosign[@]}" attest --key "$COSIGN_KEY" --type "$1" --predicate "$2" --tlog-upload=false --use-signing-config=false -y \
		"${cosign_flags[@]}" "$host/$3@$4"
}

printf '%s\n' "$LAJSTROM_TOKEN" | "${cosign[@]}" login "$host" -u "$user" --password-stdin >/dev/null
pkgs=("$@")
if [ $# -eq 0 ]; then
	while IFS= read -r p; do pkgs+=("$p"); done < <(packages)
fi
attached=0
for pkg in "${pkgs[@]}"; do
	while IFS="$sep" read -r version variant digest; do
		oci="$(sbom "$pkg" "$version" "$variant")"
		attest cyclonedx "$work/sbom.json" "$oci" "$digest"
		echo "sbom  $repo $pkg $version ${variant:--} $digest"
		attached=$((attached + 1))
	done < <(lacking "$pkg" sbom)
	while IFS="$sep" read -r version variant digest; do
		oci="$(sbom "$pkg" "$version" "$variant")"
		"${trivy[@]}" sbom --quiet --format cosign-vuln --output "$work/vuln.json" "$work/sbom.json"
		attest vuln "$work/vuln.json" "$oci" "$digest"
		echo "scan  $repo $pkg $version ${variant:--} $digest"
		attached=$((attached + 1))
	done < <(lacking "$pkg" scan)
done
echo "$attached attestations attached to $repo"

#!/usr/bin/env bash
# End-to-end test of an rpm repo with real clients against a running Lajstrom (#64;
# docs/spec/http-routing.md, rpm):
#
#   - two noarch packages built with rpmbuild in the dnf client's container, with a gzip payload,
#     which zypper's rpm reads too: one uploaded with
#     curl, PUT …/upload; the other, with an epoch and a requirement on the first, pushed with
#     oras through the OCI facade with the rpm profile (its metadata layer from lajstrom admin rpm
#     metadata);
#   - repomd.xml lists the three documents, primary has both packages;
#   - the repo's key.asc as gpgkey= with repo_gpgcheck=1 (repo_gpgcheck=0 when the server has no
#     master key and key.asc is 404): dnf install of the second pulls the first, both commands
#     run; the same with zypper when $RPM_E2E_ZYPPER_IMAGE is not empty, which imports
#     repodata/repomd.xml.key as it checks repomd.xml.asc;
#   - other bytes under the first package's name, version and arch: 409;
#   - the second package deleted through DELETE /api/v1/repos/rpm/<repo>/packages/<name>/versions/<v>:
#     the next primary and dnf no longer have it;
#   - reindex: the repo deleted with ?force=true (the registry keeps the manifests), made again
#     empty, POST …/reindex: primary, filelists and other are the same bytes as before; dnf
#     installs the first package again from the rebuilt repo, with the new repo's key (dnf
#     removed it with the second, as a dependency nothing needed any more).
#
#   test/e2e/rpm.sh [<base url> [<repo>]]     default: http://localhost:8080 e2e-<run>
#
# The script creates the rpm repo through POST /api/v1/repos on the registry
# $RPM_E2E_REGISTRY (default local), and deletes it at the end; the user of LAJSTROM_E2E_USER and
# LAJSTROM_E2E_PASSWORD must be an admin (run.sh makes one; lib.sh, e2e_credentials). It needs
# curl, jq and docker: the image $RPM_E2E_IMAGE (default Rocky Linux 9.7 from quay.io, pinned by
# digest, for linux/amd64), which builds the packages (dnf installs rpm-build from the Rocky
# mirrors) and is the dnf client; the image $RPM_E2E_ZYPPER_IMAGE (default openSUSE Leap 15.6 from
# registry.opensuse.org, pinned by digest; empty skips the zypper step); and
# ghcr.io/oras-project/oras:v1.3.6. The containers use --network host, which reaches the base
# URL on Linux and under OrbStack. lajstrom admin rpm metadata runs from $LAJSTROM_BIN, else
# bin/lajstrom, else go run ./cmd/lajstrom. It prints one line per step and exits non-zero at the
# first failure, naming the assertion or the command (lib.sh). make e2e runs it
# (test/e2e/run.sh).
set -euo pipefail
. "$(dirname "$0")/lib.sh"

base="${1:-http://localhost:8080}"
base="${base%/}"
run="$(date -u +%Y%m%d%H%M%S)-$RANDOM"
repo="${2:-e2e-$run}"
for tool in curl jq docker; do
	command -v "$tool" >/dev/null || { echo "rpm.sh: $tool is required" >&2; exit 2; }
done
root="$(cd "$(dirname "$0")/../.." && pwd)"
image="${RPM_E2E_IMAGE:-quay.io/rockylinux/rockylinux:9.7@sha256:53f4c6dcb34e1403bd93207351f0af9a593610faeb7165cb8a037346765199b0}"
zypper_image="${RPM_E2E_ZYPPER_IMAGE-registry.opensuse.org/opensuse/leap:15.6@sha256:e4d84bbc5e0fa0df64381e95ad7ea8e3867cc34f3b1d174e9948d0f559d223e2}"
oras_image=ghcr.io/oras-project/oras:v1.3.6
backend="${RPM_E2E_REGISTRY:-local}"
registry="${base#*://}" # host:port of the OCI facade, for oras
plain=""                # --plain-http for an http:// base URL
case "$base" in http://*) plain=--plain-http ;; esac

work="$(mktemp -d)"
client="lajstrom-e2e-rpm-$run" # the dnf container, with $work/client as /w
zclient="lajstrom-e2e-zypper-$run"
created="" # set once the repo is created, for the cleanup
cleanup() {
	docker exec "$client" chown -R "$(id -u):$(id -g)" /w >/dev/null 2>&1 || true
	docker rm -f "$client" "$zclient" >/dev/null 2>&1 || true
	if [ -n "$created" ]; then
		curl -sS -o /dev/null -K "$E2E_CURLRC" -X DELETE "$base/api/v1/repos/rpm/$repo?force=true" || true
	fi
	rm -rf "$work"
}
trap cleanup EXIT
e2e_credentials "$work"
mkdir -p "$work/client"
one="e2e-put-$run"  # uploaded with PUT
two="e2e-oras-$run" # pushed with oras: epoch 1, requires $one
status=""           # status of the last request

# sha256 prints the hex sha256 of a file.
sha256() {
	if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

# rpm_metadata FILE prints the rpm metadata layer of FILE, an absolute path.
rpm_metadata() {
	if [ -n "${LAJSTROM_BIN:-}" ]; then
		"$LAJSTROM_BIN" admin rpm metadata "$1"
	elif [ -x "$root/bin/lajstrom" ]; then
		"$root/bin/lajstrom" admin rpm metadata "$1"
	else
		(cd "$root" && go run ./cmd/lajstrom admin rpm metadata "$1")
	fi
}

# hdr NAME prints the value of the response header NAME (lower case) of the last request.
hdr() {
	tr -d '\r' <"$work/headers" | awk -v n="$1" 'index(tolower($0), n ": ") == 1 { sub(/^[^:]*: /, ""); v = $0 } END { print v }'
}

# get PATH OUT: GET /rpm/<repo>/PATH as $E2E_USER into OUT; the status goes to $status.
get() { status="$(curl -sS -K "$E2E_CURLRC" -o "$2" -w '%{http_code}' "$base/rpm/$repo/$1")"; }

# api METHOD PATH [BODY]: a request of $E2E_USER to /api/v1/PATH; the body goes to $work/body.
api() {
	local data=()
	[ -z "${3:-}" ] || data=(-H 'Content-Type: application/json' -d "$3")
	status="$(curl -sS -K "$E2E_CURLRC" -o "$work/body" -w '%{http_code}' -X "$1" "${data[@]}" "$base/api/v1/$2")"
}

# put FILE: PUT FILE to upload; the status goes to $status, the headers to $work/headers, the body
# to $work/body.
put() {
	status="$(curl -sS -K "$E2E_CURLRC" -o "$work/body" -D "$work/headers" -w '%{http_code}' -T "$1" "$base/rpm/$repo/upload")"
}

# in_client LOG SCRIPT: run SCRIPT with bash -e as root in the dnf container, its output in
# $work/LOG; the status is the script's.
in_client() { docker exec "$client" bash -ec "$2" >"$work/$1" 2>&1; }

# repodata DIR: fetch repomd.xml and the three documents it lists into DIR, uncompressed, as
# primary.xml, filelists.xml and other.xml.
repodata() {
	mkdir -p "$1"
	get repodata/repomd.xml "$1/repomd.xml"
	expect "GET repodata/repomd.xml" 200 "$status"
	local href typ
	for typ in primary filelists other; do
		href="$(grep -o "repodata/[0-9a-f]*-$typ.xml.gz" "$1/repomd.xml" || true)"
		[ -n "$href" ] || fail "repomd.xml lists no $typ: $(cat "$1/repomd.xml")"
		get "$href" "$1/$typ.xml.gz"
		expect "GET $href" 200 "$status"
		expect "the sha256 of $href" "$(echo "$href" | sed 's#repodata/\([0-9a-f]*\)-.*#\1#')" "$(sha256 "$1/$typ.xml.gz")"
		gzip -dc "$1/$typ.xml.gz" >"$1/$typ.xml"
	done
}

# repofile KEY: write the dnf repo file of the repo to $work/client/lajstrom.repo, with the
# credentials, repo_gpgcheck=1 and gpgkey=file:///w/KEY when KEY is not empty, repo_gpgcheck=0
# otherwise. The packages are not signed: gpgcheck=0.
repofile() {
	local check=0 key=""
	if [ -n "$1" ]; then check=1 key="gpgkey=file:///w/$1"; fi
	(umask 077 && printf '[%s]\nname=%s\nbaseurl=%s\nusername=%s\npassword=%s\nrepo_gpgcheck=%s\ngpgcheck=0\n%s\n' \
		"$repo" "$repo" "$base/rpm/$repo/" "$E2E_USER" "$E2E_PASSWORD" "$check" "$key" >"$work/client/lajstrom.repo")
}

# R-01: the repo, and the client container, which builds the packages.
api POST repos "{\"format\":\"rpm\",\"name\":\"$repo\",\"registry\":\"$backend\"}"
expect "POST /api/v1/repos rpm/$repo ($(cat "$work/body"))" 201 "$status"
created=1
docker run -d --name "$client" --network host --platform linux/amd64 -v "$work/client:/w" "$image" sleep infinity >/dev/null
in_client build.log "
	dnf -q -y install rpm-build >/dev/null
	mkdir -p /w/rpmbuild/SPECS
	spec() { # NAME EPOCH REQUIRES GREETING
		{
			echo \"Name: \$1\"; [ -z \"\$2\" ] || echo \"Epoch: \$2\"; echo 'Version: 1.0'; echo 'Release: 1'
			echo \"Summary: says \$4\"; echo 'License: Apache-2.0'; echo 'BuildArch: noarch'
			[ -z \"\$3\" ] || echo \"Requires: \$3\"
			echo '%description'; echo \"A package of the rpm e2e of Lajstrom that says \$4.\"
			echo '%install'; echo 'mkdir -p %{buildroot}/usr/bin %{buildroot}/etc/'\$1
			echo \"printf '#!/bin/sh\\\\necho \$4\\\\n' > %{buildroot}/usr/bin/\$1 && chmod 0755 %{buildroot}/usr/bin/\$1\"
			echo \"echo greeting=\$4 > %{buildroot}/etc/\$1/\$1.conf\"
			echo '%post'; echo 'true'
			echo '%files'; echo /usr/bin/\$1; echo %dir /etc/\$1; echo %config /etc/\$1/\$1.conf
			echo '%changelog'; echo '* Thu Nov 12 2026 Lajstrom e2e <e2e@lajstrom.dev> - 1.0-1'; echo '- First build'
		} > /w/rpmbuild/SPECS/\$1.spec
		rpmbuild -bb --define '_topdir /w/rpmbuild' --define '_binary_payload w9.gzdio' /w/rpmbuild/SPECS/\$1.spec >/dev/null
	}
	spec $one '' '' 'hello from $one'
	spec $two 1 '$one' 'hello from $two'
	cp /w/rpmbuild/RPMS/noarch/$one-1.0-1.noarch.rpm /w/one.rpm
	cp /w/rpmbuild/RPMS/noarch/$two-1.0-1.noarch.rpm /w/two.rpm
	sleep 1; spec $one '' '' 'other bytes'; cp /w/rpmbuild/RPMS/noarch/$one-1.0-1.noarch.rpm /w/other.rpm
	chown -R $(id -u):$(id -g) /w" || { cat "$work/build.log" >&2; fail "rpmbuild in $image"; }
cmp -s "$work/client/one.rpm" "$work/client/other.rpm" && fail "the two builds of $one have the same bytes"
ok "POST /api/v1/repos rpm/$repo: 201; rpmbuild in $image: $one-1.0-1.noarch.rpm, $two-1:1.0-1.noarch.rpm, other bytes under the first"

# R-02: PUT the first package.
put "$work/client/one.rpm"
expect "PUT $one status ($(cat "$work/body"))" 201 "$status"
expect "PUT $one Location" "/rpm/$repo/Packages/$one-1.0-1.noarch.rpm" "$(hdr location)"
digest1="$(jq -r .digest "$work/body")"
expect "PUT $one Docker-Content-Digest" "$digest1" "$(hdr docker-content-digest)"
expect "PUT $one body" "$one 0 1.0 1 noarch $one-1.0-1.noarch.rpm 1.0-1__noarch" \
	"$(jq -r '[.name, (.epoch | tostring), .version, .release, .arch, .filename, .tag] | join(" ")' "$work/body")"
get "Packages/$one-1.0-1.noarch.rpm" "$work/one.back"
expect "GET Packages/$one-1.0-1.noarch.rpm" 200 "$status"
cmp -s "$work/client/one.rpm" "$work/one.back" || fail "Packages/$one-1.0-1.noarch.rpm is not the file uploaded"
ok "PUT $one-1.0-1.noarch.rpm: 201 with $digest1, Location below Packages/, the same bytes served there"

# R-03: oras push of the second package with the rpm profile, the metadata layer from lajstrom
# admin rpm metadata.
mkdir -p "$work/two"
cp "$work/client/two.rpm" "$work/two/$two-1.0-1.noarch.rpm"
rpm_metadata "$work/two/$two-1.0-1.noarch.rpm" >"$work/two/metadata.json" || fail "lajstrom admin rpm metadata"
expect "metadata name, epoch, arch" "$two 1 noarch" "$(jq -r '[.name, (.epoch | tostring), .arch] | join(" ")' "$work/two/metadata.json")"
expect "metadata sha256" "$(sha256 "$work/two/$two-1.0-1.noarch.rpm")" "$(jq -r .sha256 "$work/two/metadata.json")"
docker run --rm -i --network host -u "$(id -u):$(id -g)" -v "$work/two:/w" -w /w "$oras_image" login ${plain:+"$plain"} \
	--registry-config /w/oras-auth.json -u "$E2E_USER" --password-stdin "$registry" <<<"$E2E_PASSWORD" >"$work/oras.log" 2>&1 ||
	{ cat "$work/oras.log" >&2; fail "oras login"; }
docker run --rm --network host -u "$(id -u):$(id -g)" -v "$work/two:/w" -w /w "$oras_image" push ${plain:+"$plain"} \
	--registry-config /w/oras-auth.json \
	--artifact-type application/vnd.lajstrom.rpm.v1 \
	--annotation lajstrom.schema=1 --annotation lajstrom.format=rpm --annotation "lajstrom.repo=$repo" \
	--annotation "lajstrom.name=$two" --annotation "lajstrom.version=1:1.0-1" --annotation lajstrom.variant=noarch \
	--annotation "org.opencontainers.image.created=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
	"$registry/rpm/$repo/$two:1_3a1.0-1__noarch" \
	"$two-1.0-1.noarch.rpm:application/vnd.lajstrom.rpm.package.v1" \
	metadata.json:application/vnd.lajstrom.rpm.metadata.v1+json >"$work/oras.log" 2>&1 ||
	{ cat "$work/oras.log" >&2; fail "oras push $two"; }
ok "oras push $two-1:1.0-1.noarch.rpm: $(grep '^Digest:' "$work/oras.log")"

# R-04: the repodata lists both.
repodata "$work/before"
for p in "$one" "$two"; do
	grep -qF "<name>$p</name>" "$work/before/primary.xml" || fail "primary lacks $p: $(head -c 2000 "$work/before/primary.xml")"
done
grep -qF "<rpm:entry name=\"$one\"/>" "$work/before/primary.xml" || fail "primary lacks the requirement of $two on $one"
grep -qF "<file>/usr/bin/$one</file>" "$work/before/primary.xml" || fail "primary lacks /usr/bin/$one"
ok "repomd.xml lists primary, filelists and other, their checksums those of the files; primary has $one and $two"

# R-05: dnf with repo_gpgcheck=1 and the repo's key, or repo_gpgcheck=0 when the server does not
# sign.
get key.asc "$work/client/key.asc"
case "$status" in
200) key=key.asc; signed="repo_gpgcheck=1, gpgkey the repo's key.asc" ;;
404) key=""; signed="repo_gpgcheck=0: the server does not sign" ;;
*) fail "GET key.asc: status $status" ;;
esac
repofile "$key"
in_client dnf.log "
	install -m 0600 /w/lajstrom.repo /etc/yum.repos.d/lajstrom.repo
	dnf -y --disablerepo='*' --enablerepo='$repo' install $two
	echo \"greeting: \$($one) / \$($two)\"
	rpm -q $one $two" || { cat "$work/dnf.log" >&2; fail "dnf install $two in $image"; }
grep -qxF "greeting: hello from $one / hello from $two" "$work/dnf.log" || { cat "$work/dnf.log" >&2; fail "the installed commands"; }
grep -qxF "$two-1.0-1.noarch" "$work/dnf.log" || { cat "$work/dnf.log" >&2; fail "rpm -q $two"; }
if [ -n "$key" ]; then
	grep -qi "importing gpg key" "$work/dnf.log" || { cat "$work/dnf.log" >&2; fail "dnf did not import the repo's key"; }
fi
ok "dnf install $two ($signed): $one came with it, both commands run"

# R-06: zypper, which fetches repomd.xml.key beside repomd.xml.asc.
if [ -n "$zypper_image" ]; then
	gpg_opts="--gpgcheck-allow-unsigned-package" # the repodata checked, the packages unsigned
	[ -n "$key" ] || gpg_opts="--no-gpgcheck"
	(umask 077 && printf 'username=%s\npassword=%s\n' "$E2E_USER" "$E2E_PASSWORD" >"$work/client/zypp.credentials")
	docker run -d --name "$zclient" --network host --platform linux/amd64 -v "$work/client:/w" "$zypper_image" sleep infinity >/dev/null
	docker exec "$zclient" bash -ec "
		install -D -m 0600 /w/zypp.credentials /etc/zypp/credentials.d/lajstrom
		zypper -n addrepo $gpg_opts '$base/rpm/$repo/?credentials=lajstrom' $repo
		zypper -n --gpg-auto-import-keys refresh $repo
		zypper -n install $two
		echo \"greeting: \$($one) / \$($two)\"" >"$work/zypper.log" 2>&1 ||
		{ cat "$work/zypper.log" >&2; fail "zypper install $two in $zypper_image"; }
	grep -qxF "greeting: hello from $one / hello from $two" "$work/zypper.log" || { cat "$work/zypper.log" >&2; fail "the commands installed by zypper"; }
	docker rm -f "$zclient" >/dev/null
	ok "zypper refresh and install $two in $zypper_image ($signed): both commands run"
else
	ok "zypper: skipped, RPM_E2E_ZYPPER_IMAGE is empty"
fi

# R-07: other bytes under the first package's name, version and arch.
put "$work/client/other.rpm"
expect "PUT of other bytes as $one-1.0-1.noarch ($(cat "$work/body"))" 409 "$status"
ok "PUT of other bytes under $one-1.0-1.noarch: 409"

# R-08: the second package deleted through the API: gone from primary and from dnf.
api DELETE "repos/rpm/$repo/packages/$two/versions/1%3A1.0-1?variant=noarch"
expect "DELETE of $two 1:1.0-1 ($(cat "$work/body"))" 204 "$status"
repodata "$work/deleted"
if grep -qF "<name>$two</name>" "$work/deleted/primary.xml"; then fail "primary still has $two after the delete"; fi
in_client gone.log "
	dnf -y remove $two >/dev/null
	dnf --disablerepo='*' --enablerepo='$repo' clean all >/dev/null
	dnf -q --disablerepo='*' --enablerepo='$repo' list --available '$two' || echo 'no $two'" ||
	{ cat "$work/gone.log" >&2; fail "dnf list after the delete"; }
grep -qxF "no $two" "$work/gone.log" || { cat "$work/gone.log" >&2; fail "dnf still lists $two after the delete"; }
ok "DELETE /api/v1/repos/rpm/$repo/packages/$two/versions/1:1.0-1?variant=noarch: 204; primary and dnf list no longer have it"

# R-09: reindex rebuilds the repo from the registry: the same documents.
api DELETE "repos/rpm/$repo?force=true"
expect "DELETE /api/v1/repos/rpm/$repo?force=true ($(cat "$work/body"))" 204 "$status"
api POST repos "{\"format\":\"rpm\",\"name\":\"$repo\",\"registry\":\"$backend\"}"
expect "POST /api/v1/repos rpm/$repo again ($(cat "$work/body"))" 201 "$status"
get "Packages/$one-1.0-1.noarch.rpm" "$work/none"
expect "GET Packages/$one-1.0-1.noarch.rpm in the repo made again" 404 "$status"
api POST "repos/rpm/$repo/reindex"
expect "POST /api/v1/repos/rpm/$repo/reindex ($(cat "$work/body"))" 202 "$status"
for _ in $(seq 1 120); do
	api GET "repos/rpm/$repo/reindex"
	[ "$(jq -r .state "$work/body")" = running ] || break
	sleep 0.5
done
expect "the state of the reindex of rpm/$repo ($(head -c 300 "$work/body"))" done "$(jq -r .state "$work/body")"
jq -e '.report | .errors == [] and .foreign == []' "$work/body" >/dev/null || fail "the report of the reindex: $(jq -c .report "$work/body")"
repodata "$work/after"
for typ in primary filelists other; do
	cmp -s "$work/deleted/$typ.xml" "$work/after/$typ.xml" ||
		fail "$typ.xml after the reindex differs: $(diff "$work/deleted/$typ.xml" "$work/after/$typ.xml" | head -n 20)"
done
get key.asc "$work/client/key2.asc"
case "$status" in 200) key=key2.asc ;; *) key="" ;; esac
repofile "$key"
in_client again.log "
	install -m 0600 /w/lajstrom.repo /etc/yum.repos.d/lajstrom.repo
	rm -rf /var/cache/dnf/$repo-*
	dnf -y --disablerepo='*' --enablerepo='$repo' install $one
	$one" || { cat "$work/again.log" >&2; fail "dnf install $one from the rebuilt repo"; }
grep -qxF "hello from $one" "$work/again.log" || { cat "$work/again.log" >&2; fail "the command after the install"; }
if [ -n "$key" ] && ! grep -qi "importing gpg key" "$work/again.log"; then
	cat "$work/again.log" >&2
	fail "dnf did not import the key of the repo made again"
fi
ok "rpm/$repo deleted and made again, reindex: $(jq -r '.report | "\(.added) added"' "$work/body"); primary, filelists and other the same bytes; dnf install $one from it with the new repo's key"

#!/usr/bin/env bash
# End-to-end test of repos that encrypt at rest (#114, ADR 0009; docs/operations.md, Encryption at
# rest) with real clients against a running Lajstrom: a raw, a pypi and an apt repo with
# policy.encryption at-rest and a recipient key that the run makes with openssl.
#
#   - a repo at-rest without a recipient is refused (400), naming policy.encryption.recipients;
#   - raw: a PUT of a file with a known string, the GET of its bytes, whole and a range; the
#     manifest in the registry, read straight from it, names +encrypted layers with the wrapped
#     key, and no blob of it holds the string;
#   - oras push of a file through /v2, oras pull of the same bytes, oras manifest fetch of the
#     manifest as pushed, under the digest oras computed;
#   - the ocicrypt check: skopeo copy --decryption-key with the recipient's private key, of the
#     manifest straight from the registry, gets the plaintext;
#   - pip install of a wheel uploaded to the encrypted pypi repo, and the module imported;
#   - apt-get install of a .deb uploaded to the encrypted apt repo, signed by the repo's key;
#   - lajstrom reindex of the raw repo, with the keys: no error.
#
#   test/e2e/encryption.sh [<base url>]     default: http://localhost:8080
#
# The server needs the master key (run.sh sets LAJSTROM_MASTER_KEY). The user of
# LAJSTROM_E2E_USER and LAJSTROM_E2E_PASSWORD needs the admin role: the run creates its repos
# through the API and deletes them at the end with lajstrom admin repo delete --force. The
# registry `local` is $LAJSTROM_E2E_REGISTRY, read with $LAJSTROM_E2E_REGISTRY_USER and
# $LAJSTROM_E2E_REGISTRY_PASSWORD when set. It needs curl, jq, openssl, python3 and docker
# (quay.io/skopeo/stable:v1.20.0, python:3.13-slim, ubuntu:24.04, or $ENCRYPTION_E2E_SKOPEO,
# $ENCRYPTION_E2E_PYTHON, $ENCRYPTION_E2E_APT, with --network host, which reaches the base URL and
# the registry on Linux and under OrbStack), and oras, or else its image
# ghcr.io/oras-project/oras:v1.3.6 ($ENCRYPTION_E2E_ORAS); it prints one line per step and exits
# non-zero at the first failure.
set -euo pipefail
umask 077
. "$(dirname "$0")/lib.sh"

base="${1:-http://localhost:8080}"
registry="${LAJSTROM_E2E_REGISTRY:-localhost:5000}"
prefix="${LAJSTROM_E2E_REGISTRY_PREFIX:-}"
skopeo_image="${ENCRYPTION_E2E_SKOPEO:-quay.io/skopeo/stable:v1.20.0}"
python_image="${ENCRYPTION_E2E_PYTHON:-python:3.13-slim}"
apt_image="${ENCRYPTION_E2E_APT:-ubuntu:24.04}"
oras_image="${ENCRYPTION_E2E_ORAS:-ghcr.io/oras-project/oras:v1.3.6}"
for tool in curl jq openssl python3 docker; do
	command -v "$tool" >/dev/null || { echo "encryption.sh: $tool is required" >&2; exit 2; }
done
export LAJSTROM_CONFIG="${LAJSTROM_CONFIG:-deploy/compose/lajstrom.example.yaml}"
if [ -n "${LAJSTROM_BIN:-}" ]; then
	lajstrom=("$LAJSTROM_BIN")
elif [ -x bin/lajstrom ]; then
	lajstrom=(bin/lajstrom)
else
	lajstrom=(go run ./cmd/lajstrom)
fi

work="$(mktemp -d)"
chmod 755 "$work" # the containers read the files the run makes here
run="enc-$(date -u +%Y%m%d%H%M%S)-$RANDOM"
repos=() # created, for the cleanup
cleanup() {
	for r in ${repos[@]+"${repos[@]}"}; do
		"${lajstrom[@]}" admin repo delete "$r" --force >/dev/null 2>&1 || true
	done
	rm -rf "$work"
}
trap cleanup EXIT
e2e_credentials "$work"
host="${base#*://}"
marker="a known string that the registry must never hold: $run"

# sha256 prints the hex sha256 of a file.
sha256() {
	if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

# reg PATH [curl args...]: a GET straight from the registry, below /v2/ and the instance prefix.
reg() {
	local path="$1" auth=()
	shift
	[ -z "${LAJSTROM_E2E_REGISTRY_USER:-}" ] || auth=(-u "$LAJSTROM_E2E_REGISTRY_USER:${LAJSTROM_E2E_REGISTRY_PASSWORD:-}")
	curl -fsS ${auth[@]+"${auth[@]}"} "$@" "http://$registry/v2/$prefix$path"
}

# noMarker NAME TAG checks the manifest NAME:TAG straight from the registry: every layer
# +encrypted with the wrapped key, and no blob of it, nor the manifest, holding the marker.
noMarker() {
	local name="$1" tag="$2" m layer
	m="$(reg "$name/manifests/$tag" -H 'Accept: application/vnd.oci.image.manifest.v1+json')" || fail "the manifest $name:$tag from the registry"
	if grep -qF "$marker" <<<"$m"; then fail "the manifest of $name:$tag in the registry holds the marker"; fi
	jq -e '.layers | length > 0 and all(.mediaType | endswith("+encrypted")) and
		all(.annotations["org.opencontainers.image.enc.keys.jwe"] != null and .annotations["org.opencontainers.image.enc.pubopts"] != null)' \
		<<<"$m" >/dev/null || fail "the layers of $name:$tag in the registry are not encrypted: $m"
	for layer in $(jq -r '.layers[].digest' <<<"$m"); do
		reg "$name/blobs/$layer" -L -o "$work/blob" || fail "the blob $layer of $name from the registry"
		if grep -qF "$marker" "$work/blob"; then fail "the blob $layer of $name in the registry holds the marker"; fi
	done
}

# create FORMAT: the repo FORMAT/$run on the registry local, encrypted at rest for the recipient.
create() {
	jq -n --arg f "$1" --arg n "$run" --rawfile k "$work/recipient.pub" \
		'{format: $f, name: $n, registry: "local", policy: {encryption: {mode: "at-rest", recipients: [$k]}}}' >"$work/repo.json"
	assert_status 201 -K "$E2E_CURLRC" -H 'Content-Type: application/json' --data-binary @"$work/repo.json" "$base/api/v1/repos"
	repos+=("$1/$run")
}

openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:3072 -out "$work/recipient.pem" 2>/dev/null
openssl pkey -in "$work/recipient.pem" -pubout -out "$work/recipient.pub"
chmod 644 "$work/recipient.pem"

# An at-rest repo needs a recovery recipient (decided on #114).
jq -n --arg n "$run-bare" '{format: "raw", name: $n, registry: "local", policy: {encryption: {mode: "at-rest"}}}' >"$work/bare.json"
repos+=("raw/$run-bare")
assert_status 400 -K "$E2E_CURLRC" -H 'Content-Type: application/json' --data-binary @"$work/bare.json" "$base/api/v1/repos"
grep -qF 'policy.encryption.recipients: at-rest needs at least one recipient' <<<"$E2E_BODY" ||
	fail "an at-rest repo without a recipient: the refusal does not name the recipients: ${E2E_BODY:0:300}"
step "a repo at-rest without a recipient refused: 400 naming policy.encryption.recipients"

for f in raw pypi apt; do create "$f"; done
step "repos raw, pypi and apt/$run encrypted at rest for a recipient key"

# raw: the frontend.
path="dist/secret-$run.txt"
printf '%s\n%s\n' "$marker" "$(head -c 70000 /dev/urandom | base64)" >"$work/secret.txt"
assert_status 201 -K "$E2E_CURLRC" -T "$work/secret.txt" "$base/raw/$run/$path"
version="$(sha256 "$work/secret.txt" | cut -c1-16)"
curl -fsS -K "$E2E_CURLRC" -o "$work/got.txt" "$base/raw/$run/$path" || fail "GET $path"
cmp -s "$work/secret.txt" "$work/got.txt" || fail "GET $path: other bytes"
curl -fsS -K "$E2E_CURLRC" -r 7-20 -o "$work/part" "$base/raw/$run/$path" || fail "GET $path bytes 7-20"
assert_eq "$(cut -c8-21 "$work/secret.txt" | head -1)" "$(cat "$work/part")" "GET $path bytes 7-20"
step "raw PUT and GET of $path: the bytes, whole and bytes 7-20"
noMarker "raw/$run/$path" "$version"
step "the registry holds raw/$run/$path:$version encrypted: +encrypted layers, the wrapped key, no marker"

# oras through /v2.
# oras_in DIR ARGS...: oras in $work/DIR, with a registry configuration of its own in $work; its
# image as the caller's user when the binary is not installed (what it writes, the cleanup removes).
if command -v oras >/dev/null; then
	oras_in() { local dir="$1"; shift; (cd "$work/$dir" && command oras "$@" --registry-config "$work/oras-auth.json" --plain-http); }
else
	oras_in() { local dir="$1"; shift; docker run --rm -i --network host -u "$(id -u):$(id -g)" -v "$work:/w" -w "/w/$dir" "$oras_image" "$@" --registry-config /w/oras-auth.json --plain-http; }
fi
printf '%s\n' "$E2E_PASSWORD" | oras_in . login -u "$E2E_USER" --password-stdin "$host" >"$work/oras.log" 2>&1 ||
	{ cat "$work/oras.log" >&2; fail "oras login"; }
mkdir -p "$work/push/model" "$work/pull"
opath="model/weights.bin"
printf '%s oras\n' "$marker" >"$work/push/$opath"
oversion="$(sha256 "$work/push/$opath" | cut -c1-16)"
oras_in push push --artifact-type application/vnd.lajstrom.raw.v1 \
	--annotation lajstrom.schema=1 --annotation lajstrom.format=raw --annotation "lajstrom.repo=$run" \
	--annotation "lajstrom.name=$opath" --annotation "lajstrom.version=$oversion" --annotation "lajstrom.raw.path=$opath" \
	--annotation "org.opencontainers.image.created=$(utc_at "$(date +%s)")" --export-manifest ../pushed.json \
	"$host/raw/$run/$opath:$oversion" "$opath:application/vnd.lajstrom.raw.file.v1" >"$work/oras.log" 2>&1 ||
	{ cat "$work/oras.log" >&2; fail "oras push"; }
pushed="sha256:$(sha256 "$work/pushed.json")"
oras_in pull pull "$host/raw/$run/$opath:$oversion" >"$work/oras.log" 2>&1 ||
	{ cat "$work/oras.log" >&2; fail "oras pull"; }
cmp -s "$work/push/$opath" "$work/pull/$opath" || fail "oras pull: other bytes"
oras_in . manifest fetch "$host/raw/$run/$opath:$oversion" >"$work/fetched.json" || fail "oras manifest fetch"
cmp -s "$work/pushed.json" "$work/fetched.json" || fail "oras manifest fetch: not the manifest pushed"
noMarker "raw/$run/$opath" "$oversion"
step "oras push and pull of $opath through Lajstrom: the bytes, the manifest as pushed ($pushed); encrypted in the registry"

# The ocicrypt check: skopeo decrypts what the registry has, with the recipient's private key.
creds=()
[ -z "${LAJSTROM_E2E_REGISTRY_USER:-}" ] || creds=(--src-creds "$LAJSTROM_E2E_REGISTRY_USER:${LAJSTROM_E2E_REGISTRY_PASSWORD:-}")
mkdir -p "$work/skopeo" && chmod 777 "$work/skopeo"
docker run --rm --network host -v "$work:/w" "$skopeo_image" copy --src-tls-verify=false ${creds[@]+"${creds[@]}"} \
	--decryption-key /w/recipient.pem "docker://$registry/${prefix}raw/$run/$path:$version" dir:/w/skopeo >"$work/skopeo.log" 2>&1 ||
	{ cat "$work/skopeo.log" >&2; fail "skopeo copy --decryption-key"; }
plain="$(sha256 "$work/secret.txt")"
[ -f "$work/skopeo/$plain" ] && cmp -s "$work/secret.txt" "$work/skopeo/$plain" ||
	fail "skopeo copy --decryption-key: no layer of the plaintext $plain in $(ls "$work/skopeo")"
step "skopeo copy --decryption-key of raw/$run/$path:$version from the registry: the plaintext (ocicrypt)"

# pip install from the encrypted pypi repo.
python3 - "$work/wheel" "$run" <<'PY'
import base64, hashlib, os, sys, zipfile
out, run = sys.argv[1], sys.argv[2]
name = "enc_" + run.replace("-", "_")
os.makedirs(out, exist_ok=True)
files = {
    f"{name}/__init__.py": f'MESSAGE = "decrypted {run}"\n',
    f"{name}-1.0.dist-info/METADATA": f"Metadata-Version: 2.1\nName: {name}\nVersion: 1.0\n",
    f"{name}-1.0.dist-info/WHEEL": "Wheel-Version: 1.0\nGenerator: e2e\nRoot-Is-Purelib: true\nTag: py3-none-any\n",
}
record = ""
for p, c in files.items():
    d = base64.urlsafe_b64encode(hashlib.sha256(c.encode()).digest()).rstrip(b"=").decode()
    record += f"{p},sha256={d},{len(c.encode())}\n"
record += f"{name}-1.0.dist-info/RECORD,,\n"
files[f"{name}-1.0.dist-info/RECORD"] = record
with zipfile.ZipFile(f"{out}/{name}-1.0-py3-none-any.whl", "w") as z:
    for p, c in files.items():
        z.writestr(p, c)
PY
wheel="$(ls "$work/wheel")"
assert_status 200 -K "$E2E_CURLRC" -F ':action=file_upload' -F protocol_version=1 -F "content=@$work/wheel/$wheel" "$base/pypi/$run/"
module="enc_${run//-/_}"
printf '%s' "$E2E_PASSWORD" >"$work/password" && chmod 644 "$work/password"
docker run --rm --network host -v "$work:/w" -e U="$E2E_USER" -e B="$base" -e R="$run" -e M="$module" "$python_image" sh -c '
	set -e
	P="$(python3 -c "import urllib.parse; print(urllib.parse.quote(open(\"/w/password\").read(), safe=\"\"))")"
	idx="$(echo "$B" | sed "s#://#://$U:$P@#")/pypi/$R/simple/"
	python3 -m venv /tmp/v && /tmp/v/bin/pip install -q --no-cache-dir --index-url "$idx" "$M"
	/tmp/v/bin/python -c "import $M; print($M.MESSAGE)"' >"$work/pip.log" 2>&1 ||
	{ cat "$work/pip.log" >&2; fail "pip install from pypi/$run"; }
grep -qF "decrypted $run" "$work/pip.log" || fail "pip install: the module says $(cat "$work/pip.log")"
step "pip install $module from the encrypted pypi/$run, and the module imported"

# apt-get install from the encrypted apt repo.
pkg="enc-$RANDOM"
docker run --rm -v "$work:/w" -e R="$run" -e N="$pkg" "$apt_image" sh -c '
	set -e
	mkdir -p /tmp/pkg/DEBIAN /tmp/pkg/usr/bin
	printf "Package: %s\nVersion: 1.0\nArchitecture: all\nMaintainer: e2e <e2e@example.org>\nDescription: encrypted at rest\n" "$N" >/tmp/pkg/DEBIAN/control
	printf "#!/bin/sh\necho decrypted %s\n" "$R" >/tmp/pkg/usr/bin/"$N" && chmod 755 /tmp/pkg/usr/bin/"$N"
	dpkg-deb --root-owner-group --build /tmp/pkg /w/"$N".deb >/dev/null' >"$work/apt.log" 2>&1 || { cat "$work/apt.log" >&2; fail "dpkg-deb in $apt_image"; }
assert_status 201 -K "$E2E_CURLRC" -T "$work/$pkg.deb" "$base/apt/$run/upload?suite=stable&component=main"
curl -fsS -K "$E2E_CURLRC" -o "$work/key.asc" "$base/apt/$run/key.asc" || fail "GET key.asc"
chmod 644 "$work/key.asc"
docker run --rm --network host -v "$work:/w" -e U="$E2E_USER" -e B="$base" -e R="$run" -e N="$pkg" "$apt_image" sh -c '
	set -e
	export DEBIAN_FRONTEND=noninteractive
	H="$(echo "$B" | sed "s#^.*://##")"
	mkdir -p /etc/apt/keyrings /etc/apt/auth.conf.d
	cp /w/key.asc /etc/apt/keyrings/e2e.asc
	printf "machine %s\nlogin %s\npassword %s\n" "${B%%://*}://${H%%/*}" "$U" "$(cat /w/password)" >/etc/apt/auth.conf.d/e2e.conf
	chmod 600 /etc/apt/auth.conf.d/e2e.conf
	rm -f /etc/apt/sources.list.d/* /etc/apt/sources.list
	echo "deb [signed-by=/etc/apt/keyrings/e2e.asc] $B/apt/$R stable main" >/etc/apt/sources.list.d/e2e.list
	apt-get update -q >/dev/null
	apt-get install -y -q "$N" >/dev/null
	"$N"' >"$work/apt.log" 2>&1 || { cat "$work/apt.log" >&2; fail "apt-get install from apt/$run"; }
grep -qF "decrypted $run" "$work/apt.log" || fail "apt-get install: the package says $(cat "$work/apt.log")"
step "apt-get install $pkg from the encrypted apt/$run, signed by its key, and the package run"

# reindex with the keys.
"${lajstrom[@]}" reindex --repo "raw/$run" >"$work/reindex.log" 2>&1 || { cat "$work/reindex.log" >&2; fail "lajstrom reindex --repo raw/$run"; }
step "lajstrom reindex --repo raw/$run with the keys"

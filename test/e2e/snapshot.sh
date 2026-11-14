#!/usr/bin/env bash
# End-to-end test of the snapshots of the control plane (#76; docs/operations.md, Snapshots of
# the control plane): the recovery of an index lost without a backup, from the registry and the
# master key, with servers of their own on new indexes.
#
#   test/e2e/snapshot.sh [<base url>]     the base URL is not used
#
#   SN-01 on a new SQLite index, one serve: a repo raw/snap with two files, a user with a reader
#         binding on it, a PAT of hers and a deploy token of the repo, an apt repo with its key;
#         lajstrom admin snapshot create writes a sealed snapshot, which list shows; the layer
#         in the registry holds neither the password nor a token;
#   SN-02 after it: a third file published, the deploy token revoked, a user made;
#   SN-03 the index lost (serve stopped, the file removed): lajstrom migrate on a new file,
#         admin snapshot restore <tag>, reindex: the three files are served; the password, the
#         PAT and the deploy token work, the revoked token too, as after the restore of a
#         backup; the user made after it does not; the apt key's fingerprint is the one before;
#   SN-04 a restore into the index that serve started on is refused, and --force restores;
#   SN-05 a restore with another master key is refused.
# With LAJSTROM_E2E_POSTGRES_DSN, a Postgres database where the scenario may create a schema of
# its own, and psql:
#   SN-06 the snapshot of the SQLite index restored into a new Postgres schema: reindex, serve,
#         and the same checks as SN-03.
# Without them SN-06 is skipped, which is not a failure. The servers use the registry
# $LAJSTROM_E2E_REGISTRY (default localhost:5000), below LAJSTROM_E2E_REGISTRY_PREFIX and with
# the user LAJSTROM_E2E_REGISTRY_USER and the password LAJSTROM_E2E_REGISTRY_PASSWORD when run.sh
# set them (#353); the snapshots go to lajstrom/control-plane there, and the scenario restores
# the one it made, by its tag. The binary is $LAJSTROM_BIN, else bin/lajstrom. It needs curl and
# jq; no secret is printed.
set -euo pipefail
. "$(dirname "$0")/lib.sh"

for tool in curl jq; do
	command -v "$tool" >/dev/null || { echo "snapshot.sh: $tool is required" >&2; exit 2; }
done
bin="${LAJSTROM_BIN:-bin/lajstrom}"
[ -x "$bin" ] || { echo "snapshot.sh: $bin is not built (make build)" >&2; exit 2; }
registry="${LAJSTROM_E2E_REGISTRY:-localhost:5000}"
unset LAJSTROM_CONFIG LAJSTROM_SERVER_LISTEN LAJSTROM_SERVER_EXTERNAL_URL LAJSTROM_SERVER_METRICS_LISTEN \
	LAJSTROM_LOG_LEVEL LAJSTROM_LOG_FORMAT LAJSTROM_DATABASE_DRIVER LAJSTROM_DATABASE_DSN LAJSTROM_BOOTSTRAP_TOKEN_FILE
LAJSTROM_MASTER_KEY="$(od -An -tx1 -N32 /dev/urandom | tr -d ' \n')"
export LAJSTROM_MASTER_KEY

work="$(mktemp -d)"
pid=""
schema=""
cleanup() {
	if [ -n "$pid" ]; then
		kill -TERM "$pid" 2>/dev/null || true
		wait "$pid" 2>/dev/null || true
	fi
	[ -z "$schema" ] || psql -q "$LAJSTROM_E2E_POSTGRES_DSN" -c "DROP SCHEMA $schema CASCADE" >/dev/null 2>&1 || true
	rm -rf "$work"
}
trap cleanup EXIT

port="$(free_port)"
base="http://127.0.0.1:$port"

# config NAME DRIVER DSN writes the configuration $work/NAME.yaml of a server on $port.
config() {
	cat >"$work/$1.yaml" <<EOF
server:
  listen: "127.0.0.1:$port"
  externalURL: "$base"
  insecureHTTP: true
  shutdownTimeout: 5s
  shutdownDelay: 0s # no edge in front to stop sending
  metricsListen: ""
log:
  level: info
  format: text
database:
  driver: $2
  dsn: "$3"
registries:
  - name: local
    url: "http://$registry"
${LAJSTROM_E2E_REGISTRY_PREFIX:+    prefix: $LAJSTROM_E2E_REGISTRY_PREFIX}
${LAJSTROM_E2E_REGISTRY_USER:+    auth:
      username: $LAJSTROM_E2E_REGISTRY_USER
      passwordEnv: LAJSTROM_E2E_REGISTRY_PASSWORD}
reconcile:
  interval: 0s
keys:
  masterKeyEnv: LAJSTROM_MASTER_KEY
EOF
}

# lj NAME ARGS... runs lajstrom with $work/NAME.yaml.
lj() {
	local name="$1"
	shift
	"$bin" "$@" --config "$work/$name.yaml" --log-level warn
}

# start NAME starts serve with $work/NAME.yaml and waits until it is ready.
start() {
	"$bin" serve --config "$work/$1.yaml" >"$work/$1.log" 2>&1 </dev/null &
	pid=$!
	local i
	for i in $(seq 60); do
		curl -fsS -o /dev/null --max-time 1 "$base/-/readyz" 2>/dev/null && return 0
		sleep 0.5
	done
	tail -n 20 "$work/$1.log" >&2
	fail "serve with $1.yaml was not ready within 30 s"
}

# stop stops the server.
stop() {
	kill -TERM "$pid"
	wait "$pid" 2>/dev/null || true
	pid=""
}

# curlrc NAME USER SECRET writes the credentials of a curl config file $work/NAME.curlrc.
curlrc() { (umask 077 && printf 'user = "%s:%s"\n' "$2" "$3" >"$work/$1.curlrc"); }

# checks WHAT: the files, the credentials and the key of SN-01 on the server, as a restore of
# the snapshot leaves them; the user made after the snapshot does not log in.
checks() {
	local f
	for f in one two three; do
		assert_status 200 -K "$work/alice.curlrc" "$base/raw/snap/$f.txt"
		expect "$1: $f.txt" "$f" "$E2E_BODY"
	done
	assert_status 200 -K "$work/pat.curlrc" "$base/raw/snap/one.txt"
	assert_status 200 -K "$work/deploy.curlrc" "$base/raw/snap/two.txt"
	assert_status 401 -K "$work/bob.curlrc" "$base/raw/snap/one.txt"
	expect "$1: the apt key" "$fingerprint" "$(lj "$2" admin key export --repo apt/debs | sha256)"
}

# sha256 prints the hexadecimal sha256 of its input.
sha256() { if command -v sha256sum >/dev/null; then sha256sum; else shasum -a 256; fi | cut -d' ' -f1; }

# SN-01: a control plane, and its snapshot.
config one sqlite "file:$work/one.db"
lj one migrate >/dev/null
lj one admin repo create raw/snap --registry local >/dev/null
lj one admin repo create apt/debs --registry local >/dev/null
password="$(head -c 18 /dev/urandom | base64 | tr -d '+/=\n')"
printf '%s' "$password" | lj one admin user create --name alice --password-stdin >/dev/null
lj one admin binding create --principal user:alice --role writer --mask raw/snap >/dev/null
curlrc alice alice "$password"
start one
for f in one two; do
	assert_status 201 -K "$work/alice.curlrc" -X PUT --data-binary "$f" "$base/raw/snap/$f.txt"
done
pat="$(lj one admin token create --user alice --name laptop | jq -r .secret)"
made="$(lj one admin token create --repo raw/snap --name ci --scopes read)"
deploy="$(jq -r .secret <<<"$made")" deploy_id="$(jq -r .id <<<"$made")"
curlrc pat alice "$pat"
curlrc deploy deploy "$deploy"
assert_status 200 -K "$work/deploy.curlrc" "$base/raw/snap/one.txt"
fingerprint="$(lj one admin key export --repo apt/debs | sha256)"
created="$(lj one admin snapshot create)"
tag="$(jq -r .tag <<<"$created")"
[ "$(jq -r '.sealed, .rows.users, .rows.tokens' <<<"$created" | tr '\n' ' ')" = "true 1 2 " ] || fail "SN-01: the snapshot: $created"
lj one admin snapshot list --json | jq -e --arg t "$tag" 'select(.tag == $t)' >/dev/null || fail "SN-01: list does not show $tag"
if [ -n "${LAJSTROM_E2E_REGISTRY_USER:-}" ]; then
	held="not looked at: the registry needs credentials"
else
	mf="$(curl -fsS -H 'Accept: application/vnd.oci.image.manifest.v1+json' \
		"http://$registry/v2/${LAJSTROM_E2E_REGISTRY_PREFIX:-}lajstrom/control-plane/manifests/$tag")"
	expect "SN-01: the layer's media type" application/vnd.lajstrom.snapshot.control-plane.v1.sealed "$(jq -r '.layers[0].mediaType' <<<"$mf")"
	curl -fsS -o "$work/layer" "http://$registry/v2/${LAJSTROM_E2E_REGISTRY_PREFIX:-}lajstrom/control-plane/blobs/$(jq -r '.layers[0].digest' <<<"$mf")"
	for s in "$password" "$pat" "$deploy" "$LAJSTROM_MASTER_KEY"; do
		if LC_ALL=C grep -a -q -F "$s" "$work/layer"; then fail "SN-01: the layer in the registry holds a secret"; fi
	done
	held="no password, token or master key in it"
fi
step "SN-01: admin snapshot create: lajstrom/control-plane:$tag, sealed, $(jq -c .rows <<<"$created"); listed; its layer: $held"

# SN-02: after the snapshot.
assert_status 201 -K "$work/alice.curlrc" -X PUT --data-binary three "$base/raw/snap/three.txt"
lj one admin token revoke "$deploy_id" >/dev/null
assert_status 401 -K "$work/deploy.curlrc" "$base/raw/snap/one.txt"
printf 'bob-password-1234' | lj one admin user create --name bob --password-stdin >/dev/null
curlrc bob bob bob-password-1234
step "SN-02: after it: three.txt published, the deploy token revoked (401), bob made"

# SN-03: the index lost, restored from the snapshot.
stop
rm -f "$work"/one.db*
config two sqlite "file:$work/two.db"
lj two migrate >/dev/null
restored="$(lj two admin snapshot restore "$tag")"
expect "SN-03: the restored tag" "$tag" "$(jq -r .tag <<<"$restored")"
lj two reindex --report >"$work/reindex.txt"
start two
checks SN-03 two
step "SN-03: migrate on a new file, admin snapshot restore $tag, reindex: three files, alice's password, her PAT and the revoked deploy token work, bob is gone, the apt key is the same"

# SN-04: into the index that serve started on.
stop
if out="$(lj two admin snapshot restore "$tag" 2>&1)"; then fail "SN-04: a restore into a full index succeeded: $out"; fi
case "$out" in *"not empty"*"--force"*) ;; *) fail "SN-04: the refusal: $out" ;; esac
out="${out#lajstrom: }"
lj two admin snapshot restore "$tag" --force >/dev/null
lj two reindex >/dev/null
start two
checks SN-04 two
stop
step "SN-04: a restore into the index serve started on: ${out:0:90}…; --force restores it"

# SN-05: another master key.
config three sqlite "file:$work/three.db"
lj three migrate >/dev/null
if out="$(LAJSTROM_MASTER_KEY="$(od -An -tx1 -N32 /dev/urandom | tr -d ' \n')" lj three admin snapshot restore "$tag" 2>&1)"; then
	fail "SN-05: a restore with another master key succeeded"
fi
case "$out" in *"does not open with the master key"*) ;; *) fail "SN-05: the refusal: $out" ;; esac
step "SN-05: a restore with another master key: ${out#lajstrom: }"

# SN-06: into Postgres.
if [ -z "${LAJSTROM_E2E_POSTGRES_DSN:-}" ] || ! command -v psql >/dev/null; then
	step "SN-06: skipped: it needs LAJSTROM_E2E_POSTGRES_DSN and psql"
	echo "PASS snapshot e2e (SN-01 to SN-05)"
	exit 0
fi
schema="e2e_snapshot_$(od -An -tx1 -N6 /dev/urandom | tr -d ' \n')"
psql -q "$LAJSTROM_E2E_POSTGRES_DSN" -c "CREATE SCHEMA $schema" >/dev/null
case "$LAJSTROM_E2E_POSTGRES_DSN" in *\?*) dsn="$LAJSTROM_E2E_POSTGRES_DSN&search_path=$schema" ;; *) dsn="$LAJSTROM_E2E_POSTGRES_DSN?search_path=$schema" ;; esac
config pg postgres "$dsn"
lj pg migrate >/dev/null
lj pg admin snapshot restore "$tag" >/dev/null
lj pg reindex >/dev/null
start pg
checks SN-06 pg
stop
step "SN-06: the snapshot of the SQLite index restored into the new Postgres schema $schema: the same files, credentials and key"
echo "PASS snapshot e2e (SN-01 to SN-06)"

#!/usr/bin/env bash
# The high-availability scenarios of docs/acceptance.md §4 (#226) against the ha profile of the
# dev stack (make dev-up PROFILES=ha, docs/dev-stack.md): two replicas of Lajstrom, A and B,
# round robin behind nginx with deploy/nginx/lajstrom.conf, on one Postgres and one
# distribution, which has no referrers API.
#
#   test/e2e/ha.sh [<edge url>]     default: http://localhost:8090
#
# A and B are reached past the edge on localhost:8091 and :8092, the backend on localhost:5007
# (LAJSTROM_HA_PORT, LAJSTROM_HA_A_PORT, LAJSTROM_HA_B_PORT and LAJSTROM_HA_DISTRIBUTION_PORT
# move them, as in compose.yaml). docker compose on deploy/compose/compose.yaml, in the project
# of COMPOSE_PROJECT_NAME (lajstrom-dev by default), runs lajstrom admin in A and psql in
# ha-postgres, reads the logs and restarts the replicas. The steps, with the ids they cover:
#
#   - push through A, read through B: a raw PUT and GET, an oras push and pull; a repo created
#     through B counts on A; a bearer token from A is accepted by B (H-12);
#   - a binding and a repo's anonymousRead changed through one replica count on the other at
#     the next request (S-07 and G-02 across replicas);
#   - the client address: the access log's remoteAddr and the audit ip of a request through
#     nginx are the address nginx saw, with nginx as proxy; a forged X-Forwarded-For is
#     believed neither through the edge nor past it (ADR 0011);
#   - H-02, H-03 (20 times), H-04, H-09, H-13;
#   - two cosign signatures of one subject through A and B at once, both listed afterwards (#28);
#   - 200 requests to /v2/token at once from one address: 429 from nginx with Retry-After (#129);
#   - H-10: the requests of a pip install in a loop through the edge while A and then B restart;
#   - H-01: the scenarios of test/e2e/ through the edge, as run.sh runs them; HA_SUITE='raw oras'
#     runs those only, HA_SUITE=none none.
#
# The run's repo, users and files are named after the run; on exit the repo is deleted through
# the API and the users are disabled; the backend keeps the bytes (make dev-down RESET=1). It
# needs curl, jq and docker; oras on PATH, else its image; the images of cosign.sh and pypi.sh.
set -euo pipefail
. "$(dirname "$0")/lib.sh"

edge="${1:-http://localhost:${LAJSTROM_HA_PORT:-8090}}"
edge="${edge%/}"
a="http://localhost:${LAJSTROM_HA_A_PORT:-8091}"
b="http://localhost:${LAJSTROM_HA_B_PORT:-8092}"
backend="http://localhost:${LAJSTROM_HA_DISTRIBUTION_PORT:-5007}"
here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/../.." && pwd)"
compose=(docker compose -f "$root/deploy/compose/compose.yaml")
nginx_ip=172.31.226.10 # ha-nginx: server.trustedProxies of deploy/compose/ha/lajstrom.yaml
for tool in curl jq docker; do
	command -v "$tool" >/dev/null || { echo "ha.sh: $tool is required" >&2; exit 2; }
done
for u in "$edge" "$a" "$b"; do
	curl -fsS -o /dev/null --max-time 5 "$u/-/readyz" 2>/dev/null ||
		{ echo "ha.sh: $u/-/readyz is not 200: is the ha profile up (make dev-up PROFILES=ha)?" >&2; exit 2; }
done

work="$(mktemp -d)"
run="ha-$(date -u +%Y%m%d%H%M%S)-$RANDOM"
repo="$run"
made=()
logs=""
cleanup() {
	[ -z "$logs" ] || kill "$logs" 2>/dev/null || true
	[ -z "${E2E_CURLRC:-}" ] || curl -sS -o /dev/null -K "$E2E_CURLRC" -X DELETE "$edge/api/v1/repos/raw/$repo?force=true" || true
	for u in ${made[@]+"${made[@]}"}; do lajstrom admin user disable "$u" >/dev/null 2>&1 || true; done
	rm -rf "$work"
}
trap cleanup EXIT

lajstrom() { "${compose[@]}" exec -T lajstrom-a /lajstrom "$@"; }
psql() { "${compose[@]}" exec -T ha-postgres psql -U lajstrom -d lajstrom -Atc "$1"; }
sha256() { if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi; }
json() { curl -fsS -K "$E2E_CURLRC" -H 'Content-Type: application/json' "$@"; }
# replica_log prints the lines of both replicas' logs that match the extended regexp $1.
replica_log() { "${compose[@]}" logs --no-log-prefix lajstrom-a lajstrom-b 2>/dev/null | grep -E "$1" || true; }
# version_digest PATH VERSION prints the digest the index has for VERSION of PATH in the repo.
version_digest() {
	psql "SELECT v.digest FROM versions v JOIN packages p ON p.id = v.package_id JOIN repos r ON r.id = p.repo_id
		WHERE r.format = 'raw' AND r.name = '$repo' AND p.name = '$1' AND v.version = '$2'"
}

# The admin of the run, and the repo, created through B.
pw="$(head -c 24 /dev/urandom | base64 | tr -d '+/=\n')"
printf '%s' "$pw" | lajstrom admin user create --name "$run-admin" --admin --password-stdin >/dev/null
made+=("$run-admin")
LAJSTROM_E2E_USER="$run-admin" LAJSTROM_E2E_PASSWORD="$pw"
export LAJSTROM_E2E_USER LAJSTROM_E2E_PASSWORD
e2e_credentials "$work"
assert_status 201 -K "$E2E_CURLRC" -H 'Content-Type: application/json' -d "{\"format\":\"raw\",\"name\":\"$repo\",\"registry\":\"local\"}" "$b/api/v1/repos"
printf 'through A %s\n' "$run" >"$work/one.txt"
assert_status 201 -K "$E2E_CURLRC" -T "$work/one.txt" "$a/raw/$repo/one.txt"
assert_status 200 -K "$E2E_CURLRC" -o "$work/one.out" "$b/raw/$repo/one.txt"
assert_eq "$(sha256 "$work/one.txt")" "$(sha256 "$work/one.out")" "the file PUT through A, read through B"
step "raw/$repo created through B; one.txt PUT through A, the same bytes GET through B"

# A bearer token from A, used on B (H-12), in a curl configuration: no token on a command line.
service="$(curl -sS -o /dev/null -D - "$a/v2/" | tr -d '\r' | sed -n 's/^[Ww][Ww][Ww]-[Aa]uthenticate: .*service="\([^"]*\)".*/\1/p')"
bearer() { # bearer SCOPE: a curl configuration in $work/bearer with a token from A for SCOPE
	local t
	t="$(curl -fsS -K "$E2E_CURLRC" "$a/v2/token?service=$service&scope=$1" | jq -r '.token')"
	(umask 077 && printf 'header = "Authorization: Bearer %s"\n' "$t" >"$work/bearer")
}
bearer "repository:raw/$repo/one.txt:pull"
assert_status 200 -K "$work/bearer" "$b/v2/raw/$repo/one.txt/tags/list"
step "H-12: a token of $a/v2/token (service $service) lists the tags of raw/$repo/one.txt on B: $(jq -c .tags <<<"$E2E_BODY")"

# oras: login to both, push through A, pull through B. oras_in DIR ARGS... runs in $work/DIR.
if command -v oras >/dev/null; then
	oras_in() { (cd "$work/$1" && shift && command oras "$@" --registry-config "$work/oras.json" --plain-http); }
else
	oras_in() {
		local d="$1"
		shift
		docker run --rm -i --network host -u "$(id -u):$(id -g)" -v "$work:/w" -w "/w/$d" ghcr.io/oras-project/oras:v1.3.6 "$@" --registry-config /w/oras.json --plain-http
	}
fi
for r in "${a#http://}" "${b#http://}"; do
	printf '%s\n' "$pw" | oras_in . login -u "$run-admin" --password-stdin "$r" >"$work/oras.log" 2>&1 || { cat "$work/oras.log" >&2; fail "oras login $r"; }
done
# oras_push DIR REGISTRY FILE CREATED: oras push of DIR/FILE with the raw profile and CREATED as
# the created time, so that the same file makes another manifest with another time; the log is
# DIR.log. The version, and the tag, of a raw file is the first 16 hex digits of its sha256.
oras_push() {
	local version
	version="$(sha256 "$work/$1/$3" | cut -c1-16)"
	oras_in "$1" push --artifact-type application/vnd.lajstrom.raw.v1 \
		--annotation lajstrom.schema=1 --annotation lajstrom.format=raw --annotation "lajstrom.repo=$repo" \
		--annotation "lajstrom.name=$3" --annotation "lajstrom.version=$version" --annotation "lajstrom.raw.path=$3" \
		--annotation "org.opencontainers.image.created=$4" "$2/raw/$repo/$3:$version" "$3:application/vnd.lajstrom.raw.file.v1" >"$work/$1.log" 2>&1
}
mkdir -p "$work/signed" "$work/pulled"
printf 'signed %s\n' "$run" >"$work/signed/signed.txt"
oras_push signed "${a#http://}" signed.txt 2026-11-09T00:00:00Z || { cat "$work/signed.log" >&2; fail "oras push through A"; }
subject="$(awk '$1 == "Digest:" { print $2 }' "$work/signed.log")"
oras_in . pull -o pulled "${b#http://}/raw/$repo/signed.txt:$(sha256 "$work/signed/signed.txt" | cut -c1-16)" >"$work/oras.log" 2>&1 || { cat "$work/oras.log" >&2; fail "oras pull through B"; }
assert_eq "$(sha256 "$work/signed/signed.txt")" "$(sha256 "$work/pulled/signed.txt")" "oras pull through B of the push through A"
step "oras push through A ($subject), oras pull through B: the same bytes"

# A binding and the repo's anonymousRead, changed through one replica, count on the other.
user="$run-user" upw="$(head -c 24 /dev/urandom | base64 | tr -d '+/=\n')"
(umask 077 && printf '{"name": "%s", "password": "%s"}' "$user" "$upw" >"$work/user.json")
json -o /dev/null -d @"$work/user.json" "$a/api/v1/users"
made+=("$user")
(umask 077 && printf 'user = "%s:%s"\n' "$user" "$upw" >"$work/user.curlrc")
assert_status 404 -K "$work/user.curlrc" -o /dev/null "$b/raw/$repo/one.txt"
binding="$(json -d "{\"principal\":\"user:$user\",\"role\":\"reader\",\"repoMask\":\"raw/$repo\"}" "$a/api/v1/bindings" | jq -r .id)"
assert_status 200 -K "$work/user.curlrc" -o /dev/null "$b/raw/$repo/one.txt"
assert_status 204 -K "$E2E_CURLRC" -X DELETE "$b/api/v1/bindings/$binding"
assert_status 404 -K "$work/user.curlrc" -o /dev/null "$a/raw/$repo/one.txt"
step "a user's GET on B: 404, 200 once A made a reader binding, 404 on A once B deleted it"
assert_status 401 -o /dev/null "$b/raw/$repo/one.txt"
json -o /dev/null -X PATCH -d '{"anonymousRead": true}' "$a/api/v1/repos/raw/$repo"
assert_status 200 -o /dev/null "$b/raw/$repo/one.txt"
json -o /dev/null -X PATCH -d '{"anonymousRead": false}' "$b/api/v1/repos/raw/$repo"
assert_status 401 -o /dev/null "$a/raw/$repo/one.txt"
step "an anonymous GET: 401 on B, 200 once A turned anonymousRead on, 401 on A once B turned it off"

# The client address (ADR 0011): a change through the edge and one past it, each with a forged
# X-Forwarded-For; the access log line and the audit row of each.
for to in edge a; do
	eval "u=\$$to"
	[ "$to" = edge ] && on=true || on=false
	assert_status 200 -K "$E2E_CURLRC" -o /dev/null -A "$run" -H "X-Request-Id: $run-$to" -H 'X-Forwarded-For: 203.0.113.9' \
		-H 'Content-Type: application/json' -X PATCH -d "{\"anonymousRead\": $on}" "$u/api/v1/repos/raw/$repo"
done
audit_ip() { json "$edge/api/v1/audit?repo=raw/$repo&action=repo.update&limit=10" | jq -r --arg r "$1" '.events[] | select(.details.requestId == $r) | .ip'; }
seen="$("${compose[@]}" logs --no-log-prefix ha-nginx 2>/dev/null | grep -F "\"$run\"" | awk '{ print $1 }')"
line="$(replica_log "requestId=$run-edge ")"
assert_eq "remoteAddr=$seen" "$(grep -oE 'remoteAddr=[^ ]+' <<<"$line")" "the access log's remoteAddr through the edge, the address nginx saw"
grep -qE "proxy=$nginx_ip:[0-9]+" <<<"$line" || fail "no proxy=$nginx_ip in: $line"
assert_eq "$seen" "$(audit_ip "$run-edge")" "the audit ip of the change through the edge"
past="$(replica_log "requestId=$run-a ")"
peer="$(grep -oE 'remoteAddr=[0-9a-f.:]+:[0-9]+ ' <<<"$past" | sed 's/remoteAddr=//; s/:[0-9]* $//')"
[ -n "$peer" ] && ! grep -q 'proxy=' <<<"$past" || fail "past the edge, the access log does not name the peer: $past"
assert_eq "$peer" "$(audit_ip "$run-a")" "the audit ip of the change past the edge"
step "client address: through nginx remoteAddr=$seen proxy=$nginx_ip and audit ip $seen, past it the peer $peer; the forged X-Forwarded-For nowhere"

# H-02: the same 100 MiB at once through A and B.
head -c 104857600 /dev/urandom >"$work/big"
for r in a b; do
	eval "u=\$$r"
	curl -sS -K "$E2E_CURLRC" -T "$work/big" -o "$work/h02-$r.json" -w '%{http_code}' "$u/raw/$repo/big" >"$work/h02-$r.code" &
done
wait
assert_eq "201 201" "$(cat "$work/h02-a.code") $(cat "$work/h02-b.code")" "H-02: the statuses of A and B"
assert_eq "$(jq -r .digest "$work/h02-a.json")" "$(jq -r .digest "$work/h02-b.json")" "H-02: the digests of A and B"
assert_eq 1 "$(psql "SELECT count(*) FROM versions v JOIN packages p ON p.id = v.package_id JOIN repos r ON r.id = p.repo_id WHERE r.name = '$repo' AND p.name = 'big'")" "H-02: versions of big in the index"
assert_eq 1 "$(curl -fsS "$backend/v2/raw/$repo/big/tags/list" | jq '.tags | length')" "H-02: tags of big in the backend"
step "H-02: 100 MiB PUT through A and B at once: 201 and 201, $(jq -r .digest "$work/h02-a.json"), one version, one tag"

# H-03: one tag, two manifests (the same file, another created time), through A and B at once,
# 20 times.
mkdir -p "$work/ra" "$work/rb"
for i in $(seq 1 20); do
	printf 'race %s %s\n' "$run" "$i" | tee "$work/ra/race.txt" >"$work/rb/race.txt"
	tag="$(sha256 "$work/ra/race.txt" | cut -c1-16)"
	ra=0 rb=0
	oras_push ra "${a#http://}" race.txt 2026-11-09T00:00:01Z & pa=$!
	oras_push rb "${b#http://}" race.txt 2026-11-09T00:00:02Z & pb=$!
	wait "$pa" || ra=$?
	wait "$pb" || rb=$?
	if [ "$ra" = 0 ] && [ "$rb" != 0 ]; then win=ra lose=rb; elif [ "$rb" = 0 ] && [ "$ra" != 0 ]; then win=rb lose=ra; else
		cat "$work/ra.log" "$work/rb.log" >&2
		fail "H-03 round $i: oras exits $ra (A) and $rb (B), want one 0"
	fi
	won="$(awk '$1 == "Digest:" { print $2 }' "$work/$win.log")"
	grep -q "tag invalid: .* is taken by $won" "$work/$lose.log" || { cat "$work/$lose.log" >&2; fail "H-03 round $i: the loser is not TAG_INVALID naming $won"; }
	puts="$(replica_log "method=PUT path=/v2/raw/$repo/race.txt/manifests/$tag " | grep -oE 'status=[0-9]+' | sort | tr '\n' ' ')"
	assert_eq "status=201 status=409 " "$puts" "H-03 round $i: the manifest PUTs in the access logs"
	held="$(curl -fsS -o /dev/null -D - -H 'Accept: application/vnd.oci.image.manifest.v1+json' "$backend/v2/raw/$repo/race.txt/manifests/$tag" | tr -d '\r' | awk -F': ' 'tolower($1) == "docker-content-digest" { print $2 }')"
	assert_eq "$won" "$held" "H-03 round $i: the backend's digest of $tag"
	assert_eq "$won" "$(version_digest race.txt "$tag")" "H-03 round $i: the index's digest of $tag"
done
step "H-03: 20 tags, each pushed through A and B at once: one 201 and one 409 TAG_INVALID, the winner in the backend and the index"

# H-04: a blob upload begun on A, its chunks to B, finished on A.
bearer "repository:raw/$repo/chunked:push,pull"
head -c 3000000 /dev/urandom >"$work/chunked"
head -c 1500000 "$work/chunked" >"$work/c1"
tail -c 1500000 "$work/chunked" >"$work/c2"
loc() { tr -d '\r' <"$1" | awk -F': ' 'tolower($1) == "location" { print $2 }' | sed 's|^http://[^/]*||'; }
assert_status 202 -K "$work/bearer" -X POST -D "$work/h" -o /dev/null "$a/v2/raw/$repo/chunked/blobs/uploads/"
assert_status 202 -K "$work/bearer" -X PATCH -D "$work/h" -o /dev/null -H 'Content-Type: application/octet-stream' \
	-H 'Content-Range: 0-1499999' --data-binary @"$work/c1" "$b$(loc "$work/h")"
assert_status 202 -K "$work/bearer" -X PATCH -D "$work/h" -o /dev/null -H 'Content-Type: application/octet-stream' \
	-H 'Content-Range: 1500000-2999999' --data-binary @"$work/c2" "$b$(loc "$work/h")"
next="$(loc "$work/h")"
case "$next" in *\?*) next="$next&" ;; *) next="$next?" ;; esac
assert_status 201 -K "$work/bearer" -X PUT -o /dev/null "$a${next}digest=sha256:$(sha256 "$work/chunked")"
assert_status 200 -K "$work/bearer" -I "$b/v2/raw/$repo/chunked/blobs/sha256:$(sha256 "$work/chunked")"
step "H-04: POST on A, two PATCH on B, PUT on A: 201; the blob sha256:$(sha256 "$work/chunked" | cut -c1-12)… on B"

# H-09: reindex on A and B at once: one runs, the other is 409. A reindex that ends before the
# other starts lets both run: tried up to five times.
for try in 1 2 3 4 5; do
	for r in a b; do
		eval "u=\$$r"
		curl -sS -K "$E2E_CURLRC" -X POST -o "$work/h09-$r.json" -w '%{http_code}' "$u/api/v1/repos/raw/$repo/reindex" >"$work/h09-$r.code" &
	done
	wait
	codes="$(printf '%s\n%s\n' "$(cat "$work/h09-a.code")" "$(cat "$work/h09-b.code")" | sort | tr '\n' ' ')"
	[ "$codes" != "202 202 " ] || continue
	break
done
assert_eq "202 409 " "$codes" "H-09: the statuses of reindex on A and B"
assert_eq CONFLICT "$(jq -r '.error.code' "$work"/h09-*.json | grep -v null)" "H-09: the code of the 409"
step "H-09: reindex on A and B at once: 202 and 409 CONFLICT ($(jq -r '.error.message' "$work"/h09-*.json | grep -v null)), try $try"

# H-13: a wheel of the run, uploaded through A; then its simple page asked on A and B at once.
project="$run" dist="$(tr - _ <<<"$run")"
mkdir -p "$work/wheel"
wheel_py='import base64, hashlib, sys, zipfile
name, dist = sys.argv[1], sys.argv[2]
info = dist + "-1.0.dist-info/"
files = {dist + "/__init__.py": b"", info + "METADATA": ("Metadata-Version: 2.1\nName: %s\nVersion: 1.0\n" % name).encode(),
         info + "WHEEL": b"Wheel-Version: 1.0\nGenerator: ha.sh\nRoot-Is-Purelib: true\nTag: py3-none-any\n"}
rec = ["%s,sha256=%s,%d" % (p, base64.urlsafe_b64encode(hashlib.sha256(b).digest()).rstrip(b"=").decode(), len(b)) for p, b in files.items()]
with zipfile.ZipFile(dist + "-1.0-py3-none-any.whl", "w") as z:
    for p, b in files.items():
        z.writestr(p, b)
    z.writestr(info + "RECORD", "\n".join(rec + [info + "RECORD,,"]) + "\n")'
if command -v python3 >/dev/null; then (cd "$work/wheel" && python3 -c "$wheel_py" "$project" "$dist"); else
	docker run --rm -u "$(id -u):$(id -g)" -v "$work/wheel:/w" -w /w python:3.13 python -c "$wheel_py" "$project" "$dist"
fi
wheel="$dist-1.0-py3-none-any.whl"
assert_status 200 -K "$E2E_CURLRC" -o /dev/null -F ':action=file_upload' -F "content=@$work/wheel/$wheel" "$a/pypi/e2e/"
for r in a b; do
	eval "u=\$$r"
	curl -fsS -K "$E2E_CURLRC" -D "$work/h13-$r.h" -o "$work/h13-$r.html" "$u/pypi/e2e/simple/$project/" &
done
wait
assert_eq "$(sha256 "$work/h13-a.html")" "$(sha256 "$work/h13-b.html")" "H-13: the simple page of A and of B"
etag() { tr -d '\r' <"$1" | awk -F': ' 'tolower($1) == "etag" { print $2 }'; }
assert_eq "$(etag "$work/h13-a.h")" "$(etag "$work/h13-b.h")" "H-13: the ETag of A and of B"
assert_eq 1 "$(psql "SELECT count(*) FROM index_cache c JOIN repos r ON r.id = c.repo_id WHERE r.format = 'pypi' AND r.name = 'e2e' AND c.scope = 'simple/$project' AND c.kind = 'simple.html'")" "H-13: index_cache rows"
step "H-13: $wheel through A; pypi/e2e/simple/$project/ on A and B at once: the same bytes and ETag $(etag "$work/h13-a.h"), one index_cache row"

# Two cosign signatures of the oras push above, through A and B at once (#28): distribution has
# no referrers API, so each rewrites the sha256-<hex> index of the subject; both must be listed.
mkdir -p "$work/docker"
(umask 077 && printf '%s:%s' "$run-admin" "$pw" | base64 | tr -d '\n' | jq -Rn --arg a "${a#http://}" --arg b "${b#http://}" \
	'input as $c | {auths: {($a): {auth: $c}, ($b): {auth: $c}}}' >"$work/docker/config.json")
cosign() {
	docker run --rm -i --network host -u "$(id -u):$(id -g)" -v "$work:/w" -w /w -e HOME=/w -e DOCKER_CONFIG=/w/docker \
		-e COSIGN_PASSWORD= -e HTTPS_PROXY=http://127.0.0.1:9 -e NO_PROXY=localhost ghcr.io/sigstore/cosign/cosign:v3.1.4 "$@"
}
for k in ka kb; do cosign generate-key-pair --output-key-prefix "$k" >/dev/null 2>&1 || fail "cosign generate-key-pair $k"; done
for r in a b; do
	eval "u=\$$r"
	cosign sign --key "k$r.key" --tlog-upload=false --use-signing-config=false -y --allow-http-registry \
		"${u#http://}/raw/$repo/signed.txt@$subject" >"$work/sign-$r.log" 2>&1 &
	eval "sign_$r=\$!"
done
wait "$sign_a" || { cat "$work/sign-a.log" >&2; fail "cosign sign through A"; }
wait "$sign_b" || { cat "$work/sign-b.log" >&2; fail "cosign sign through B"; }
oras_in . discover --format json "${b#http://}/raw/$repo/signed.txt@$subject" >"$work/discover.json" 2>"$work/oras.log" ||
	{ cat "$work/oras.log" >&2; fail "oras discover"; }
assert_eq 2 "$(jq '[.referrers[] | select(.artifactType == "application/vnd.dev.sigstore.bundle.v0.3+json")] | length' "$work/discover.json")" \
	"#28: the signatures oras discover lists through B"
for r in a b; do
	cosign verify --key "k$r.pub" --insecure-ignore-tlog=true --allow-http-registry "${b#http://}/raw/$repo/signed.txt@$subject" \
		>/dev/null 2>"$work/verify.log" || { cat "$work/verify.log" >&2; fail "cosign verify --key k$r.pub"; }
done
curl -fsS "$backend/v2/raw/$repo/signed.txt/tags/list" | jq -e --arg t "sha256-${subject#sha256:}" '.tags | index($t)' >/dev/null ||
	fail "#28: the backend has no tag sha256-${subject#sha256:}"
step "#28: cosign sign through A and B at once, two keys: oras discover lists both, cosign verify passes with each; the backend's tag sha256-${subject:7:12}…"

# #129: 200 requests to /v2/token at once from one address. nginx lets 20 a second through, with
# a burst of 40, and answers the rest 429 with Retry-After; the replicas answer none of them 429.
# One curl sends them all (--parallel): 200 processes would start too slowly to be a burst.
for _ in $(seq 1 200); do printf 'url = "%s"\noutput = "/dev/null"\n' "$edge/v2/token?service=$service"; done >"$work/token.curlrc"
curl -s -Z --parallel-max 200 -K "$work/token.curlrc" -H "X-Request-Id: $run-token" -w '%{http_code} %header{retry-after}\n' >"$work/token.codes"
limited="$(grep -c '^429 1$' "$work/token.codes" || true)"
passed="$(grep -c '^200 ' "$work/token.codes" || true)"
[ "$limited" -ge 100 ] && [ $((limited + passed)) = 200 ] || fail "#129: 200 token requests: $(sort "$work/token.codes" | uniq -c | tr '\n' ' ')"
assert_eq "$passed" "$(replica_log "requestId=$run-token " | grep -c 'status=200')" "#129: token requests the replicas answered 200"
sleep 2
assert_status 200 -o /dev/null "$edge/v2/token?service=$service"
step "#129: 200 token requests at once: $passed reached the replicas, $limited answered 429 with Retry-After: 1 by nginx; 2 s later 200"

# H-10: the requests of a pip install, the simple page and the wheel, in a loop through the edge
# while A restarts, and B once A is ready again: none may fail.
loop() {
	while [ ! -f "$work/h10.stop" ]; do
		curl -s -o /dev/null -o /dev/null -K "$E2E_CURLRC" -w '%{http_code} %{time_total}\n' "$edge/pypi/e2e/simple/$project/" \
			"$edge/pypi/e2e/files/$project/1.0/$wheel"
	done >"$work/h10.codes"
}
loop &
looper=$!
sleep 1
for r in a b; do
	"${compose[@]}" restart "lajstrom-$r" >/dev/null 2>&1 || fail "docker compose restart lajstrom-$r"
	eval "u=\$$r"
	until curl -fsS -o /dev/null --max-time 1 "$u/-/readyz" 2>/dev/null; do sleep 0.2; done
	sleep 1
done
touch "$work/h10.stop"
wait "$looper"
assert_eq "" "$(grep -v '^200 ' "$work/h10.codes" | cut -d' ' -f1 | sort | uniq -c | tr '\n' ' ')" "H-10: the requests that were not 200, of $(wc -l <"$work/h10.codes" | tr -d ' ')"
step "H-10: $(wc -l <"$work/h10.codes" | tr -d ' ') requests through the edge while A and then B restarted: all 200, the slowest in $(sort -k2 -n "$work/h10.codes" | tail -n 1 | cut -d' ' -f2) s"

# H-01: the scenarios of test/e2e/ through the edge, as run.sh runs them against one server, with
# lajstrom admin in replica A and the logs of both replicas as the server log. auth.sh goes to A
# past the edge: the throttle of failed logins counts per replica (docs/auth.md), so through the
# round robin the 11th wrong password of X-04 is only the 6th that one replica saw.
suite="${HA_SUITE-raw oras auth rbac oci-token cosign tokens apt pypi maven}"
[ "$suite" != none ] || suite=""
mkdir -p "$work/bin"
cat >"$work/bin/lajstrom" <<EOF
#!/bin/sh
# lajstrom of the scenarios: in replica A, with LAJSTROM_ADMIN_PASSWORD when it is set; admin deb
# metadata reads a file of this machine, so it runs here.
case "\$1 \$2 \$3" in "admin deb metadata")
	[ ! -x '$root/bin/lajstrom' ] || exec '$root/bin/lajstrom' "\$@"
	cd '$root' && exec go run ./cmd/lajstrom "\$@" ;;
esac
exec docker compose -f '$root/deploy/compose/compose.yaml' exec -T \${LAJSTROM_ADMIN_PASSWORD+-e LAJSTROM_ADMIN_PASSWORD} lajstrom-a /lajstrom "\$@"
EOF
chmod +x "$work/bin/lajstrom"
since="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
"${compose[@]}" logs -f --no-log-prefix --since "$since" lajstrom-a lajstrom-b >"$work/server.log" 2>&1 &
logs=$!
unset LAJSTROM_E2E_FORWARDED LAJSTROM_CONFIG
LAJSTROM_MASTER_KEY="$("${compose[@]}" --profile ha config --format json | jq -r '.services["lajstrom-a"].environment.LAJSTROM_MASTER_KEY')"
export LAJSTROM_MASTER_KEY
failed=""
for n in $suite; do
	t0=$SECONDS
	to="$edge" how="through the edge"
	[ "$n" != auth ] || to="$a" how="on A, past the edge"
	if LAJSTROM_BIN="$work/bin/lajstrom" LAJSTROM_SERVER_LOG="$work/server.log" LAJSTROM_E2E_REGISTRY="${backend#http://}" \
		"$here/$n.sh" "$to" >"$work/$n.log" 2>&1 </dev/null; then
		step "H-01: $n.sh $how: $(grep '^PASS' "$work/$n.log" || echo "exit 0") ($((SECONDS - t0)) s)"
	else
		failed="$failed $n.sh"
		printf 'FAIL H-01: %s.sh %s, after %d s:\n' "$n" "$how" $((SECONDS - t0))
		grep -E '^(FAIL |  at |  in )' "$work/$n.log" || tail -n 20 "$work/$n.log"
	fi
done
[ -z "$failed" ] || fail "H-01:$failed"
if [ -n "$suite" ]; then
	for r in a b; do
		served="$("${compose[@]}" logs --no-log-prefix --since "$since" "lajstrom-$r" 2>/dev/null | grep -c 'msg=request' || true)"
		[ "$served" -gt 0 ] || fail "H-01: replica $r served no request of the scenarios"
		eval "served_$r=$served"
	done
	step "H-01: the replicas served $served_a and $served_b requests of the scenarios"
fi
echo "PASS ha e2e against $edge, A $a, B $b"

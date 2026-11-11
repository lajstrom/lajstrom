#!/usr/bin/env bash
# End-to-end test of a pypi repo with real clients against a running Lajstrom (#32, #33;
# docs/quickstart/pypi.md): in python:3.13, a project built with python -m build (a wheel and an
# sdist), uploaded with twine; the same files again (200: twine succeeds); other bytes under the
# same file name (409, which twine reports); pip install from the simple index in a clean venv
# and the module imported from site-packages; uv pip install; uv publish of the next version and
# uv publish --check-url of the same files, which skips them; pip install --upgrade gets the
# next version. The index reads of pip go through the core metadata of PEP 658 (.metadata
# before the file), which the script checks in the server's access log when it has it.
#
#   test/e2e/pypi.sh [<base url> [<repo>]]     default: http://localhost:8080 e2e
#
# The pypi repo is created with lajstrom admin repo create --registry local when it does not
# exist. lajstrom admin runs straight against the index database of the configuration the server
# runs with: $LAJSTROM_CONFIG, else deploy/compose/lajstrom.example.yaml, with the same
# LAJSTROM_DATABASE_* overrides. The binary is $LAJSTROM_BIN, else bin/lajstrom, else go run
# ./cmd/lajstrom. $LAJSTROM_SERVER_LOG, the server's log file, adds the check of PEP 658. Each
# run uses a project and a user of its own, with the writer role on the repo (lajstrom admin
# binding create), and disables the user at the end; the distributions
# stay (pypi files cannot be deleted yet). The password goes to lajstrom on standard input and
# into the container in a file. It needs curl, jq and docker (python:3.13, or $PYPI_E2E_IMAGE,
# with --network host, which reaches the base URL on Linux and under OrbStack), prints one line
# per step and exits non-zero at the first failure.
set -euo pipefail
umask 077

base="${1:-http://localhost:8080}"
base="${base%/}"
repo="${2:-e2e}"
for tool in curl jq docker; do
	command -v "$tool" >/dev/null || { echo "pypi.sh: $tool is required" >&2; exit 2; }
done
export LAJSTROM_CONFIG="${LAJSTROM_CONFIG:-deploy/compose/lajstrom.example.yaml}"
if [ -n "${LAJSTROM_BIN:-}" ]; then
	lajstrom=("$LAJSTROM_BIN")
elif [ -x bin/lajstrom ]; then
	lajstrom=(bin/lajstrom)
else
	lajstrom=(go run ./cmd/lajstrom)
fi
image="${PYPI_E2E_IMAGE:-python:3.13}"

work="$(mktemp -d)"
run="$(date -u +%Y%m%d%H%M%S)$RANDOM"
user="e2e-pypi-$run"
project="E2e.Pypi_$run" # a spelling to normalize: e2e-pypi-<run>
normalized="e2e-pypi-$run"
module="e2e_pypi_$run"
cleanup() {
	"${lajstrom[@]}" admin user disable "$user" >/dev/null 2>&1 || true
	rm -rf "$work"
}
trap cleanup EXIT

step() { printf '%s\n' "$*"; }
fail() { printf 'FAIL %s\n' "$*" >&2; exit 1; }

step "repo pypi/$repo"
"${lajstrom[@]}" admin repo list --format pypi --json | jq -e --arg r "$repo" '.repos[] | select(.name == $r)' >/dev/null ||
	"${lajstrom[@]}" admin repo create "pypi/$repo" --registry local >/dev/null
password="$(head -c 24 /dev/urandom | base64 | tr -d '/+=')"
printf '%s' "$password" | "${lajstrom[@]}" admin user create --name "$user" --password-stdin >/dev/null
"${lajstrom[@]}" admin binding create --principal "user:$user" --role writer --mask "pypi/$repo" >/dev/null
printf '%s:%s\n' "$user" "$password" >"$work/credentials"

# The client side, in python:3.13: every step prints a line, the first failure exits non-zero.
cat >"$work/client.sh" <<'CLIENT'
set -euo pipefail
IFS=: read -r user password </w/credentials
url="$BASE/pypi/$REPO"
host="${BASE#*://}"
scheme="${BASE%%://*}"
index="$scheme://$user:$password@$host/pypi/$REPO/simple/"
export TWINE_USERNAME="$user" TWINE_PASSWORD="$password"
pip install -q --root-user-action=ignore build twine uv >/dev/null 2>&1
mkdir -p /p/"$MODULE" && cd /p
printf '[build-system]\nrequires = ["setuptools>=68"]\nbuild-backend = "setuptools.build_meta"\n[project]\nname = "%s"\nversion = "1.0"\nrequires-python = ">=3.9"\n' "$PROJECT" >pyproject.toml
echo 'def hello(): return "v1"' >"$MODULE/__init__.py"
python -m build -q . >/dev/null 2>&1
echo "twine upload of $(ls dist | tr '\n' ' ')"
twine upload --non-interactive --disable-progress-bar --repository-url "$url/" dist/* >/dev/null
echo "twine upload of the same files again: 200"
twine upload --non-interactive --disable-progress-bar --repository-url "$url/" dist/* >/dev/null
echo "other bytes under the same file name: 409"
echo 'def hello(): return "other"' >"$MODULE/__init__.py"
python -m build -q --wheel --outdir /other . >/dev/null 2>&1 # outside the project: setuptools would take other/ for a package
if out="$(twine upload --non-interactive --disable-progress-bar --repository-url "$url/" /other/*.whl 2>&1)"; then
	echo "FAIL: other bytes under a file name that exists were accepted" >&2; exit 1
fi
grep -q "409 Conflict" <<<"$out" || { echo "FAIL: twine said: $out" >&2; exit 1; }
cd /
echo "pip install from the simple index"
python -m venv /v && /v/bin/pip install -q --index-url "$index" "$PROJECT" >/dev/null
got="$(/v/bin/python -c "import $MODULE; print($MODULE.hello(), $MODULE.__file__)")"
case "$got" in "v1 /v/lib/"*) ;; *) echo "FAIL: imported $got" >&2; exit 1 ;; esac
echo "uv pip install"
uv venv -q /u && VIRTUAL_ENV=/u uv pip install -q --index-url "$index" "$NORMALIZED" >/dev/null
[ "$(/u/bin/python -c "import $MODULE; print($MODULE.hello())")" = v1 ] || { echo "FAIL: uv's install" >&2; exit 1; }
echo "uv publish of 1.1, then --check-url skips the same files"
cd /p && echo 'def hello(): return "v1.1"' >"$MODULE/__init__.py" && sed -i 's/version = "1.0"/version = "1.1"/' pyproject.toml
rm -rf dist && python -m build -q . >/dev/null 2>&1
uv publish -q --publish-url "$url/" -u "$user" -p "$password" dist/* >/dev/null
out="$(uv publish --publish-url "$url/" --check-url "$index" -u "$user" -p "$password" dist/* 2>&1)"
[ "$(grep -c 'already exists, skipping' <<<"$out")" = 2 ] || { echo "FAIL: uv publish --check-url said: $out" >&2; exit 1; }
cd /
echo "pip install --upgrade gets 1.1"
/v/bin/pip install -q --upgrade --index-url "$index" "$PROJECT" >/dev/null
[ "$(/v/bin/python -c "import $MODULE; print($MODULE.hello())")" = v1.1 ] || { echo "FAIL: the upgrade" >&2; exit 1; }
CLIENT

step "the clients in $image"
docker run --rm --network host -e BASE="$base" -e REPO="$repo" -e PROJECT="$project" -e NORMALIZED="$normalized" -e MODULE="$module" \
	-v "$work:/w:ro" "$image" bash /w/client.sh || fail "the clients"

step "the page of $normalized lists four files with their core metadata"
page="$(curl -fsS -u "$user:$password" -H 'Accept: application/vnd.pypi.simple.v1+json' "$base/pypi/$repo/simple/$normalized/")"
[ "$(jq '.files | length' <<<"$page")" = 4 ] || fail "files: $(jq -c '[.files[].filename]' <<<"$page")"
[ "$(jq '[.files[] | select(.["core-metadata"] | type == "object")] | length' <<<"$page")" = 4 ] || fail "core-metadata missing"
[ "$(jq -c .versions <<<"$page")" = '["1.0","1.1"]' ] || fail "versions: $(jq -c .versions <<<"$page")"

if [ -n "${LAJSTROM_SERVER_LOG:-}" ] && [ -f "$LAJSTROM_SERVER_LOG" ]; then
	step "pip read the core metadata (PEP 658) before the wheel"
	grep -q "path=/pypi/$repo/files/$normalized/1.0/[^ ]*\.whl\.metadata status=200" "$LAJSTROM_SERVER_LOG" ||
		grep -q "\"path\":\"/pypi/$repo/files/$normalized/1.0/[^\"]*\\.whl\\.metadata\",\"status\":200" "$LAJSTROM_SERVER_LOG" ||
		fail "no .metadata request in $LAJSTROM_SERVER_LOG"
fi
step "ok"

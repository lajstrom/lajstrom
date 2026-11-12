#!/usr/bin/env bash
# End-to-end test of a pypi repo with real clients against a running Lajstrom (#32, #33;
# docs/quickstart/pypi.md): in python:3.13, a project built with python -m build (a wheel and an
# sdist), uploaded with twine; the same files again (200: twine succeeds); other bytes under the
# same file name (409, which twine reports); poetry publish -r internal of the same files (200);
# pip install from the simple index in a clean venv and the module imported from site-packages;
# uv pip install; the client configurations of the quick start (#580), each in a home of its own
# and naming the index without credentials: pip with pip.conf and the credentials in ~/.netrc,
# uv add with [[tool.uv.index]] in pyproject.toml and UV_INDEX_INTERNAL_USERNAME and _PASSWORD,
# uv pip install with [[index]] in uv.toml, poetry add in a project after poetry source add and
# poetry config http-basic.internal; uv publish of the next version and uv publish --check-url
# of the same files, which skips them; pip install --upgrade gets the next version; the page
# asked for as the meta version latest of PEP 691, JSON and HTML, is version 1's. The index
# reads of pip go through the core metadata of PEP 658 (.metadata before the file), which the
# script checks in the server's access log when it has it. Last, a wheel of the next version with
# the build tag 1 goes beside its wheel (#726), and pip install --force-reinstall takes it.
#
#   test/e2e/pypi.sh [<base url> [<repo>]]     default: http://localhost:8080 e2e
#
# The pypi repo is created with lajstrom admin repo create --registry local when it does not
# exist. lajstrom admin runs straight against the index database of the configuration the server
# runs with: $LAJSTROM_CONFIG, else deploy/compose/lajstrom.example.yaml, with the same
# LAJSTROM_DATABASE_* overrides. The binary is $LAJSTROM_BIN, else bin/lajstrom, else go run
# ./cmd/lajstrom. $LAJSTROM_SERVER_LOG, the server's log file, adds the check of PEP 658. Each
# run uses a project and a user of its own, with the writer role on the repo (lajstrom admin
# binding create), and disables the user at the end. The distributions stay, under the run's
# own project name: DELETE /api/v1/repos/pypi/<repo>/packages/<name>/versions/<v> deletes them
# (with ?variant=sdist or a wheel's tag set, one file; docs/api.md), but it needs delete on the
# repo, a maintainer's or an admin's, which the writer has not. The password goes to lajstrom on
# standard input and into the container in a file. It needs curl, jq and docker (python:3.13, or
# $PYPI_E2E_IMAGE, with --network host, which reaches the base URL on Linux and under OrbStack),
# prints one line per step and exits non-zero at the first failure.
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
plain="$url/simple/" # the index without credentials, as the configurations of the quick start name it
export TWINE_USERNAME="$user" TWINE_PASSWORD="$password"
pip install -q --root-user-action=ignore build twine uv poetry >/dev/null 2>&1
# imports DIR: hello() of the module, imported with the Python of the environment DIR.
imports() { "$1/bin/python" -c "import $MODULE; print($MODULE.hello())"; }
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
echo "poetry publish -r internal of the same files: 200"
(
	export HOME=/hp # Poetry's configuration, apart from the other clients'
	poetry config -q repositories.internal "$url/"
	poetry config -q http-basic.internal "$user" "$password"
	out="$(poetry publish -n -r internal 2>&1)" || { echo "FAIL: poetry publish said: $out" >&2; exit 1; }
)
cd /
echo "pip install from the simple index"
python -m venv /v && /v/bin/pip install -q --index-url "$index" "$PROJECT" >/dev/null
got="$(/v/bin/python -c "import $MODULE; print($MODULE.hello(), $MODULE.__file__)")"
case "$got" in "v1 /v/lib/"*) ;; *) echo "FAIL: imported $got" >&2; exit 1 ;; esac
echo "uv pip install"
uv venv -q /u && VIRTUAL_ENV=/u uv pip install -q --index-url "$index" "$NORMALIZED" >/dev/null
[ "$(/u/bin/python -c "import $MODULE; print($MODULE.hello())")" = v1 ] || { echo "FAIL: uv's install" >&2; exit 1; }
echo "pip install with the index in pip.conf and the credentials in ~/.netrc"
(
	export HOME=/n # pip.conf and ~/.netrc only here: python -m build reads them too
	mkdir -p /n/.config/pip
	printf '[global]\nindex-url = %s\n' "$plain" >/n/.config/pip/pip.conf
	printf 'machine %s login %s password %s\n' "${host%:*}" "$user" "$password" >/n/.netrc
	python -m venv /n/v && /n/v/bin/pip install -q "$PROJECT" >/dev/null
	[ "$(imports /n/v)" = v1 ] || { echo "FAIL: pip install with pip.conf and ~/.netrc" >&2; exit 1; }
)
echo "uv add with [[tool.uv.index]] in pyproject.toml; uv pip install with [[index]] in uv.toml"
(
	export HOME=/hu UV_INDEX_INTERNAL_USERNAME="$user" UV_INDEX_INTERNAL_PASSWORD="$password"
	mkdir -p /hu/add /hu/pip && cd /hu/add && uv init -q --bare --name consumer
	printf '\n[[tool.uv.index]]\nname = "internal"\nurl = "%s"\ndefault = true\n' "$plain" >>pyproject.toml
	uv add -q "$PROJECT"
	[ "$(imports .venv)" = v1 ] || { echo "FAIL: uv add" >&2; exit 1; }
	cd /hu/pip && printf '[[index]]\nname = "internal"\nurl = "%s"\ndefault = true\n' "$plain" >uv.toml
	uv venv -q && uv pip install -q "$PROJECT"
	[ "$(imports .venv)" = v1 ] || { echo "FAIL: uv pip install with uv.toml" >&2; exit 1; }
)
echo "poetry add in a project with the source of poetry source add"
(
	export HOME=/hp
	mkdir -p /hp && cd /hp && poetry new -n -q consumer && cd consumer
	poetry source add -n -q --priority=primary internal "$plain"
	poetry config -q http-basic.internal "$user" "$password"
	out="$(poetry add -n "$PROJECT" 2>&1)" || { echo "FAIL: poetry add said: $out" >&2; exit 1; }
	[ "$(imports "$(poetry env info -p)")" = v1 ] || { echo "FAIL: poetry add: not the module of 1.0" >&2; exit 1; }
)
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
echo "twine upload of a wheel of 1.1 with the build tag 1; pip install --force-reinstall takes it"
cd /p && echo 'def hello(): return "v1.1-1"' >"$MODULE/__init__.py"
python -m build -q --wheel -C--build-option=--build-number=1 --outdir /b . >/dev/null 2>&1
twine upload --non-interactive --disable-progress-bar --repository-url "$url/" /b/*-1.1-1-py3-none-any.whl >/dev/null
cd /
/v/bin/pip install -q --force-reinstall --no-deps --index-url "$index" "$PROJECT==1.1" >/dev/null
[ "$(/v/bin/python -c "import $MODULE; print($MODULE.hello())")" = v1.1-1 ] || { echo "FAIL: pip did not take the wheel with the build tag" >&2; exit 1; }
CLIENT

step "the clients in $image"
docker run --rm --network host -e BASE="$base" -e REPO="$repo" -e PROJECT="$project" -e NORMALIZED="$normalized" -e MODULE="$module" \
	-v "$work:/w:ro" "$image" bash /w/client.sh || fail "the clients"

step "the page of $normalized lists five files, the wheels with their core metadata, one with a build tag"
page="$(curl -fsS -u "$user:$password" -H 'Accept: application/vnd.pypi.simple.v1+json' "$base/pypi/$repo/simple/$normalized/")"
[ "$(jq '.files | length' <<<"$page")" = 5 ] || fail "files: $(jq -c '[.files[].filename]' <<<"$page")"
[ "$(jq '[.files[] | select(.filename | endswith("-1.1-1-py3-none-any.whl"))] | length' <<<"$page")" = 1 ] || fail "no wheel with the build tag"
[ "$(jq '[.files[] | select((.["core-metadata"] | type == "object") == (.filename | endswith(".whl")))] | length' <<<"$page")" = 5 ] ||
	fail "core-metadata on a wheel missing or on an sdist: $(jq -c '[.files[] | [.filename, .["core-metadata"]]]' <<<"$page")"
[ "$(jq -c .versions <<<"$page")" = '["1.0","1.1"]' ] || fail "versions: $(jq -c .versions <<<"$page")"

step "Accept latest+json and latest+html are served as v1 (PEP 691)"
for kind in json html; do
	ctype="$(curl -fsS -o /dev/null -w '%{content_type}' -u "$user:$password" -H "Accept: application/vnd.pypi.simple.latest+$kind" "$base/pypi/$repo/simple/$normalized/")"
	[ "$ctype" = "application/vnd.pypi.simple.v1+$kind" ] || fail "latest+$kind: Content-Type $ctype"
done

if [ -n "${LAJSTROM_SERVER_LOG:-}" ] && [ -f "$LAJSTROM_SERVER_LOG" ]; then
	step "pip read the core metadata (PEP 658) before the wheel"
	grep -q "path=/pypi/$repo/files/$normalized/1.0/[^ ]*\.whl\.metadata status=200" "$LAJSTROM_SERVER_LOG" ||
		grep -q "\"path\":\"/pypi/$repo/files/$normalized/1.0/[^\"]*\\.whl\\.metadata\",\"status\":200" "$LAJSTROM_SERVER_LOG" ||
		fail "no .metadata request in $LAJSTROM_SERVER_LOG"
fi
step "ok"

# Quick start: a pypi repo

A pypi repo serves wheels and source distributions to `pip`, `uv` and `poetry`: `twine upload`
or `uv publish` puts them in, the simple index (PEP 503 HTML and PEP 691 JSON) lets clients find
them, and every file is an OCI artifact in the registry behind Lajstrom, one manifest per file,
with its core metadata in a layer of its own. The endpoints are specified in
[http-routing.md](../spec/http-routing.md) §3 (pypi); what lands in the registry is the
[manifest profile](../spec/manifest-profile.md). `test/e2e/pypi.sh` runs the publish and install
steps with checks ([the end-to-end script](#the-end-to-end-script)).

## 1. Start the dev stack, make a repo and a user

You need Go, Docker, `curl` and `jq`, and `crane` to look into the registry (optional); the
Python side runs in `python:3.13`, so nothing of Python is needed on the host.

```sh
make dev-up                        # Zot, distribution and Postgres: docs/dev-stack.md
make run                           # serves deploy/compose/lajstrom.example.yaml on 127.0.0.1:8080
```

`make run` serves your copy, `deploy/compose/lajstrom.yaml`, instead when there is one; then set
`cfg` below to that file. The steps assume a fresh `lajstrom.db`: after another quick start, stop
`make run`, remove `lajstrom.db` and start it again, or the commands below exit 1 with
`user alice already exists`. In another terminal, from the repository's root, once `make run`
has logged `listening` (`serve` creates the tables of `lajstrom.db`; before that, `admin` fails
with `no database at …/lajstrom.db`):

```sh
cfg=deploy/compose/lajstrom.example.yaml
bin/lajstrom admin repo create pypi/internal --registry local --config $cfg
bin/lajstrom admin user create --name alice --config $cfg     # asks for a password
bin/lajstrom admin binding create --principal user:alice --role writer --mask pypi/internal --config $cfg
```

The binding lets alice publish to `pypi/internal` and install from it: the role `writer` is
`read` and `write` on the repos its mask names. Without it the repo answers her `404`, as a repo
that does not exist would; a user who only installs gets `reader`, and a build machine a deploy
token of the repo ([auth.md](../auth.md)). Nothing is open: without credentials every request is
`401`.

twine, uv publish and poetry publish always send credentials: the user's own name with their
password or one of their [personal access tokens](../auth.md) (any other name is `401`), or a
deploy token of the repo with any user name, `__token__` for example. twine and uv ask for the
password; `TWINE_PASSWORD` and `UV_PUBLISH_PASSWORD` give it without a prompt, in CI
for example.

## 2. Publish

Any project with a `pyproject.toml` will do. The smallest, `my-pkg`, outside the checkout:

```sh
mkdir -p ~/my-pkg/my_pkg && cd ~/my-pkg
echo 'def hello(): return "hello"' > my_pkg/__init__.py
cat > pyproject.toml <<'EOF'
[build-system]
requires = ["setuptools>=68"]
build-backend = "setuptools.build_meta"

[project]
name = "my-pkg"
version = "1.0"
requires-python = ">=3.9"
EOF
```

The clients run in `python:3.13` on the host's network, where `localhost:8080` is Lajstrom
(Linux and OrbStack; Docker Desktop is not tested, and its `--network host` needs Settings,
Resources, Network, Enable host networking), with the project's directory as `/w`:

```sh
docker run --rm -it --network host -v "$PWD:/w" -w /w python:3.13 bash
pip install --root-user-action=ignore build twine uv   # in the container; and poetry for Poetry
```

Then, in the container:

```sh
python -m build                                     # dist/<name>-<version>-py3-none-any.whl and .tar.gz
twine upload --repository-url http://localhost:8080/pypi/internal/ -u alice dist/*
uv publish --publish-url http://localhost:8080/pypi/internal/ -u alice dist/*   # the same files again: 200
```

Poetry publishes the files in `dist/`, with the repository and its credentials in its
configuration:

```sh
poetry config repositories.internal http://localhost:8080/pypi/internal/
poetry config http-basic.internal alice <password>
poetry publish -r internal                          # the same files again: 200
```

`poetry publish --build` builds them again first, with Poetry's backend: other bytes under the
same file names, so `409` here.

- One upload is one file and one manifest; the wheel's tag set (`py3-none-any`,
  `cp311-cp311-manylinux_2_17_x86_64`), with its build tag when it has one (`1-py3-none-any`:
  a rebuilt wheel of the same version, which pip prefers), or `sdist` tells the files of a
  version apart.
- The file is the truth: the server reads its `METADATA` (`PKG-INFO` for an sdist), and the name
  and version of the file name, of the core metadata and of the form must agree, or the upload
  is `400` with the reason. The digests twine sends (`sha256_digest`, `md5_digest`,
  `blake2_256_digest`) must be the file's.
- The same bytes again are `200`, and nothing changes. Other bytes under a file name that is
  there (the same version and tag set) are `409 File already exists`: files are immutable.
- **Skipping what is there:**
  `uv publish --check-url http://alice:<password>@localhost:8080/pypi/internal/simple/` skips the
  files the index already has. The check URL needs the credentials in it: uv sends those of `-u`
  to the upload URL only, so without them the first check is `401` and that file is sent again
  (`200`, nothing changes). twine 6 and later refuse `--skip-existing` for any repository but
  PyPI's own, before they send anything.
- The project name is stored normalized (PEP 503): `My_Pkg`, `my-pkg` and `MY.PKG` are one
  project, `my-pkg`; the original spelling stays in the core metadata, so `pip show` prints it.
- A version that is not PEP 440 (`latest`) is accepted and flagged
  `lajstrom.pypi.nonstandard`; pip 22 and later ignore it.

## 3. Install

```sh
pip install --index-url http://alice:<password>@localhost:8080/pypi/internal/simple/ my-pkg
uv pip install --system --index-url http://alice:<password>@localhost:8080/pypi/internal/simple/ my-pkg
```

`uv pip install` refuses to run outside a virtual environment: `--system` installs into the
container's Python, `uv venv` makes an environment instead.

In `pip.conf` (`~/.config/pip/pip.conf`, or `/etc/pip.conf`), with the credentials in
`~/.netrc` (`machine localhost login alice password <password>`) rather than in the URL:

```ini
[global]
index-url = http://localhost:8080/pypi/internal/simple/
```

uv, in a project's `pyproject.toml` for `uv add my-pkg`, with `UV_INDEX_INTERNAL_USERNAME` and
`UV_INDEX_INTERNAL_PASSWORD` in the environment:

```toml
[[tool.uv.index]]
name = "internal"
url = "http://localhost:8080/pypi/internal/simple/"
default = true
```

In `uv.toml`, which `uv pip install my-pkg` reads too, the table is `[[index]]` with the same
keys; `[[tool.uv.index]]` there is a parse error.

Poetry, in the project that depends on `my-pkg` (`poetry new consumer && cd consumer` makes one):

```sh
poetry source add --priority=primary internal http://localhost:8080/pypi/internal/simple/
poetry config http-basic.internal alice <password>
poetry add my-pkg
```

The index serves what pip and uv need to resolve without downloading: `data-requires-python`,
and the core metadata of each wheel (PEP 658, 714) at `<file URL>.metadata`, announced with its
sha256 as `data-core-metadata`, so `pip install` reads a few kilobytes per candidate and
downloads only the files it installs. An sdist has no `data-core-metadata`, as on PyPI: a
build may change its `PKG-INFO`, so pip builds it to read its dependencies. Nor has a wheel
whose core metadata the index does not hold (it was over the size ingest keeps, or the
manifest is degraded), and pip downloads it to read it. The pages are cached in the index database and rendered again
after every publish; they answer `If-None-Match` with `304`.

```sh
curl -s -u alice http://localhost:8080/pypi/internal/simple/                       # the projects, HTML
curl -s -u alice -H 'Accept: application/vnd.pypi.simple.v1+json' \
  http://localhost:8080/pypi/internal/simple/my-pkg/ | jq '.versions, [.files[].filename]'
```

## 4. In the registry

Each file is a manifest under `pypi/internal/<normalized name>`, tagged with the version and the
tag set (`1.0__py3-none-any`, [the tag scheme](../spec/manifest-profile.md)), with the file and
the metadata layer:

```sh
crane ls localhost:5000/pypi/internal/my-pkg
crane manifest localhost:5000/pypi/internal/my-pkg:1.0__py3-none-any | jq .annotations
```

`bin/lajstrom reindex --repo pypi/internal --config $cfg` rebuilds the index of the repo from
those manifests ([operations.md](../operations.md#reindex)).

## 5. Clean up

A version goes with `DELETE` on the API: every file of the version, or with `?variant=` one of
them, `sdist` or the wheel's tag set
([api.md](../api.md#delete-a-version-delete-apiv1reposformatnamepackagespackageversionsversion)).
It needs `delete` on the repo, which alice's `writer` does not have: the `maintainer` role, or an
admin ([operations.md](../operations.md#the-first-admin)):

```sh
curl -u admin -X DELETE "http://localhost:8080/api/v1/repos/pypi/internal/packages/my-pkg/versions/1.0?variant=sdist"   # 204
```

Everything: leave the container (`exit`) and stop `make run` (Ctrl-C) first, so that the server
folds `lajstrom.db-wal` back into `lajstrom.db`.

```sh
make dev-down RESET=1   # the registries forget what they hold
rm lajstrom.db          # the index, with the repo and alice
rm -r ~/my-pkg          # the project; sudo on Linux: the container wrote dist/ as root
```

## The end-to-end script

`test/e2e/pypi.sh` runs sections 2 and 3 with checks, in `python:3.13` (or the image
`PYPI_E2E_IMAGE` names): a project built with `python -m build`, a wheel and an sdist, uploaded
with twine; the same files again (`200`); other bytes under the same file name (`409`, which
twine reports); `poetry publish -r internal` of the same files (`200`); `pip install` from the
simple index in a clean venv; `uv pip install`; the configurations of section 3, each client in a
home of its own: `pip.conf` with the credentials in `~/.netrc`, `[[tool.uv.index]]` in a
project's `pyproject.toml` for `uv add`, `[[index]]` in `uv.toml` for `uv pip install`, and
`poetry source add` in a consumer project for `poetry add`; `uv publish` of the next version,
then `uv publish --check-url`, which skips the files that are there; and
`pip install --upgrade`, which gets the next version. With `LAJSTROM_SERVER_LOG`
naming the server's log file, it also checks in the access log that pip read the core metadata
(PEP 658) before the file. It creates the repo when it does not exist, and a user of its own
with `writer` on it, with `bin/lajstrom admin` against the configuration of `LAJSTROM_CONFIG`
(the example by default), so it runs from the repository's root:

```sh
test/e2e/pypi.sh http://localhost:8080 internal
```

It disables its user at the end and deletes nothing: the distributions stay.
`poetry publish --build` (`409`) is not in it.

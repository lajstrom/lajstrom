# Quick start: a pypi repo

A pypi repo serves wheels and source distributions to `pip`, `uv` and `poetry`: `twine upload`
or `uv publish` puts them in, the simple index (PEP 503 HTML and PEP 691 JSON) lets clients find
them, and every file is an OCI artifact in the registry behind Lajstrom, one manifest per file,
with its core metadata in a layer of its own. The endpoints are specified in
[http-routing.md](../spec/http-routing.md) §3 (pypi); what lands in the registry is the
[manifest profile](../spec/manifest-profile.md). `test/e2e/pypi.sh` runs the publish and install
steps with checks ([the end-to-end script](#the-end-to-end-script)).

## 1. Start the dev stack, make a repo and a user

You need Go, Docker and `curl`; the Python side runs in `python:3.13`, so nothing of Python is
needed on the host.

```sh
make dev-up                        # Zot, distribution and Postgres: docs/dev-stack.md
make run                           # serves deploy/compose/lajstrom.example.yaml on :8080
```

In another terminal:

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

twine and uv publish always send credentials; a user's password or one of their
[personal access tokens](../auth.md) both work, with any user name for a token.

## 2. Publish

The clients run in `python:3.13` on the host's network, where `localhost:8080` is Lajstrom
(Linux and OrbStack), with the directory of a project with a `pyproject.toml` as `/w`:

```sh
docker run --rm -it --network host -v "$PWD:/w" -w /w python:3.13 bash
pip install --root-user-action=ignore build twine uv   # in the container; and poetry for section 3
```

Then, in the container:

```sh
python -m build                                     # dist/<name>-<version>-py3-none-any.whl and .tar.gz
twine upload --repository-url http://localhost:8080/pypi/internal/ -u alice dist/*
uv publish --publish-url http://localhost:8080/pypi/internal/ -u alice dist/*
```

- One upload is one file and one manifest; the wheel's tag set (`py3-none-any`,
  `cp311-cp311-manylinux_2_17_x86_64`) or `sdist` tells the files of a version apart.
- The file is the truth: the server reads its `METADATA` (`PKG-INFO` for an sdist), and the name
  and version of the file name, of the core metadata and of the form must agree, or the upload
  is `400` with the reason. The digests twine sends (`sha256_digest`, `md5_digest`,
  `blake2_256_digest`) must be the file's.
- The same bytes again are `200`, and nothing changes. Other bytes under a file name that is
  there (the same version and tag set) are `409 File already exists`: files are immutable.
- **Skipping what is there:** `uv publish --check-url http://localhost:8080/pypi/internal/simple/`
  skips the files the index already has. twine 6 and later refuse `--skip-existing` for any
  repository but PyPI's own, before they send anything.
- The project name is stored normalized (PEP 503): `My_Pkg`, `my-pkg` and `MY.PKG` are one
  project, `my-pkg`; the original spelling stays in the core metadata, so `pip show` prints it.
- A version that is not PEP 440 (`latest`) is accepted and flagged
  `lajstrom.pypi.nonstandard`; pip 22 and later ignore it.

## 3. Install

```sh
pip install --index-url http://alice:<password>@localhost:8080/pypi/internal/simple/ my-pkg
uv pip install --index-url http://alice:<password>@localhost:8080/pypi/internal/simple/ my-pkg
```

In `pip.conf` (`~/.config/pip/pip.conf`, or `/etc/pip.conf`), with the credentials in
`~/.netrc` (`machine localhost login alice password <password>`) rather than in the URL:

```ini
[global]
index-url = http://localhost:8080/pypi/internal/simple/
```

uv, in `pyproject.toml` or `uv.toml`, with `UV_INDEX_INTERNAL_USERNAME` and
`UV_INDEX_INTERNAL_PASSWORD` in the environment:

```toml
[[tool.uv.index]]
name = "internal"
url = "http://localhost:8080/pypi/internal/simple/"
default = true
```

Poetry:

```sh
poetry source add --priority=primary internal http://localhost:8080/pypi/internal/simple/
poetry config http-basic.internal alice <password>
```

The index serves what pip and uv need to resolve without downloading: `data-requires-python`,
and the core metadata of each file (PEP 658, 714) at `<file URL>.metadata`, announced with its
sha256 as `data-core-metadata`, so `pip install` reads a few kilobytes per candidate and
downloads only the files it installs. A file whose core metadata the index does not hold (it
was over the size ingest keeps, or the manifest is degraded) has no `data-core-metadata`, and
pip downloads it to read it. The pages are cached in the index database and rendered again
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

`lajstrom reindex --repo pypi/internal` rebuilds the index of the repo from those manifests
([operations.md](../operations.md#reindex)).

## The end-to-end script

`test/e2e/pypi.sh` runs sections 2 and 3 with checks, in `python:3.13` (or the image
`PYPI_E2E_IMAGE` names): a project built with `python -m build`, a wheel and an sdist, uploaded
with twine; the same files again (`200`); other bytes under the same file name (`409`, which
twine reports); `pip install` from the simple index in a clean venv; `uv pip install`;
`uv publish` of the next version, then `uv publish --check-url`, which skips the files that are
there; and `pip install --upgrade`, which gets the next version. With `LAJSTROM_SERVER_LOG`
naming the server's log file, it also checks in the access log that pip read the core metadata
(PEP 658) before the file. It creates the repo when it does not exist, and a user of its own
with `writer` on it, with `bin/lajstrom admin` against the configuration of `LAJSTROM_CONFIG`
(the example by default), so it runs from the repository's root:

```sh
test/e2e/pypi.sh http://localhost:8080 internal
```

It disables its user at the end; the distributions stay, since pypi files cannot be deleted yet.
Poetry, `pip.conf` with `~/.netrc` and the `[[tool.uv.index]]` configuration are not in it.

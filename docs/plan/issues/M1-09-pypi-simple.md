---
id: M1-09
title: "feat(format/pypi): simple index (PEP 503, 691, 658) and file download"
milestone: M1
lane: formats
size: M
priority: p0
labels: [area/formats, kind/feature]
depends: [M1-08, M0-05]
---
**Goal**

`pip install --index-url https://host/pypi/<repo>/simple/ <project>` works, with JSON and HTML
index variants and PEP 658 metadata so pip resolves without downloading wheels.

**Spec and context**

`docs/spec/http-routing.md` §3 pypi; PEPs 503, 691, 658, 592 (yank, read-only for now).

**Scope**

- Routes: `/simple/`, `/simple/{project}/` (HTML and `application/vnd.pypi.simple.v1+json` by
  `Accept`; non-normalised → 301), `/files/{project}/{version}/{filename}` (ServeBlob helper),
  `/files/…/{filename}.metadata`.
- `RenderIndex` from the index DB with `index_cache` for the project page; links with
  `#sha256=`, `data-requires-python`, `data-core-metadata="sha256=…"`.
- Ordering and `versions` field per PEP 691; `meta.api-version: 1.1`.

**Acceptance criteria**

- [ ] e2e: a Python container does `pip install --index-url …` of a package with two wheels
      and an sdist (from fixtures ingested via M1-10 or `oras`) and `pip download --no-deps`
      picks the right wheel for its platform; `uv pip install` works too.
- [ ] tests: HTML and JSON renderers (golden files), normalisation redirect, cache invalidation
      after ingest.
- [ ] docs: `docs/quickstart/pypi.md` (pip, uv, poetry config).

---
id: M1-10
title: "feat(format/pypi): legacy upload API (twine) → ingest"
milestone: M1
lane: formats
size: M
priority: p0
labels: [area/formats, kind/feature]
depends: [M1-08, M0-06]
---
**Goal**

`twine upload --repository-url https://host/pypi/<repo>/ dist/*` publishes, one manifest per
file, through the ingest pipeline.

**Scope**

- `POST /pypi/{repo}/` with `multipart/form-data`: `:action=file_upload`,
  `protocol_version=1`, `content` (file), metadata fields (`name`, `version`, `filetype`,
  `pyversion`, `md5_digest`, `sha256_digest`, `blake2_256_digest`, core metadata fields).
- Stream the file part to the pipeline (`Upload` with one file); verify the digests the client
  sent; `Build` creates the metadata layer from the parsed METADATA (server-side truth), not from
  form fields (they are cross-checked and mismatches are 400).
- Status codes twine understands: 200 ok, 400 with a message, 409 on existing file with
  different content (twine `--skip-existing` expects 400/409 with "already exists" text).

**Acceptance criteria**

- [ ] e2e: `twine upload` of a wheel + sdist, then `pip install` (M1-09) from a clean venv;
      re-upload with `--skip-existing` is a no-op; re-upload of different bytes → 409.
- [ ] `python -m build && uv publish --publish-url …` works.
- [ ] tests: multipart parsing with a streamed 200 MiB file (constant memory), digest mismatch.

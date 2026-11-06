---
id: M1-01
title: "feat(facade): /v2 proxy: blob uploads, blob and manifest reads, tags/list, _catalog"
milestone: M1
lane: core
size: L
priority: p0
labels: [area/facade, kind/feature]
depends: [M0-03, M0-08]
---
**Goal**

`oras`, `cosign`, `helm` and `docker` can talk to Lajstrom as if it were a registry; everything
except manifest writes is proxied to the backend through the provider.

**Spec and context**

`docs/spec/http-routing.md` §2; OCI distribution spec 1.1; `docs/architecture.md` §1.

**Scope**

- `internal/facade`: router for `/v2/`, name parsing `<format>/<repo>/<rest>` with repo lookup
  (404 `NAME_UNKNOWN`), error bodies in the distribution JSON format with the right codes.
- Blob upload session: `POST …/blobs/uploads/` (monolithic with `?digest=`, and chunked
  `PATCH`/`PUT`), `Location` rewriting so the client keeps talking to us, streaming to the
  backend's own upload session (keep the backend `Location` in a short-lived DB row or in the
  opaque upload id).
- `HEAD/GET …/blobs/<digest>` (GET redirect logic in M1-04; here: stream),
  `HEAD/GET …/manifests/<ref>` (pass `Accept`, return `Docker-Content-Digest`),
  `GET …/tags/list` from the index with `n`/`last` pagination, `GET /v2/_catalog` from the
  control plane, `GET /v2/` with the auth challenge (401 until M1-03 is in; a config switch
  allows anonymous on the dev stack).
- Mount uploads (`?mount=&from=`) passed through when the backend supports it.

**Acceptance criteria**

- [ ] Integration: `oras push localhost:8080/raw/assets/hello:1 file.txt` succeeds end to end
      (with M1-02 the manifest is indexed; without it the PUT returns 501 `UNSUPPORTED`).
- [ ] `oras pull`, `oras manifest fetch`, `oras repo tags` work; `crane ls` works.
- [ ] Conformance: the OCI distribution conformance test suite (pull + push categories) passes
      against the facade on an `oci`-kind repo (a minimal `oci` passthrough mode is enabled for
      this test only; full passthrough is post-MVP).
- [ ] Streaming: a 1 GiB upload and download through the facade run with constant memory.
- [ ] docs: `docs/quickstart/oras.md`.

**Pointers**

`distribution/distribution` `registry/api/v2` for the route table and error codes (read, do not
copy). oras-go's `remote.Repository` already does chunked uploads to the backend.

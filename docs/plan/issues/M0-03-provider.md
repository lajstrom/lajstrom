---
id: M0-03
title: "feat(provider): Provider interface and generic oras-go implementation (distribution, Zot)"
milestone: M0
lane: core
size: L
priority: p0
labels: [area/provider, kind/feature, contract]
depends: [M0-01]
---
**Goal**

The only component that talks to a backend registry. A `generic` implementation on oras-go v2
that works against docker distribution and Zot, with capabilities, prefixing and credentials.

**Spec and context**

`docs/spec/layout.md` §2 (Provider, Repository, Capabilities); `docs/architecture.md` §5.

**Scope**

- `internal/provider`: interfaces, `Capabilities`, sentinel errors (`ErrNotFound`,
  `ErrUnsupported`, `ErrDenied`), `New(cfg)` factory by type.
- `internal/provider/generic`: `remote.Registry`/`remote.Repository` from oras-go with Basic
  or bearer credentials from config, prefix handling (`<prefix>` added/removed transparently),
  `ListRepositories` via `_catalog` with pagination (and a `catalog: false` config switch),
  `Delete` via the manifests DELETE endpoint, referrers via oras-go (API or tag schema; expose
  which one in capabilities), `BlobRedirectURL` by issuing a `GET` with redirects disabled and
  returning the `Location` when the backend answers 307 (capability-gated).
- Capability probing on startup: `/v2/` reachable, referrers support, delete support (config
  override `capabilities:` for backends that lie).
- Retries with backoff on 5xx/timeouts (small; configurable), per-request timeouts.

**Acceptance criteria**

- [ ] Integration tests (build tag `integration`, `LAJSTROM_TEST_REGISTRY`) against Zot and
      distribution: push blob, push manifest+tag, resolve, fetch, tags list, referrers (where
      supported), delete (where supported), list repositories with a prefix, blob redirect
      detection.
- [ ] Unit tests with `httptest` for prefixing, pagination, retry and capability override.
- [ ] A 1 GiB blob push/fetch streams with constant memory (test with an `io.Reader` that
      generates data; assert RSS does not grow, or at least that no `[]byte` of the blob is
      allocated: use `io.Copy` only).
- [ ] docs: `docs/registries/README.md` lists what `generic` was tested against.

**Out of scope**

ECR (M1-13), Harbor/GHCR specifics beyond what `generic` already does.

**Pointers**

oras-go `registry/remote`, `remote/auth`, `remote/retry`. Note distribution's `_catalog` has no
reliable pagination; cap the page size and stop on repeat.

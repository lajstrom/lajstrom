---
id: M0-05
title: "feat(format): Plugin interface, registry, Router/Deps helpers"
milestone: M0
lane: formats
size: M
priority: p0
labels: [area/formats, kind/feature, contract]
depends: [M0-01, M0-02]
---
**Goal**

`internal/format`: the `Plugin` interface with its types, a registry that the server iterates to
mount routes, and the helpers plugins share (route prefixing, error → HTTP mapping, Basic auth
extraction placeholder until M2, index-cache helpers).

**Spec and context**

`docs/spec/layout.md` §2 (Plugin, Entry, Upload, Built, Deps); `docs/spec/http-routing.md` §3.

**Scope**

- Interface and types exactly as merged in M0-01, extended as needed with a documented reason.
- `Register(p Plugin)` / `All()`; a format name must not be a reserved segment (test).
- `Router` wrapper: `r.Handle("GET /{repo}/simple/{project}/", h)` becomes
  `/pypi/{repo}/simple/{project}/`; repo lookup middleware that resolves `{repo}` to
  `index.Repo` and returns 404 when missing or wrong format.
- `WriteError(w, err)` mapping sentinel errors to statuses; `Document` type for RenderIndex.
- A `fake` plugin in `internal/format/formattest` for server tests.

**Acceptance criteria**

- [ ] A test server with the fake plugin answers `/fake/{repo}/ping` → 200 and
      `/fake/missing/ping` → 404, `/api/...` is not shadowed.
- [ ] Registering a plugin named `v2` panics at startup with a clear message.
- [ ] tests: error mapping table; repo middleware caching behaviour (one DB lookup per request).
- [ ] docs: layout.md §2 reflects the merged shapes.

**Out of scope**

Any real format (M0-07 raw is the first).

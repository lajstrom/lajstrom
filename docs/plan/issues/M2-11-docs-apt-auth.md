---
id: M2-11
title: "docs: apt quickstart, auth and RBAC guide"
milestone: M2
lane: platform
size: S
priority: p1
labels: [area/docs, kind/docs]
depends: [M2-04, M2-07]
---
**Goal**

Operators can set up an apt repo with signing and give a team the right permissions from the
docs alone.

**Scope**

- `docs/quickstart/apt.md` (publish with PUT and with `oras`, client sources list, key
  pinning), `docs/auth.md` (users, PATs, deploy tokens, bindings, masks, anonymous read, token
  flow for OCI clients, examples for CI).

**Acceptance criteria**

- [ ] Reviewed by someone who did not write the auth code; they set up a reader and a writer
      following the doc.

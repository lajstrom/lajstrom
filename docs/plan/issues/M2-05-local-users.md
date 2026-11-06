---
id: M2-05
title: "feat(auth): local users, argon2id, Basic auth on all frontends"
milestone: M2
lane: core
size: M
priority: p0
labels: [area/auth, kind/feature]
depends: [M0-04, M1-03]
---
**Goal**

Real principals from the control plane replace the static config users of M1.

**Spec and context**

`docs/spec/rbac.md` §1, §6.

**Scope**

- `users` table CRUD in the store; argon2id hashing with sane parameters; `disabled` flag.
- `Authenticator` middleware for frontends: Basic → user+password or token (M2-06); the
  principal is put in the request context; 401 with `WWW-Authenticate: Basic realm="lajstrom"`
  when required.
- `/v2/token` (M1-03) switched to the same authenticator; static users removed (or kept behind
  `auth.static` for dev only, with a startup warning).
- `lajstrom admin user create|list|disable|set-password` (bootstrap of the first admin without
  HTTP; `--admin` creates the admin binding).

**Acceptance criteria**

- [ ] tests: hashing parameters, timing-safe compare, disabled user, bootstrap command.
- [ ] e2e: pip/twine/oras/curl all authenticate with the same `user:password`.

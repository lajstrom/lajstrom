---
id: M0-09
title: "chore(dev): docker compose dev stack: Zot, distribution, Postgres"
milestone: M0
lane: platform
size: S
priority: p0
labels: [area/dev, kind/chore, good first issue]
depends: []
---
**Goal**

`make dev-up` gives every contributor the same registries and database in under a minute.

**Scope**

- `deploy/compose/compose.yaml`: Zot (pinned version, config enabling referrers, search
  extension off, `storage.dedupe`, anonymous read/write for dev), docker distribution v3
  (pinned, `storage.delete.enabled: true`, filesystem storage), Postgres 17 (pinned) with a
  `lajstrom` database; health checks on all three; published ports documented.
- `deploy/compose/lajstrom.example.yaml` pointing at both registries (Zot as `local`,
  distribution as `second`) and Postgres; a SQLite variant via env.
- `make dev-up`, `make dev-down`, `make dev-logs`; `.env.example`.
- `docs/dev-stack.md`: ports, how to point `oras` at Zot, how to reset.

**Acceptance criteria**

- [ ] `make dev-up` on macOS (OrbStack/Docker Desktop) and Linux; `oras push localhost:5000/t:1
      README.md` works against Zot and `localhost:5001` against distribution.
- [ ] `docker compose ps` shows all healthy; `make dev-down` removes volumes with `-v` only when
      `RESET=1`.
- [ ] docs written.

**Pointers**

Zot image `ghcr.io/project-zot/zot-linux-arm64` vs amd64: use the multi-arch `zot` tag.

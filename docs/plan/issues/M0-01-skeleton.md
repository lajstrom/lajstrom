---
id: M0-01
title: "chore(core): Go module skeleton: cmd/lajstrom, config, server, Makefile"
milestone: M0
lane: core
size: L
priority: p0
labels: [area/core, kind/chore, contract]
depends: []
assignee: gecube
---
**Goal**

A buildable module with the package layout of `docs/spec/layout.md`, a `lajstrom serve` that
starts an HTTP server from a config file, and the Make targets everyone will use. After this
merges, the other M0 issues plug into real packages instead of guessing.

**Spec and context**

`docs/spec/layout.md` §1, §5, §6; `docs/spec/http-routing.md` §1, §5.

**Scope**

- `go.mod` bumped to the current stable Go; `cmd/lajstrom` with cobra: `serve`, `version`,
  `migrate` (stub calling index), `reindex` (stub), `admin` (stub group).
- `internal/config`: YAML file + `LAJSTROM_*` env overrides, validation with clear errors,
  `deploy/compose/lajstrom.example.yaml`.
- `internal/server`: `http.Server` with timeouts, `ServeMux` with the reserved first segments,
  middleware chain (request id, slog access log, recovery), graceful shutdown on SIGTERM,
  `/-/healthz`, `/-/version`.
- Empty but compiling packages with the interface files named in CODEOWNERS:
  `internal/format/plugin.go`, `internal/provider/provider.go`, `internal/index/store.go`,
  `internal/ingest/pipeline.go`, containing the interfaces from `docs/spec/layout.md` §2 as the
  first concrete version (adjust shapes as needed; document deviations in the PR).
- `Makefile` with the targets of layout.md §5 (`dev-up`/`e2e` may call scripts that M0-09/M1-11
  fill in).
- `.golangci.yml` kept; `make check` == CI.

**Acceptance criteria**

- [ ] `make build && ./bin/lajstrom serve --config deploy/compose/lajstrom.example.yaml` serves
      `/-/healthz` → 200 and `/-/version` → JSON.
- [ ] `make check` passes and equals the CI job list in CONTRIBUTING.md.
- [ ] Interfaces compile and have doc comments pointing to the spec sections.
- [ ] tests: config parsing (file, env override, invalid values), server middleware (request id
      propagated, panic → 500 with log), graceful shutdown.
- [ ] docs: `docs/spec/layout.md` §2 updated to the merged interface shapes; CLAUDE.md commands
      verified.

**Out of scope**

Any protocol, provider, DB implementation (M0-03, M0-04, M0-07).

**Pointers**

Keep the router stdlib (`net/http` patterns `GET /raw/{repo}/{path...}`). cobra and yaml.v3 are
approved. Put the reserved-segment check in the server so a format cannot register `api`.

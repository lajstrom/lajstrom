# CLAUDE.md — working on Lajstrom with Claude Code

Lajstrom is an artifact registry (raw, pypi, apt, maven; later rpm, npm, terraform) whose only
storage is an OCI registry. Lajstrom owns the protocols, the index and the permissions, and not
a single blob. Go, single binary, SQLite or Postgres as a derived index.

## Read before changing anything

1. `docs/architecture.md` — the model and the five flows.
2. `docs/spec/manifest-profile.md` — what every artifact looks like in the registry (normative).
3. `docs/spec/layout.md` — package layout, interfaces, approved dependencies, test levels.
4. `docs/spec/http-routing.md`, `docs/spec/index-schema.md`, `docs/spec/rbac.md` — when the task
   touches endpoints, the database, or auth.
5. The issue you are implementing (`gh issue view <n>`). Its acceptance criteria are the
   definition of done.
6. `docs/spec/guarantees.md` — the invariants Lajstrom promises, and what each one assumes.

## Invariants you must not break

- The OCI registry is the single source of truth. Never store artifact bytes anywhere else
  (no local files, no DB blobs). Only `internal/provider/*` talks to the registry.
- Derived tables (`packages`, `versions`, `files`, `index_cache`, `oci_repos`) must be
  rebuildable from manifest annotations by `lajstrom reindex`. If a feature needs per-version
  data, put it in the manifest (annotation or metadata layer), not only in the DB.
- One write path: every publish goes through `internal/ingest`. Frontends never push manifests
  themselves; `make lint` refuses a provider write or oras-go in `internal/format` (forbidigo,
  depguard `one-write-path`).
- Tags are immutable and append-only: never re-point a tag; collision with another digest is 409.
- No client plugins: protocols are emulated on the server.
- Only `internal/index/*` talks to the database. Only `internal/provider/*` talks to the registry.
  `make lint` enforces both (depguard) and rejects `fmt.Print*` and the std `log` outside `cmd/`.

## Contracts: do not change without an issue labelled `contract`

`docs/spec/*.md`, `docs/adr/*.md`, this file, and the exported interfaces of `internal/format`,
`internal/provider`, `internal/index`, `internal/ingest`. If your task seems to require changing
one, stop, explain why in the issue, and wait for the lead. Do not "just extend the interface".

## Layout (details in docs/spec/layout.md)

```
cmd/lajstrom            CLI: serve, reindex, migrate, version, admin, healthcheck
internal/server         router (net/http ServeMux), middleware
internal/facade         OCI distribution API /v2 proxy + token + manifest hook
internal/format/<fmt>   one package per format: parse.go (pure), routes.go (HTTP), index.go (render)
internal/oci/profile    media types, annotations, Escape/EscapeComponent, manifest build/validate
internal/ingest         pipeline + publication sessions
internal/provider       Provider interface; generic (oras-go), ecr
internal/index          Store interface, models, migrations; sqlite, postgres
internal/auth, api, keys, audit, config, testutil, version
deploy/compose          dev stack (zot, distribution, postgres)
docs/                   architecture, spec, adr, plan, registries
```

## Commands

```sh
make build            # bin/lajstrom
make test             # go test -race ./...
make lint             # golangci-lint, pinned version
make check            # everything CI runs: run this before every push
make dev-up           # Zot + distribution + Postgres in containers
make test-integration # needs dev-up; real registry and Postgres
make e2e              # real clients (oras, pip, apt-get, mvn) against the stack
```

## Code rules

- Go stable; standard library first. New dependencies only from the approved list in
  `docs/spec/layout.md` §3; anything else needs a maintainer decision in the PR. No cgo, no ORM,
  no web framework.
- Wrap errors with `%w`; sentinel errors per package (`ErrNotFound`, `ErrConflict`,
  `ErrUnsupported`); map to HTTP status in one place per package.
- `context.Context` on every I/O call. Stream blobs when serving and when pushing to the
  registry; an upload streams into the registry (ADR 0014); a temporary file only where the
  operator turns it on (#425). Formats move one by one (raw and apt have); until its move, a format's
  upload is still spooled for one request, never kept, never served from. Artifact bytes live
  only in the registry.
- `log/slog` for logs; never log secrets, tokens, passwords or Authorization headers.
- Tests: table tests; fixtures built in-process with `internal/testutil` (make a `.deb`, a
  `.whl`, a `.jar` in memory). Do not vendor binary packages from distributions.
  Integration tests use `//go:build integration` and `LAJSTROM_TEST_REGISTRY`.
- Keep a pull request to one issue and ideally under 400 changed lines; split otherwise.
- Do not copy code from other projects (Gitea, Nexus, distribution). Read them for protocol
  details and write our own; CI runs a snippet scan.
- Do not edit `.github/workflows/*`, branch protection, or repository settings in a feature PR.

## Commits and pull requests

- Conventional Commits subject: `feat(pypi): serve PEP 691 JSON simple index`. Types: feat, fix,
  docs, test, refactor, perf, build, ci, chore, revert. Scope = package or area.
- Every commit: `Signed-off-by:` (`git commit -s`) and, for commits you helped write,
  `Assisted-by: Claude Code`. Both are checked.
- The PR description is written by the human: what, why, how tested, demo command. Tick the
  issue's acceptance criteria in it.
- Never push to `main`; never force-push a shared branch; do not merge, the human does.

## How to work an issue

1. Read the issue and the specs it links. Restate the acceptance criteria in your own words.
2. Propose a plan (files, tests, questions). For `size/M` and `size/L`, wait for the human.
3. Write the tests for the acceptance criteria first when the behaviour is clear.
4. Implement; run `make check`; fix everything it reports.
5. Summarise what changed and what you did not do, so the human can write the PR description.

## Things that look like shortcuts and are not

- Adding a column to a derived table without a corresponding annotation: breaks reindex.
- Re-tagging on conflict "to be helpful": breaks immutability and signatures.
- Calling the registry from a frontend handler "just for this file": breaks the provider boundary.
- Auto-creating repos on push: a decision was made not to (ADR 0004).
- Decoding a tag to find the version: tags are keys; read annotations or the index.

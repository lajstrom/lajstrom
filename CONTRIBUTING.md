# Contributing to Lajstrom

Thank you for your interest in Lajstrom. New here? Read [docs/onboarding.md](docs/onboarding.md)
(one hour from zero to a claimed issue) and [docs/process.md](docs/process.md) (how an issue
becomes a merged pull request). This page holds the legal and mechanical rules every pull
request must satisfy.

Everyone taking part is expected to follow the [Code of Conduct](CODE_OF_CONDUCT.md).
Security issues are never reported in public issues; see [SECURITY.md](SECURITY.md).

## Summary: what every pull request needs

1. Every commit is signed off under the Developer Certificate of Origin (`git commit -s`).
2. You have signed the Contributor License Agreement (once, before your first merge).
3. Any AI assistance is disclosed with an `Assisted-by:` commit trailer.
4. A description, written by you, of what the change does and why.
5. Tests for the change, and an ADR if the change is architectural.
6. New dependencies are under a license from the allowlist below.
7. CI is green.

## Developer Certificate of Origin (DCO)

Every commit must carry a `Signed-off-by:` line. By adding it you certify the
[Developer Certificate of Origin 1.1](https://developercertificate.org/): that you wrote the change
or otherwise have the right to submit it under the project's open source license.

```sh
git commit -s -m "feat(index): rebuild the index from registry tags"
```

The `Signed-off-by:` name and email must match the commit author. To fix a missing sign-off on the
last commit, run `git commit --amend -s --no-edit`; for older commits, use
`git rebase --signoff main` and push the branch again. The `dco` CI job fails if any commit in the
pull request lacks a sign-off.

## Contributor License Agreement (CLA)

In addition to the DCO, contributors sign the [Contributor License Agreement](CLA.md) once, before
their first pull request is merged. The CLA lets the project owner license and relicense
contributions under OSI-approved licenses and includes a patent license; the DCO records the
provenance of each individual commit. The reasoning is in
[ADR 0003](docs/adr/0003-license-and-contribution-terms.md).

Signing is handled by a bot that runs inside this repository's GitHub Actions; no external
service is involved:

- On your first pull request the CLA bot posts a comment and sets a failing `CLAAssistant` check.
- Read [CLA.md](CLA.md), then sign by posting this exact comment on the pull request:

  ```text
  I have read the CLA Document and I hereby sign the CLA
  ```

- The bot records your GitHub user name, user id, the pull request and the time in
  `signatures/version1/cla.json` on the `cla-signatures` branch of this repository, and the check
  turns green. Comment `recheck` to re-run the check.
- If you contribute on behalf of your employer, your employer must also accept the corporate part
  of the CLA. Contact the maintainers before your first pull request.

## AI-assisted contributions

Lajstrom is built with a lot of help from AI tools, and contributions made with AI assistance are
welcome. They are held to the same bar as any other contribution, plus a few rules:

- **A human is responsible.** The human author of the pull request is responsible for its
  correctness, its tests and its license cleanliness, exactly as if they had typed every line.
  Understand and review what you submit; "the tool wrote it" is not an answer in review.
- **Disclose the tool.** Every commit that contains AI-assisted work carries a trailer naming the
  tool, for example:

  ```text
  feat(api): add tag listing endpoint

  Assisted-by: <tool name>
  Signed-off-by: Your Name <your-email>
  ```

  Use one `Assisted-by:` line per tool. Plain autocompletion of a single identifier does not
  need a trailer; generated functions, tests, documentation or refactorings do.
- **No verbatim copying.** Do not copy code from other projects, whether by hand or through a
  tool. If a tool produces code that you recognise from another project, do not submit it.
- **Snippet scan.** CI runs a snippet scan (`snippet-scan` job) against a public database of open
  source code. A flagged match blocks the merge until it is explained in the pull request and a
  maintainer has decided how to handle it: rewrite it, or record it as an approved component in
  `scanoss.json` (`bom.include`). A match that is not copied code, an idiom that many projects
  write the same way, is removed for that file and that component only (`bom.remove` with
  `path` and `purl`); another component matching the same file still fails. A push to `main`
  scans the whole tree, so an update of the database can flag a file that passed before.
- **Human-written description.** Pull requests without a human-written description of what the
  change does and why are closed.
- **No automation at scale.** Fully automated or bulk AI-generated pull requests and issues are
  closed without review.

## Dependency license policy

Lajstrom is licensed under Apache-2.0 and must stay distributable under it. Direct and transitive
dependencies must use one of these licenses (SPDX identifiers):

| Allowed without discussion |
| --- |
| Apache-2.0, MIT, BSD-2-Clause, BSD-3-Clause, ISC, MPL-2.0, Unlicense, CC0-1.0, 0BSD |

Every other license is **denied by default** and needs an explicit maintainer decision, recorded
in the pull request that introduces the dependency. This includes, among others, GPL, LGPL, AGPL,
SSPL, BSL (Business Source License) and EUPL, as well as dependencies with no detectable license.

The `licenses` CI job enforces the allowlist with
[go-licenses](https://github.com/google/go-licenses). Prefer the standard library and few,
well-maintained dependencies.

When go-licenses cannot classify a license text that is on the allowlist, the module goes on the
`--ignore` list of the Makefile (`LICENSE_IGNORES`) and of `ci.yml`, with the verification
recorded here. The list is for unreadable texts of allowed licenses, never for denied ones.

| Module | License | Verified |
| --- | --- | --- |
| `modernc.org/mathutil` | BSD-3-Clause (the third clause says "the names of the authors" instead of "the name of the copyright holder", which the classifier scores below its threshold) | 2026-11-07, `LICENSE` in the module read by hand |

## Architecture decision records (ADRs)

Architectural changes need an ADR in [`docs/adr/`](docs/adr/), submitted in the same pull request as
the change or before it. "Architectural" means anything that changes one of the design principles,
the storage model, a public protocol or API, the security model (RBAC, audit), or adds a
significant dependency. See [docs/adr/README.md](docs/adr/README.md) for the format. If in doubt,
open an issue and ask.

## Pull requests

- **Small and focused.** One logical change per pull request. Large changes are easier to land as
  a series of small ones; open an issue first to agree on the approach.
- **Tests.** New behaviour comes with tests; bug fixes come with a test that fails without the fix.
- **Conventional Commits** style for commit subjects and the pull request title:
  `type(scope): summary`, where `type` is one of `feat`, `fix`, `docs`, `test`, `refactor`,
  `perf`, `build`, `ci`, `chore` or `revert`. Use the imperative mood and keep it under 72
  characters. Mark breaking changes with `!` (for example `feat(api)!: ...`).
- **Description.** Fill in the pull request template: what changes, why, and how it was tested.
- **History.** Branch protection requires a linear history; maintainers squash or rebase on merge.
  Rebase on `main` instead of merging `main` into your branch.
- **Review.** At least one maintainer approval and green CI are required to merge.

## Building and testing locally

You need Go (the current stable release is what CI uses) and Git.

```sh
make check   # the CI job list below: build-test, lint, vulncheck, licenses (plus gofmt)
make         # lists every target
```

`make check` runs the same commands as CI, with the same tool versions. The raw commands, if you
want to run one step on its own:

```sh
go build ./...
go vet ./...
go test -race ./...
```

Lint, vulnerability and license checks, as run in CI:

```sh
# golangci-lint (configuration in .golangci.yml)
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0 run

# known vulnerabilities in dependencies and the standard library
go run golang.org/x/vuln/cmd/govulncheck@latest ./...

# dependency licenses against the allowlist
go run github.com/google/go-licenses@v1.6.0 check ./... \
  --allowed_licenses=Apache-2.0,MIT,BSD-2-Clause,BSD-3-Clause,ISC,MPL-2.0,Unlicense,CC0-1.0,0BSD \
  --ignore modernc.org/mathutil
```

The integration tests are not part of `make check`: they need the dev stack
([docs/dev-stack.md](docs/dev-stack.md)), and CI runs them against both registries:

```sh
make dev-up
export LAJSTROM_TEST_POSTGRES_DSN='postgres://lajstrom:lajstrom@localhost:5432/lajstrom?sslmode=disable'
LAJSTROM_TEST_REGISTRY=localhost:5000 make test-integration   # integration (zot)
LAJSTROM_TEST_REGISTRY=localhost:5001 make test-integration   # integration (distribution)
```

The end-to-end scenarios, real clients against a server of their own, run with `make e2e`;
[docs/testing.md](docs/testing.md) describes every test level and how to add a scenario.

The snippet scan needs Python 3 and the ScanOSS client (it sends file fingerprints, not source code,
to the public OSSKB service):

```sh
python3 -m pip install scanoss==1.54.3
scanoss-py scan --output /tmp/scanoss-results.json .
scanoss-py inspect undeclared --input /tmp/scanoss-results.json
```

## CI overview

| Job | What it checks |
| --- | --- |
| `build-test` | `go build`, `go vet`, `go test -race ./...` |
| `changes` | whether the pull request changes a path that the integration tests depend on (below) |
| `integration (zot)`, `integration (distribution)`, `integration (zot-pgbouncer)` | `go test -race -tags integration ./...` against Zot or distribution, the store suite on SQLite and Postgres; the dev stack's images as service containers; `zot-pgbouncer` reaches Postgres through PgBouncer in transaction mode, a database per test |
| `integration-result` | both integration cells passed, or nothing needed them; the required check for the integration tests |
| `e2e` | `make e2e`: the end-to-end scenarios with real clients against Zot as a service container, in 6 minutes at most ([docs/testing.md](docs/testing.md)), the client images pulled once beforehand and the maven and gradle caches of `maven.sh` kept between runs (actions/cache); then `apt.sh` again with `APT_E2E_IMAGE` `ubuntu:22.04` and `debian:12`, in 3 minutes at most, and `cosign.sh` against distribution, a registry without the referrers API (#28) |
| `lint` | golangci-lint; `make api-lint`, vacuum on the OpenAPI document of the management API |
| `vulncheck` | govulncheck |
| `licenses` | dependency licenses against the allowlist |
| `snippet-scan` | ScanOSS match against public open source code, in 10 minutes at most; the client from PyPI (three bounded attempts, a pip cache), and an API that is unavailable or gives no result within 4 minutes makes a warning, not a failure |
| `dco` | `Signed-off-by:` on every commit of the pull request |
| `codeql` | CodeQL static analysis (separate workflow) |
| `CLAAssistant` | CLA signature (separate workflow) |
| `snapshot` | `goreleaser release --snapshot` without publishing, on pushes to `main` and on pull requests that change the release configuration (Release workflow, [docs/release.md](docs/release.md)) |

The integration cells run on every push to `main` and on a pull request that changes Go code
(`*.go`), `go.mod` or `go.sum`, `deploy/`, `.github/workflows/`, the `Makefile`,
`internal/index/migrations/` or `test/`. On any other pull request, a doc-only one for example,
they are skipped and CI takes as long as the other jobs. A job skipped by its condition counts
as passed for branch protection, but a skipped matrix job reports one check under its unexpanded
name instead of one per registry, so required cells would keep a doc-only pull request waiting.
Branch protection therefore requires `integration-result`, which passes when the cells passed or
were skipped because nothing needed them, and fails otherwise. The `e2e` job runs on the same
paths; it is one job, not a matrix, so its own check can be required.

## License

By contributing you agree that your contributions are licensed under the
[Apache License 2.0](LICENSE) and under the terms of the [CLA](CLA.md).

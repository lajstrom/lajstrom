# Testing

Lajstrom is tested at five levels, from pure functions to real clients against a running
server. The levels and where they live are fixed in [layout.md](spec/layout.md) §4; this page
says how to run each one, what it needs, and how to add an end-to-end scenario.

| Level | Command | Needs | In CI |
| --- | --- | --- | --- |
| Unit | `make test` | Go | `build-test` |
| Store conformance | `make test`; on Postgres with `LAJSTROM_TEST_POSTGRES_DSN` | Go; Postgres for its half | `build-test` (SQLite), `integration` (both) |
| Integration | `make test-integration` with `LAJSTROM_TEST_REGISTRY` | the dev stack | `integration (zot)`, `integration (distribution)` |
| OCI conformance | `test/conformance/run.sh` | a running Lajstrom, an `oci` repo | no |
| Fuzz | `go test -fuzz=<name>` | Go | the seed corpus, as part of `make test` |
| End-to-end | `make e2e` | the dev stack, Docker, `curl`, `jq` | `e2e` |

`make check` runs what every pull request must pass without the stack: gofmt, build, vet, unit
tests, lint, vulncheck, licenses. The integration, conformance and end-to-end levels need the
[dev stack](dev-stack.md) (`make dev-up`) and are not part of it.

## Unit

```sh
make test          # go test -race ./...
```

Next to the code, table tests, no network and no containers. Fixtures are built in the test
process with [internal/testutil](../internal/testutil) (a `.deb`, a `.whl`, a `.jar` in memory);
binary packages from distributions are never checked in.

## Store conformance

[internal/index/storetest](../internal/index/storetest) is one suite that every `index.Store`
passes. It runs on in-memory SQLite in `make test`, and on Postgres too when
`LAJSTROM_TEST_POSTGRES_DSN` is set; each run takes a schema of its own:

```sh
LAJSTROM_TEST_POSTGRES_DSN='postgres://lajstrom:lajstrom@localhost:5432/lajstrom?sslmode=disable' go test -race ./internal/index/...
```

## Integration

Tests behind `//go:build integration`, against a real registry: the provider, the facade,
ingest, reindex, apt-get against the apt frontend. `LAJSTROM_TEST_REGISTRY` is the registry's
`host:port`; without it `make test-integration` stops with a hint.

```sh
make dev-up
export LAJSTROM_TEST_POSTGRES_DSN='postgres://lajstrom:lajstrom@localhost:5432/lajstrom?sslmode=disable'
LAJSTROM_TEST_REGISTRY=localhost:5000 make test-integration   # Zot
LAJSTROM_TEST_REGISTRY=localhost:5001 make test-integration   # distribution
```

Every test works in repositories with a random name, so runs can repeat and run side by side.
`LAJSTROM_TEST_REGISTRY_PREFIX`, `_USERNAME`, `_PASSWORD` and `_TLS=1` point the provider tests
at a registry outside the stack ([registries/](registries/README.md) has the commands per
vendor); `LAJSTROM_TEST_REGISTRY_S3` adds the tests of a registry on S3 (the `s3` and
`s3-minio` profiles, [dev-stack.md](dev-stack.md)).

## OCI conformance

The conformance suite of the OCI distribution spec, against the facade of a running Lajstrom on
an `oci` repo. [test/conformance/README.md](../test/conformance/README.md) has the commands, the
variables and what passes.

## Fuzz

`FuzzEscape` and `FuzzValidate` (`internal/oci/profile`), `FuzzParse` and `FuzzControl`
(`internal/format/deb`). `go test` runs their seed corpus with the unit tests; fuzz one for a
while by hand:

```sh
go test -run '^$' -fuzz '^FuzzEscape$' -fuzztime 30s ./internal/oci/profile/
```

A crash leaves its input in `testdata/fuzz/<name>/`; commit it with the fix, it is a seed from
then on.

## End-to-end

```sh
make dev-up
make e2e                       # every scenario
make e2e E2E='raw oras'        # those two; test/e2e/run.sh raw oras is the same
```

[test/e2e/run.sh](../test/e2e/run.sh) builds `bin/lajstrom`, writes a configuration in a
temporary directory (a fresh SQLite index, the registry `local` at `$LAJSTROM_E2E_REGISTRY`,
by default `localhost:5000`, the Zot of the stack, the repos `raw/scratch`, `apt/ubuntu`,
`apt/e2e` and `pypi/e2e`, none readable anonymously, `auth.anonymousRead: true`, a master key of
its own, no metrics listener, no reconcile loop), runs `lajstrom migrate`, makes the user
`e2e-admin` with the admin binding and a random password, which the scenarios get as
`LAJSTROM_E2E_USER` and `LAJSTROM_E2E_PASSWORD`, starts `lajstrom serve` on a free port of
`127.0.0.1` and waits for
`/-/readyz`, at most 30 s. Then it runs every `test/e2e/<name>.sh` but `lib.sh` and `run.sh`,
the known ones in a fixed order and any other after them, and prints a line per scenario:

```text
=== summary
PASS raw.sh            3 s
PASS oras.sh           0 s
PASS auth.sh           1 s
PASS rbac.sh           4 s
PASS oci-token.sh      3 s
PASS tokens.sh        10 s
PASS apt.sh            5 s
PASS pypi.sh          13 s
e2e: 8 of 8 scenarios ran, 0 failed, in 47 s against http://127.0.0.1:45376
```

Those are the times on an Apple silicon Mac with OrbStack, the images already pulled; the 47 s
include `make build`, and `pypi.sh` installs build, twine and uv from PyPI in its container. A failed scenario does not stop the others. The harness exits 1 when one
failed, 2 when it could not start (the build, the migration, a server that is not ready), and
stops the server and removes the directory on exit; `LAJSTROM_E2E_KEEP=1` keeps it, with
`server.log` and a log per scenario. It unsets the `LAJSTROM_SERVER_*`, `LAJSTROM_LOG_*` and
`LAJSTROM_DATABASE_*` overrides of your shell, so the server always runs the file it wrote.

| Scenario | What it checks | Clients |
| --- | --- | --- |
| `raw.sh` | R-01 to R-04, a range, R-09, R-10, DELETE | curl |
| `oras.sh` | oras through `/v2`: the challenge without credentials, `oras login`, push with the raw profile, the repeat, another manifest under the tag (409), pull, tags, manifest fetch, the file on the raw frontend | oras on `PATH`, else its image |
| `auth.sh` | local users, HTTP Basic, disable, enable, set-password | curl, `lajstrom admin user` |
| `rbac.sh` | the S-02 matrix of the six principals on the raw frontend and on `/v2` (the tokens' actions included), a repo that may not be read answering as one that does not exist (S-08, X-05), the reader's `oras push` refused and the writer's through, a deploy token held to its repo (S-04), a binding and a repo's `anonymousRead` counting from the next request (S-07), creating a repo with a taken name | curl, oras on `PATH`, else its image, `lajstrom admin user` and `binding` |
| `oci-token.sh` | the docker bearer flow, oras login, push and pull, token key rotations | curl, oras image, `lajstrom admin key` |
| `tokens.sh` | personal access tokens and deploy tokens on raw, apt (`auth.conf`) and `/v2` | curl, `ubuntu:24.04`, oras image |
| `apt.sh` | a `.deb` by PUT and one by oras, `apt-get update`, `install` and `download` from the signed repo, the conflicts | `ubuntu:24.04` (`linux/amd64`), oras image |
| `pypi.sh` | `python -m build`, twine upload, the same files again, other bytes (409), `pip install`, `uv pip install`, `uv publish` and `--check-url`, `pip install --upgrade`, PEP 658 in the access log | `python:3.13` (build, twine, uv from PyPI) |

The images are `ubuntu:24.04` for `linux/amd64`, emulated on Apple silicon,
`ghcr.io/oras-project/oras:v1.3.6` and `python:3.13`; the first run pulls them. The containers use
`--network host`, which reaches the server's `127.0.0.1` on Linux and under OrbStack; Docker
Desktop is not measured (its host networking is a setting of its own). The scenarios run under
whatever `bash` is first on `PATH`; the bash 3.2 of macOS is enough.

### When a scenario fails

Each scenario sources [test/e2e/lib.sh](../test/e2e/lib.sh). An assertion that fails prints the
script, what was wrong and the lines that called it; any other command that fails where
`set -e` would stop the script (a `curl` that cannot connect, a `jq` that gets no JSON) is caught
by its ERR trap, which prints the command as written:

```text
FAIL raw.sh: R-01 GET Content-Length: got '4000', want '4001'
  at raw.sh:95: expect "R-01 GET Content-Length" 4001 "$(hdr content-length)"

FAIL raw.sh: exit 7 from: status="$(curl -sS --globoff "${how[@]}" -o "$out" -D "$work/headers.raw" -w '%{http_code}' "$@" "$base/raw/$repo/$path")"
  at raw.sh:47 in req
  at raw.sh:58 in put: req PUT "$encoded" -T "$file" -H 'Content-Type: text/plain'
  at raw.sh:91: put "$work/v1" "$file"
```

`run.sh` repeats those lines after the scenario, with the last 50 lines of the server log. bash
3.2 gives no line number for a failure inside a function: there the trap names the function
(`in req of raw.sh`) and the lines of its callers.

### Adding a scenario

One script per format or feature, `test/e2e/<name>.sh`, executable. `run.sh` picks it up by
itself; add its name to `order` in `run.sh` to place it among the others.

- **Standalone.** `test/e2e/<name>.sh [<base url> [<repo>]]`, with defaults that work against
  `make run` (`http://localhost:8080`), so that it runs by hand against any server. `run.sh`
  passes the base URL only: the defaults must name repos of its configuration, or the script
  creates what it needs with `lajstrom admin repo create` (as `tokens.sh` does). Say in the
  header what the server needs: the repos, the variables, the images.
- **The environment.** `LAJSTROM_CONFIG` (for `lajstrom admin`, which works on the server's
  database), `LAJSTROM_BIN`, `LAJSTROM_MASTER_KEY`, `LAJSTROM_SERVER_LOG` and
  `LAJSTROM_E2E_REGISTRY` are set by `run.sh`; fall back to `bin/lajstrom` and
  `deploy/compose/lajstrom.example.yaml` without them, as the others do. Standard input is
  `/dev/null`.
- **lib.sh.** `set -euo pipefail`, then `. "$(dirname "$0")/lib.sh"`. Print one line per step
  with `step` once it passed; check with `assert_eq WANT GOT WHAT`, `assert_status WANT <curl
  args>` and `fail`. Leave the ERR trap alone.
- **Unique names.** Every user, package, path and OCI repository is named after the run (a
  timestamp and `$RANDOM`), so that the script runs again against the same stack and beside
  other runs; the dev stack's Zot is shared.
- **Clean up.** A `trap … EXIT` that removes the temporary directory, deletes what can be
  deleted and disables the users it made; what cannot be deleted yet (apt versions) stays and
  is named after the run. A command of the cleanup that fails fails the scenario, after its
  `PASS` line: end the ones that may fail with `|| true`.
- **No secrets on a command line.** Passwords and tokens go to `curl -K`, to `--password-stdin`
  or into files; the trap prints commands as they are written, never their values.
- **Clients in containers.** Pin the image tag, use `--network host` and mount a directory of
  the run. A container that writes there runs as the caller, `-u "$(id -u):$(id -g)"`, or
  `chown`s what it wrote: on Linux, the CI runner, files root made in a directory of root's
  cannot be removed by the cleanup.
- **End with** `PASS <name> e2e against <base>` and exit 0.

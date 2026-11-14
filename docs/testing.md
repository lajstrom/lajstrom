# Testing

Lajstrom is tested at eight levels, from pure functions to real clients against a running
server. Five of them, unit, store conformance, integration, end-to-end and fuzz, and where they
live are fixed in [layout.md](spec/layout.md) §4; OCI conformance, upgrade and load are this
page's. It says how to run each one, what it needs, and how to add an end-to-end scenario.

| Level | Command | Needs | In CI |
| --- | --- | --- | --- |
| Unit | `make test` | Go | `build-test` |
| Store conformance | `make test`; on Postgres with `LAJSTROM_TEST_POSTGRES_DSN` | Go; Postgres for its half | `build-test` (SQLite), `integration` (both) |
| Integration | `make test-integration` with `LAJSTROM_TEST_REGISTRY` | the dev stack | `integration (zot)`, `integration (distribution)`, `integration (zot-pgbouncer)`; `integration (zot-yugabyte)` (the YugabyteDB workflow): nightly, on demand, and on a pull request that changes `internal/index`; not required |
| OCI conformance | `test/conformance/run.sh` | a running Lajstrom, an `oci` repo | no |
| Fuzz | `make fuzz`; `go test -fuzz=<name>` | Go | the seed corpus, as part of `make test`; `fuzz (<target>)` (the Fuzz workflow): each target for 10 minutes, weekly and on demand; not required |
| End-to-end | `make e2e` | the dev stack, Docker, `curl`, `jq` | `e2e`: `make e2e` against Zot (`ociproxy.sh` with distribution as its upstream), then `apt.sh` in `ubuntu:22.04` and `debian:12`, `cosign.sh` against distribution with and without the catalog, and `npm.sh` against distribution; `e2e (gitea)`: the scenarios that pass against Gitea ([below](#a-registry-with-credentials-and-a-prefix)), not required; `ha` (the HA workflow): `ha.sh` without H-01 on the ha profile, nightly, on demand, and on a pull request that changes ingest, the index, the facade, the profile or the scripts it runs; not required |
| Upgrade | `test/upgrade/run.sh` | the dev stack, Docker, `curl`, `jq`, `psql`; a release from v0.1.0 on | `upgrade` (the Upgrade workflow): on a pull request that changes a migration, and on demand; skipped until v0.1.0; not required |
| Load | `make load` | the dev stack, `oras`, `curl`, `jq`, `lsof`, 4 GiB of free disk (11 GiB with 8 uploads at once) | `load` (the Load workflow): nightly, on demand, and on a pull request that changes the load test; not required |

`make check` runs what every pull request must pass without the stack: gofmt, build, vet, unit
tests, lint, api-lint, vulncheck, licenses. The integration, conformance, end-to-end, upgrade and
load levels need the [dev stack](dev-stack.md) (`make dev-up`) and are not part of it.

## Unit

```sh
make test          # go test -race ./...
```

Next to the code, table tests, no network and no containers. Fixtures are built in the test
process with [internal/testutil](../internal/testutil) (a `.deb`, a `.whl`, a `.jar` in memory);
binary packages from distributions are never checked in.

The unit tests include the check of [guarantees.md](spec/guarantees.md) (#447,
[tools/guarantees](../tools/guarantees)): every guarantee that holds names a test, every other
names an issue, and every test an entry names exists and is not skipped unconditionally. Renaming
or deleting such a test fails `make test` with the guarantee's id, so the entry changes in the
same pull request. `make guarantees` prints a table of what each guarantee proves, for the release
notes.

## Store conformance

[internal/index/storetest](../internal/index/storetest) is one suite that every `index.Store`
passes. It runs on in-memory SQLite in `make test`, and on Postgres too when
`LAJSTROM_TEST_POSTGRES_DSN` is set; each run takes a schema of its own:

```sh
LAJSTROM_TEST_POSTGRES_DSN='postgres://lajstrom:lajstrom@localhost:5432/lajstrom?sslmode=disable' go test -race ./internal/index/...
```

Behind a pooler in transaction mode (PgBouncer, Odyssey) the schema, a `search_path` in the DSN,
does not reach the server connection that runs a statement. There,
`LAJSTROM_TEST_POSTGRES_ISOLATION=database` gives each test a database of its own: the DSN's user
must create and drop databases, and the pooler must route every database name to the server
(PgBouncer: `* = host=…`) and let in the roles the tests create (PgBouncer: `auth_user`).

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
`s3-minio` profiles, [dev-stack.md](dev-stack.md)), and `LAJSTROM_TEST_ECR_ENDPOINT` with
`LAJSTROM_TEST_ECR_REGISTRY` the ECR provider against moto (the `ecr` profile,
[registries/ecr.md](registries/ecr.md#tested-against-moto)). `LAJSTROM_TEST_REGISTRY_NODELETE` adds the
facade's test of a registry that refuses deletes, which the stack does not run (#61):

```sh
docker run -d --rm --name nodelete -p 127.0.0.1:5019:5000 -e REGISTRY_STORAGE_DELETE_ENABLED=false registry:3.1.2
LAJSTROM_TEST_REGISTRY=localhost:5001 LAJSTROM_TEST_REGISTRY_NODELETE=localhost:5019 \
  go test -tags integration ./internal/facade -run TestIntegrationDelete
docker stop nodelete
```

## OCI conformance

The conformance suite of the OCI distribution spec, against the facade of a running Lajstrom on
an `oci` repo. [test/conformance/README.md](../test/conformance/README.md) has the commands, the
variables and what passes.

## Fuzz

`FuzzEscape`, `FuzzUnescape` and `FuzzValidate` (`internal/oci/profile`), `FuzzParse`,
`FuzzInspector`, `FuzzParseControl` and `FuzzParseAr` (`internal/format/deb`), `FuzzWheelFilename`,
`FuzzCoreMetadata`, `FuzzZipEntries`, `FuzzSdistInspector` and `FuzzZipInspector` (`internal/format/pypi`), `FuzzPOM`
(`internal/format/maven`), `FuzzPackageJSON` and `FuzzTarballInspector` (`internal/format/npm`), and `FuzzParse` and
`FuzzInspector` (`internal/format/rpm`). Each checks properties of what the parser accepts (round trips,
idempotence), that what it refuses stays refused, and that what it allocates grows with the
input (`testutil.CheckAllocated`); `FuzzZipEntries`, that the scan of a zip directory refuses
what `zip.NewReader` refuses, that its count, which bounds it (#243), is never below what
`zip.NewReader` reads, and that it reads the members as `File.Open` does (#696).
`go test` runs their seed corpus with the unit tests; `make fuzz` fuzzes each for 30 s, one
after another. The [Fuzz workflow](../.github/workflows/fuzz.yml) fuzzes each of its targets for
10 minutes every Sunday and on demand, and keeps a crasher as the artifact
`crasher-<target>`; it lists fifteen of the seventeen, not yet the two of rpm. It is not a
required check.

```sh
make fuzz                                                    # every target, 30 s each
make fuzz FUZZTIME=10m FUZZ_TARGETS=./internal/format/maven:FuzzPOM
go test -run '^$' -fuzz '^FuzzEscape$' -fuzztime 30s ./internal/oci/profile/
```

The seed corpus in `testdata/fuzz/<name>/` is built in process from the `testutil` fixtures;
`TestFuzzCorpus` of each package writes it again:

```sh
LAJSTROM_FUZZ_CORPUS=write go test -run '^TestFuzzCorpus$' ./internal/...
```

A crash leaves its input in `testdata/fuzz/<name>/`; commit it with the fix and a regression
test, it is a seed from then on.

## End-to-end

```sh
make dev-up
make e2e                       # every scenario
make e2e E2E='raw oras'        # those two; test/e2e/run.sh raw oras is the same
```

[test/e2e/run.sh](../test/e2e/run.sh) builds `bin/lajstrom`, writes a configuration in a
temporary directory (a fresh SQLite index, the registry `local` at `$LAJSTROM_E2E_REGISTRY`,
by default `localhost:5000`, the Zot of the stack, with the `capabilities` of
`$LAJSTROM_E2E_CAPABILITIES` when set, e.g. `'{catalog: false}'`, the repos `raw/scratch`, `apt/ubuntu`,
`apt/e2e` and `pypi/e2e`, none readable anonymously, the upstream `npmjs` at
`$LAJSTROM_E2E_NPM_UPSTREAM`, by default `https://registry.npmjs.org/`, for the proxy repo of
`npm.sh`, the upstream `e2e-oci` of format `oci` at `http://$LAJSTROM_E2E_OCI_UPSTREAM`, by
default the address of the registry `local`, for the oci proxy repo of `ociproxy.sh`,
`auth.anonymousRead: true`, a master key of
its own, no metrics listener, no reconcile loop, `server.trustedProxies: ["127.0.0.1/32"]` so that the
per-address throttles are on and a scenario can name a client in `X-Forwarded-For`, which it
gets as `LAJSTROM_E2E_FORWARDED=1`, and `auth.bootstrap.tokenFile` naming a file of the
directory), runs `lajstrom migrate`, writes a random bootstrap token into that file, starts `lajstrom serve` on a free port of
`127.0.0.1` below the ephemeral range (on Linux a port that a client socket holds, even in
TIME_WAIT, looks free but cannot be bound, #372) and waits for
`/-/readyz`, at most 30 s; when the server is not ready by then, or exits before, the harness
prints the end of its log, whether it runs or its exit status, and what `/-/readyz` answers.
Once it is ready, it makes the first admin, `e2e-admin`, with `lajstrom admin bootstrap` and the
token, as an installation makes it ([auth.md](auth.md#bootstrap-the-first-admin)), and a random
password, which the scenarios get as `LAJSTROM_E2E_USER` and `LAJSTROM_E2E_PASSWORD`. Then it
runs every `test/e2e/<name>.sh` but `lib.sh` and `run.sh`, the known ones in a fixed order, any
other after them and `ha.sh` last, against the ha profile when it is up
([below](#the-ha-stack)), and prints a line per scenario. The sample is from a run before
`npm.sh`, `docker.sh`, `first-admin.sh`, `ociproxy.sh` and `retention.sh` existed; a whole run
now has sixteen lines, `npm.sh` after `maven.sh`, then `docker.sh`, `first-admin.sh`,
`ociproxy.sh` and `retention.sh`, which
`order` in `run.sh` does not name, and `ha.sh` last:

```text
=== summary
PASS raw.sh            1 s
PASS oras.sh           1 s
PASS auth.sh           1 s
PASS rbac.sh           4 s
PASS oci-token.sh      2 s
PASS cosign.sh         3 s
PASS tokens.sh        10 s
PASS apt.sh            8 s
PASS pypi.sh          28 s
PASS maven.sh         25 s
PASS ha.sh            92 s
e2e: 11 of 11 scenarios ran, 0 failed, 0 skipped, in 182 s against http://127.0.0.1:49348
```

and without the ha profile up, `make e2e E2E='raw ha'`:

```text
=== ha.sh
run.sh: the ha profile is not up (make dev-up PROFILES=ha): ha.sh skipped
=== summary
PASS raw.sh            1 s
SKIP ha.sh                  (the ha profile is not up)
e2e: 1 of 2 scenarios ran, 0 failed, 1 skipped, in 9 s against http://127.0.0.1:32281
```

Those are the times on an Apple silicon Mac with OrbStack, the images already pulled and the
maven caches warm; the 182 s include `make build`, and `pypi.sh` installs build, twine, uv and
poetry from PyPI in its container, which took 84 s in one run and 10 to 30 s in others. A failed scenario does not stop the others. The harness exits 1 when one
failed, 2 when it could not start (the build, the migration, a server that is not ready), and
stops the server and removes the directory on exit; `LAJSTROM_E2E_KEEP=1` keeps it, with
`server.log` and a log per scenario. It unsets the `LAJSTROM_SERVER_*`, `LAJSTROM_LOG_*`,
`LAJSTROM_DATABASE_*` and `LAJSTROM_BOOTSTRAP_TOKEN_FILE` overrides of your shell, so the server
always runs the file it wrote, and `LAJSTROM_ADMIN_PASSWORD`, which `lajstrom admin` refuses.
With `LAJSTROM_E2E_INDEX_DSN`, a Postgres DSN, the index is that database instead of a new SQLite
file: `run.sh` migrates it and leaves it on exit ([Upgrade](#upgrade) sets it).

| Scenario | What it checks | Clients |
| --- | --- | --- |
| `raw.sh` | R-01 to R-04, a range, a version deleted through the API and the others still served, R-09, R-10, the `maxFileSize` policy of a repo of its own (`413`), DELETE | curl, `lajstrom admin repo` |
| `oras.sh` | oras through `/v2`: the challenge without credentials, `oras login`, push with the raw profile, the repeat, a manifest created a year back (400, O-03), another manifest under the tag (409), pull, tags, manifest fetch, the file on the raw frontend, the delete; on an `oci` repo of the run's own, the push rows (#245) and one image under two tags, whose delete by one tag is 409 DENIED naming the other and by digest deletes both (#284) | oras on `PATH`, else its image |
| `auth.sh` | local users, HTTP Basic, disable, enable, set-password, the throttle of failed logins (X-04) and of `/v2/token` | curl, `lajstrom admin user` |
| `rbac.sh` | the S-02 matrix of the six principals on the raw frontend and on `/v2` (the tokens' actions included), a repo that may not be read answering as one that does not exist (S-08, X-05), the reader's `oras push` refused and the writer's through, a deploy token held to its repo (S-04), a binding and a repo's `anonymousRead` counting from the next request (S-07), creating a repo with a taken name | curl, oras on `PATH`, else its image, `lajstrom admin user` and `binding` |
| `oci-token.sh` | the docker bearer flow, oras login, push and pull, token key rotations | curl, oras image, `lajstrom admin key` |
| `cosign.sh` | a raw file pushed with oras and signed with `cosign sign --key` (a key pair of the run, no transparency log): `cosign verify`, `oras discover` and the referrers API with `artifactType` list the signature; cosign's legacy `sha256-<hex>.sig` signed and verified; another key refused; a reader verifies and its `cosign sign` is `DENIED`; with `LAJSTROM_E2E_REGISTRY=localhost:5001` (distribution, no referrers API) it checks the `sha256-<hex>` tag schema the provider wrote (#28); `lajstrom reindex --repo` finds the signatures as referrers, also with `LAJSTROM_E2E_CAPABILITIES='{catalog: false}'` (#37) | `ghcr.io/sigstore/cosign/cosign:v3.1.4`, oras on `PATH`, else its image, `lajstrom admin user` and `binding` |
| `docker.sh` | docker images on an `oci` repo of the run's own (#71): `docker login` as the writer and as a reader of the repo; two single-platform images from `docker import --platform`, pushed with `docker push`; `crane index append` into one multi-arch tag, an OCI index whose bytes the backend holds under its digest; `docker pull --platform` of each platform, and the file of that platform in the image; `docker buildx build --platform linux/amd64,linux/arm64 --push` with provenance, an index of both platforms and their attestation manifests, where the builder can push several platforms (the containerd image store, or a builder with another driver than `docker`), else skipped and said; `cosign sign` and `verify` of the index by its digest, and the referrers API listing the signature; the reader's pull through, its `docker push` and `crane tag` refused, a pull without credentials refused; a push row per tag in the audit log, by the writer, none by the reader | the docker CLI and daemon, with a configuration directory of the run's own; crane on `PATH`, else `gcr.io/go-containerregistry/crane:v0.22.1`; `ghcr.io/sigstore/cosign/cosign:v3.1.4`; `lajstrom admin user` and `binding` |
| `tokens.sh` | personal access tokens and deploy tokens on raw, apt (`auth.conf`) and `/v2` | curl, `ubuntu:24.04`, oras image |
| `first-admin.sh` | the bootstrap token of the first admin (#440), on servers of its own with new indexes: without a token file a warning and no token; with one, only its sha256 in the index and no log line with it, a wrong token `403`, the throttle `429`, `lajstrom admin bootstrap` and its audit event, `404` after it, a restart with the file warning; with `LAJSTROM_E2E_POSTGRES_DSN` and `psql`, two replicas on one new schema make one token and one admin, else that part is skipped | curl, jq, `lajstrom admin bootstrap`; `psql` for the replicas |
| `apt.sh` | a `.deb` by PUT and one by oras; the repo's `anonymousRead` on: `key.asc` into `/etc/apt/keyrings`, `apt-get update` (the `Packages` by-hash), `install` and `apt-cache policy` without credentials; off: the anonymous `apt-get update` fails with apt's 401, then with credentials in `/etc/apt/auth.conf.d` `update`, `install`, `download` and the sha256 of the files; a second `update` with `Acquire::By-Hash=no` (the `Packages` by name); a copy of the suite whose `Release` hashes are wrong fails `apt-get update` with apt's `Hash Sum mismatch`; the conflicts | `ubuntu:24.04` (`linux/amd64`), or the image of `APT_E2E_IMAGE`; oras image |
| `rpm.sh` | an rpm repo of its own through the API; two noarch packages by `rpmbuild`, one by PUT and one by oras with an epoch and a requirement; `repomd.xml` and its three documents against their checksums; `dnf install` with `repo_gpgcheck=1` and the repo's `key.asc` (or `repo_gpgcheck=0` when the server does not sign); `zypper refresh --gpg-auto-import-keys` and `install` (`RPM_E2E_ZYPPER_IMAGE=` skips it); other bytes under a NEVRA (409); a version deleted through the API and gone from primary and `dnf list`; the repo deleted with `?force=true`, made again, reindexed, the same `primary`, `filelists` and `other`, and `dnf install` from it with the new repo's key | Rocky Linux 9.7 and openSUSE Leap 15.6, pinned by digest (`linux/amd64`); oras image |
| `pypi.sh` | `python -m build`, twine upload, the same files again, other bytes (409), `poetry publish`, `pip install`, `uv pip install`, the client configurations of the quick start (`pip.conf` with `~/.netrc`, `[[tool.uv.index]]` for `uv add`, `[[index]]` in `uv.toml`, `poetry source add` for `poetry add`), `uv publish` and `--check-url`, `pip install --upgrade`, PEP 658 in the access log, the page asked for as PEP 691's `latest` (JSON and HTML) served as version 1, a wheel with a build tag beside its wheel and taken by `pip install --force-reinstall` (#726) | `python:3.13` (build, twine, uv, poetry from PyPI) |
| `maven.sh` | a maven repo and a deploy token through the API; `mvn deploy` of a jar with sources, javadoc and pom (M-01: every PUT 201, one manifest with four file layers), the files and their four checksums (M-07), a second version and `maven-metadata.xml` (M-03), `mvn dependency:get` into an empty local repository and a version range (M-02), the same version again (200) and with a changed jar (409) (M-04), a `.sha1` that disagrees with its jar (M-05), Gradle `publish` and resolve | `maven:3.9.16-eclipse-temurin-21`, `gradle:8.14.6-jdk21` (plugins from Maven Central, cached in the volumes `lajstrom-e2e-m2` and `lajstrom-e2e-gradle`) |
| `npm.sh` | an npm repo, a user (writer) and a deploy token through the API; npm: `npm login` with the password (the prompts answered in a pseudo-terminal), `whoami`, `publish` of an unscoped package (three versions, one `--tag beta`) and a scoped one, `install` into an empty project with an empty cache, `dist-tag add`, `ls`, `rm` and an install by tag, `deprecate` and its warning, then cleared; pnpm with the deploy token as `_authToken`: `whoami`, `publish` of an unscoped and a scoped package that depends on npm's, `add` from an empty store; yarn: `yarn npm login`, `whoami`, `publish`, `tag add`, `list`, `remove`, `add` from an empty cache; a proxy repo of the upstream `npmjs`: `npm install chalk@4.1.2` from an empty cache twice, each tarball fetched once (`proxy.fetch` in the audit log) and in the registry, none the second time; the M4 demo ([roadmap.md](roadmap.md)): a group of the hosted repo and the proxy, `registry=<group>` the one registry line, through which npm, pnpm and yarn each publish a scoped package that depends on `chalk@4.1.2` and install it from an empty cache (pnpm with the group's deploy token, which reads only), every `dist.tarball` under the group and nothing fetched again, and a public name published to the hosted repo hiding the upstream's versions but with `group.merge`; `npm audit` through the proxy, whose advisories come back without a deny list and not for the names of `upstream.deny`, which never leave the server (J-09, #514); `npm unpublish` as a writer (403), then as maintainer of a version and of a package; `npm dist-tag` and `deprecate` through the group, then the hosted repo deleted, made again and reindexed: every packument of the run as before; `npm logout` revokes its token. `NPM_E2E_UPSTREAM=` leaves the proxy out, and the group has the hosted repo only | `node:26.13.0-slim` (npm 11.20.0; pnpm 10.34.9 and yarn 4.18.1 installed from registry.npmjs.org) |
| `ociproxy.sh` | an `oci` proxy repo (#873, [ADR 0018](adr/0018-oci-proxy-repos-pull-through-from-any-registry.md), [the quick start](quickstart/oci-proxy.md)) of the upstream `e2e-oci`, which crane seeds with two indexes of `linux/amd64` and `linux/arm64`: `crane pull` of one platform fetches its two blobs and the backend has its manifest under its keep-tag but not the tag, the other platform completes the index and the backend has the tag, a second pull of both is served again and fetches no blob (the lines `a blob fetched from the upstream` of the server log and its access lines); `ctr images pull` with a `hosts.toml` mirror of the upstream's host at `/v2/oci/<repo>` and `override_path = true` fetches its platform's layer and both configs through the proxy, and a new containerd's pull is served again with nothing fetched; a group of `oci` repos (#848, [ADR 0019](adr/0019-oci-groups-one-member-per-repository-path.md)) of a hosted repo for `acme/*` and of the proxy for the rest: crane pushes to the hosted repo and pulls through the group, pulls the proxy's image and a new one through the group (two blobs fetched, `proxy.fetch` with `details.via` the group), and an upload through the group is `405` with `Allow: GET, HEAD` | crane on `PATH`, else `gcr.io/go-containerregistry/crane:v0.22.1` pinned by digest; containerd 2.4.2, the static release checked against its sha256, in `debian:12` with `--privileged` |
| `retention.sh` | `lajstrom admin retention` against the server's API (#72): an npm repo and a maintainer with a token through `lajstrom admin`, three versions published with curl as npm does and a dist-tag on the oldest; the lists; a dry run of `--keep-last 1` (exit status 2, the middle version only), `--apply` (deleted, the dist-tagged and the newest kept, an audit row with the job and the token), and a rerun with nothing to do | curl, `lajstrom admin` |
| `snapshot.sh` | the snapshots of the control plane (#76), on servers of their own with new indexes: a sealed snapshot of a SQLite index with a user, a PAT, a deploy token and an apt key, whose layer holds no secret; after it a file, a revoke and a user; the index lost, `lajstrom migrate`, `admin snapshot restore`, `reindex`: the files, the credentials (the revoked token again) and the key come back, the later user does not; a restore into a full index refused, `--force` restoring it; another master key refused; with `LAJSTROM_E2E_POSTGRES_DSN` and `psql`, the snapshot of the SQLite index restored into a new Postgres schema, else that part is skipped | curl, jq, `lajstrom admin`; `psql` for Postgres |
| `migrate-npm.sh` | moving packages from Verdaccio (#441, [migrating.md](migrating.md)), run only when the Verdaccio of the `verdaccio` profile answers (`make dev-up PROFILES=verdaccio`; `LAJSTROM_E2E_VERDACCIO`), else `SKIP`: `run.sh` logs its user in and configures the upstream `verdaccio` with the token; three private packages published to Verdaccio with curl as npm does, `latest` below the highest, a `next` and a deprecation; a proxy repo of it and a maintainer with a token; `lajstrom admin repo prefetch` of the dist-tags' versions, then `--all-versions`, then everything `held`, 7 `proxy.fetch` rows; the dist-tags, the deprecations, `dist.integrity` and the tarballs' bytes as Verdaccio has them; `upstream.offline`, a prefetch refused (409), Verdaccio stopped when `MIGRATE_E2E_VERDACCIO_CONTAINER` names its container; `npm install` from an empty cache resolved through the dist-tags, with Verdaccio's integrity; `PATCH {"kind": "hosted"}`: 5 journal ops, every packument with Verdaccio's dist-tags, deprecations, integrity and publish times, the same after the repo is deleted, made again and reindexed, and a publish to it | curl, `lajstrom admin`, `node:26.13.0-slim` |
| `ha.sh` | [acceptance.md](acceptance.md) §4 on the `ha` profile, two replicas behind nginx; run only when the profile is up ([below](#the-ha-stack)) | curl, oras on `PATH`, else its image, `ghcr.io/sigstore/cosign/cosign:v3.1.4`, `python3` or `python:3.13`, and the images of the scenarios it runs through the edge |

The images are `ubuntu:24.04` for `linux/amd64`, emulated on Apple silicon,
`ghcr.io/oras-project/oras:v1.3.6`, `ghcr.io/sigstore/cosign/cosign:v3.1.4`, `python:3.13`,
`maven:3.9.16-eclipse-temurin-21`, `gradle:8.14.6-jdk21`, `node:26.13.0-slim`, `debian:12` and,
without a crane on `PATH`, `gcr.io/go-containerregistry/crane:v0.22.1`; the first run pulls them,
and `ociproxy.sh` downloads containerd's release from GitHub in each run. `apt.sh` and `tokens.sh` take their apt client
from `APT_E2E_IMAGE` when it is set; `apt.sh` passes in `ubuntu:22.04`, `ubuntu:24.04` and
`debian:12`:

```sh
APT_E2E_IMAGE=debian:12 test/e2e/run.sh apt
```

`apt.sh` reads in the server's access log whether apt asked for the `Packages` by-hash or by name,
and as whom; run by hand without `LAJSTROM_SERVER_LOG` it leaves those checks out and says so.
It turns the `anonymousRead` of its repo on and off through the API, and sets it back on exit.

The containers use `--network host`, which reaches the server's `127.0.0.1` on Linux and under
OrbStack; Docker Desktop is not measured (its host networking is a setting of its own).
`docker.sh` needs the daemon itself to reach the server's `127.0.0.1`, which it does on Linux and
under OrbStack, and the daemon takes plain HTTP from `127.0.0.0/8` without configuration. The
scenarios run under whatever `bash` is first on `PATH`; the bash 3.2 of macOS is enough.

### A registry with credentials and a prefix

The registry `local` of the run may need credentials and an instance prefix, as Gitea's does
([registries/gitea.md](registries/gitea.md)): `LAJSTROM_E2E_REGISTRY_USER` names the user,
`LAJSTROM_E2E_REGISTRY_PASSWORD_FILE` a file with its password or token, and
`LAJSTROM_E2E_REGISTRY_PREFIX` the prefix, ending in `/` (#353). `run.sh` writes them into
`registries[].auth` and `registries[].prefix`, the password through the variable
`LAJSTROM_E2E_REGISTRY_PASSWORD`, and passes all four to the scenarios; `cosign.sh` asks the
backend below the prefix and with those credentials, through the backend's token realm when it
challenges for a bearer, and `first-admin.sh` writes them into the configurations of its own
servers, which were never ready without them (#800). Against the Gitea of `deploy/compose/gitea`, the scenarios that pass
there (Gitea serves no ranges, which `raw.sh` checks); CI runs the same as the job `e2e (gitea)`,
which is not required:

```sh
deploy/compose/gitea/gitea.sh up
LAJSTROM_E2E_REGISTRY=localhost:5009 LAJSTROM_E2E_REGISTRY_USER=lajstrom \
  LAJSTROM_E2E_REGISTRY_PASSWORD_FILE=deploy/compose/gitea/.work/gitea.token \
  LAJSTROM_E2E_REGISTRY_PREFIX=lajstrom-it/ test/e2e/run.sh oras auth rbac oci-token cosign tokens
deploy/compose/gitea/gitea.sh down
```

Forgejo, Gitea's fork, is the profile `forgejo` of the same compose file, on `localhost:5014`
([registries/forgejo.md](registries/forgejo.md)). It serves ranges, so `raw.sh` passes there
too. CI runs these seven against it as the job `e2e (forgejo)`, not required (#816). The job
then runs the provider's integration tests against Forgejo with
`LAJSTROM_TEST_REGISTRY_SPOOL_UNKNOWN_LENGTH=1`, without the two upload steps that read the
session's status, which Forgejo answers one byte ahead (#804):

```sh
deploy/compose/gitea/gitea.sh up forgejo
LAJSTROM_E2E_REGISTRY=localhost:5014 LAJSTROM_E2E_REGISTRY_USER=lajstrom \
  LAJSTROM_E2E_REGISTRY_PASSWORD_FILE=deploy/compose/gitea/.work/forgejo.token \
  LAJSTROM_E2E_REGISTRY_PREFIX=lajstrom-it/ test/e2e/run.sh oras auth rbac oci-token cosign tokens raw
```

### A registry that deletes nothing, or reads blobs whole

Some registries make Lajstrom answer otherwise on purpose (#352): where a registry deletes no
manifests, every delete is refused and nothing changes; where its blobs are read whole, a range
is answered with the whole blob. `serve` probes every registry before it listens and logs what
it found, so once the server is ready `run.sh` reads the registry `local`'s line `registry
capabilities` from its log, and the line `registry needs workarounds` when there is one, prints
them as a step and passes two of them to the scenarios (#800):

| Variable | From the log | Not the default |
| --- | --- | --- |
| `LAJSTROM_E2E_DELETE_MANIFEST` | `deleteManifest` | `false`: a delete is `405 UNSUPPORTED` through `/v2`, `501` through `/raw/` and the API, and nothing is deleted (Nexus's docker format; `capabilities.deleteManifest: false`) |
| `LAJSTROM_E2E_BLOBS_READ_WHOLE` | `blobsReadWhole` | `true`: a range is `200` with every byte and no `Content-Range` (Nexus's oci format) |

`raw.sh`, `oras.sh` and `cosign.sh` then want those answers, and check after each refused
delete that nothing changed; what they pushed stays in the registry. `auth.sh` and `rbac.sh` want
the refusal where a delete passes the authorization (`501` on `/raw/`, `405 UNSUPPORTED` on
`/v2`), and `retention.sh` an `--apply` that exits 1 with the version an error naming the
registry, every version left and no audit row of a delete; it leaves out the second `--apply`. The refusals run against
Zot too, with `LAJSTROM_E2E_CAPABILITIES='{deleteManifest: false}'`. When the server logged no
capabilities (its probe failed at the start), the variables stay as your shell set them; a
scenario run by hand takes them from there, and an unset one as Zot's. Against the Nexus of
`deploy/compose/nexus` ([registries/nexus.md](registries/nexus.md)), the password in a file:

```sh
deploy/compose/nexus/nexus.sh up
printf lajstrom > nexus.pw
LAJSTROM_E2E_REGISTRY=localhost:5008 LAJSTROM_E2E_REGISTRY_USER=lajstrom LAJSTROM_E2E_REGISTRY_PASSWORD_FILE=nexus.pw \
  test/e2e/run.sh raw oras cosign auth rbac retention first-admin   # the docker format: deleteManifest=false
LAJSTROM_E2E_REGISTRY=localhost:5015 LAJSTROM_E2E_REGISTRY_USER=lajstrom LAJSTROM_E2E_REGISTRY_PASSWORD_FILE=nexus.pw \
  test/e2e/run.sh raw oras cosign auth rbac retention first-admin   # the oci format: blobsReadWhole=true
deploy/compose/nexus/nexus.sh down
```

### The ha stack

`ha.sh` runs the scenarios of [acceptance.md](acceptance.md) §4 against the `ha` profile of the
dev stack ([dev-stack.md](dev-stack.md#the-ha-profile)): two replicas, A and B, round robin behind
nginx with [deploy/nginx/lajstrom.conf](../deploy/nginx/lajstrom.conf), on one Postgres and one
distribution. `make e2e` runs it only when docker compose shows the profile's `ha-nginx` running,
against `http://localhost:8090` and not against the server of the run; otherwise the summary says
`SKIP ha.sh`, which is not a failure. By hand:

```sh
make dev-up PROFILES=ha
test/e2e/ha.sh                       # every step
HA_SUITE=none test/e2e/ha.sh         # without H-01, the scenarios of this page through the edge
HA_SUITE='raw oras' test/e2e/ha.sh   # H-01 with those two only
```

The `e2e` job of CI has no ha profile, so `ha.sh` is skipped there. The
[HA workflow](../.github/workflows/ha.yml) runs it on a runner of its own, in about 6 to 8
minutes: `make dev-up PROFILES=ha`, then `HA_SUITE=none make e2e E2E=ha`. It runs nightly, on
demand, and on a pull request that changes `internal/ingest`, `internal/index`,
`internal/facade`, the profile (`deploy/compose/compose.yaml`, `deploy/compose/ha/`,
`deploy/nginx/`), `ha.sh`, `lib.sh`, `run.sh` or the workflow. It is not a required check; when
it fails, the whole logs of the profile's services are kept as the artifact `ha-logs`.

It reaches A and B past the edge on ports 8091 and 8092, the backend on 5007, and the containers
with `docker compose` in the project of `COMPOSE_PROJECT_NAME` (`lajstrom-dev` by default), on
the files of `COMPOSE_FILE` when it is set: `lajstrom admin` in A, `psql` in `ha-postgres`, the
logs, the kills, stops and restarts of the replicas, `ha-postgres` and `ha-distribution`. On a
machine where others share `lajstrom-dev`, run the profile in a project of its own: other ports
(`LAJSTROM_HA_PORT`, `LAJSTROM_HA_A_PORT`, `LAJSTROM_HA_B_PORT`, `LAJSTROM_HA_DISTRIBUTION_PORT`)
and an override file with another subnet for the network `ha`, the replicas' addresses on it,
an image name of its own and a copy of
[deploy/compose/ha/lajstrom.yaml](../deploy/compose/ha/lajstrom.yaml) whose `trustedProxies` is
nginx's new address; `ha.sh` reads that address from nginx's container. For a project
`ha-mine`, with `COMPOSE_FILE` naming compose.yaml and the override and the ports exported:

```sh
docker compose -p ha-mine --profile ha up -d --build --wait ha-nginx   # the profile's services only
COMPOSE_PROJECT_NAME=ha-mine HA_SUITE=none make e2e E2E=ha
docker compose -p ha-mine --profile ha down -v                          # that project only
```

Its steps, each naming the ids it covers:

| Step | What it checks |
| --- | --- |
| across replicas | a repo created through B counts on A; a raw `PUT` through A is the same bytes through B; `oras push` through A, `oras pull` through B; a bearer token from A's `/v2/token` accepted by B (H-12) |
| control plane | a reader binding made through A lets the user read through B at the next request, deleted through B it stops on A; a repo's `anonymousRead` turned on through A counts on B, off through B counts on A (S-07, G-02) |
| client address | a change through nginx with a forged `X-Forwarded-For`: the access log's `remoteAddr` and the audit row's `ip` are the address nginx saw, with nginx as `proxy`; past the edge, the peer (ADR 0011) |
| H-02 | the same 100 MiB `PUT` through A and B at once: both 201 with one digest, one version, one tag in the backend |
| H-03 | 20 times, one tag pushed with `oras` through A and B at once as two manifests (the same file, another created time): one 201 and one 409 `TAG_INVALID` naming the winner, whose digest the backend and the index hold |
| H-04 | a blob upload begun on A, two chunks `PATCH`ed to B, finished on A: 201, and the blob on B |
| H-09 | reindex on A and B at once: 202 and 409 `CONFLICT` |
| H-13 | a wheel uploaded through A; its simple page asked on A and B at once: the same bytes and `ETag`, one `index_cache` row |
| #28 | two `cosign sign`, through A and B at once, of one subject on distribution, which has no referrers API: `oras discover` lists both, `cosign verify` passes with each key, the backend has the `sha256-<hex>` tag |
| #129 | 200 requests to `/v2/token` at once from one address: nginx answers those over 20 a second (a burst of 40) 429 with `Retry-After: 1`, the replicas none |
| H-05 | a 500 MiB `PUT` to A, and A killed with `SIGKILL` once 100 MiB are sent: curl's connection error; the same `PUT` through the edge 201, its digest in the index and its file in the backend; A started again with nothing in `/tmp`, where it spools; one version, no tag claim left, and a reindex of the repo with prune that adds, removes and reports nothing |
| H-06 | `ha-postgres` stopped, and started again once each replica, past the edge, has been asked for the wheel of H-13, a raw `PUT` and `/-/readyz`: 503, 503 and 503; meanwhile three loops run: the requests of a `pip install` and raw `PUT`s of a new file each through the edge, and A's `/-/readyz`, each answered 200, 201 or 503, and 200 or 201 again once both replicas serve, without a restart of A or B; each failed `PUT` is 201 again, and the index is whole as in H-05 |
| H-07 | the same with `ha-distribution`: 502 or 503 during the outage, `readyz` not checked (the registry answered within the last minute); the replicas serve again once the circuit of each has closed (30 s) |
| H-08 | a maven deploy of a jar and a pom, each with `.sha1` and `.md5`, then `maven-metadata.xml` and its checksums, each `PUT` to the other replica: all 201; the session is an `open` row of the database before the metadata `PUT` and `committed` after it; the replica that took the metadata logged the commit, the other did not; the jar is the same through the other |
| H-11 | an apt repo of the run; a `.deb` uploaded through A, `apt-get update` through B lists it; a second `.deb` through A, and `apt-get update` through B less than a second after the upload's 201 (the access log lines of both) lists it too (A-09) |
| H-10 | the requests of a `pip install` (the simple page and the wheel) in a loop through the edge while A restarts, and then B: every one 200 |
| H-01 | the scenarios of this page through the edge, with `lajstrom admin` in A and the logs of both replicas as the server log, and both replicas serving them; `auth.sh` goes to A past the edge, since the throttle of failed logins counts per replica ([auth.md](auth.md#failed-logins)) and the 11th wrong password of X-04 through the round robin is the 6th of either replica |

While Postgres is out, a request that needs the index answers 503 with `Retry-After: 5`
(`index.ErrUnavailable`, [#560](https://github.com/lajstrom/lajstrom/issues/560)), and H-06
takes nothing else. While the registry is out, its name does not resolve on the network `ha`,
and a request that needs it answers 503 before the circuit opens as after
(`provider.ErrUnavailable`, [#561](https://github.com/lajstrom/lajstrom/issues/561)); H-07
takes 502 or 503 and nothing else.

### A proxy of npmjs.org

[test/e2e/manual/npm-proxy.sh](../test/e2e/manual/npm-proxy.sh) installs a public package with
its whole tree through a proxy repo of registry.npmjs.org ([proxy-and-group.md](spec/proxy-and-group.md),
#310). It needs the network, so it lives below `manual/`, which `make e2e` does not run; in
`make e2e`, `npm.sh` installs a smaller tree, `chalk@4.1.2` (6 packages), through a proxy of the same
upstream. It needs `npm` on `PATH` and the registry of the dev stack:

```sh
make dev-up
test/e2e/manual/npm-proxy.sh                      # jest@29.7.0
test/e2e/manual/npm-proxy.sh express @babel/core  # any package specs
```

It serves `bin/lajstrom` as `run.sh` does, with the upstream `npmjs` (`$NPM_PROXY_UPSTREAM`,
`https://registry.npmjs.org/` by default), a proxy repo `npm/npmjs-<run>` readable anonymously,
debug logs and the metrics listener. Then it runs `npm install` twice, each time into an empty
project with an empty npm cache, and counts from the access log and the metrics: the first
install must fetch each tarball once from the upstream, the second must ask the upstream for
none, the tarballs coming from the registry. What it pushed stays in the registry under
`npm/npmjs-<run>/`. On 2026-11-10, npm 11.20.0 on Node 26, against the Zot of the dev stack:

```text
install 1: jest@29.7.0: 267 packages in 11 s; Lajstrom answered 268 tarball requests for 266 tarballs; the upstream was asked for 266 tarballs and 261 packuments; the proxy fetched 266
install 2: jest@29.7.0: 267 packages in 3 s; Lajstrom answered 268 tarball requests for 266 tarballs; the upstream was asked for 0 tarballs and 0 packuments; the proxy fetched 0
```

The tree has 261 names, and the upstream was asked once for each: npm asks for a packument
again before the first answer is in, and the requests that miss one document at once share one
upstream request (#383; before it, 270).

npm asks for one tarball (`semver`) three times, once per place in the tree. The second install
asks the upstream for no packument either: the packuments are still within `metadataTTL`.

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

## Upgrade

```sh
make dev-up
export LAJSTROM_UPGRADE_POSTGRES_DSN='postgres://lajstrom:lajstrom@localhost:5432/lajstrom?sslmode=disable'
test/upgrade/run.sh                                   # the previous release's whole suite
LAJSTROM_UPGRADE_FROM=<ref> test/upgrade/run.sh raw oras pypi
```

[test/upgrade/run.sh](../test/upgrade/run.sh) runs the binary of the release before on the
schema the next one migrated, as during a rolling update and after a rollback
([index-schema.md](spec/index-schema.md#upgrades), INV-41 of [guarantees.md](spec/guarantees.md)).
It checks the previous release out in a git worktree and builds both binaries. In a schema of its
own in the database of `LAJSTROM_UPGRADE_POSTGRES_DSN`, the previous binary migrates, then this
checkout's, which applies the next release's migrations and refuses one that breaks the release
before, then the previous one again, which must run on what it finds. Last, the previous
release's `test/e2e/run.sh` runs with `LAJSTROM_E2E_INDEX_DSN` on that schema: that release's
binary and scenarios, those of the arguments or all. On exit the schema is dropped and the
worktree removed; the exit status is `run.sh`'s, or 2 when a step before it failed.

The previous release is the highest tag `vX.Y.Z` from v0.1.0 on that is an ancestor of `HEAD`;
`v0.0.1` reserved the name before there was any code. Without such a tag the script says so and
exits 0. `LAJSTROM_UPGRADE_FROM` names any other ref whose `run.sh` knows
`LAJSTROM_E2E_INDEX_DSN` (#485); an older one cannot run. `TestUpgrade` (`internal/index`)
checks the same rules on every pull request with the migration sets of four simulated releases;
this script runs the real previous release. The [Upgrade workflow](../.github/workflows/upgrade.yml)
runs it on a pull request that changes a migration, and by hand with a ref to upgrade from.

## Load

```sh
make dev-up
make load                                  # 1 GiB, 1 upload at a time, 8 downloads at once
LAJSTROM_LOAD_UPLOADS=8 make load          # 8 uploads at once too; 11 GiB of free disk
LAJSTROM_LOAD_SIZE_MIB=256 make load       # a smaller object, for a smaller disk
```

[test/load/run.sh](../test/load/run.sh) serves `bin/lajstrom` as `make e2e` does, pushes one
object of random bytes through a raw repo (`curl -T`) and through `/v2` (`oras push`),
`LAJSTROM_LOAD_UPLOADS` (1) times at once each way, downloads it with `LAJSTROM_LOAD_PARALLEL`
(8) clients at once each way (`curl`, `oras blob fetch --output -`), checks every sha256, and
samples the server's resident set, its Go heap and the temporary files it holds open every
0.2 s. It exits 1 when the peak resident set of a phase passes `LAJSTROM_LOAD_MAX_RSS_MIB`
(256), 2 when a transfer fails. It is not part of `make e2e`: it moves 18 GiB (32 GiB with 8
uploads) and needs 3 + 1 GiB of free disk at the peak for one upload at a time, 10 + 1 GiB for 8,
which it checks before it starts. On exit it deletes what it pushed from the registry and
removes its files. The [Load workflow](../.github/workflows/load.yml) runs it nightly and on
demand with 8 uploads at once; `LAJSTROM_LOAD_SUMMARY` names a file, such as the job summary, to
append the table to as Markdown. The method and the numbers are in
[operations.md](operations.md#performance).

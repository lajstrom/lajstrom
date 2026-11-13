# Changelog

All notable changes to Lajstrom are in this file. The format is
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html). Before 1.0, a minor version may
change the HTTP API, the configuration and the CLI; the notes of each release say what changed
and how to upgrade ([README](README.md#our-promise-to-users)).

## [Unreleased]

<!-- at tag: rename to [0.1.0] - 2026-12-04, add the commits merged since 20ce9499, point the links at the bottom at v0.1.0 -->

The first release with functionality, planned as v0.1.0 for 2026-12-04 (#59). The notes of the
release are drafted in [docs/releases/v0.1.0.md](docs/releases/v0.1.0.md). This list was generated
from the `feat`, `fix` and `perf` commits since v0.0.1, and the `build` and `ci` commits that an
operator notices, then cut down to what an operator sees; it covers `main` of 2026-11-13, up to
20ce9499.

### Added

- **OIDC login and group bindings** (#69, M6): one OpenID Connect provider (`auth.oidc`) logs
  people in through the browser (`/-/oidc/login`, authorization code flow with PKCE) or the
  terminal (`lajstrom login`, a device flow Lajstrom serves itself); a login creates the user at
  first and mints a personal access token that every client uses; the IdP's groups become
  `group:<name>` principals that bindings name, replaced at each login and lapsing with the
  login's token.
- **Formats**, hosted repositories with Lajstrom's own protocols on the server, no client plugins:
  - raw files: `curl -T` in, `curl` out, `DELETE` of a path; every upload a version, addressed by its digest;
  - pypi: `twine upload` and `uv publish`; the simple index as PEP 503 HTML and PEP 691 JSON (`latest` too), with PEP 658 metadata for wheels; a wheel's build tag is a variant of its own;
  - apt: `.deb` upload with `curl` or `oras push`; signed `Release`/`InRelease`, by-hash indexes; `binary-all` beside the concrete architectures;
  - maven: `mvn deploy` and Gradle `publish` through publication sessions; `maven-metadata.xml` and all four checksums; a checksum file that disagrees keeps the version from being published;
  - npm: `npm publish` (and pnpm's and yarn's), `install`, `dist-tag`, `deprecate` and `unpublish`; `npm login`, `whoami`, `search`; scoped packages;
  - rpm (#64, M6): an `.rpm` in with `curl -T` or `oras push`, binary and source packages; `dnf`, `yum` and `zypper` install from repodata (`repomd.xml`, primary, filelists, other) signed with the repo's key, whose keyring is served for `gpgkey=`.
- **Every upload streams into the registry** (ADR 0014): the uploads of all six formats and a
  proxy repo's fetch take no local disk. Only a `/v2` chunk of unknown length that
  `registries[].uploads.spoolUnknownLength` spools does, bounded by `server.spoolBudget`.
- **Proxy and group repos**, for npm and oci:
  - an npm proxy repo of an upstream of `upstreams[]`, npmjs.org for example: a file is fetched on the first request, once across replicas, checked, and kept in the registry as any other artifact; documents are cached with a TTL, misses with a shorter one, and an `offline` repo serves what it has; allow and deny patterns per repo; `npm audit` is forwarded to the upstream;
  - an npm group repo: one URL for up to 16 repos in order, at most one of them hosted, which takes the writes;
  - an `oci` proxy repo (ADR 0018, #442, M5): a pull-through cache of any registry of the distribution API, Docker Hub with its `library/` names and rate limits, ghcr.io, quay.io, registry.k8s.io; a blob is fetched once across replicas, streamed to the first client while it goes into the registry, and resumed with `Range` when the upstream's connection breaks; the upstream's tags follow where the backend lets a tag move; credentials from a file or the environment, from IAM for an upstream on ECR, from the GCP metadata server for Artifact Registry;
  - a group of `oci` repos (ADR 0019): read-only, one member per repository path by route patterns;
  - prefetch (#441, #886): `POST …/prefetch` and `lajstrom admin repo prefetch` fetch a list of npm names, or of images with their platforms, through a proxy repo, with a report and `--failed` to resume;
  - moving from another registry (#441): a proxy of the old registry, a prefetch, then `PATCH {"kind": "hosted"}` turns the proxy into a hosted repo with the dist-tags and deprecations it served; a proxied version keeps its upstream publish time ([docs/migrating.md](docs/migrating.md)).
- **OCI facade** on `/v2` in front of the registry:
  - a token server of its own (the docker token flow);
  - `oci` pass-through repos: `docker push` and `pull`, multi-arch indexes made by buildx or `crane`, with roles and an audit row per push (#71);
  - the referrers API with the tag-schema fallback;
  - cosign signatures, SBOMs and attestations through the one write path, audited, serialised per subject across replicas;
  - `DELETE` of a manifest, its referrers first; a delete by tag of a manifest that other tags still name is refused.
- **The registry is the only storage.** Every artifact is an OCI artifact in the registry you run. The index (SQLite or Postgres) is rebuilt from the registry by `lajstrom reindex`.
- **Registries:**
  - any registry of the OCI distribution spec, with a page per registry in [docs/registries](docs/registries/README.md): distribution, Zot, GHCR, GitLab, Harbor, Gitea, Forgejo (a column and a CI job of its own, told from Gitea by the vendor table), Nexus, JFrog Container Registry, Project Quay;
  - Amazon ECR, `registries[].type: ecr`: the token from the AWS credential chain, refreshed before it expires; repositories created with the tag mutability and encryption of `registries[].ecr`; measured on a live ECR on 2026-11-13 (#19), where four of its answers needed fixes; a warning at start about a lifecycle policy that would expire Lajstrom's tags;
  - eventually consistent registries (#446): a publish is indexed only once the registry serves what was pushed (`ingest.visibilityTimeout`, then `503` naming what it does not serve yet); reindex removes a row only after two `MANIFEST_UNKNOWN` answers 10 minutes apart and past `reconcile.graceAge`;
  - OCI names in the 1.0 grammar of the distribution spec, without the words of Zot's layout or Harbor's routes (ADR 0017); a package stored under its earlier name keeps that repository; `registries[].prefix` is checked against the same grammar;
  - a registry's limits checked before a write: the depth of names (`capabilities.maxNameDepth`; Docker Hub 1, GitLab 4), `maxBlobSize` (GHCR's 10 GB), the chunk size (GHCR's 4 MiB, `registries[].uploads.chunkSize`); a repo on a registry that takes none of its names is refused when it is created;
  - registries without a catalog: `lajstrom reindex --attach` names the OCI repositories of a repo;
  - `lajstrom registry probe` measures what a registry supports, ECR included: routing words in names, the lag of writes and deletes, monotonic reads over fresh connections.
- **Authentication:**
  - the first admin from a one-time bootstrap token that the operator mounts as a file (`auth.bootstrap.tokenFile`), exchanged with `lajstrom admin bootstrap` or `POST /api/v1/bootstrap`; or `lajstrom admin user create --admin` against the database;
  - local users with an immutable uid (UUID v7), rename and delete;
  - argon2id passwords, from standard input, a prompt or `--password-file`, never from the environment;
  - personal access tokens and deploy tokens with scopes and expiry;
  - throttles of failed logins per account and per address;
  - `server.trustedProxies` for the client address behind an edge.
- **Authorization:**
  - roles `reader`, `writer`, `maintainer`, `admin` bound to repo masks, for users and for OIDC groups;
  - anonymous read per repo;
  - the 401/404/403 rule, so that a denial does not reveal a repo.
- **Management API** `/api/v1`: repos (hosted, proxy, group), users, bindings, tokens, audit log, `whoami`; `DELETE` of a version, every variant or one; reindex of a repo. A repo's packages and versions a page at a time, one version with its variants and files, and `GET /api/v1/info` for a client before sign-in. A conditional delete (`If-Match`, `If-Unmodified-Since`; a version an npm dist-tag names only with `?force=true`) and a batch delete, `…/versions:delete`, with a result per item. A 401 has the code `UNAUTHENTICATED`. An OpenAPI 3.1 document is served at `/api/v1/openapi.yaml`.
- **Retention** (#72): `lajstrom admin retention` keeps the last N versions and drops those older than a maximum age, per repo and package pattern, never a version a dist-tag names; a dry run unless `--apply`; a client of the API that a CronJob runs, each deletion audited with the job's id.
- **Repo policy** in the API and in `repos[].policy`: `maxFileSize` (also for `/v2` pushes), `blobRedirect`, `maven.sessionTimeout` and `maven.maxSessionFiles` (256). A file is bounded by default: npm 512 MiB, pypi and maven 4 GiB, apt, raw and rpm 8 GiB, oci 16 GiB, or the registry's `maxBlobSize` when smaller.
- **Audit log** of every write and control-plane change. The row of a registry write is written before the write and completed with its outcome: `ok`, `failed`, or `unknown` when the outcome could not be recorded (`GET /api/v1/audit?state=`). A change made with `lajstrom admin` names the host it ran on.
- **CLI:**
  - `serve`, `reindex`, `migrate`, `version`;
  - `admin` (`user`, `binding`, `token`, `repo`, `key`, `deb`, `rpm`, `bootstrap`, `retention`; `repo prefetch`);
  - `login`, the device flow of the OIDC login;
  - `healthcheck`, for the distroless image, over TLS too;
  - `registry probe`.
- **Operations:**
  - Prometheus metrics on a listener of their own; `/-/healthz` and `/-/readyz`;
  - registry requests retried when idempotent, and a circuit breaker per registry (503 with `Retry-After`); 503 with `Retry-After` while the database is unreachable;
  - `server.stallTimeout`; `server.maxConcurrentUploads` (32); `server.uploadSessions`, the live `/v2` upload sessions of a principal, with metrics of the refusals;
  - a drain on SIGTERM, SIGINT and SIGHUP: readiness answers 503 and the replica keeps serving for `server.shutdownDelay` (5 s) before the listener closes;
  - the replica's clock compared with the database's (`lajstrom_clock_skew_seconds`; not ready past 60 s), and before `reindex` and `admin` too;
  - a second `serve` on one SQLite file refused;
  - migrations expand or contract: a binary refuses a schema newer than it can run on, and a migration that would break the release before unless `lajstrom migrate --allow-breaking`; `serve` can run as a Postgres user with data rights only;
  - Postgres through PgBouncer in transaction mode, tested in CI; YugabyteDB experimental, in a CI cell that does not block;
  - reindex pushes again a referrers index that lacks referrers, on a registry without the referrers API, and reports the versions whose OCI repository the registry no longer knows;
  - a request whose client went away is logged as 499, not as an error;
  - TLS in Lajstrom or at the edge;
  - the master key from an environment variable or a file;
  - rotation of the apt and rpm signing keys and of the token signing key.
- **Release artifacts:**
  - binaries for linux and darwin on amd64 and arm64;
  - a multi-arch distroless image on ghcr.io and Docker Hub;
  - SBOMs and cosign signatures;
  - a Homebrew cask;
  - the Helm chart as an OCI artifact, `oci://ghcr.io/lajstrom/charts/lajstrom`, signed, its
    version the release's.
- **Deployment:**
  - a production Docker Compose example;
  - Kubernetes manifests with a migration Job and a Gateway API route, and a Helm chart of them
    (#82) with the migration Job before each upgrade, an optional generated bootstrap token, a
    Job that recovers a lost password, an HTTPRoute or Ingress, a ServiceMonitor and an
    autoscaler, under the restricted Pod Security Standard;
  - nginx and Caddy in front, a backup and restore runbook;
  - guides for Postgres poolers, CloudNativePG and YugabyteDB ([databases.md](docs/deploy/databases.md)),
    and for Talos Linux nodes that pull through Lajstrom ([talos.md](docs/guides/talos.md)).

### Security

- `server.externalURL` is required and must be `https://`; an `http://` URL is accepted only on a loopback host with `server.insecureHTTP: true`, for the dev stack. A configured URL keeps the token realm and audience away from the `Host` that the client sends. `server.insecureHTTP` on an address beyond loopback logs a warning.
- Passwords and token secrets are never logged; tokens are stored as sha256 hashes and shown once. The bootstrap token is the operator's: only its hash is stored, it is valid while no admin exists and for `auth.bootstrap.ttl` (24 h), and wrong tries are throttled per address.
- The OIDC login takes ID tokens with asymmetric algorithms only, keeps its state and device codes as hashes and redeems each once; its pages run no script, under a CSP of `default-src 'none'`.
- What a client uploads is read within bounds: the directory of a zip, what an sdist unpacks to, the blocks of a `control.tar.xz`, the head of an rpm, the `${project.*}` references of a POM, the layers of a manifest, the scopes of a token request.
- A blob's digest is checked while it streams through `/v2`.
- A version pushed on `/v2` whose `created` time is more than `server.maxCreatedAhead` (5 min) after the server's clock, or `server.maxCreatedBehind` (1 h) before it, is refused, so that a writer cannot age a version into or out of a retention rule.
- Credentials are never sent over a redirect to `http`, and a bearer token realm must be on the registry's host or in `registries[].tokenRealmHosts`. A redirect of an `oci` proxy repo's upstream to another host is followed over https to public addresses only: never to loopback, a private network or a cloud metadata endpoint.
- Security headers on every response; no cross-origin writes.

## [0.0.1] - 2026-11-05

The name, reserved. No functionality.

[Unreleased]: https://github.com/lajstrom/lajstrom/compare/v0.0.1...HEAD
[0.0.1]: https://github.com/lajstrom/lajstrom/releases/tag/v0.0.1

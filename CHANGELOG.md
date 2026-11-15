# Changelog

All notable changes to Lajstrom are in this file. The format is
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html). Before 1.0, a minor version may
change the HTTP API, the configuration and the CLI; the notes of each release say what changed
and how to upgrade ([README](README.md#our-promise-to-users)).

## [Unreleased]

<!-- at tag: rename to [0.1.0] - 2026-12-04, add the commits merged since 2d240ddb, point the links at the bottom at v0.1.0 -->

The first release with functionality, planned as v0.1.0 for 2026-12-04 (#59). The notes of the
release are drafted in [docs/releases/v0.1.0.md](docs/releases/v0.1.0.md). This list was generated
from the `feat`, `fix` and `perf` commits since v0.0.1, and the `build` and `ci` commits that an
operator notices, then cut down to what an operator sees; it covers `main` of 2026-11-15, up to
2d240ddb.

### Added

- **OIDC login and group bindings** (#69, M6): one OpenID Connect provider (`auth.oidc`) logs
  people in through the browser (`/-/oidc/login`, authorization code flow with PKCE) or the
  terminal (`lajstrom login`, a device flow Lajstrom serves itself); a login creates the user at
  first and mints a personal access token that every client uses; the IdP's groups become
  `group:<name>` principals that bindings name, replaced at each login and lapsing with the
  login's token.
- **Formats**, hosted repositories with Lajstrom's own protocols on the server, no client plugins:
  - raw files: `curl -T` in, `curl` out, `DELETE` of a path; every upload a version, addressed by its digest;
  - pypi: `twine upload` and `uv publish`; the simple index as PEP 503 HTML and PEP 691 JSON (`latest` too), with PEP 658 metadata for wheels; a wheel's build tag is a variant of its own; a version yanked and unyanked (PEP 592, #81) with `PUT` and `DELETE …/versions/<version>/yank`, kept as entries of the package's journal in the registry, so reindex keeps them;
  - apt: `.deb` upload with `curl` or `oras push`; signed `Release`/`InRelease`, by-hash indexes; `binary-all` beside the concrete architectures; the signature block of `InRelease` with its CRC24 checksum line, as `gpg --clearsign` writes it, without which gpgv before 2.5 refused some `InRelease` files (#1125); source packages (#79), a `.dsc` and the files it names in one multipart `POST`, checked against its checksums, with a `Sources` index per component for `apt-get source` and `build-dep`; `Contents-<arch>` indexes for `apt-file`, from the file list of each `.deb` read at upload;
  - maven: `mvn deploy` and Gradle `publish` through publication sessions; `maven-metadata.xml` and all four checksums; a checksum file that disagrees keeps the version from being published; SNAPSHOT versions (#80) in a repo whose `policy.maven.versionPolicy` is `snapshot`, each deploy a build of its own under its timestamped version, never overwritten, the latest named by the SNAPSHOT version's `maven-metadata.xml`;
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
  - a token server of its own (the docker token flow), and `POST /v2/token`, the OAuth2 form of the password grant that containerd sends before `GET` (#908);
  - `oci` pass-through repos: `docker push` and `pull`, multi-arch indexes made by buildx or `crane`, with roles and an audit row per push (#71);
  - the referrers API with the tag-schema fallback; `registries[].referrers.alwaysTag` writes the tag-schema index beside the API too, for a registry whose garbage collector deletes the manifests no tag names (#326);
  - cosign signatures, SBOMs and attestations through the one write path, audited, serialised per subject across replicas;
  - `DELETE` of a manifest, its referrers first; a delete by tag of a manifest that other tags still name is refused.
- **Attachments** (#89): the signatures, SBOMs, scan reports, provenance and other attestations
  that `cosign attest`, `cosign sign` or `oras attach` push onto a version of any format through
  `/v2` are indexed per version and listed by `GET …/versions/<version>/attachments`, newest
  first with every result kept. Each is labelled `verified` when a key of `attachments.trust`,
  or a keyless identity of `attachments.keyless` checked offline against a `trusted_root.json`
  with sigstore-go, signed it for its predicate type and it names the version's manifest, else
  `untrusted`, `unsigned`, `invalid` or `unsupported`; a label never blocks a push. Reindex
  rebuilds them, asking the registry for the referrers of each version, and labels them again
  when the trust changes. `lajstrom admin trusted-root fetch` writes a trusted root fetched
  through TUF.
- **The registry is the only storage.** Every artifact is an OCI artifact in the registry you run. The index (SQLite or Postgres) is rebuilt from the registry by `lajstrom reindex`.
- **Registries:**
  - any registry of the OCI distribution spec, with a page per registry in [docs/registries](docs/registries/README.md): distribution, Zot, GHCR, GitLab, Harbor, Gitea, Forgejo (a column and a CI job of its own, told from Gitea by the vendor table), Nexus, JFrog Container Registry, Project Quay;
  - Amazon ECR, `registries[].type: ecr`: the token from the AWS credential chain, refreshed before it expires; repositories created with the tag mutability and encryption of `registries[].ecr`; measured on a live ECR on 2026-11-13 (#19), where four of its answers needed fixes; a warning at start about a lifecycle policy that would expire Lajstrom's tags;
  - eventually consistent registries (#446): a publish is indexed only once the registry serves what was pushed (`ingest.visibilityTimeout`, then `503` naming what it does not serve yet, on `/v2`, every frontend, the management API and a prefetch's result, #667); reindex removes a row only after two `MANIFEST_UNKNOWN` answers 10 minutes apart and past `reconcile.graceAge`;
  - OCI names in the 1.0 grammar of the distribution spec, without the words of Zot's layout or Harbor's routes (ADR 0017); a package stored under its earlier name keeps that repository; a package found at both forms has every version served, each from where reindex found its manifest (#642); `registries[].prefix` is checked against the same grammar;
  - a registry's limits checked before a write: the depth of names (`capabilities.maxNameDepth`; Docker Hub 1, GitLab 4), the length of names (Harbor's 245 bytes, its project included, #1041), `maxBlobSize` (GHCR's 10 GB), the chunk size (GHCR's 4 MiB, `registries[].uploads.chunkSize`); a repo on a registry that takes none of its names is refused when it is created;
  - a registry's own limit or quota (ECR's `LimitExceededException`, Harbor's project quota, the size limit of Gitea and Forgejo) is `403` naming the limit, not `409` or `502`, and a warning in the log (#357);
  - registries that fold the case of tags (`capabilities.tagsFoldCase`, on for Gitea and Forgejo, #426): a tag is claimed lower-cased, so of two versions whose tags differ only in case the second is a `409` naming the first, before anything is pushed;
  - Quay's build routes (`capabilities.buildRoutes`, on for Quay, #662): a new package whose name ends in a build route, which Quay's master refuses to create, is refused naming the rule before anything is pushed (`400`; a format proxy's file `403` before its download); a package the index has keeps taking versions; the probe measures it;
  - the answers of some registries read as they are meant: Harbor's `412` to the delete of a manifest an index lists is `409`, as on Zot, ECR and GitLab (#1041); a client's cancel of an upload on JFrog or GHCR, which refuse cancels, is `204` (#1044); JFrog's `404` to the status of an upload session with no bytes yet is offset 0, no longer the end of the session (#1045); Gitea and Forgejo no longer get every request twice, the first time without a token (#1042);
  - registries without a catalog: `lajstrom reindex --attach` names the OCI repositories of a repo;
  - `lajstrom registry probe` measures what a registry supports, ECR included: routing words in names, the lag of writes and deletes, monotonic reads over fresh connections.
- **Authentication:**
  - the first admin from a one-time bootstrap token that the operator mounts as a file (`auth.bootstrap.tokenFile`), exchanged with `lajstrom admin bootstrap` or `POST /api/v1/bootstrap`; or `lajstrom admin user create --admin` against the database;
  - local users with an immutable uid (UUID v7), rename and delete;
  - argon2id passwords, from standard input, a prompt or `--password-file`, never from the environment;
  - personal access tokens and deploy tokens with scopes and expiry;
  - a deploy token over a repo mask (`oci/*`), which also matches the repos created after it, for a fleet of nodes that reads several repos of one host (#911): `POST /api/v1/deploy-tokens` by an admin with its password, or `lajstrom admin token create --mask`;
  - throttles of failed logins per account and per address;
  - `server.trustedProxies` for the client address behind an edge.
- **Authorization:**
  - roles `reader`, `writer`, `maintainer`, `admin` bound to repo masks, for users and for OIDC groups;
  - anonymous read per repo;
  - the 401/404/403 rule, so that a denial does not reveal a repo.
- **Management API** `/api/v1`: repos (hosted, proxy, group), users, bindings, tokens (deploy tokens over a mask too), webhooks, audit log, `whoami`; `DELETE` of a version, every variant or one; reindex of a repo. A repo's packages and versions a page at a time, one version with its variants and files, its attachments, and `GET /api/v1/info` for a client before sign-in. The yank of a pypi version. A conditional delete (`If-Match`, `If-Unmodified-Since`; a version an npm dist-tag names only with `?force=true`) and a batch delete, `…/versions:delete`, with a result per item. A 401 has the code `UNAUTHENTICATED`. An OpenAPI 3.1 document is served at `/api/v1/openapi.yaml`.
- **Retention** (#72): `lajstrom admin retention` keeps the last N versions and drops those older than a maximum age, per repo and package pattern, never a version a dist-tag names; a dry run unless `--apply`; a client of the API that a CronJob runs, each deletion audited with the job's id. `--keep-builds N` keeps the newest N builds of each maven SNAPSHOT version; `--keep-last` and `--max-age` decide the releases alone and delete no build (#80).
- **Repo policy** in the API and in `repos[].policy`: `maxFileSize` (also for `/v2` pushes), `blobRedirect`, `maven.sessionTimeout`, `maven.maxSessionFiles` (256) and `maven.versionPolicy` (`release` or `snapshot`). A file is bounded by default: npm 512 MiB, pypi and maven 4 GiB, apt, raw and rpm 8 GiB, oci 16 GiB, or the registry's `maxBlobSize` when smaller.
- **Audit log** of every write and control-plane change. The row of a registry write is written before the write and completed with its outcome: `ok`, `failed`, or `unknown` when the outcome could not be recorded (`GET /api/v1/audit?state=`). A change made with `lajstrom admin` names the host it ran on.
- **Webhooks** (#77): a signed `POST` when a version is published or deleted through Lajstrom, or
  a repo is created or deleted: a CloudEvent with the Standard Webhooks headers (HMAC-SHA256), for
  the webhooks of a repo, made by its maintainers, or of a mask, made by an admin. The event is
  written in the transaction of its audit row and delivered by any replica that has the master
  key, `webhooks.workers` at a time, tried again with a backoff up to `webhooks.maxAttempts` (8,
  about 4 hours). A test ping, a delivery log kept for `webhooks.retention` (7 days), a new
  secret on demand; `…/webhooks` of a repo and `/api/v1/webhooks` in the API, `lajstrom admin
  webhook` on the command line.
- **Snapshots of the control plane** (#76): `lajstrom admin snapshot create` writes the repos,
  group members, users, OIDC identities, tokens, bindings, keys and webhooks, and the audit rows
  of a window when asked, into `lajstrom/control-plane` of a registry, in one read of the
  database; sealed with AES-256-GCM under a key derived from the master key unless
  `--unsealed`. `snapshot list` lists them; `snapshot restore` writes one into an empty
  database, or with `--force` into one it empties, and `lajstrom reindex` then brings back the
  rest: a database lost without a backup comes back from the registry and the master key. A
  snapshot of a SQLite index restores into Postgres.
- **FQDN mode** (#75): with `server.fqdn.domain`, every repo also has the host
  `<repo>.<format>.<domain>`, and `<repo>.<domain>` when no repo of another format has its name,
  served as its path form; npm's `dist.tarball` and the `Location` of a redirect or an upload
  follow the host the client used. `/v2`, the management API and the system endpoints stay on
  the main host, and the token realm never follows `Host`; `serve` warns when the certificate of
  `server.tls` does not cover `*.<domain>`.
- **CLI:**
  - `serve`, `reindex`, `migrate`, `version`;
  - `admin` (`user`, `binding`, `token`, `repo`, `key`, `deb`, `rpm`, `bootstrap`, `retention`, `webhook`, `snapshot`, `trusted-root`; `repo prefetch`);
  - `login`, the device flow of the OIDC login;
  - `healthcheck`, for the distroless image, over TLS too;
  - `registry probe`.
- **Operations:**
  - Prometheus metrics on a listener of their own; `/-/healthz` and `/-/readyz`;
  - registry requests retried when idempotent, and a circuit breaker per registry (503 with `Retry-After`), which the failures of one repository alone do not open (#972); 503 with `Retry-After` while the database is unreachable;
  - `server.stallTimeout`; `server.maxConcurrentUploads` (32); `server.uploadSessions`, the live `/v2` upload sessions of a principal, with metrics of the refusals;
  - a drain on SIGTERM, SIGINT and SIGHUP: readiness answers 503 and the replica keeps serving for `server.shutdownDelay` (5 s) before the listener closes;
  - the replica's clock compared with the database's (`lajstrom_clock_skew_seconds`; not ready past 60 s), and before `reindex` and `admin` too;
  - a second `serve` on one SQLite file refused;
  - migrations expand or contract: a binary refuses a schema newer than it can run on, and a migration that would break the release before unless `lajstrom migrate --allow-breaking`; `serve` can run as a Postgres user with data rights only;
  - Postgres through PgBouncer in transaction mode, tested in CI; YugabyteDB experimental, in a CI cell that does not block, where two migrators at once pass: a migration is run again on its catalog conflicts (`40001`, `42710`; #616); CockroachDB experimental, a dialect of the `postgres` driver with migrations of its own, applied one statement at a time, and read committed asked for by every transaction, measured on one node and not tested in CI (#617);
  - a pooled Postgres connection that the server ended is replaced before it is used, not answered `503`; a connection attempt waits at most 5 s unless the DSN's `connect_timeout` or `PGCONNECT_TIMEOUT` says otherwise (#1123);
  - reindex pushes again a referrers index that lacks referrers, on a registry without the referrers API, and reports the versions whose OCI repository the registry no longer knows;
  - a request whose client went away is logged as 499, not as an error;
  - TLS in Lajstrom or at the edge;
  - the master key from an environment variable or a file;
  - rotation of the apt and rpm signing keys and of the token signing key;
  - apt and rpm signing keys held in AWS KMS or GCP Cloud KMS (#78; `keys.kms`, `lajstrom admin key rotate --kms`), whose private key never leaves the KMS; a KMS that does not answer makes the signed indexes `503` with `Retry-After`, never unsigned.
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
  - nginx and Caddy in front, with the wildcard host and certificate of the FQDN mode; a backup
    and restore runbook, and the restore of a snapshot;
  - guides for Postgres poolers, CloudNativePG, YugabyteDB and CockroachDB
    ([databases.md](docs/deploy/databases.md)), for Talos Linux nodes that pull through Lajstrom
    ([talos.md](docs/guides/talos.md)), and for signed SBOMs and scan reports
    ([sbom-and-scans.md](docs/guides/sbom-and-scans.md)).

### Security

- `server.externalURL` is required and must be `https://`; an `http://` URL is accepted only on a loopback host with `server.insecureHTTP: true`, for the dev stack. A configured URL keeps the token realm and audience away from the `Host` that the client sends. `server.insecureHTTP` on an address beyond loopback logs a warning.
- Passwords and token secrets are never logged; tokens are stored as sha256 hashes and shown once. The bootstrap token is the operator's: only its hash is stored, it is valid while no admin exists and for `auth.bootstrap.ttl` (24 h), and wrong tries are throttled per address.
- The OIDC login takes ID tokens with asymmetric algorithms only, keeps its state and device codes as hashes and redeems each once; its pages run no script, under a CSP of `default-src 'none'`.
- What a client uploads is read within bounds: the directory of a zip, what an sdist unpacks to, the blocks of a `control.tar.xz`, the head of an rpm, the `${project.*}` references of a POM, the layers of a manifest, the scopes of a token request.
- A blob's digest is checked while it streams through `/v2`.
- A version pushed on `/v2` whose `created` time is more than `server.maxCreatedAhead` (5 min) after the server's clock, or `server.maxCreatedBehind` (1 h) before it, is refused, so that a writer cannot age a version into or out of a retention rule.
- Credentials are never sent over a redirect to `http`, and a bearer token realm must be on the registry's host or in `registries[].tokenRealmHosts`. A redirect of an `oci` proxy repo's upstream to another host is followed over https to public addresses only: never to loopback, a private network or a cloud metadata endpoint.
- Security headers on every response; no cross-origin writes.
- A repo that does not exist and one the caller may not read get the same answer on every
  frontend, status, headers and body: npm's frontend answered the one in text and the other in
  JSON, and a `401` named the action only on a repo that exists (#1091).
- A webhook reaches a host of `webhooks.allowedHosts`, and any other only over `https` and at
  public addresses, after every redirect too: never loopback, a private network or the metadata
  service. Its secret is sealed with the master key, shown once and never logged; the audit row
  names the URL's host, not the URL.
- A deploy token over a mask is created with the admin's password, never with a token, so that a
  stolen token cannot mint one that needs no expiry.
- A snapshot of the control plane is sealed with the master key by default; sealed or not, it
  holds no password, token or private key in clear.
- A KMS's signature is checked against the stored public key before the document is served, and
  a keyless attachment is verified offline, against the operator's trusted root.

## [0.0.1] - 2026-11-05

The name, reserved. No functionality.

[Unreleased]: https://github.com/lajstrom/lajstrom/compare/v0.0.1...HEAD
[0.0.1]: https://github.com/lajstrom/lajstrom/releases/tag/v0.0.1

# Changelog

All notable changes to Lajstrom are in this file. The format is
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html). Before 1.0, a minor version may
change the HTTP API, the configuration and the CLI; the notes of each release say what changed
and how to upgrade ([README](README.md#our-promise-to-users)).

## [Unreleased]

The first release with functionality, planned as v0.1.0 for 2026-12-04 (#59). The notes of the
release are drafted in [docs/releases/v0.1.0.md](docs/releases/v0.1.0.md). This list was generated
from the `feat`, `fix` and `perf` commits since v0.0.1, then cut down to what an operator sees; it
covers `main` of 2026-11-11.

### Added

- **Formats**, hosted repositories with Lajstrom's own protocols on the server, no client plugins:
  - raw files: `curl -T` in, `curl` out, `DELETE` of a path; every upload a version, addressed by its digest;
  - pypi: `twine upload` and `uv publish`; the simple index as PEP 503 HTML and PEP 691 JSON, with PEP 658 metadata;
  - apt: `.deb` upload with `curl` or `oras push`; signed `Release`/`InRelease`, by-hash indexes;
  - maven: `mvn deploy` and Gradle `publish` through publication sessions; `maven-metadata.xml` and all four checksums;
  - npm: `npm publish`, `install`, `dist-tag`, `deprecate` and `unpublish`; `npm login`, `whoami`, `search`; scoped packages.
- **Proxy and group repos**, for npm:
  - a proxy repo of an upstream of `upstreams[]`, npmjs.org for example: a file is fetched on the first request, once across replicas, checked, and kept in the registry as any other artifact; documents are cached with a TTL, misses with a shorter one, and an `offline` repo serves what it has; allow and deny patterns per repo; `npm audit` is forwarded to the upstream;
  - a group repo: one URL for up to 16 repos in order, at most one of them hosted, which takes the writes.
- **OCI facade** on `/v2` in front of the registry:
  - a token server of its own (the docker token flow);
  - `oci` pass-through repos;
  - the referrers API with the tag-schema fallback;
  - cosign signatures, SBOMs and attestations through the one write path, audited, serialised per subject across replicas;
  - `DELETE` of a manifest, its referrers first; a delete by tag of a manifest that other tags still name is refused.
- **The registry is the only storage.** Every artifact is an OCI artifact in the registry you run. The index (SQLite or Postgres) is rebuilt from the registry by `lajstrom reindex`.
- **Registries:**
  - any registry of the OCI distribution spec, with a page per registry in [docs/registries](docs/registries/README.md): distribution, Zot, GHCR, GitLab, Harbor, Gitea and Forgejo, Nexus, JFrog Container Registry;
  - Amazon ECR, `registries[].type: ecr`: the token from the AWS credential chain, refreshed before it expires; repositories created with the tag mutability and encryption of `registries[].ecr` (tested against moto, not yet against ECR);
  - registries without a catalog: `lajstrom reindex --attach` names the OCI repositories of a repo;
  - `lajstrom registry probe` measures what a registry supports.
- **Authentication:**
  - local users with an immutable uid (UUID v7), rename and delete;
  - argon2id passwords;
  - personal access tokens and deploy tokens with scopes and expiry;
  - throttles of failed logins per account and per address;
  - `server.trustedProxies` for the client address behind an edge.
- **Authorization:**
  - roles `reader`, `writer`, `maintainer`, `admin` bound to repo masks;
  - anonymous read per repo;
  - the 401/404/403 rule, so that a denial does not reveal a repo.
- **Management API** `/api/v1`: repos (hosted, proxy, group), users, bindings, tokens, audit log, `whoami`; `DELETE` of a version, every variant or one; reindex of a repo. An OpenAPI 3.1 document is served at `/api/v1/openapi.yaml`.
- **Repo policy** in the API and in `repos[].policy`: `maxFileSize` (also for `/v2` pushes), `blobRedirect`, and `maven.sessionTimeout`.
- **Audit log** of every write and control-plane change, written in the transaction of the change.
- **CLI:**
  - `serve`, `reindex`, `migrate`, `version`;
  - `admin` (`user`, `binding`, `token`, `repo`, `key`, `deb`);
  - `healthcheck`, for the distroless image, over TLS too;
  - `registry probe`.
- **Operations:**
  - Prometheus metrics on a listener of their own; `/-/healthz` and `/-/readyz`;
  - registry requests retried when idempotent, and a circuit breaker per registry (503 with `Retry-After`);
  - `server.stallTimeout`, `server.maxConcurrentUploads`;
  - a drain on shutdown;
  - TLS in Lajstrom or at the edge;
  - the master key from an environment variable or a file;
  - rotation of the apt signing keys and of the token signing key.
- **Release artifacts:**
  - binaries for linux and darwin on amd64 and arm64;
  - a multi-arch distroless image on ghcr.io and Docker Hub;
  - SBOMs and cosign signatures;
  - a Homebrew cask.
- **Deployment:**
  - a production Docker Compose example;
  - Kubernetes manifests;
  - nginx and Caddy in front, a backup and restore runbook.

### Security

- `server.externalURL` is required and must be `https://`; an `http://` URL is accepted only on a loopback host with `server.insecureHTTP: true`, for the dev stack. A configured URL keeps the token realm and audience away from the `Host` that the client sends.
- Passwords and token secrets are never logged; tokens are stored as sha256 hashes and shown once.
- What a client uploads is read within bounds: the directory of a zip, what an sdist unpacks to, the blocks of a `control.tar.xz`, the `${project.*}` references of a POM, the layers of a manifest, the scopes of a token request.
- A blob's digest is checked while it streams through `/v2`.
- Credentials are never sent over a redirect to `http`, and a bearer token realm must be on the registry's host or in `registries[].tokenRealmHosts`.
- Security headers on every response; no cross-origin writes.

## [0.0.1] - 2026-11-05

The name, reserved. No functionality.

[Unreleased]: https://github.com/lajstrom/lajstrom/compare/v0.0.1...HEAD
[0.0.1]: https://github.com/lajstrom/lajstrom/releases/tag/v0.0.1

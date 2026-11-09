# Changelog

All notable changes to Lajstrom are in this file. The format is
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html). Before 1.0, a minor version may
change the HTTP API, the configuration and the CLI; the notes of each release say what changed
and how to upgrade ([README](README.md#our-promise-to-users)).

## [Unreleased]

The first release with functionality, planned as v0.1.0 for 2026-12-04 (#59). The notes of the
release are drafted in [docs/releases/v0.1.0.md](docs/releases/v0.1.0.md).

### Added

- **Formats**, hosted repositories with Lajstrom's own protocols on the server, no client plugins:
  - raw files: `curl -T` in, `curl` out, every upload a version addressed by its digest;
  - pypi: `twine upload` and `uv publish`; the simple index as PEP 503 HTML and PEP 691 JSON, with PEP 658 metadata;
  - apt: `.deb` upload with `curl` or `oras push`; signed `Release`/`InRelease`, by-hash indexes;
  - maven: `mvn deploy` and Gradle `publish` through publication sessions; `maven-metadata.xml` and all four checksums.
- **OCI facade** on `/v2` in front of the registry:
  - a token server of its own (the docker token flow);
  - `oci` pass-through repos;
  - the referrers API with the tag-schema fallback;
  - cosign signatures, SBOMs and attestations through the one write path, audited, serialised per subject across replicas.
- **The registry is the only storage.** Every artifact is an OCI artifact in the registry you run. The index (SQLite or Postgres) is rebuilt from the registry by `lajstrom reindex`.
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
- **Management API** `/api/v1`: repos, users, bindings, tokens, audit log, `whoami`. An OpenAPI 3.1 document is served at `/api/v1/openapi.yaml`.
- **Audit log** of every write and control-plane change, written in the transaction of the change.
- **CLI:**
  - `serve`, `reindex`, `migrate`, `version`;
  - `admin` (`user`, `binding`, `token`, `repo`, `key`, `deb`);
  - `healthcheck`, for the distroless image.
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

- An `http://` `server.externalURL` is refused except on a loopback host with `server.insecureHTTP: true`: clients reach Lajstrom over TLS only.
- Passwords and token secrets are never logged; tokens are stored as sha256 hashes and shown once.

## [0.0.1] - 2026-11-05

The name, reserved. No functionality.

[Unreleased]: https://github.com/lajstrom/lajstrom/compare/v0.0.1...HEAD
[0.0.1]: https://github.com/lajstrom/lajstrom/releases/tag/v0.0.1

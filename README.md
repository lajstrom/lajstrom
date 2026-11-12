<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/assets/logo-wordmark-dark.svg">
  <img alt="Lajstrom" src="docs/assets/logo-wordmark.svg" width="308" height="64">
</picture>

[![CI](https://img.shields.io/github/actions/workflow/status/lajstrom/lajstrom/ci.yml?branch=main&label=ci)](https://github.com/lajstrom/lajstrom/actions/workflows/ci.yml)
[![Go version](https://img.shields.io/github/go-mod/go-version/lajstrom/lajstrom)](go.mod)
[![License](https://img.shields.io/github/license/lajstrom/lajstrom)](LICENSE)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/lajstrom/lajstrom/badge)](https://scorecard.dev/viewer/?uri=github.com/lajstrom/lajstrom)
[![Commit activity](https://img.shields.io/github/commit-activity/m/lajstrom/lajstrom)](https://github.com/lajstrom/lajstrom/commits/main)
[![Roadmap](https://img.shields.io/badge/roadmap-v0.1.0%20due%202026--12--04-orange)](docs/roadmap.md)

# Lajstrom

Artifact registry on top of any OCI registry.

One server, many repository formats: apt, rpm, maven, pypi, npm and raw files, and any OCI
artifact through the facade.
Every artifact is stored as an OCI artifact in the registry you already run: ECR, Harbor, Zot,
GHCR, docker distribution. Lajstrom owns no bytes, only the index, and the index can always be
rebuilt from the registry.

*Lajstrom* is Hungarian for "register, inventory".

Clients keep their own protocol: `pip`, `apt-get`, `mvn`, `npm` and `oras` talk to Lajstrom as
to their usual repository, with no plugin. Each upload is checked, written to the registry and
indexed in SQLite or Postgres; `lajstrom reindex` rebuilds that index from the registry alone.
Users, tokens and per-repo roles live in Lajstrom; the registry only ever sees Lajstrom's
credentials. v0.1.0 is planned for 2026-12-04 ([roadmap](docs/roadmap.md)).
https://lajstrom.dev

## Install

No release is out yet. Until v0.1.0 (due 2026-12-04), build from source with Go 1.27:

```sh
git clone https://github.com/lajstrom/lajstrom.git && cd lajstrom
make build && bin/lajstrom version
```

From v0.1.0 on, the image:

```sh
docker run --rm ghcr.io/lajstrom/lajstrom:v0.1.0 version
```

The archives with checksums and signatures, Homebrew, and the smallest configuration that starts
are in [docs/install.md](docs/install.md).

## What works

What each format does today on main, each row run by its quick start and by an end-to-end test
with the real clients (`test/e2e/`):

| Format | Publish with | Install with | Also |
| --- | --- | --- | --- |
| [raw](docs/quickstart/raw.md) | `curl -T` | `curl`, `oras pull` | every version kept, `?digest=` pins one, `Range`, a JSON listing |
| [pypi](docs/quickstart/pypi.md) | `twine upload`, `uv publish` | `pip`, `uv`, `poetry` | PEP 503 and PEP 691 indexes, PEP 658 metadata |
| [apt](docs/quickstart/apt.md) | `curl -T` a `.deb`, `oras push` | `apt-get` on Debian and Ubuntu | signed `InRelease` and `Release.gpg`, `by-hash`, key rotation |
| [maven](docs/quickstart/maven.md) | `mvn deploy`, Gradle `publish` | `mvn`, Gradle | md5, sha1, sha256 and sha512 checksums; a release appears whole or not at all; no SNAPSHOT versions yet |
| [npm](docs/quickstart/npm.md) | `npm publish`, `pnpm publish`, `yarn npm publish` | `npm`, `pnpm`, `yarn` | dist-tags; a proxy of npmjs.org and groups |
| [rpm](docs/quickstart/rpm.md) | `curl -T` an `.rpm`, `oras push` | `dnf`, `yum`, `zypper` | GPG-signed `repomd.xml`, binary and source packages, every arch in one repodata |
| [OCI](docs/quickstart/oras.md) | `oras push`, `cosign sign` | `oras pull`, `crane`, `cosign verify` | the `/v2` API with its token flow, referrers; cosign is run by [test/e2e/cosign.sh](test/e2e/cosign.sh), not by the quick start |
| [docker images](docs/quickstart/docker.md) | `docker push`, `docker buildx build --push`, `crane index append`, `cosign sign` | `docker pull`, `crane`, `cosign verify` | `oci` repos pass images through, multi-arch indexes included; roles and the audit of pushes; run by [test/e2e/docker.sh](test/e2e/docker.sh) |

Every format shares local users, personal access and deploy tokens, roles
bound to repos, and an audit log ([docs/auth.md](docs/auth.md)).

## Documentation

| Page | What it is for |
| --- | --- |
| [Start here](docs/onboarding.md) | one hour from zero to a claimed issue |
| [Install](docs/install.md) | the image, the archives with checksums and signatures, Homebrew, or `make build` |
| [Configuration](docs/configuration.md) | every key of `lajstrom.yaml`, its default and its environment variable |
| [Quick start: raw](docs/quickstart/raw.md) | `curl -T` a file in, `curl` it out, see the manifest in the registry |
| [Quick start: apt](docs/quickstart/apt.md) | a `.deb` in with `curl -T` or `oras push`, out with `apt-get install` from a signed repo |
| [Quick start: pypi](docs/quickstart/pypi.md) | `twine upload` or `uv publish` in, `pip install` or `uv pip install` out, with PEP 658 metadata |
| [Quick start: maven](docs/quickstart/maven.md) | `mvn deploy` or Gradle's `publish` in, `mvn dependency:get` or a Gradle build out, with all four checksums |
| [Quick start: rpm](docs/quickstart/rpm.md) | an `.rpm` in with `curl -T` or `oras push`, out with `dnf install` or `zypper install` from a GPG-checked repo |
| [Quick start: npm](docs/quickstart/npm.md) | `npm publish`, `pnpm` and `yarn npm publish` in, `npm install` out, dist-tags, tokens, a proxy of npmjs.org, coming from Verdaccio |
| [Quick start: OCI with oras](docs/quickstart/oras.md) | `oras push` and `oras pull` through `/v2`, a raw file through ingest, referrers, deletes |
| [Quick start: docker](docs/quickstart/docker.md) | `docker push` and `docker pull` through an `oci` repo, multi-arch images made by buildx or crane, cosign, a reader |
| [Management API](docs/api.md) | `/api/v1`: repos, users, bindings, tokens, deletes, the audit log |
| [Dev stack](docs/dev-stack.md) | `make dev-up`: Zot, distribution and Postgres for development and tests |
| [Deploy with Compose](docs/deploy/compose.md) | one host for production: Lajstrom, Postgres and a registry with passwords, secrets in files, a backup |
| [Testing](docs/testing.md) | the test levels and what each needs; `make e2e` and how to add a scenario |
| [Authentication](docs/auth.md) | local users, roles and bindings, the 401/404/403 rule, bearer tokens for `/v2`, personal access and deploy tokens, CI, `whoami` |
| [Deploy on Kubernetes](docs/deploy/kubernetes.md) | plain manifests: two replicas on Postgres, probes, the restricted security context |
| [Install with Helm](docs/deploy/helm.md) | the same as a chart: secrets from Secrets, the migration Job before each upgrade, HTTPRoute or Ingress, ServiceMonitor, HPA; the values |
| [Operations](docs/operations.md) | users, the master key and key rotation, readiness, limits, metrics, audit, reindex, backup and restore |
| [Behind a reverse proxy](docs/reverse-proxy.md) | TLS at the edge, `externalURL`, unbuffered uploads, rate limits on `/v2/token`; nginx and Caddy |
| [Guarantees](docs/spec/guarantees.md) | what Lajstrom guarantees, and the test for each |
| [Threat model](docs/security/threat-model.md) | what is protected and from whom, STRIDE over the flows, each threat with its mitigation or its issue, what the edge must do |
| [Releasing](docs/release.md) | how a tag becomes binaries, images, SBOMs and signatures; verifying a release |
| [Architecture](docs/architecture.md) | components, storage model, the five flows |
| [Comparison](docs/comparison.md) | Lajstrom next to Nexus, Artifactory, Harbor and others: a feature matrix with sources, where it differs, where it is not the right choice yet |
| [Specs](docs/spec/) | manifest profile, layout and interfaces, HTTP routing, index schema, RBAC |
| [ADRs](docs/adr/) | decisions and why |
| [Roadmap](docs/roadmap.md) | milestones, lanes, week by week |
| [Process](docs/process.md) | issues, pull requests, reviews, working with AI assistants |

## Our promise to users

Lajstrom holds what people build and ship, so the people who run it have to be able to rely on
it, and plan around it. Keeping them happy matters more to us than any single feature. While
Lajstrom is 0.x, that means:

- **A steady release cadence.** Releases come out on a regular schedule that we announce and
  keep, not when a big feature happens to be done.
- **No API stability guarantee yet.** The HTTP API, the configuration and the CLI may still
  change between 0.x versions. When they do, the release notes say so and say how to upgrade.
- **No LTS for now.** There are no long-term support branches; fixes ship in the next release.
- **The license stays.** Lajstrom is Apache-2.0 and will stay Apache-2.0. We will not change the
  license short of a truly extreme situation.

## Contributing

Contributions are welcome; see [CONTRIBUTING.md](CONTRIBUTING.md). Every commit needs a DCO
sign-off (`git commit -s`), and contributors sign the [CLA](CLA.md) once before their first merge.
AI-assisted contributions are allowed under the
[AI policy](CONTRIBUTING.md#ai-assisted-contributions): a human author is responsible for the
change and discloses the tools used. Design decisions are recorded in [docs/adr](docs/adr/).

## Security

Please report vulnerabilities privately; see [SECURITY.md](SECURITY.md).

## License

Lajstrom is licensed under the [Apache License 2.0](LICENSE).
Copyright 2026 The Lajstrom Authors.

## Star history

<a href="https://star-history.com/#lajstrom/lajstrom&Date">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="https://api.star-history.com/svg?repos=lajstrom/lajstrom&type=Date&theme=dark">
    <img alt="Star history of lajstrom/lajstrom" src="https://api.star-history.com/svg?repos=lajstrom/lajstrom&type=Date" width="600">
  </picture>
</a>

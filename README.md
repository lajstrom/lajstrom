<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/assets/logo-wordmark-dark.svg">
  <img alt="Lajstrom" src="docs/assets/logo-wordmark.svg" width="308" height="64">
</picture>

[![CI](https://img.shields.io/github/actions/workflow/status/lajstrom/lajstrom/ci.yml?branch=main&label=ci)](https://github.com/lajstrom/lajstrom/actions/workflows/ci.yml)
[![Go version](https://img.shields.io/github/go-mod/go-version/lajstrom/lajstrom)](go.mod)
[![License](https://img.shields.io/github/license/lajstrom/lajstrom)](LICENSE)
[![Commit activity](https://img.shields.io/github/commit-activity/m/lajstrom/lajstrom)](https://github.com/lajstrom/lajstrom/commits/main)
[![Roadmap](https://img.shields.io/badge/roadmap-v0.1.0%20due%202026--12--04-orange)](docs/roadmap.md)

# Lajstrom

Artifact registry on top of any OCI registry.

One server, many repository formats: apt, rpm, maven, pypi, npm, terraform modules, raw files.
Every artifact is stored as an OCI artifact in the registry you already run: ECR, Harbor, Zot,
GHCR, docker distribution. Lajstrom owns no bytes, only the index, and the index can always be
rebuilt from the registry.

*Lajstrom* is Hungarian for "register, inventory".

## Status

Building the MVP (November 2026): raw, pypi, apt and maven hosted repositories, OCI facade,
reindex, local users and RBAC, v0.1.0 planned for 2026-12-04. See the [roadmap](docs/roadmap.md).
https://lajstrom.dev

## Documentation

| Page | What it is for |
| --- | --- |
| [Start here](docs/onboarding.md) | one hour from zero to a claimed issue |
| [Quick start: raw](docs/quickstart/raw.md) | `curl -T` a file in, `curl` it out, see the manifest in the registry |
| [Quick start: apt](docs/quickstart/apt.md) | a `.deb` in with `curl -T` or `oras push`, out with `apt-get install` from a signed repo |
| [Quick start: pypi](docs/quickstart/pypi.md) | `twine upload` or `uv publish` in, `pip install` or `uv pip install` out, with PEP 658 metadata |
| [Quick start: maven](docs/quickstart/maven.md) | `mvn deploy` or Gradle's `publish` in, `mvn dependency:get` or a Gradle build out, with all four checksums |
| [Dev stack](docs/dev-stack.md) | `make dev-up`: Zot, distribution and Postgres for development and tests |
| [Testing](docs/testing.md) | the test levels and what each needs; `make e2e` and how to add a scenario |
| [Authentication](docs/auth.md) | local users, argon2id passwords, HTTP Basic on every route, the first admin, `whoami` |
| [Operations](docs/operations.md) | the master key, apt signing keys, `key.asc` for clients, key rotation |
| [Architecture](docs/architecture.md) | components, storage model, the five flows |
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

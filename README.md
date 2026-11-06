<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/assets/logo-wordmark-dark.svg">
  <img alt="Lajstrom" src="docs/assets/logo-wordmark.svg" width="308" height="64">
</picture>

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

| | |
| --- | --- |
| [Start here](docs/onboarding.md) | one hour from zero to a claimed issue |
| [Architecture](docs/architecture.md) | components, storage model, the five flows |
| [Specs](docs/spec/) | manifest profile, layout and interfaces, HTTP routing, index schema, RBAC |
| [ADRs](docs/adr/) | decisions and why |
| [Roadmap](docs/roadmap.md) | milestones, lanes, week by week |
| [Process](docs/process.md) | issues, pull requests, reviews, working with AI assistants |

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

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

Design phase. Nothing to run yet. https://lajstrom.dev

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

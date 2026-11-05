# Lajstrom

Artifact registry on top of any OCI registry.

One server, many repository formats: apt, rpm, maven, pypi, npm, terraform modules, raw files.
Every artifact is stored as an OCI artifact in the registry you already run: ECR, Harbor, Zot,
GHCR, docker distribution. Lajstrom owns no bytes, only the index, and the index can always be
rebuilt from the registry.

*Lajstrom* is Hungarian for "register, inventory".

## Status

Design phase. Nothing to run yet. https://lajstrom.dev

# 0001. The OCI registry is the only storage

- Status: accepted
- Date: 2026-11-05
- Deciders: gecube

## Context

Lajstrom serves several package formats (apt, rpm, maven, pypi, npm, terraform modules and raw
files). Classic artifact repositories keep artifact bytes in their own blob store, which then has
to be sized, backed up, replicated and secured in addition to the container registry that most
teams already run.

OCI registries (ECR, Harbor, Zot, GHCR, docker distribution) are already operated, replicated,
access-controlled and backed up in most environments, and since OCI 1.1 they can store arbitrary
artifacts, not only container images.

## Considered options

- A Lajstrom-owned blob store (filesystem or object storage), with or without a registry.
- The OCI registry as the only storage, with a database used only as an index.

## Decision

The OCI registry is the only storage and the single source of truth. Every artifact, of every
format, is stored as an OCI artifact in the configured registry. Lajstrom never stores artifact
bytes.

The database is a derived index plus a small control plane (configuration, users and permissions,
audit). The index can always be rebuilt from the registry; losing it costs a reindex, not data.

## Consequences

- Operators back up and replicate one system, the registry, using tools they already have.
- Any data the index needs must be derivable from what is stored in the registry. Format metadata
  that cannot be derived has to be stored in the registry as part of the artifact.
- A full reindex must be possible and reasonably fast; it doubles as disaster recovery and as the
  way to repair drift between index and registry.
- Lajstrom depends on the capabilities and limits of the backend registry (artifact support,
  listing, rate limits). Differences between registries need to be handled or documented.
- Write paths need care: a write is complete only when the registry has accepted it, and the
  index must tolerate artifacts that appear in the registry without going through Lajstrom.

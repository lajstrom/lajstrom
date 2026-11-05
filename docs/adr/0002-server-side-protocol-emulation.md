# 0002. Server-side protocol emulation, no client plugins

- Status: accepted
- Date: 2026-11-05
- Deciders: gecube

## Context

Artifacts are stored in an OCI registry (ADR 0001), but the clients that consume them speak their
own protocols: apt, rpm repositories, maven, pypi, npm, the terraform module registry protocol and
plain HTTP for raw files. None of them speaks OCI natively.

One way to bridge the gap is a client-side plugin or transport for each tool, which talks OCI
directly. That pushes installation, upgrades and configuration onto every machine and CI runner
that consumes packages, and some tools have no plugin mechanism at all.

## Considered options

- Client-side plugins or transports that read OCI directly.
- A server that emulates each native protocol and maps it to OCI artifacts.

## Decision

Clients keep their native tools and configuration; emulation of each protocol happens on the
server. Lajstrom exposes the endpoints each tool expects and translates between that protocol and
OCI artifacts in the registry. Lajstrom ships no client plugins.

Container images are the exception: OCI clients already speak the registry protocol, so Docker
images pass through to the backend registry untouched, with Lajstrom's RBAC and audit in front.

## Consequences

- Adoption needs only a repository URL and credentials in the existing tool configuration.
- The server must implement each protocol faithfully, including the repository metadata each client
  expects, generated from the index. Every format is a separate, testable module.
- Compatibility has to be tested against the real clients, across the versions people use.
- All traffic goes through Lajstrom, so it must be efficient at streaming content to and from the
  registry, and it is the single place where authorization and audit are enforced.
- For pass-through of images, Lajstrom must not rewrite manifests or blobs.

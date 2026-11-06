# 0005. Manifest profile: OCI 1.1 artifacts with one immutable tag per variant

- Status: proposed
- Date: 2026-11-06
- Deciders: gecube

## Context

Every package version has to be stored as an OCI object (ADR 0001) in a way that `oras push`,
the protocol frontends, `reindex` and third-party tools (cosign, notation, SBOM generators,
other UIs) agree on. The ecosystem survey in `docs/architecture.md` §6 shows three recurring
patterns: the file's role encoded in the layer media type (Helm, conda, ModelPack), the file
name in `org.opencontainers.image.title` (ORAS, apt-transport-oci), and platforms either as an
image index with `platform` (OpenTofu, pyoci) or as a separate tag per variant (conda).

An image index per version is mutable: adding an architecture later means re-pointing the tag,
which conflicts with registries that enforce tag immutability (ECR option, Harbor rules) and
with signatures whose subject is the index.

The 2026-11-05 draft encoded `_` as `__` in tags and used `_` as the separator between version
and variant. That makes the encoding ambiguous (`a_` + `_b` and `a` + `__b` both give `a___b`).

## Considered options

- Image index per version with per-platform manifests (pyoci, OpenTofu style).
- One manifest per (version, variant) with its own immutable tag (conda style).
- Reuse an existing convention per format (pyoci for pypi, apt-transport-oci for apt).

## Decision

One OCI image manifest (image-spec 1.1) per (package, version, variant), with:

- `artifactType: application/vnd.lajstrom.<format>.v1`, empty config;
- file layers typed `application/vnd.lajstrom.<format>.<role>.v1[+suffix]`, each with
  `org.opencontainers.image.title` = file name;
- one optional metadata layer `application/vnd.lajstrom.<format>.metadata.v1+json`;
- `lajstrom.*` annotations that are sufficient to rebuild the index (schema, format, repo, name,
  version, variant, format-specific keys);
- tag `Escape(version)[__Escape(variant)]`, **immutable and append-only**; `Escape` maps every
  byte outside `[A-Za-z0-9.-]` to `_` + two hex digits (`_` itself → `_5f`), so `__` never occurs
  inside an escaped value and the separator is unambiguous. Tags longer than 128 characters are
  replaced by a hash form (`_h` + 40 hex) that no escaped value can produce.

A tag collision with a different digest is a `409`, never an overwrite. Existing conventions
(Helm, OpenTofu, Flux, conda, …) are passed through repos of type `oci` and not re-encoded.
The normative text is `docs/spec/manifest-profile.md`.

## Consequences

- Works with immutable-tag registries; cosign and notation signatures have a stable subject.
- A version with N wheels or N architectures is N manifests and N tags; the index groups them.
  Listing a version means listing tags with a common prefix, or reading the index DB.
- `oras pull` of a tag restores the original file names without Lajstrom.
- The escape scheme differs from the draft (`_` → `_5f`, separator `__`); underscores in wheel
  tags become longer but stay under the limit. Nothing decodes tags on the server; reindex reads
  annotations.
- pyoci- and apt-transport-oci-compatible layouts are export formats, not the storage format.

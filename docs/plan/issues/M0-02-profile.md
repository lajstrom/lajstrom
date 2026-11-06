---
id: M0-02
title: "feat(oci): manifest profile library: media types, annotations, Escape/EscapeComponent, build and validate"
milestone: M0
lane: formats
size: M
priority: p0
labels: [area/formats, kind/feature, contract, good first issue]
depends: []
---
**Goal**

`internal/oci/profile`: the one place that knows what a Lajstrom manifest looks like. Pure Go,
no I/O, used by every plugin, the facade hook and reindex.

**Spec and context**

`docs/spec/manifest-profile.md` (all sections), ADR 0005.

**Scope**

- Constants: artifact types, layer role media types, metadata media type, annotation keys,
  the empty config descriptor.
- `Escape`, `Unescape`, `EscapeComponent`, `Tag(version, variant)`, `IsHashedTag`.
- `Annotations` struct ↔ `map[string]string` with required-key validation.
- `Build(format, entry, layers) (ocispec.Manifest, error)` producing canonical JSON (sorted
  keys, no whitespace) and `Validate(manifest) (Parsed, error)` returning format, repo, name,
  version, variant, attrs, file layers, metadata layer, with the error list of spec §5 (except the
  blob-existence check, which is the facade's).
- `RepositoryPath(prefix, format, repo, packagePath)`.

**Acceptance criteria**

- [ ] Every example in manifest-profile.md §4.2 and §4.3 is a test case and passes.
- [ ] Property test: `Unescape(Escape(s)) == s` for random byte strings; `Escape` output always
      matches the OCI tag grammar; `EscapeComponent` output always matches the path-component
      grammar or is the hash form; `Tag` is injective on a random sample of (version, variant)
      pairs (no two distinct pairs collide).
- [ ] `Validate` rejects each condition of spec §5 with a distinct error (table test).
- [ ] `Build` output round-trips through `Validate` and its digest is stable across runs.
- [ ] `FuzzEscape` and `FuzzValidate` targets exist (short corpus).
- [ ] docs: package doc comment links the spec.

**Out of scope**

Format-specific parsing (control, METADATA, POM): that is each plugin's `Validate`.

**Pointers**

`github.com/opencontainers/image-spec/specs-go/v1` for types; `go-digest`. Keep the package free
of `net/http` and `provider` imports.

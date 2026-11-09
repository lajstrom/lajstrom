# Spec: Lajstrom manifest profile

- Status: normative, v1 (2026-11-06). Changes need an issue labelled `contract` and, if the wire
  format changes, an ADR.
- Implemented by: `internal/oci/profile` (pure Go, no I/O).
- Related: [ADR 0005](../adr/0005-manifest-profile-and-immutable-tags.md),
  [index-schema.md](index-schema.md).

Every artifact Lajstrom stores is an OCI image manifest (OCI image-spec 1.1) in the backend
registry. This document defines exactly what that manifest looks like, so that `oras push`,
the protocol frontends, `reindex` and third-party tools all agree.

## 1. Manifest

nginx `1:1.18.0-6ubuntu14.4` for `amd64` in the suite `jammy`, component `main`, of the apt repo
`ubuntu-internal`: the tag is `1_3a1.18.0-6ubuntu14.4__jammy_2fmain_2famd64` (section 4.2).

```json
{
  "schemaVersion": 2,
  "mediaType": "application/vnd.oci.image.manifest.v1+json",
  "artifactType": "application/vnd.lajstrom.deb.v1",
  "config": {
    "mediaType": "application/vnd.oci.empty.v1+json",
    "digest": "sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a",
    "size": 2
  },
  "layers": [
    {
      "mediaType": "application/vnd.lajstrom.deb.package.v1",
      "digest": "sha256:9f3…",
      "size": 1258496,
      "annotations": { "org.opencontainers.image.title": "nginx_1.18.0-6ubuntu14.4_amd64.deb" }
    },
    {
      "mediaType": "application/vnd.lajstrom.deb.metadata.v1+json",
      "digest": "sha256:c01…",
      "size": 1830,
      "annotations": { "org.opencontainers.image.title": "metadata.json" }
    }
  ],
  "annotations": {
    "lajstrom.schema": "1",
    "lajstrom.format": "apt",
    "lajstrom.repo": "ubuntu-internal",
    "lajstrom.name": "nginx",
    "lajstrom.version": "1:1.18.0-6ubuntu14.4",
    "lajstrom.variant": "jammy/main/amd64",
    "lajstrom.deb.arch": "amd64",
    "lajstrom.deb.suite": "jammy",
    "lajstrom.deb.component": "main",
    "org.opencontainers.image.created": "2026-11-06T10:00:00Z"
  }
}
```

Rules:

| Element | Rule |
| --- | --- |
| `mediaType` | always `application/vnd.oci.image.manifest.v1+json`. Image indexes are never used for a package version (they are mutable). |
| `artifactType` | `application/vnd.lajstrom.<format>.v1`. `<format>` is the plugin name: `raw`, `pypi`, `deb`, `maven` in the MVP; `rpm`, `npm`, `nuget`, `cargo`, `go` later. It selects the plugin and filters referrers. Note: the apt *protocol* stores `deb` *packages*; the artifactType is `deb`, the annotation `lajstrom.format` is `apt` (the repo format). |
| `config` | always the OCI empty descriptor (`application/vnd.oci.empty.v1+json`, the 2-byte blob `{}`). No logic in config. |
| `layers` | one or more file layers plus at most one metadata layer. Order: file layers first, metadata layer last. Every layer carries `org.opencontainers.image.title` with the exact file name (no directories, except for `raw`, whose title is the original `/`-separated path, section 4.1). |
| `subject` | not set by Lajstrom. Referrers (signatures, SBOMs) set `subject` to our manifest. |
| `annotations` | the search keys listed in section 3. Anything large goes to the metadata layer. |

## 2. Media types

### File layers

`application/vnd.lajstrom.<format>.<role>.v1[+suffix]`. The role says what the file is; the
suffix follows the OCI convention for the encoding of the bytes.

| Format | Role media type | File |
| --- | --- | --- |
| raw | `application/vnd.lajstrom.raw.file.v1` | any file |
| pypi | `application/vnd.lajstrom.pypi.wheel.v1+zip` | `*.whl` |
| pypi | `application/vnd.lajstrom.pypi.sdist.v1.tar+gzip` | `*.tar.gz` sdist |
| pypi | `application/vnd.lajstrom.pypi.sdist.v1+zip` | `*.zip` sdist (legacy) |
| deb | `application/vnd.lajstrom.deb.package.v1` | `*.deb` (ar archive) |
| maven | `application/vnd.lajstrom.maven.artifact.v1` | jar, war, aar, zip, … (layer annotations `lajstrom.maven.extension`, `lajstrom.maven.classifier`, the latter absent when the file has none); the file name is `<artifactId>-<version>[-<classifier>].<extension>`, the extension can have dots (`tar.gz`), and an `.asc` signature is an artifact too, with the extension of the signed file and `.asc` (`jar.asc`, `pom.asc`) |
| maven | `application/vnd.lajstrom.maven.pom.v1+xml` | `*.pom` |
| rpm (later) | `application/vnd.lajstrom.rpm.package.v1` | `*.rpm` |
| npm (later) | `application/vnd.lajstrom.npm.tarball.v1.tar+gzip` | `*.tgz` |

### Metadata layer

`application/vnd.lajstrom.<format>.metadata.v1+json`, file name `metadata.json`. One JSON object
whose content is defined per format:

| Format | Content of `metadata.json` |
| --- | --- |
| raw | `{"path": "<original path>", "contentType": "<as uploaded>", "size": n, "sha256": "<hex>"}` |
| pypi | the core metadata (PEP 566 JSON form) of the distribution: the fields with their keys in lowercase and `-` as `_` (`name`, `version`, `summary`, `requires_python`, `requires_dist`, `classifier`, …), the multiple-use fields as lists, the body as `description`; plus `filename`, `sha256`, `packagetype` (`bdist_wheel`/`sdist`) and `core_metadata`, the `METADATA` or `PKG-INFO` file as the distribution has it, which the simple index serves for PEP 658 |
| deb | `{"control": {<control fields as strings>}, "md5sums": {<path>: <md5>}, "size": n, "sha256": "<hex>", "md5": "<hex>", "sha1": "<hex>", "filename": "…"}`; `md5` and `sha1` of the whole file are optional (apt's `Packages` carries `MD5sum`, `SHA1` and `SHA256`; an upload through Lajstrom writes all three, a manifest pushed without the two still validates) |
| maven | `{"groupId", "artifactId", "version", "packaging", "files": [{"name", "extension", "classifier", "sha1", "md5", "sha256", "sha512", "size"}], "pom": {<parsed essentials: parent, dependencies>}}`; one entry per file layer, the POM included; the four checksums are of the whole file, computed at ingest, and are what the `.md5`, `.sha1`, `.sha256` and `.sha512` files serve (Gradle asks for `.sha512`) |

The metadata layer is optional for `raw` and mandatory for every other format. UI, search and
reindex read it without unpacking the package. Reindex must work from annotations alone when the
metadata layer is missing or unreadable (degraded rows are flagged, not dropped).

## 3. Annotations

All keys are prefixed `lajstrom.`. Values are strings. Keys marked **R** are required on every
manifest; reindex rejects manifests without them (they are listed as "foreign" in the UI and
skipped).

| Key | R | Value |
| --- | --- | --- |
| `lajstrom.schema` | R | `"1"`. Bump only with an ADR. |
| `lajstrom.format` | R | repo format: `raw`, `pypi`, `apt`, `maven`, … (the *protocol*, matches the first URL segment) |
| `lajstrom.repo` | R | repo name as created in the control plane, e.g. `ubuntu-internal` |
| `lajstrom.name` | R | package name in the format's own spelling (original case): `nginx`, `com.example:lib-core` (maven `groupId:artifactId`), raw: the full path; for pypi the PEP 503 normalized name, `requests` for `Requests`, so that every spelling of a project is one package (the original spelling is the `name` of the metadata layer; decided on #31) |
| `lajstrom.version` | R | version string exactly as the format spells it: `1:1.18.0-6ubuntu14.4`, `2.32.3`, `1.2.0` |
| `lajstrom.variant` | | platform discriminator, present when the format has one: deb `<suite>/<component>/<arch>`, the values of `lajstrom.deb.suite`, `.component` and `.arch` joined with `/` (`jammy/main/amd64`, `bookworm/main/all`); rpm arch (later); pypi wheel tag (`py3-none-any`, `cp311-cp311-manylinux_2_17_x86_64`) or `sdist`. Absent for maven, npm, raw. |
| `lajstrom.deb.arch`, `lajstrom.deb.suite`, `lajstrom.deb.component` | deb | upload metadata and the three parts of `lajstrom.variant`, kept as annotations (and in `versions.attrs`) so that the apt indexes group by them without parsing the variant; `arch` is the control file's `Architecture`. Suite and component are properties of the upload, not separate repos: the same `.deb` in two suites is two manifests over the same blobs |
| `lajstrom.pypi.nonstandard` | pypi | `true` when the version is not a PEP 440 version: accepted and kept as written; pip 22 and later ignore such versions |
| `lajstrom.maven.groupId`, `lajstrom.maven.artifactId`, `lajstrom.maven.packaging` | maven | coordinates |
| `lajstrom.raw.path` | raw | original path, case preserved, `/`-separated |
| `lajstrom.uploaded-by` | | the principal that performed the ingest, in its canonical form: `user:<uid>`, `token:<id>` (informational, not trusted for authz; a user's name is looked up by its uid, rbac.md §1) |
| `org.opencontainers.image.created` | R | RFC 3339 UTC time of ingest |
| `org.opencontainers.image.source`, `org.opencontainers.image.revision` | | passed through from the client if present (CI provenance) |

Clients that push with `oras` set the required annotations themselves
(`oras push --annotation lajstrom.format=apt …`). The frontends set them from the protocol.
The plugin's `Validate` checks that annotations and metadata layer agree with the bytes
(for example the control file inside the `.deb` says the same name, version and arch).

## 4. Naming: OCI repository and tag

### 4.1 Repository path

The OCI repository for a package is

```
<prefix><format>/<repo>/<package path>
```

- `<prefix>` is the backend-registry prefix of the Lajstrom instance (e.g. `art/`), may be empty.
- `<format>` and `<repo>` are already OCI-safe (repo names are validated on creation:
  `^[a-z0-9]+(?:[-._][a-z0-9]+)*$`, max 64 chars).
- `<package path>` is produced by the plugin's `EscapeName`:

| Format | Package path |
| --- | --- |
| raw | each path component escaped with `EscapeComponent` (section 4.3), joined with `/`: `Docs/Readme.MD` → `docs/readme.md` (the original case is in `lajstrom.raw.path`). A component whose escaped form is longer than 64 bytes is replaced by `h0__` + the first 32 hex characters of `sha256(component)`, as in section 4.3 step 3: registries limit the OCI repository name to 255 characters, and `reindex` reads `lajstrom.raw.path`, so nothing decodes the path. A path whose OCI repository name is still longer than 255 bytes is refused with `400`. A raw path is unique without regard to case, since the OCI repository name is lowercase: the first spelling wins, and a path whose package path is taken by another spelling is a `409`. |
| pypi | `EscapeComponent(lajstrom.name)`, the normalized name: `Requests` → `requests` |
| deb | `EscapeComponent(package)`: `libstdc++6` → `libstdc_2b_2b6` |
| maven | `EscapeComponent(groupId lowercased)/EscapeComponent(artifactId)`: `com.example:Lib-Core` → `com.example/lib-core` |

Examples: `art/apt/ubuntu-internal/nginx`, `art/pypi/internal/requests`,
`art/maven/releases/com.example/lib-core`, `art/raw/assets/docs/readme.md`.

### 4.2 Tag

```
tag = Escape(version) [ "__" Escape(variant) ]
```

- Tags are **immutable and append-only**. A new architecture, a new suite or component of a
  `.deb`, or a new wheel is a new tag. A tag
  is never re-pointed. Ingest of a tag that exists with a different digest is a `409 Conflict`;
  with the same digest it is idempotent (`201`). A frontend upload of the same files under a tag
  that holds them is the idempotent repeat as well, whatever `org.opencontainers.image.created`
  the new build carries: two builds of one file differ in nothing but that, so the file layers
  decide, and the manifest in the registry stays the first. Through the OCI facade another
  manifest under the tag is always the `409`.
- The separator between version and variant is `__` (two underscores). `Escape` never produces
  `__`, so the split is unambiguous.
- If the resulting tag is longer than 128 characters it is replaced by
  `_h` + first 40 hex characters of the `sha256` of the unhashed tag string
  (`Escape(version)[__Escape(variant)]`, injective in the pair because `Escape` is prefix-free and
  never produces `__`). `Escape` output never starts with `_h`, so hashed tags cannot collide
  with escaped ones. The index stores the tag; the
  truth is in annotations, so nothing needs to decode a hashed tag.

Examples:

| version | variant | tag |
| --- | --- | --- |
| `1:1.18.0-6ubuntu14.4` | `jammy/main/amd64` | `1_3a1.18.0-6ubuntu14.4__jammy_2fmain_2famd64` |
| `2.32.3` | `py3-none-any` | `2.32.3__py3-none-any` |
| `2.32.3` | `sdist` | `2.32.3__sdist` |
| `1.2.0` | | `1.2.0` |
| `1.0.0+build.7` | | `1.0.0_2bbuild.7` |
| `2.1~rc1` | `jammy/main/all` | `2.1_7erc1__jammy_2fmain_2fall` |
| `manylinux_2_17` (as a variant) | | `…__manylinux_5f2_5f17` |

The deb tag is `Escape(version)__Escape(suite/component/arch)`: the `/` of the variant becomes
`_2f`, and the same `.deb` in `jammy` and `jammy-updates` is two tags,
`1.0.0-1__jammy_2fmain_2famd64` and `1.0.0-1__jammy-updates_2fmain_2famd64`, over one file
layer with one file name. A deb tag longer than 128 characters is hashed like any other.

### 4.3 `Escape` and `EscapeComponent`

`Escape(s string) string`, for tags. Operates on the UTF-8 bytes of `s`:

1. bytes `A–Z a–z 0–9 . -` are copied;
2. every other byte, including `_`, becomes `_` followed by two lowercase hex digits
   (`_` → `_5f`, `:` → `_3a`, `+` → `_2b`, `~` → `_7e`, `/` → `_2f`, space → `_20`);
3. if the first output character is `.` or `-` (not allowed as the first character of an OCI tag)
   it is encoded as `_2e` / `_2d` as well.

The output matches `^[A-Za-z0-9_][A-Za-z0-9._-]*$` and the encoding is prefix-free (every `_` is
followed by exactly two hex digits), so `Unescape` is unambiguous. Note the deliberate deviation
from the 2026-11-05 draft, which encoded `_` as `__`: that made the version/variant separator
ambiguous (`a_` + `_b` vs `a` + `__b`). See ADR 0005.

`EscapeComponent(s string) string`, for repository path components:

1. lower-case ASCII letters (the original spelling is kept in `lajstrom.name` / `lajstrom.raw.path`);
2. apply `Escape`;
3. if the result does not match the OCI path-component grammar
   `^[a-z0-9]+(?:(?:[._]|__|[-]+)[a-z0-9]+)*$` (for example because it starts with `_` or
   contains `.-`), replace it with `h0__` + first 32 hex characters of `sha256(s)`. `Escape`
   never produces `__`, so a hashed component cannot collide with an escaped one.

Package names of the MVP formats are ASCII-restricted by their own specs, so step 3 is a safety
net, not a normal path.

`Unescape` is provided for tooling and tests; **no server code path depends on decoding a tag or
a path**. Reindex reads `lajstrom.*` annotations.

## 5. Validation on ingest

The ingest pipeline rejects a manifest with `400` (facade: `MANIFEST_INVALID`) when:

- `artifactType` is not `application/vnd.lajstrom.<format>.v1` for a plugin that is enabled on
  the target repo (repos of type `oci` accept anything and index nothing);
- `config` is not the empty descriptor;
- a required annotation is missing, or `lajstrom.format` / `lajstrom.repo` do not match the URL
  the manifest was pushed to;
- the repository path computed from the annotations (section 4.1) differs from the path pushed to;
- the tag computed from the annotations (section 4.2) differs from the tag pushed to;
- a layer media type is not one of the format's roles, or a layer has no
  `org.opencontainers.image.title` (or a title with a directory, except for `raw`);
- a layer descriptor is malformed: the digest is not `<algorithm>:<encoded>` of a known
  algorithm, or the size is negative;
- the plugin's `Validate` finds the bytes and the annotations disagree (deb control vs
  annotations, wheel file name vs version/variant, POM coordinates vs path);
- a layer referenced by the manifest has not been uploaded (the facade checks blob existence in
  the backend before `PUT manifest`).

`409 Conflict` (facade: `TAG_INVALID` with a message) when the tag exists with another digest.
Sizes: a single file is limited by the backend registry's blob limit; Lajstrom enforces an
optional per-repo `maxFileSize` from the control plane.

## 6. Referrers

Signatures and attestations attach with `subject` = our manifest and are passed through to the
backend unchanged. Lajstrom lists referrers with the referrers API when the provider has it, and
with the `sha256-<digest>` tag schema otherwise (`Capabilities.Referrers`). Lajstrom never
rewrites a referrer. Artifact types the UI recognises: cosign (`application/vnd.dev.cosign.*`),
notation (`application/vnd.cncf.notary.signature`), SPDX, CycloneDX, in-toto/DSSE.

## 7. Repos of type `oci` (passthrough; after the MVP)

Anything pushed to `/v2/oci/<repo>/…` is proxied as-is. Known conventions (Helm, OpenTofu, Flux,
Timoni, CUE, Wasm, ModelPack, Docker images) are recognised by artifactType/config media type for
the UI only. No annotations are required, nothing is indexed beyond repository, tag and digest.

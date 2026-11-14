# Spec: Lajstrom manifest profile

- Status: normative, v1 (2026-11-06; section 4.3 changed by ADR 0017 on 2026-11-11; the pypi
  variant of a wheel with a build tag, sections 3 and 4.2, on 2026-11-12, #726; the rpm
  profile, sections 1 to 5, on 2026-11-12, #64; attachments, section 6, on 2026-11-14, #89). Changes need an issue labelled `contract`
  and, if the wire format changes, an ADR.
- Implemented by: `internal/oci/profile` (pure Go, no I/O).
- Related: [ADR 0005](../adr/0005-manifest-profile-and-immutable-tags.md),
  [ADR 0017](../adr/0017-the-grammar-of-oci-names.md),
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
| `artifactType` | `application/vnd.lajstrom.<type>.v1`. `<type>` is the profile type of the repo's format, the format's name but `deb` for apt: `raw`, `pypi`, `deb`, `maven` in the MVP; `npm` in M4 (ADR 0012); `rpm` in M6 (#64); `nuget`, `cargo`, `go` later. It selects the plugin and filters referrers. Note: the apt *protocol* stores `deb` *packages*; the artifactType is `deb`, the annotation `lajstrom.format` is `apt` (the repo format). An npm package's OCI repository also holds the journal of its dist-tags, manifests of their own types (section 8). |
| `config` | always the OCI empty descriptor (`application/vnd.oci.empty.v1+json`, the 2-byte blob `{}`). No logic in config. |
| `layers` | one or more file layers (exactly one for `raw` and `npm`) plus at most one metadata layer. Order: file layers first, metadata layer last. Every layer carries `org.opencontainers.image.title` with the exact file name (no directories, except for `raw`, whose title is the original `/`-separated path, section 4.1). |
| `subject` | not set by Lajstrom. Referrers (signatures, SBOMs) set `subject` to our manifest. |
| `annotations` | the search keys listed in section 3. Anything large goes to the metadata layer. |

## 2. Media types

### File layers

`application/vnd.lajstrom.<type>.<role>.v1[.tar][+suffix]`, `<type>` that of the `artifactType`.
The role says what the file is; the suffix follows the OCI convention for the encoding of the
bytes.

| Format | Role media type | File |
| --- | --- | --- |
| raw | `application/vnd.lajstrom.raw.file.v1` | any file |
| pypi | `application/vnd.lajstrom.pypi.wheel.v1+zip` | `*.whl` |
| pypi | `application/vnd.lajstrom.pypi.sdist.v1.tar+gzip` | `*.tar.gz` sdist |
| pypi | `application/vnd.lajstrom.pypi.sdist.v1+zip` | `*.zip` sdist (legacy) |
| deb | `application/vnd.lajstrom.deb.package.v1` | `*.deb` (ar archive) |
| maven | `application/vnd.lajstrom.maven.artifact.v1` | jar, war, aar, zip, … (layer annotations `lajstrom.maven.extension`, `lajstrom.maven.classifier`, the latter absent when the file has none); the file name is `<artifactId>-<version>[-<classifier>].<extension>`, the extension can have dots (`tar.gz`), and an `.asc` signature is an artifact too, with the extension of the signed file and `.asc` (`jar.asc`, `pom.asc`) |
| maven | `application/vnd.lajstrom.maven.pom.v1+xml` | `*.pom` |
| rpm | `application/vnd.lajstrom.rpm.package.v1` | `*.rpm`, a binary or a source package of rpm v3 or v4 (a lead, a signature header, a header), exactly one per manifest, titled `<name>-<version>-<release>.<arch>.rpm` from the header's `NAME`, `VERSION`, `RELEASE` and the arch of `lajstrom.variant` (`hello-2.12-1.el9.x86_64.rpm`, `hello-2.12-1.el9.src.rpm`), the name `rpmbuild` gives it and dnf's `Packages/` serves it under: no epoch |
| npm | `application/vnd.lajstrom.npm.tarball.v1.tar+gzip` | the package tarball, exactly one per manifest, titled `<name without the scope>-<version>.tgz` (`widgets-1.2.0.tgz` for `@acme/widgets`), the file name npm serves it under; layer annotations `lajstrom.npm.integrity` (`sha512-<base64>`, npm's `dist.integrity`) and `lajstrom.npm.shasum` (sha1, lowercase hex, npm's `dist.shasum`), so that a version without its metadata layer still renders its `dist` |

### Metadata layer

`application/vnd.lajstrom.<type>.metadata.v1+json`, file name `metadata.json`. One JSON object
whose content is defined per format:

| Format | Content of `metadata.json` |
| --- | --- |
| raw | `{"path": "<original path>", "contentType": "<as uploaded>", "size": n, "sha256": "<hex>"}` |
| pypi | the core metadata (PEP 566 JSON form) of the distribution: the fields with their keys in lowercase and `-` as `_` (`name`, `version`, `summary`, `requires_python`, `requires_dist`, `classifier`, …), the multiple-use fields as lists, the body as `description`; plus `filename`, `sha256`, `packagetype` (`bdist_wheel`/`sdist`) and `core_metadata`, the `METADATA` or `PKG-INFO` file as the distribution has it, which the simple index serves for PEP 658 for a wheel only (a build may change an sdist's `PKG-INFO`) |
| deb | `{"control": {<control fields as strings>}, "md5sums": {<path>: <md5>}, "size": n, "sha256": "<hex>", "md5": "<hex>", "sha1": "<hex>", "filename": "…"}`; `md5` and `sha1` of the whole file are optional (apt's `Packages` carries `MD5sum`, `SHA1` and `SHA256`; an upload through Lajstrom writes all three, a manifest pushed without the two still validates) |
| maven | `{"groupId", "artifactId", "version", "packaging", "files": [{"name", "extension", "classifier", "sha1", "md5", "sha256", "sha512", "size"}], "pom": {<parsed essentials: parent, dependencies>}}`; one entry per file layer, the POM included; the four checksums are of the whole file, computed at ingest, and are what the `.md5`, `.sha1`, `.sha256` and `.sha512` files serve (Gradle asks for `.sha512`) |
| rpm | `{"name", "epoch", "version", "release", "arch", "summary", "description", "packager", "url", "license", "vendor", "group", "buildhost", "sourcerpm", "buildtime", "installedSize", "archiveSize", "headerStart", "headerEnd", "provides", "requires", "conflicts", "obsoletes", "recommends", "suggests", "supplements", "enhances", "files", "changelogs", "filename", "size", "sha256"}`: what the repodata of a version needs (http-routing.md, rpm), read from the header. `epoch` is the header's `EPOCH`, `0` without one; `version`, `release` and `arch` are those of the annotations (`arch` `src` or `nosrc` for a source package); the strings are the header's, the C locale's of an internationalised one, `""` for a tag the header lacks; `buildtime` is seconds since 1970; `installedSize` is `LONGSIZE` or `SIZE`, `archiveSize` the payload's uncompressed size (the signature header's `LONGARCHIVESIZE` or `PAYLOADSIZE`, else the header's `ARCHIVESIZE`), `0` when the package has none; `headerStart` and `headerEnd` are the offsets of the header in the file, primary's `rpm:header-range`. Each of the eight relations is a list, `[]` when empty, of `{"name", "flags", "epoch", "version", "release", "pre"}` in the order of the header: `flags` the header's sense flags as a number, `epoch`, `version` and `release` the parts of its version (`""` each when it has none), `pre` true for a requirement of the install scripts (the flags `PREREQ`, `SCRIPT_PRE` or `SCRIPT_POST`). `files` is `[{"path", "type"}]` in the order of the header, `type` `dir` for a directory, `ghost` for a `%ghost`, absent otherwise; `changelogs` is `[{"author", "date", "text"}]` in the order of the header, the newest first. `filename` is the file layer's title, `size` and `sha256` those of the file. `Validate` reads the header of the file layer and refuses a metadata layer that is not what the header says, field by field. At most 16 MiB (a header that makes more is `400`) |
| npm | `{"name", "version", "manifest": {<the version document>}, "readme", "filename", "size", "integrity", "shasum", "sha256", "hasShrinkwrap", "hasInstallScript"}`; `manifest` is the version object of the publish body (http-routing.md, npm) without `dist`, `readme` and every key that begins with `_` (`_id`, `_resolved`, `_from`, `_nodeVersion`, …): what the packument serves in `versions`, with the `dist`, `_id` and `deprecated` it renders; an `oras push` writes the tarball's `package.json` there. `readme` is the version document's, or the body's top-level one (yarn), absent for npm's placeholder `ERROR: No README data found!`. `size` and the checksums are of the tarball. `hasShrinkwrap`: the tarball has `npm-shrinkwrap.json` in its top directory (`package/`); `hasInstallScript`: its `package.json` has a `preinstall`, `install` or `postinstall` script, or the top directory has `binding.gyp` (npm runs `node-gyp rebuild`); both are read from the tarball, never from the body. `Validate` checks `name`, `version`, `filename`, `size` and the checksums against the annotations and the tarball layer, and the tarball's `package.json` for the name and version. At most 2 MiB (a publish above it is `413`) |

The metadata layer is optional for `raw` and mandatory for every other format: a manifest of
another format without one is refused on ingest (`400`), and reindex does not index it (the
report lists it in `errors`). Reindex reads the layer without unpacking the package. When a
manifest lists it and the registry does not have its blob, or its bytes are not JSON, reindex
writes the row from the annotations alone, without the plugin's `Validate`: a degraded row
(`versions.degraded`), flagged, not dropped; ingest refuses such a manifest.

## 3. Annotations

All keys are prefixed `lajstrom.`. Values are strings. Keys marked **R** are required on every
manifest of a version, and reindex indexes no manifest without them: one without
`lajstrom.schema` (or that is no image manifest) is foreign, listed in the report's `foreign` and
skipped; one with `lajstrom.schema` that lacks another is listed in the report's `errors` and
skipped. The journal manifests of npm have a set of their own (section 8).

| Key | R | Value |
| --- | --- | --- |
| `lajstrom.schema` | R | `"1"`. Bump only with an ADR. |
| `lajstrom.format` | R | repo format: `raw`, `pypi`, `apt`, `maven`, … (the *protocol*, matches the first URL segment) |
| `lajstrom.repo` | R | repo name as created in the control plane, e.g. `ubuntu-internal` |
| `lajstrom.name` | R | package name in the format's own spelling (original case): `nginx`, `com.example:lib-core` (maven `groupId:artifactId`), raw: the full path; for pypi the PEP 503 normalized name, `requests` for `Requests`, so that every spelling of a project is one package (the original spelling is the `name` of the metadata layer; decided on #31); rpm: the header's `NAME` |
| `lajstrom.version` | R | version string exactly as the format spells it: `1:1.18.0-6ubuntu14.4`, `2.32.3`, `1.2.0`; raw: the first 16 hex digits of the sha256 of its file, whose layer digest is sha256; rpm: `[<epoch>:]<version>-<release>` from the header's `EPOCH`, `VERSION` and `RELEASE`, the epoch written only when it is not `0` (`2.12-1.el9`, `1:2.12-1.el9`), so that what rpm compares as one version is one tag |
| `lajstrom.variant` | | platform discriminator, present when the format has one: deb `<suite>/<component>/<arch>`, the values of `lajstrom.deb.suite`, `.component` and `.arch` joined with `/` (`jammy/main/amd64`, `bookworm/main/all`); rpm the arch, as the repodata writes it: the header's `ARCH` (`x86_64`, `aarch64`, `noarch`) for a binary package, `src` for a source package (`nosrc` when it lacks a source or a patch, `NOSOURCE`, `NOPATCH`); pypi: the wheel's compressed tag set (`py3-none-any`, `cp311-cp311-manylinux_2_17_x86_64`), after its PEP 427 build tag and `-` when the file name has one (`1-py3-none-any`, section 4.2), or `sdist`. Absent for maven, npm, raw. |
| `lajstrom.deb.arch`, `lajstrom.deb.suite`, `lajstrom.deb.component` | deb | upload metadata and the three parts of `lajstrom.variant`, kept as annotations (and in `versions.attrs`) so that the apt indexes group by them without parsing the variant; `arch` is the control file's `Architecture`. Suite and component are properties of the upload, not separate repos: the same `.deb` in two suites is two manifests over the same blobs |
| `lajstrom.rpm.*` | rpm | none: the variant is the arch, and the parts of the version are in the metadata layer |
| `lajstrom.pypi.nonstandard` | pypi | `true` when the version is not a PEP 440 version: accepted and kept as written; pip 22 and later ignore such versions |
| `lajstrom.maven.groupId`, `lajstrom.maven.artifactId`, `lajstrom.maven.packaging` | maven | coordinates |
| `lajstrom.npm.*` | npm | on a version, none beyond the layer annotations of section 2; `lajstrom.name` is the name as npm writes it, `@acme/widgets` with its scope. The journal's keys are in section 8 |
| `lajstrom.raw.path` | raw | original path, case preserved, `/`-separated; the same as `lajstrom.name` and as the `org.opencontainers.image.title` of the file layer |
| `lajstrom.uploaded-by` | | the principal that performed the ingest, in its canonical form: `user:<uid>`, `token:<id>` (informational, not trusted for authz; a user's name is looked up by its uid, rbac.md §1) |
| `org.opencontainers.image.created` | R | RFC 3339 UTC time of ingest |
| `org.opencontainers.image.source`, `org.opencontainers.image.revision` | | passed through from the client if present (CI provenance) |

Clients that push with `oras` set the required annotations themselves
(`oras push --annotation lajstrom.format=apt …`). The frontends set them from the protocol.
The plugin's `Validate` checks that annotations and metadata layer agree with the bytes
(for example the control file inside the `.deb` says the same name, version and arch).

### 3.1 Proxied artifacts

An artifact that a proxy repo fetched from its upstream ([ADR 0013](../adr/0013-proxy-and-group-repos.md),
[proxy-and-group.md](proxy-and-group.md) §5) is an ordinary manifest of its format's profile,
built and validated as an upload of the same files would be, with two annotations more and
without `lajstrom.uploaded-by`:

| Key | Value |
| --- | --- |
| `lajstrom.origin.url` | the URL the file was asked for, before any redirect, without userinfo, query or fragment: `https://registry.npmjs.org/left-pad/-/left-pad-1.3.0.tgz`. For an artifact of several files, the URL of the first file layer |
| `lajstrom.origin.checksum` | the upstream's checksum that was verified before the ingest, `<algorithm>:<lowercase hex>`: `sha512:…`, `sha256:…`, `sha1:…` |

`org.opencontainers.image.created` of such a manifest is the upstream's publish time of the
version when the upstream's document gives one (npm: the packument's `time`; #441), and the time
of the fetch otherwise: a version keeps its date when its proxy becomes a hosted repo
(proxy-and-group.md §2.6), and two fetches of the same bytes build the same manifest.

Only the fetcher writes them: a manifest that a client pushes or uploads with a key that starts
with `lajstrom.origin.` is rejected with `400` (section 5) on every manifest Lajstrom manages, a
version's manifest and a referrer (section 6), on a hosted repo and on a proxy one alike. An
`oci` repo's pass-through is not checked (section 7): Lajstrom stores its manifests, a referrer
among them, as they came, and never indexes them. Reindex reads the keys as it reads every
annotation, so a proxy repo is rebuilt from the registry like a hosted one, and the index keeps
them in `versions.attrs`.

## 4. Naming: OCI repository and tag

### 4.1 Repository path

The OCI repository for a package is

```
<prefix><format>/<repo>/<package path>
```

- `<prefix>` is the backend-registry prefix of the Lajstrom instance (e.g. `art/`), may be empty.
- `<format>` and `<repo>` are already OCI-safe (repo names are validated on creation:
  `^[a-z0-9]+(?:[-._][a-z0-9]+)*$`, max 64 chars, and none that section 4.3 reserves, nor
  `tags`).
- `<package path>` is produced by the plugin's `EscapeName`:

| Format | Package path |
| --- | --- |
| raw | each path component escaped with `EscapeComponent` (section 4.3), joined with `PackagePath`: `Docs/Readme.MD` → `docs/readme.md` (the original case is in `lajstrom.raw.path`). A component whose escaped form (section 4.3 step 2) is longer than 64 bytes is replaced by `h0_h` + the first 32 hex characters of `sha256(component)`, as in section 4.3 step 3 (`EscapeComponentMax`): registries limit the OCI repository name to 255 characters, and `reindex` reads `lajstrom.raw.path`, so nothing decodes the path. A path whose OCI repository name is still longer than the registry takes is refused with `400`: 255 bytes with the instance prefix on distribution and most registries, 256 on ECR (`provider.Capabilities.MaxNameLength`). A raw path is unique without regard to case, since the OCI repository name is lowercase: the first spelling wins, and a path whose package path is taken by another spelling is a `409`. |
| pypi | `EscapeComponent(lajstrom.name)`, the normalized name: `Requests` → `requests` |
| deb | `EscapeComponent(package)`: `libstdc++6` → `libstdc_2b_2b6` |
| rpm | `EscapeComponent(NAME)`: `libstdc++` → `libstdc_2b_2b`, `NetworkManager` → `networkmanager` (the case stays in `lajstrom.name`) |
| maven | `PackagePath(EscapeComponent(groupId lowercased), EscapeComponent(artifactId))`: `com.example:Lib-Core` → `com.example/lib-core` |
| npm | `EscapeComponent(name)`; a scoped name `@scope/name` is two components without the `@`, `PackagePath(EscapeComponent(scope), EscapeComponent(name))`: `lodash` → `lodash`, `@acme/widgets` → `acme/widgets`, `@types/node` → `types/node`, `JSONStream` → `jsonstream` (an old npmjs.org name; the case stays in `lajstrom.name`). An unscoped name has no `/`, so a path of one component is an unscoped package and one of two a scoped one |

Examples: `art/apt/ubuntu-internal/nginx`, `art/pypi/internal/requests`,
`art/maven/releases/com.example/lib-core`, `art/raw/assets/docs/readme.md`,
`art/npm/internal/acme/widgets`.

Names that differ only in ASCII case have one package path, unless a component is replaced by
its hash (section 4.3 step 3, or a long raw component), which is of the original spelling but
for a maven groupId, lower-cased first: a raw path, a maven artifactId and an npm name keep
their case in `lajstrom.name` and in the hash; pypi and deb names are lowercase already. The
first spelling published holds the package path, and a publish of another spelling is a `409`
naming the first, refused before its manifest is pushed (index-schema.md, Coordination; #244).

### 4.2 Tag

```
tag = Escape(version) [ "__" Escape(variant) ]
```

- Tags are **immutable and append-only**. A new architecture, a new suite or component of a
  `.deb`, or a new wheel is a new tag. A tag
  is never re-pointed. Ingest of a tag that exists with a different digest is a `409 Conflict`;
  with the same digest it is idempotent, answered with the route's success (`201` on `/v2`;
  http-routing.md §3 has each frontend's). A frontend upload of the same files under a tag
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
| `2.32.3` | `1-py3-none-any` (build tag `1`) | `2.32.3__1-py3-none-any` |
| `2.32.3` | `sdist` | `2.32.3__sdist` |
| `1.2.0` | | `1.2.0` |
| `1.0.0+build.7` | | `1.0.0_2bbuild.7` |
| `2.1~rc1` | `jammy/main/all` | `2.1_7erc1__jammy_2fmain_2fall` |
| `2.12-1.el9` | `x86_64` (rpm) | `2.12-1.el9__x86_5f64` |
| `1:2.12-1.el9` | `src` (rpm) | `1_3a2.12-1.el9__src` |
| `manylinux_2_17` (as a variant) | | `…__manylinux_5f2_5f17` |

The deb tag is `Escape(version)__Escape(suite/component/arch)`: the `/` of the variant becomes
`_2f`, and the same `.deb` in `jammy` and `jammy-updates` is two tags,
`1.0.0-1__jammy_2fmain_2famd64` and `1.0.0-1__jammy-updates_2fmain_2famd64`, over one file
layer with one file name. A deb tag longer than 128 characters is hashed like any other.

The pypi variant of a wheel is the file name's parts after the version: the build tag, when
there is one, and the tag set. `foo_bar-1.1.0-1-py3-none-any.whl` is `1.1.0__1-py3-none-any`,
beside `1.1.0__py3-none-any` of `foo_bar-1.1.0-py3-none-any.whl`: a build tag re-releases a fixed
wheel of a version, and pip prefers the higher build (PEP 427). A build tag starts with a digit
and no part of a wheel file name has a `-`, so the variant of a wheel with a build tag is never
that of a wheel without one. Before #726 the variant was the tag set alone, so a wheel with a
build tag uploaded then keeps the tag of its tag set: written in the newest form, read in all.
Validation takes both forms for such a file name, an upload of a file the index has in the
earlier form goes to its tag (the same bytes are the idempotent repeat, other bytes `409`), and
the wheel of the same version and tag set without a build tag is a `409` that names it.

The rpm tag is `Escape(version)__Escape(arch)`: one tag per architecture of a version, and the
source package beside them (`2.12-1.el9__src`). Two versions that differ only in the epoch are two
tags over two file layers with one title, since the file name has no epoch: a repo holds one
content per file name, so the upload route refuses the second with a `409` (http-routing.md, rpm).

The npm tag is `Escape(version)` (`1.2.0`, `2.0.0-rc.1`); npm has no variant. The tags `_j…`
of an npm package's OCI repository are its journal (section 8): every `_` that `Escape` writes
is followed by two hex digits and a hashed tag begins with `_h`, so no version has such a tag.

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

`EscapeComponent(s string) string`, for repository path components
([ADR 0017](../adr/0017-the-grammar-of-oci-names.md)):

1. lower-case ASCII letters (the original spelling is kept in `lajstrom.name` / `lajstrom.raw.path`);
2. apply `Escape`;
3. if the result does not match the path-component grammar of the distribution specification
   1.1, `^[a-z0-9]+(?:(?:[._]|__|[-]+)[a-z0-9]+)*$` (for example because it starts with `_` or
   contains `.-`), replace it with `h0_h` + first 32 hex characters of `sha256(s)`, and stop.
   `Escape` writes `_` only before two hex digits and `h` is none, so a hashed component cannot
   collide with an escaped one;
4. write each `-` of a run of two or more as `_2d`: `a--b` → `a_2d_2db`;
5. if the result ends in `blobs`, `.uploads` or `.sync`, or is `index.json`, `oci-layout`,
   `manifests` or `referrers`, write its last byte as `_` and two lowercase hex digits:
   `blobs` → `blob_73`, `my.uploads` → `my.upload_73`, `x.sync` → `x.syn_63`,
   `index.json` → `index.jso_6e`, `manifests` → `manifest_73`, `referrers` → `referrer_73`.

`PackagePath(components ...string) string` joins escaped components into a package path with
`/`, and writes the last byte of a component `tags` that a component starting with `list`
follows as `_73`: `x/tags/list` → `x/tag_73/list`, `x/tags/listing.txt` →
`x/tag_73/listing.txt` (the amendment of ADR 0017 for Harbor's routes; revisited when Harbor
fixes them). The first component follows the repo name, which `PackagePath` does not see: repo
names that step 5 reserves, and `tags`, are refused at creation.

The output matches the grammar of ADR 0017, `^[a-z0-9]+(?:[._-][a-z0-9]+)*$` (the distribution
specification's 1.0 grammar, which Gitea and Forgejo check), and is no word of step 5, which
Zot's storage and Harbor's routes keep for themselves; a package path has no `tags` before a
`list…`. Steps 4 and 5 and `PackagePath` write `_` and two hex digits for a byte that `Escape`
copies, a form `Escape` never writes, so the encoding stays injective but for case; the
component of a name that keeps to the grammar is that name, unless step 5 reserves it or it is a
`tags` before a `list…`. Package names of the MVP formats are
ASCII-restricted by their own specs, so step 3 is a safety net, not a normal path; a raw path
meets it with a leading `.` or `_` (`.gitignore`, `_config.yml`).

**Written in the newest form, read in all.** Before ADR 0017, step 3 wrote `h0__` and steps 4
and 5 did not exist; before its amendment, step 5 had no `manifests` and `referrers` and
`PackagePath` no `tags`. A package keeps the OCI repository it was first written to: a writer
looks the package up in the index at its package path and at each earlier form of it
(`EarlierPackagePaths`, `index.PackageAt`), and writes to the `name_oci` it finds; the newest
form is for a package the index does not have. Reindex, and validation on ingest (section 5),
take any form as the package path of the annotations; a push to an earlier form is for a package
the index has there only, and a push to the newest form of a package the index has at an earlier
one is a `409`. A package that is at two forms all the same keeps one, and reindex reports the
other (index-schema.md, Invariants and rules).

`Unescape`, `UnescapeComponent` (which decodes a component of any form to the same lower-cased
name, and refuses a hashed one) and `UnescapePackagePath` (the same for a package path, with the
mark of `tags`) are provided for tooling and tests; **no server code
path depends on decoding a tag or a path**. Reindex reads `lajstrom.*` annotations.

## 5. Validation on ingest

The ingest pipeline rejects a manifest with `400` (facade: `MANIFEST_INVALID`) when:

- `artifactType` is not `application/vnd.lajstrom.<type>.v1` of the target repo's format
  (section 1; repos of type `oci` accept anything and index nothing);
- `config` is not the empty descriptor;
- a required annotation is missing, or `lajstrom.format` / `lajstrom.repo` do not match the URL
  the manifest was pushed to;
- an annotation key starts with `lajstrom.origin.` and the proxy fetcher did not write the
  manifest (section 3.1): a version's manifest or a referrer (section 6), on a hosted or a proxy
  repo; an `oci` repo's manifests are not checked;
- the repository path computed from the annotations (section 4.1) differs from the path pushed
  to in every form of section 4.3, or the path pushed to is an earlier form and the index has no
  package there;
- the tag computed from the annotations (section 4.2) differs from the tag pushed to;
- a layer media type is not one of the format's roles, or a layer has no
  `org.opencontainers.image.title` (or a title with a directory, except for `raw`);
- a layer descriptor is malformed: the digest is not `<algorithm>:<encoded>` of a known
  algorithm, or the size is negative;
- the plugin's `Validate` finds the bytes and the annotations disagree (deb control vs
  annotations, rpm header vs annotations and metadata layer, wheel file name vs version/variant
  in either form of section 4.2, POM coordinates vs path);
- a layer referenced by the manifest has not been uploaded; on `/v2` the facade checks blob
  existence in the backend first and answers `400 MANIFEST_BLOB_UNKNOWN` naming the digest;
- pushed on `/v2`, `org.opencontainers.image.created` is more than `server.maxCreatedAhead`
  (5 minutes) after the server's clock or more than `server.maxCreatedBehind` (an hour) before
  it, and the repo does not have the manifest already (the facade checks it first). It is a
  version's publish time, which retention reads; the frontends write the server's.

`409 Conflict` (facade: `TAG_INVALID` with a message) when the tag exists with another digest,
when the index has the package at an earlier form of its path (section 4.3), or when the
package path is another spelling's, or is being published under another spelling
(section 4.1). They are found under the claims, before the registry write.
Sizes: a file, and a blob of `/v2`, is bounded by the smaller of the repo's bound and the
registry's `capabilities.maxBlobSize` (the configuration's, or the vendor table's: GHCR's
10 GB a layer). The repo's bound is the `maxFileSize` of its policy in the control plane, on
every repo, an `oci` one included; when the policy sets none, the default of its format (#246,
from the package size survey of #439): npm 512 MiB, pypi and maven 4 GiB, apt and raw 8 GiB,
`oci` 16 GiB, and 8 GiB for a format this list does not name. A policy's `maxFileSize` overrides
the default, higher or lower: an `oci` repo of ML models needs 64 to 256 GiB, a mirror of Ollama
models terabytes.

## 6. Referrers

Signatures and attestations attach with `subject` = our manifest and are passed through to the
backend unchanged, by the referrer path of ingest (`PutReferrer`), which indexes nothing but
the attachments of section 6.1; so is a manifest under cosign's legacy tag `sha256-<hex>.sig`,
`.att` or `.sbom`. Lajstrom lists referrers with the referrers API when the provider has it, and
with the `sha256-<digest>` tag schema otherwise (`Capabilities.Referrers`). Lajstrom never
rewrites a referrer. `profile.ReferrerKind` tells a referrer's kind from its manifest alone, one
of the kinds of section 6.1, for the `kind` of its audit row: from its predicate type, cosign's
legacy tag, or the media types it knows, cosign (`application/vnd.dev.cosign.*`), notation
(`application/vnd.cncf.notary.signature`), SPDX, CycloneDX, the sigstore bundle, in-toto/DSSE.

### 6.1 Attachments (#89)

A referrer of a version's manifest on a managed repo carries the version's **attachments**: its
signatures, SBOMs, scan reports, provenance and other attestations, made and signed by whoever
produced them (the lead's decisions on #89: no scanner in the server, results live in the
registry as referrers, every distinct result is kept). Lajstrom stores them as they came, lists
them per version ([index-schema.md](index-schema.md#attachments-89)), and labels each with
whether a key the operator trusts signed it (section 6.3). It never signs one on a producer's
behalf, never rewrites one, and never refuses a push for its label. An `oci` repo's referrers
are not attachments: such a repo has no versions. Neither is a referrer of a referrer.

An attachment is one layer of a referrer manifest. A sigstore bundle (cosign v3) has one layer;
cosign's legacy manifests under `sha256-<hex>.sig` and `.att` hold one layer per signature or
attestation, and cosign moves the tag to a manifest with one layer more for each new one; an
SBOM pushed with `oras attach` is a layer of its own. A manifest without layers is one
attachment. `profile.Attachments` lists the attachments of a referrer manifest. Each has a
kind:

| Kind | Recognised by |
| --- | --- |
| `signature` | the predicate type `https://sigstore.dev/cosign/sign/v1` (cosign v3 signs with a bundle); the layer media type `application/vnd.dev.cosign.simplesigning.v1+json` (cosign's legacy `.sig`), the artifact type `application/vnd.dev.cosign.artifact.sig.v1+json`; notation's `application/vnd.cncf.notary.signature` |
| `sbom` | the predicate types `https://spdx.dev/Document` and `https://cyclonedx.org/bom`, and those that begin with them and a `/` (`https://cyclonedx.org/bom/v1.6`); unsigned: a layer of SPDX (`application/spdx+json`, `text/spdx`) or CycloneDX (`application/vnd.cyclonedx`, `application/vnd.cyclonedx+json`), cosign's legacy `.sbom` |
| `scan` | the predicate type `https://cosign.sigstore.dev/attestation/vuln/v1` (trivy `--format cosign-vuln`); a CycloneDX statement whose predicate has a non-empty `vulnerabilities` (a combined document, accepted and classified as a scan) |
| `provenance` | the predicate types that begin with `https://slsa.dev/provenance/` |
| `attestation` | any other DSSE envelope or sigstore bundle, with its predicate type: OpenVEX, a custom predicate, the verdicts of ADR 0008 (`dev.lajstrom.verdict/v1`, #101) |
| `other` | anything else |

The predicate type is the signed statement's where Lajstrom reads one (section 6.3), else
cosign's annotation: `dev.sigstore.bundle.predicateType` on the manifest (v3), `predicateType`
on the layer (legacy). A CycloneDX document with vulnerabilities is a scan only where its
statement is read; `profile.ReferrerKind`, which has the manifest alone, calls it an SBOM.

### 6.2 Attachment annotations

A producer may set these on a referrer manifest, so that a listing filters without reading a
blob. All are optional and unsigned: Lajstrom copies them into the index as they are.

| Key | Value |
| --- | --- |
| `lajstrom.attachment.tool` | the producer: `trivy`, `syft`, `grype` |
| `lajstrom.attachment.toolVersion` | its version: `0.60.0` |
| `lajstrom.attachment.database` | the snapshot of the vulnerability database a scan used; absent on an SBOM |
| `lajstrom.attachment.input` | the digest of the SBOM a scan was computed from, `<algorithm>:<hex>` |

They mirror the identity of a result (#89, decision 3): a scan is (tool, tool version,
database, artifact, input), an SBOM (tool, tool version, artifact), the artifact being the
subject. Another key that begins with `lajstrom.attachment.` is kept with the other
annotations and has no meaning. An `input` that is not a digest is not indexed as one (the row
keeps it in `annotations` only). `oras attach --annotation` sets them; `cosign attest` (v3.1)
sets no manifest annotation, so an attestation made with it has none of them.

### 6.3 Verification

The operator's trust (`attachments.trust`, [configuration.md](../configuration.md)) names
public keys, each trusted for every predicate type or for those it lists; a signature counts as
the predicate type `https://sigstore.dev/cosign/sign/v1`. Each attachment gets one label:

| Verification | Meaning |
| --- | --- |
| `verified` | a trusted key, trusted for its predicate type, signed it, and what it signed names the version's manifest |
| `untrusted` | it carries a signature that no trusted key verifies |
| `unsigned` | it carries no signature: a raw SBOM, an envelope without signatures, an `other` |
| `invalid` | its envelope does not parse, or its payload is not an in-toto statement, or a trusted key signed a statement or a payload that names another manifest |
| `unsupported` | signed in a way Lajstrom does not verify: a Fulcio certificate (keyless), notation, a bundle with a message signature instead of a DSSE envelope, a layer larger than `attachments.maxBlobSize` |

What is checked:

- **DSSE**: a sigstore bundle's `dsseEnvelope` (media types
  `application/vnd.dev.sigstore.bundle.v0.3+json` and `application/vnd.dev.sigstore.bundle+json;version=0.1`,
  `0.2`, `0.3`), and the layer of a legacy `.att` (`application/vnd.dsse.envelope.v1+json`). A
  signature over the envelope's pre-authentication encoding of `payloadType` and `payload`
  (DSSE v1), the payload type `application/vnd.in-toto+json`, and a statement whose `subject`
  has a `sha256` digest equal to the version's manifest digest. Without the last check a signed
  attestation of another artifact would verify on this one.
- **cosign's simple signing** (a legacy `.sig` layer): the signature, base64 in the layer's
  annotation `dev.cosignproject.cosign/signature`, over the layer's bytes, whose
  `critical.image.docker-manifest-digest` is the version's manifest digest.
- **Keys**: PEM `PUBLIC KEY` (PKIX): ECDSA P-256, P-384 and P-521, over SHA-256, SHA-384 and
  SHA-512 of the signed bytes (cosign's `cosign.pub` is P-256); Ed25519; RSA with PKCS #1 v1.5
  and SHA-256. A KMS key that `cosign attest --key awskms://…` uses verifies with its public
  key.
- **Not checked**: a transparency log or a timestamp. A key signature needs no Rekor entry
  (`cosign attest --tlog-upload=false`), and one it has is not looked at. Keyless verification
  (Fulcio identities) is not built: it needs a dependency that layout.md §3 does not list (#89,
  option A), and such an attachment is `unsupported`.

The label is decided when the attachment is indexed, by ingest and by reindex, against the
trust of the replica that indexes it, and a change of the trust reaches each attachment at the
next reindex of its repo (index-schema.md). The layers are read through the provider, at most
`attachments.maxBlobSize` of each.

## 7. Repos of type `oci` (passthrough)

Anything pushed to `/v2/oci/<repo>/…` is passed to the backend as-is (http-routing.md §2). No
annotations are required, and nothing is indexed but the repository, in `oci_repos`; its tags and
digests are the backend's. Recognising known conventions (Helm, OpenTofu, Flux, Timoni, CUE, Wasm,
ModelPack, Docker images) by artifactType or config media type is for the UI, after the MVP.

## 8. The npm journal: dist-tags and deprecations (ADR 0012)

What changes on an npm package, its dist-tags and the deprecation of its versions, is an
append-only journal in the package's OCI repository: one manifest per change, never rewritten,
its tag never re-pointed. The index holds the current state (index-schema.md, npm), and reindex
replays the journal. The 7th change of `@acme/widgets` in the repo `internal`, under the tag
`_j00000007` of `art/npm/internal/acme/widgets`:

```json
{
  "schemaVersion": 2,
  "mediaType": "application/vnd.oci.image.manifest.v1+json",
  "artifactType": "application/vnd.lajstrom.npm.dist-tags.v1",
  "config": {
    "mediaType": "application/vnd.oci.empty.v1+json",
    "digest": "sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a",
    "size": 2
  },
  "layers": [
    {
      "mediaType": "application/vnd.oci.empty.v1+json",
      "digest": "sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a",
      "size": 2
    }
  ],
  "annotations": {
    "lajstrom.schema": "1",
    "lajstrom.format": "npm",
    "lajstrom.repo": "internal",
    "lajstrom.name": "@acme/widgets",
    "lajstrom.npm.seq": "7",
    "lajstrom.npm.op": "add",
    "lajstrom.npm.tag": "beta",
    "lajstrom.npm.version": "1.3.0-beta.2",
    "lajstrom.uploaded-by": "user:019a6b1e-5c3d-7f00-8a11-2b3c4d5e6f70",
    "org.opencontainers.image.created": "2026-11-10T10:00:00Z"
  }
}
```

| artifactType | `lajstrom.npm.op` | Keys of the op | Meaning |
| --- | --- | --- | --- |
| `application/vnd.lajstrom.npm.dist-tags.v1` | `add` | `lajstrom.npm.tag`, `lajstrom.npm.version` | the dist-tag names the version from now on |
| `application/vnd.lajstrom.npm.dist-tags.v1` | `rm` | `lajstrom.npm.tag` | the dist-tag is removed |
| `application/vnd.lajstrom.npm.deprecation.v1` | `deprecate` | `lajstrom.npm.version`, `lajstrom.npm.message` | the version is deprecated with the message |
| `application/vnd.lajstrom.npm.deprecation.v1` | `undeprecate` | `lajstrom.npm.version` | the version's deprecation is cleared |

| Element | Rule |
| --- | --- |
| `config`, `layers` | the empty descriptor, and one layer that is the empty descriptor again (image-spec 1.1's form of an artifact without blobs): the change is in the annotations |
| annotations | required: `lajstrom.schema`, `lajstrom.format` (`npm`), `lajstrom.repo`, `lajstrom.name`, `lajstrom.npm.seq`, `lajstrom.npm.op`, the keys of the op and `org.opencontainers.image.created`; `lajstrom.uploaded-by` as on a version. No `lajstrom.version` nor `lajstrom.variant`: an entry is not a version |
| `lajstrom.npm.seq` | the entry's number in the package's journal, decimal: `1` for the first, one more for each next |
| tag | `_j` and the `seq` zero-padded to eight digits (`_j00000007`; more digits after `_j99999999`) |
| `lajstrom.npm.tag` | `^[A-Za-z][A-Za-z0-9._-]{0,127}$`: never begins with a digit, so never read as a version. `latest` is moved by an `add` and never removed |
| `lajstrom.npm.version` | a version the package has when the entry is written |
| `lajstrom.npm.message` | 1 to 4096 bytes of UTF-8 |

**Writing** (ingest only, for the npm routes of http-routing.md). An append holds the package's
**journal lock**: the referrers lock of index-schema.md (`Store.WithReferrersLock`) keyed by the
package's OCI repository and the digest of the empty blob (`sha256:44136fa…`, the config's
digest above), which no manifest has, so the key never meets the referrers lock or the tag lock
of a manifest. Under it the writer takes `n`, the highest `seq` the index has for the package;
looks up the tag of `n + 1` in the registry and, while there is one (an entry whose index row
was lost), validates it, indexes it and looks up the next; pushes its own entries as `n + 1`,
`n + 2`, …; and writes their `journal` rows, the package's new `journal_state` and the
invalidation of its documents in `index_cache` before it lets the lock go. The order of the
journal is the order of the lock. An append is never made under another lock: a publish appends
after its version is confirmed and its claims released, an unpublish after its deletes.

**Replay.** The state of a package is the fold of its entries in `seq` order: `add` sets the
dist-tag to the version, `rm` removes it, `deprecate` sets the version's message, `undeprecate`
clears it. An entry may name a version deleted since; the state keeps it and the packument
leaves it out (http-routing.md, npm).

**Reindex.** The `_j` tags of an npm package's OCI repository are read as its version tags are
(mark and sweep: a digest the index has is seen without a read), and the package's state is
replayed once the repository is read. An entry is reported and skipped when its annotations
break the rules above, when its tag is not the tag of its `seq`, or when its `lajstrom.name` is
not the package of the repository. A missing `seq` is reported and the replay goes on over the
rest. Entries in a repository without versions are reported as orphans (the report's
`orphanJournal`) and not indexed: no package row holds them, and the next append to a package of
that name adopts them.

**Deleting.** A journal entry goes with its package only: the unpublish of a whole package
deletes its versions, then every entry, under the journal lock and no other (no other writer
touches a `_j` tag, so no tag lock is taken for an entry). Through `/v2` the facade pushes
none (section 5: not the artifactType of the format) and deletes none (http-routing.md, npm).
A proxy repo (ADR 0013) journals nothing: a proxied package's dist-tags and deprecations are
the upstream's.

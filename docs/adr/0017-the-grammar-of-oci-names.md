# 0017. The grammar of OCI names

- Status: accepted; amended for Harbor's routes (below, Amendment)
- Date: 2026-11-11
- Deciders: gecube

## Context

Lajstrom names the OCI repository of a package `<prefix><format>/<repo>/<package path>`, and
`EscapeComponent` makes each component of the package path
([manifest-profile.md](../spec/manifest-profile.md) §4). Until now the components kept to the
grammar of the distribution specification 1.1, which allows `__` and runs of `-` between
letters and digits, and a name that does not fit was hashed to `h0__` and 32 hex digits, unique
because `Escape` never writes `__` ([ADR 0005](0005-manifest-profile-and-immutable-tags.md)).

Two registries of [docs/registries](../registries/quirks.md) do not take every such name:

- **Gitea and Forgejo** check the 1.0 grammar, one `.`, `_` or `-` between letters and digits:
  every hashed component (a raw `.gitignore` or `_config.yml`, a raw component over 64 bytes)
  and every `--` (`a--b`) is refused (read in their source, #598).
- **Zot** skips in its catalog and its garbage collector every repository whose path ends in
  `blobs`, `.uploads` or `.sync`, with everything below it (`strings.HasSuffix` on the path,
  measured). Reindex from an empty database lists the catalog, so it misses npm's
  `@netlify/blobs` (`npm/<repo>/netlify/blobs`), the pypi package `blobs`, every raw file below
  a directory `blobs/`; and a repository `<x>/blobs/sha256` stops every collection of `<x>`
  (#597). Zot also keeps a repository's `index.json` and `oci-layout` in its directory:
  measured on 2026-11-11 on the dev stack's Zot v2.1.23, `x/index.json` cannot be pushed beside
  `x` (the blob upload is 404), and `x` beside `x/index.json` is 500.

distribution keeps its own data in `_manifests`, `_layers` and `_uploads`, which no component
can be: both grammars start a component with a letter or a digit. GitLab's and Docker Hub's
depth and Quay's `build` are limits of one vendor each, for a capability (#599).

Lajstrom 0.1.0 is not released: no user holds data under the current names. Dev stands and test
registries do.

## Considered options

- Check the name in the provider: a clear `400` on Gitea and Forgejo, a warning on Zot. The
  packages still cannot be stored on two registries, and reindex on Zot stays blind.
- List Zot's repositories another way, from `oci_repos`: that table is lost with the database,
  the case reindex is for.
- Write every name in a grammar that every registry takes, and keep reading the current form.

## Decision

**The grammar.** Every OCI name Lajstrom writes keeps to the intersection of what the registries
of docs/registries accept, depth aside:

1. each component matches `^[a-z0-9]+(?:[._-][a-z0-9]+)*$`, the 1.0 grammar, which Gitea and
   Forgejo check and every later grammar includes; the name is at most 255 bytes;
2. no component ends in `blobs`, `.uploads` or `.sync`, or is `index.json` or `oci-layout`:
   the words of Zot's storage layout (`profile.ReservedComponent`). Rule 1 rules out
   distribution's.

**The encoding** (`EscapeComponent`, manifest-profile.md §4.3). A name is hashed exactly when it
was before, to `h0_h` and the same 32 hex digits: `Escape` writes `_` only before two hex digits
and `h` is none, so no escaped name contains `h0_h`. Any other name is escaped as before, and
then each `-` of a run of two or more is written `_2d`, and the last byte of a component that
breaks rule 2 is written as `_` and its two hex digits. `Escape` never escapes a byte it copies,
so these escapes mark themselves: the encoding stays deterministic, injective (but for case)
and reversible (`UnescapeComponent`), and a name that kept to the grammar keeps its component.

| Name | Package path before | Package path now |
| --- | --- | --- |
| npm `@netlify/blobs` | `netlify/blobs` | `netlify/blob_73` |
| pypi `blobs` | `blobs` | `blob_73` |
| raw `x/blobs/sha256/0f` | `x/blobs/sha256/0f` | `x/blob_73/sha256/0f` |
| raw `cache/my.sync`, `x/index.json` | `cache/my.sync`, `x/index.json` | `cache/my.syn_63`, `x/index.jso_6e` |
| deb `a--b` | `a--b` | `a_2d_2db` |
| raw `.gitignore` | `h0__bc37d034bad564583790a46f19d807ab` | `h0_hbc37d034bad564583790a46f19d807ab` |
| raw `site/_config.yml` | `site/h0__ecec67b0e1d7e17a83587c6d27b6baaa` | `site/h0_hecec67b0e1d7e17a83587c6d27b6baaa` |
| maven `com.example:Lib-Core`, deb `libstdc++6` | `com.example/lib-core`, `libstdc_2b_2b6` | the same |

A repo whose name breaks rule 2 is refused at creation, by the configuration and by the
management API. The instance prefix is the operator's and is not escaped: a prefix with a
component that breaks rule 1 or 2 is refused when the configuration is validated (#610).

**Written in the new form, read in both.** Each component now has one earlier form
(`profile.LegacyPackagePath`): `h0__` for `h0_h`, the marked bytes unescaped.

- **Existing packages keep their OCI name; the new form applies to new packages.** Every write
  path writes to the `name_oci` the index has for the package: an upload, a publication session
  and the proxy fetch look the package up at both forms (`index.PackageAt`); the npm journal and
  deletes take `name_oci` from the index, and `/v2` pushes and referrers go where the client
  names. Otherwise a new version of a package stored under the earlier name would land in a
  second OCI repository. Reads go through `name_oci` as before, and those that compute a package
  path from a name look at both forms.
- Reindex takes a manifest found at either form as the package of its annotations and keeps the
  path where it found it. A `/v2` push to the earlier form is accepted for a package the index
  has there, and a push to the new form of a package the index has at the earlier one is a `409`
  naming it.
- `UnescapeComponent` decodes both forms of a name to the same name, for tooling and tests.

It ships in v0.1.0, before any release, so there is nothing to migrate.

## Consequences

- Every name Lajstrom writes can be stored on Gitea and Forgejo, and Zot lists and collects it:
  reindex from an empty database finds every package written in the new form.
- Only the names that broke the grammar change; a marked component is two bytes longer a mark.
  Raw's 64-byte limit applies before the marks, as the hash rule did, so a name is hashed in both
  forms or in neither.
- A write of an affected name reads the index twice; every other name, once.
- A stand that holds earlier names keeps them. On Zot, an earlier name that ends in a reserved
  word stays out of the catalog: reindex finds it only through `oci_repos` or
  `lajstrom reindex --attach`. A package published in the new form while the index lacks its
  earlier form is split into two OCI repositories: reindex such a stand before it takes writes.
- Clients that push through `/v2` compute the new form (manifest-profile.md §4.3).
- #599 is left with the depth limits and Quay's `build`, which change no encoding.

## Amendment: Harbor's routes (2026-11-11)

**Context.** Harbor v2.15.4 answers `HEAD /v2/<name>/blobs/<digest>` with `404` or `400` at random
when `<name>` has a component `manifests` or `referrers`, or the components `tags/list`; the
`GET` of the blob is always `200`. Measured by the lead, 20 tries each: `x/manifests/y` 13 times
`404`, `x/manifests` 14, `x/referrers/y` 3, `x/tags/list` 17 times `400`; `x`, `x/tags`,
`x/blobs`, `x/uploads` never. Its `artifactinfo` middleware tries the URL against its patterns
in the order of a Go map, which is random
([artifact_info.go](https://github.com/goharbor/harbor/blob/v2.15.4/src/server/middleware/artifactinfo/artifact_info.go#L42-L48),
[L110-L140](https://github.com/goharbor/harbor/blob/v2.15.4/src/server/middleware/artifactinfo/artifact_info.go#L110-L140)).
The manifest and referrers patterns end in `(?P<reference>.*)$`, so
`/v2/x/manifests/y/blobs/<digest>` is also a manifest URL of `x`; the tag-list pattern is not
anchored at the end, so `/v2/x/tags/list/blobs/<digest>`, and `/v2/x/tags/listing/…` as well, is
also the tag list of `x` ([patterns.go](https://github.com/goharbor/harbor/blob/v2.15.4/src/lib/patterns.go#L35-L46),
the same on main; [quirks.md](../registries/quirks.md#measured-on-harbor) section 2). Reported as
[harbor#24363](https://github.com/goharbor/harbor/issues/24363). Kubernetes manifests kept in a
raw repo (`k8s/manifests/app.yaml`) are common.

**Decision.** Rule 2 also covers, for every registry:

- a component `manifests` or `referrers`;
- a component `tags` that a component starting with `list` follows.

Each is written as Zot's words are, with its last byte escaped: `manifest_73`, `referrer_73`,
`tag_73`. The mark of the pair goes on `tags`, not on the `list…` component: the tag-list pattern
is not anchored, so escaping the last byte of `listing` (`listin_67`) would leave `/tags/list` in
the URL, and escaping the byte of `list` that would break it is a mechanism of its own. Escaping
`tags` breaks the pattern whatever follows, and reads back the same way as the other words. The
rule looks at the next component, so it belongs to the package path (`profile.PackagePath`), not
to `EscapeComponent`. The first component of a package path follows the repo name, which the
escape does not see: a repo named `tags`, `manifests` or `referrers` is refused at creation, as
the words of Zot's layout are.

| Name | Package path before | Package path now |
| --- | --- | --- |
| raw `k8s/manifests/app.yaml` | `k8s/manifests/app.yaml` | `k8s/manifest_73/app.yaml` |
| raw `x/referrers/y` | `x/referrers/y` | `x/referrer_73/y` |
| raw `x/tags/list`, `x/tags/listing.txt` | `x/tags/list`, `x/tags/listing.txt` | `x/tag_73/list`, `x/tag_73/listing.txt` |
| npm `@tags/list` | `tags/list` | `tag_73/list` |
| raw `x/tags`, `x/tags/other`, `x/manifest` | the same | the same |

A name now has up to three forms: before ADR 0017, of ADR 0017 before this amendment, and
amended. Written in the newest, read in all: `profile.EarlierPackagePaths` lists the earlier forms
of a package path, `index.PackageAt` looks at each, existing packages keep their `name_oci`, and
reindex takes a manifest found at any of them.

**Consequences.** These names change on every registry for the bug of one: an operator who
browses Zot or distribution sees `manifest_73`. Harbor is not in CI; the integration test runs on
Zot and distribution. The rule is revisited when Harbor ships the fix of harbor#24363: from then
on new packages may be written without these escapes again, and the escaped forms stay readable.

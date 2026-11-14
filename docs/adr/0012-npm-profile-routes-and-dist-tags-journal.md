# 0012. npm: the profile, the routes, and dist-tags as a journal in the registry

- Status: accepted; amended by #81 (below, Amendment)
- Date: 2026-11-10
- Deciders: gecube

## Context

M4 (decided on 2026-11-10) is a Verdaccio-style npm registry: a hosted repo, a caching proxy of
npmjs.org and a group that serves both under one URL. The proxy and the group are ADR 0013
(#303); this record is what is npm's own (#302).

npm differs from the formats of the MVP in one way. Besides immutable versions, a package has
state that changes: `dist-tags` (`latest`, `next`, `beta`), names that `npm install pkg@beta`
resolves and that `npm publish`, `npm dist-tag add` and `npm unpublish` move, and deprecation
messages, set and cleared on published versions. ADR 0001 keeps everything in the registry and
ADR 0005 makes tags immutable, so this state can be neither a re-pointed OCI tag nor a row only
the database holds: `reindex` must restore it. The lead decided that dist-tags live in the
registry as an append-only journal: each change a small manifest that refers to the package,
never rewritten; the index holds the current map; reindex replays the journal.

### What the clients ask of a registry

From the registry API docs
([package metadata](https://github.com/npm/registry/blob/main/docs/responses/package-metadata.md))
and a capture made for this record on 2026-11-10: npm 10.9.9 (Node 22), npm 11.19.1 (Node 24),
pnpm 9.15.9 and 10.34.9, yarn 4.18.1, each in its container against a throwaway logging
registry written for the capture, mounted below a path (`/npm/test/`) as Lajstrom's repos are.
The capture's registry and scripts are not kept; the e2e of #312 runs the same commands against
Lajstrom. The scoped name is `@acme%2fwidgets` in every URL a client builds (pnpm writes `%2F`).

| Command | What goes over the wire |
| --- | --- |
| install | `GET /<name>`. npm 10 and 11 ask for the full packument (`Accept: application/json`), yarn 4 too (no `Accept`), pnpm 9 and 10 for the abbreviated one (`Accept: application/vnd.npm.install-v1+json; q=1.0, application/json; q=0.8, */*`); `npm outdated`, `npm publish` and `npm unpublish` also ask for the abbreviated one first. Tarballs: npm and pnpm fetch `dist.tarball` as written; yarn builds `/<name>/-/<file>` itself, with `%2f` in a scoped name. `npm ci` and `pnpm install --frozen-lockfile` fetch tarballs only. |
| publish | `PUT /<name>`, JSON: `_id`, `name`, `dist-tags` (one tag, `latest` unless `--tag`), `versions` (one version document with `dist.integrity`, `dist.shasum` and a placeholder `dist.tarball`), `access` (`null`, `public`, `restricted`), `_attachments` (one entry `<name>-<version>.tgz`, `@acme/widgets-1.2.0.tgz` for a scoped name: `content_type`, base64 `data`, `length`); yarn adds a top-level `readme`. pnpm 9 and 10 pack and run npm's publish, whose version document then carries `_resolved` (a local path), `_from` and `_integrity`. npm 11 reads the packument first and refuses, on the client, a prerelease or a version below the highest without `--tag`; npm 10 sends both, with `latest`. |
| dist-tag | `GET /-/package/<name>/dist-tags`; `PUT …/dist-tags/<tag>` with a JSON string body, the version; `DELETE …/dist-tags/<tag>`. yarn sends the `GET` without credentials. |
| deprecate | `GET /<name>?write=true`, then `PUT /<name>` with the whole packument as read, `_rev` included and `versions[v].deprecated` set (`""` clears it). |
| unpublish | One version: `GET /<name>?write=true` twice, `PUT /<name>/-rev/<rev>` with the packument without the version and without the dist-tags that named it, `GET ?write=true` again, `DELETE <path of dist.tarball>/-rev/<rev>`: the tarball URL's path on the host, not joined to the registry URL. The whole package: `DELETE /<name>/-rev/<rev>`. |
| login | npm's default `--auth-type=web`: `POST /-/v1/login` with `{}`; a `404` makes npm prompt for a user name and a password and send the legacy `PUT /-/user/org.couchdb.user:<name>` with `{_id, name, password, type: "user", roles: [], date}`, without credentials or with the stale bearer it had; the answer's `token` becomes `_authToken`. The web flow answers `{loginUrl, doneUrl}` and npm polls `doneUrl` (`202` with `Retry-After`, then `200 {"token"}`). `yarn npm login` sends the legacy `PUT` only. |
| logout | npm: `DELETE /-/user/token/<the token>`. yarn: no request. |
| whoami, ping | `GET /-/whoami` → `{"username"}`; `GET /-/ping`. |
| audit | npm 10, 11 and yarn 4: `POST /-/npm/v1/security/advisories/bulk`, `{name: [versions]}` (npm gzips it). pnpm 9: `POST /-/npm/v1/security/audits`, pnpm 10: `…/audits/quick`, both with npm 6's dependency tree. |
| search | `GET /-/v1/search?text=&size=20&from=0&quality=…&popularity=…&maintenance=…` |

pnpm 9 and 10 hand `login`, `whoami`, `ping`, `dist-tag`, `view`, `deprecate`, `search` and
`unpublish` to the npm CLI. npm sends `Authorization: Bearer` from `_authToken` (Basic from
`_auth`), with `npm-command` and `npm-auth-type` headers. Two yarn 4 defaults matter for a
private registry: it sends no credentials for an unscoped package unless `npmAlwaysAuth: true`,
and does not retry a `401` with them; and `npmMinimalAgeGate` is 1440 minutes, so a version
published in the last day is not installed unless `npmPreapprovedPackages` names it. Both are
client configuration for the docs (#313); the second needs a true `time` in the packument.

## Considered options

For the journal entries:

- **Referrers of a version** (`subject` = the version manifest a tag names). Deleting a version
  deletes its referrers (#61), so an unpublish would erase history; a removal names no
  version; the referrers API lists in no order.
- **Tagged manifests in the package's OCI repository**, one immutable tag per entry, numbered.

For their order across replicas:

- **Claims** in `tag_claims` on the next entry's tag: a race is a `409` and the loser retries
  after the winner's confirm.
- **A lock per package** held for the append: the referrers lock of index-schema.md, keyed by
  the package's OCI repository and a digest that no manifest has.

For the version document a packument serves:

- **The publish body's** version object, as npmjs.org serves it, checked against the tarball's
  `package.json` for name and version.
- **The tarball's `package.json`**, normalised by Lajstrom as npm normalises it (`bin`,
  `directories.bin`, `gypfile`), so that the packument cannot say other dependencies than the
  tarball ("manifest confusion"), at the price of reimplementing npm's normalisation.

## Decision

**The profile** (normative in [manifest-profile.md](../spec/manifest-profile.md)). A version is
one manifest, `artifactType` `application/vnd.lajstrom.npm.v1`: the tarball layer
`application/vnd.lajstrom.npm.tarball.v1.tar+gzip` (title `<name without scope>-<version>.tgz`,
annotations `lajstrom.npm.integrity` and `lajstrom.npm.shasum`) and the metadata layer
`application/vnd.lajstrom.npm.metadata.v1+json` with the publish body's version document,
without `dist`, `readme` and the keys that begin with `_`. The tarball's `package.json` is first given
the three normalisations npm's CLI applies to the document it sends but not to the file it packs
(npm 10 and 11, checked with the real clients on 2026-11-10): `bin` as a string becomes
`{<name without its scope>: <path>}`; `directories.bin` without `bin` becomes a `bin` of every
file of that directory in the tarball, by its base name; and a `binding.gyp` at the package's
root with neither an `install` nor a `preinstall` script adds the script `install` `node-gyp
rebuild`. Paths are compared without a leading `./`. Then the document must agree with
the tarball's `package.json` on what decides what an install runs: `name`, `version`,
`dependencies`, `optionalDependencies`, `peerDependencies`, `bundleDependencies` (or
`bundledDependencies`), `bin`, and the install scripts `preinstall`, `install` and
`postinstall`, each compared as JSON values, a missing key equal to an empty one; any other
answer is `400` naming the key ("manifest confusion" is refused at publish, without
reimplementing npm's normalisation of what an install does not run on; decided by the lead on
2026-11-10, the three normalisations corrected the same day after #345 found that npm packs the
raw file). Package path
`EscapeComponent(name)`, for `@scope/name` `EscapeComponent(scope)/EscapeComponent(name)`; tag
`Escape(version)`, no variant.

**The journal.** Each change of a package's dist-tags or deprecations is one manifest in the
package's OCI repository: `artifactType` `application/vnd.lajstrom.npm.dist-tags.v1` (`add`,
`rm`) or `application/vnd.lajstrom.npm.deprecation.v1` (`deprecate`, `undeprecate`), the empty
config and one empty layer, the change in `lajstrom.npm.*` annotations, its number in
`lajstrom.npm.seq` and the tag `_j<seq>` (eight digits, `_j00000001`), which `Escape` can never
produce. Entries are numbered from 1 without gaps, under the package's journal lock (the
referrers lock of the package's OCI repository and the empty blob's digest). Under it the writer
looks for the entry after the index's last in the registry, adopts any it finds, pushes the
next and indexes it; the order is the lock's. An OCI tag is never re-pointed: the current map is
derived. `npm publish` appends an `add` of the body's tag after the version is indexed;
deprecations are entries of the same journal, in M4.

**The index** ([index-schema.md](../spec/index-schema.md), npm). `journal`, one row per entry,
and `journal_state`, the fold: the current dist-tags and deprecations of each package, written
with each entry and rebuilt by reindex from the entries in order. The packument drops a tag
whose version is gone and, without `latest`, names the highest release.

**The routes** ([http-routing.md](../spec/http-routing.md), npm): the full and the abbreviated
packument from `index_cache`, tarballs through `format.ServeBlob`, publish, the packument
update of `deprecate` and `unpublish` (checked against `_rev`, deletions through the delete path
of #61), dist-tags, `ping`, `whoami`, the legacy login answering a new personal access token
(no self-registration; the web login is `404`, and npm falls back to the password prompt),
`logout`, `audit` and `search`. A bearer carries a personal access token or a deploy token, as
`_authToken` sends it ([rbac.md](../spec/rbac.md) §1).

## Consequences

- dist-tags and deprecations survive the loss of the database: reindex replays the journal, and
  the derived map can be compared with the replay in a test (#307).
- One `latest` entry per publish: a package has about as many journal tags as versions, and
  reindex reads each tag like a version tag. No compaction in M4; a snapshot entry that a
  replay starts from is the way, later.
- A writer that crashed after its push leaves an entry the index lacks; the next append or
  reindex adopts it. The journal lock is held across a registry push, as the referrers lock
  already is.
- `npm unpublish`'s packument update writes the `rm` entries of the tags that named the
  version; a version deleted through `/v2` or `/api/v1` writes none, its tags drop out of the
  packument, and would name it again if the same version were published anew. Lajstrom allows
  that republish; npmjs.org does not.
- The body's version document is trusted for what an install does not run on (description,
  keywords, repository, engines…); what it runs on must equal the tarball's `package.json`
  (dependencies of every kind, `bin`, the install scripts), so the packument cannot hide a
  dependency or a script the tarball has (`hasInstallScript` is computed from the tarball).
  The three normalisations above are the ones that touch what an install runs; without them
  `npm publish` of a package with a string `bin`, a `directories.bin` or a `binding.gyp` is
  refused although nothing was hidden. A new normalisation in a later npm shows up as a `400`
  for an honest package and is added here.
- `Bearer ljs_dep_…` is accepted outside `/v2` (docs/auth.md said it is refused; the
  implementation changes it).
- The proxy and the group (ADR 0013) reuse the profile; a proxied package's dist-tags are the
  upstream's and are not journaled.
- The Go shape of the journal write is one method of `format.Publisher`, `Journal`,
  implemented by ingest, the one write path; its signature is in layout.md §2 with this ADR
  (decided by the lead on 2026-11-10), and #307 implements it.

## Amendment: pypi yanks in the journal (#81, 2026-11-14)

PEP 592 lets an index mark the files of a release as yanked: pip and uv skip them unless a
requirement pins the version with `==` or `===`. Like a deprecation, a yank is state that changes
on a published version, is set and cleared, and must survive the loss of the database. The issue
asked to decide between an annotation on a referrer and a control-plane marker.

- **A referrer** of each manifest of the version: a pypi version is several manifests, one per
  file, so a yank would be several writes that can stop half way. Deleting a manifest deletes its
  referrers. Lajstrom never rewrites a referrer, so an unyank has nowhere to go but a delete, and
  the referrers API lists in no order. These are the reasons the Considered options above gave
  against referrers for npm.
- **A control-plane row**: reindex cannot rebuild it from the registry, and the database would
  hold per-version data that no manifest has.
- **An entry of the package's journal**, as `npm deprecate` is: one small manifest per change,
  numbered under the journal lock, with the state in `journal_state` and replayed by reindex.

Decision: the journal. A pypi package journals its yanks under the tags `_j<seq>` of its OCI
repository. The artifact type is `application/vnd.lajstrom.pypi.yank.v1`, the ops are `yank`
(`lajstrom.pypi.version`, and an optional `lajstrom.pypi.reason`) and `unyank`
(`lajstrom.pypi.version`), and the numbering is in `lajstrom.pypi.seq` and `lajstrom.pypi.op`. The
artifact type decides an entry's format and the prefix of its keys, so an npm entry is exactly
what it was. A yank names a version, every file of it, as PyPI yanks releases. Ingest writes the
entries through `Publisher.Journal`, the management API's `PUT` and `DELETE
…/versions/<version>/yank` call it (`write`, like a deprecation), the simple index renders
`data-yanked` and `yanked` from the state, and every entry is an audit row `pypi.yank`.

Consequences:

- No new table, migration or Go signature: `journal` and `journal_state` take the new kind
  `yanked`, and `format.JournalOp` carries the reason in `Message`.
- A yank survives reindex as a deprecation does, and the facade refuses to push or delete a
  pypi entry through `/v2`, as it refuses an npm one.
- A deleted version keeps its yank, and the same version uploaded again is yanked until it is
  unyanked: the yank names the version, as a dist-tag does (Consequences above). When every
  version of a pypi package is deleted, its entries are orphans, and the next yank or unyank of
  that name adopts them; pypi has no unpublish of a package that would delete them.
- PEP 694, the upload API 2.0, is still a draft (peps.python.org, 2026-11-14) and is not part of
  this amendment.

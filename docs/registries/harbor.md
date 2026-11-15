# Harbor

What Harbor answered on 2026-11-07, measured with `curl`, `oras`, Harbor's REST API, the
integration tests and the conformance suite through the facade (issue #21). Harbor puts a
project model, a token service, robot accounts, immutability rules, quotas and a garbage
collector around its own build of distribution. The [summary row](README.md) is in the README;
this page has the details. Lajstrom runs on Harbor as type `generic` below a project's prefix, or
as type `harbor`, which adds Harbor's REST API ([Type harbor](#type-harbor), #73).

## Version tested

- Harbor **v2.15.4** (released 2026-10-09; `/api/v2.0/systeminfo` says `v2.15.4-d8841742`),
  from the online installer `harbor-online-installer-v2.15.4.tgz`, sha256
  `eeb899cf7b626c925790adba87db9008dd1d73a693a72442651b5bdece9805c6` (its md5 matches the
  release's `md5sum`). `gh release view` names v2.14.6 the latest: a patch of the previous
  minor, published after v2.15.4. 2.16.0 is at rc2.
- The registry inside is Harbor's distribution, `github.com/docker/distribution
  v2.8.3-55-gac7c4ced` (the release notes: `v2.8.3-harbor.2`), on filesystem storage. The
  referrers API, the token service, quotas and immutability live in Harbor's core, which
  proxies `/v2/` to it.
- Every image is **linux/amd64 only**: single-platform Docker v2 manifests, no index
  (`crane manifest goharbor/harbor-core:v2.15.4`; `crane config` says `amd64`). The nine that
  run: `harbor-core@sha256:7d2ba530…32b112`, `harbor-db@sha256:a32d51e6…c5403f`,
  `harbor-jobservice@sha256:f143578e…5d2b82`, `harbor-log@sha256:bd5879d2…ff8289`,
  `harbor-portal@sha256:01edf478…754a75`, `harbor-registryctl@sha256:30bd1ace…943fb7`,
  `nginx-photon@sha256:7eff0287…fa1311`, `registry-photon@sha256:dc0cb388…2f83c3`,
  `valkey-photon@sha256:bb022e8f…6eb54f`, all built 2026-10-08. `goharbor/prepare:v2.15.4`
  generates the configuration.
- On macOS with OrbStack, arm64: the images run emulated (OrbStack's Rosetta). It was not
  slow: `harbor.sh up` from nothing to healthy in about 20 s with the images pulled (the first
  run, which pulled them, in about 30 s), `down` in 12 to 15 s; a 2 GiB blob in one `PUT` took
  15 s, and the tests ran as fast as against the dev stack's registries (the facade package in
  3 s). The tests with Go 1.27.3 at d11dd34.
- On 2026-11-14 the probe ran again ([The probe](#the-probe-2026-11-14-850)), on the same
  images: `harbor.sh up` pulled the ten images (prepare included, about 3.4 GB) and was healthy
  in 31 s, emulated as before. The probe took 6 s, and `down` 15 s.

## Run it

[deploy/compose/harbor/harbor.sh](../../deploy/compose/harbor/harbor.sh) with
[harbor.yml](../../deploy/compose/harbor/harbor.yml): Harbor as the compose project `harbor` on
`http://localhost:5005`, with its own network, beside the dev stack and not part of it (`make
dev-down` does not touch it):

```sh
deploy/compose/harbor/harbor.sh up      # download the installer, check the sha256, prepare, compose up, wait for /api/v2.0/health
deploy/compose/harbor/harbor.sh down    # compose down -v, and remove the installer, the generated files, the data and the logs
```

`harbor.yml` is the installer's template with `hostname: localhost`, `http.port: 5005`, no
`https` block and the installer's default passwords (the admin password applies on the first
start). `up` copies it into the installer and puts `data_volume` and the log directory under
`LAJSTROM_HARBOR_DIR` (default `deploy/compose/harbor/.work`, ignored by git; on macOS a path
Docker shares), runs `prepare`, which writes `docker-compose.yml` and `common/config` with
secrets of its own, publishes the port on 127.0.0.1 instead of every interface, and starts it.
Not `up --wait`: jobservice exits until core answers and restarts, which `--wait` takes for a
failure. `LAJSTROM_HARBOR_GC_TIME_WINDOW_HOURS=0` sets core's `GC_TIME_WINDOW_HOURS` for the
[garbage collection](#garbage-collection) experiment.

Then a private project and a robot account of it, with the admin user
([Commands](#commands)):

| What | How |
| --- | --- |
| Project | `lajstrom-it` (id 2), private, no quota |
| Robot | `robot$lajstrom-it+ci`, level project, never expires; `repository`: pull, push, delete, list, read; `artifact`: delete, list, read; `tag`: create, list, delete; `accessory`: list |
| Lajstrom | `prefix: lajstrom-it/`, `auth.username: robot$lajstrom-it+ci`, the secret in the variable `passwordEnv` names |

`POST /api/v2.0/projects/{p}/robots`, the older per-project robot API, is gone in 2.15 (404
`path … was not found`); robots are created with `POST /api/v2.0/robots`. The robot's name holds a `$`:
quote it in a shell.

## The row

`lajstrom registry probe` measures every row of this table but the listing API, auth, rate
limits and the garbage collector, which still need a person;
[README.md](README.md#measuring-a-registry-lajstrom-registry-probe) says how. It ran in full on
2026-11-14 ([The probe](#the-probe-2026-11-14-850)).

| Registry | Version | Auth | Create on push | Referrers | Catalog | Delete manifest | Immutable tags | Blob redirect | Result |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Harbor | v2.15.4, filesystem storage, `prefix: lajstrom-it/` (a project) | Bearer from Harbor's token service, a project robot | yes, inside an existing project, at any depth; outside one 401 `project … not found` | **yes**: OCI 1.1 referrers API, `artifactType` filter, `OCI-Subject` on the push | **no** for a robot: `_catalog` is for system admins with Basic auth; the REST API lists a project's repositories | yes, by digest, and the subject's referrers go with it; by tag 400 | per project, immutability rules: an existing tag is 412 `PRECONDITION`, and so is a digest delete of a manifest under one | no redirect: streamed, one request; `Accept-Ranges: bytes`, a range 206 | pass, but for a chunk out of order, which Harbor takes |
| Harbor | the same, no prefix | Bearer, robot | **no**: every repository is outside a project | the probe fails: 400 `invalid repository name: lajstrom-probe` | no | — | — | — | **fail**: 401 on every repository, `Capabilities` fails |
| Harbor | the same, an immutability rule `**` for `**` in the project | Bearer, robot | yes | yes | no | **no** for a tagged manifest: 412, which the provider reads as a conflict; untagged ones, referrers among them, are deleted | **yes** | no redirect | fail: the delete steps (409 at `/raw/`) |

The probe reports `createOnPush=true referrers=true catalog=false deleteManifest=true
immutableTags=false blobRedirect=true maxBlobSize=0` (the serve log and the test log, with the
prefix). That is right for a project without rules; no override in `registries[].capabilities`
is needed. `immutableTags: true` would describe a project with a rule, but nothing in Lajstrom
reads it yet; the refusals reach ingest and the frontends as conflicts either way.

## The probe, 2026-11-14 (#850)

Before this run, the probe had measured only the routing words on Harbor, on 2026-11-11
([quirks.md](quirks.md#measured-on-harbor)). This was its first full run here, with the
lists read before each write and the deletes (#845) and the monotonic reads (#564). Lajstrom at
20ce9499 ran against a fresh Harbor v2.15.4 (`v2.15.4-d8841742`) from a copy of harbor.sh.
The copy changed only the compose project name, the port, the container names and the log
port, because other runs shared the machine. The project `lajstrom-it` and its robot came from
[Commands](#commands). It ran as the robot, as a configuration would:

```sh
LAJSTROM_HARBOR_DIR=<scratch> deploy/compose/harbor/harbor.sh up      # here on port 57210
bin/lajstrom registry probe --url http://localhost:57210 --registry-prefix lajstrom-it/ \
  --username 'robot$lajstrom-it+ci' --password-file robot.secret --max-manifest-size 32MiB -v
```

The provider's own probe said `vendor=harbor createOnPush=true referrers=true catalog=false
deleteManifest=true immutableTags=false blobRedirect=true maxBlobSize=0 consistency=unknown
maxNameDepth=0`. Nothing was left behind.

| Row | Harbor v2.15.4 |
| --- | --- |
| Create on push | yes, with the first manifest |
| Where a repository may be created | below `lajstrom-it/`; outside it not tried; at any depth, inside another repository too; 255 characters refused: a 500 |
| Routing words in a name (`manifests`, `referrers`, `tags/list`) | **misroutes**: blob `HEAD` in `x/manifests/y` 14×404, `x/manifests` 10×404, `x/referrers/y` 2×404, `x/tags/list` 16×400 of 20; `GET` 200 |
| Referrers API | yes, with the `artifactType` filter; no tag schema |
| `OCI-Subject` on a push with `subject` | yes |
| Catalog (`_catalog`) and its limits | no: 401 `UNAUTHORIZED` |
| A listing API of its own | *not measured by the probe* |
| Tag list order and pagination | lexical, byte by byte; `n`/`last`/`Link` |
| Read-after-write lag | none: each read saw each of 3 writes at once (manifest by digest, manifest by tag, blob, tag list); each read saw each of 3 deletes at once (manifest by digest, manifest by tag, tag list) |
| Monotonic reads | none: no read saw a change undone; 3 writes, a tag move, 3 deletes, each read 5 times more over new connections after the first that saw it, from 1 address |
| Delete a manifest by digest | yes, 202, with its tags; its referrers go with it; **412 `VIOLATE_FOREIGN_KEY_CONSTRAINT`** for a manifest an index lists |
| Delete by tag | no: 400 `UNSUPPORTED` |
| Delete a repository | no `/v2` API; no catalog to look in, its tag list 200 |
| Immutable tags, where the rule lives | none: a tag re-points with 201 |
| Blob redirect | none: the bytes |
| `Range` and `Accept-Ranges` | 206, 416 past the end; `Accept-Ranges: bytes` |
| Largest blob measured | 8 MiB in one `PUT` and in one `PATCH`; no limit met |
| Manifest size limit | 4 MiB; one byte more is 413 `REQUEST_ENTITY_TOO_LARGE` |
| Lajstrom's manifest profile (`artifactType`, empty config, own media types) | accepted, stored as pushed |
| Auth | *not measured by the probe* |
| Rate limits seen | *not measured by the probe* |
| Upload: chunk out of order | **appended**, no 416 |
| Upload: chunk size limit | none met: 8 MiB in one `PATCH` |
| Upload: session status (`GET`) | works, 204 with `Range` |
| Upload: cancel (`DELETE`) | works: 204, then its `GET` 404 `BLOB_UPLOAD_UNKNOWN` |
| Upload: mount (`POST ?mount=&from=`) | works, 201 |
| Upload: wrong digest, empty blob | 400 `DIGEST_INVALID`, the session ends; the empty blob stored |
| Upload continued by another replica | works (the provider: one starts the session, another streams a chunk and closes it) |
| Garbage collector and an untagged referrer (#37) | *not measured by the probe* |
| What the probe finds | `referrers=true catalog=false` |
| Overrides needed | none |

The routing words, name by name: `x` `HEAD` 20×200; `x/manifests/y` 6×200 and 14×404;
`x/manifests` 10×200 and 10×404; `x/referrers/y` 18×200 and 2×404; `x/tags/list` 4×200 and
16×400. Every `GET` was 200.

What is new against the [column](README.md#capabilities), all of it Harbor's:

- **Read-after-write lag, monotonic reads**: measured for the first time, none. With no
  catalog for the robot, the lag row reads the tag list alone. Before a repository's first
  manifest the list is 404 `NOT_FOUND` "repository … not found", and right after the push it
  names the tag. The vendor table's entry for Harbor leaves its consistency unknown, so a
  publish reads back its tag and blobs too.
- **Name length.** Harbor takes fewer than 255 characters. The probe's upload session in a
  name of 255 characters was 500 `{}`, and in one of 256 it was 500 `UNKNOWN` "unknown error"
  with the name in its detail. Upload sessions opened with curl, each name below
  `lajstrom-it/l850len/` and the project counted, found the bound: 230, 240 and 245
  characters 202; 246, 248, 250 and 254 characters 500. Lajstrom assumed 255 for Harbor
  until #1041 ([What the provider must do differently](#what-the-provider-must-do-differently), item 5).
- **The delete of a manifest an image index lists** is 412 `VIOLATE_FOREIGN_KEY_CONSTRAINT` "the
  deleting artifact is referenced by others": the index first, then the manifest, as on Zot
  (405) and GitLab B (409). The provider gave this 412 no sentinel: `classifyResponse` makes a
  412 `ErrConflict` only when its message says "immutable". So it was a refusal of the registry,
  where GitLab B's 409 is `ErrConflict`. Since #1041 it is `ErrConflict` by its code
  ([What the provider must do differently](#what-the-provider-must-do-differently), item 6).
- **A wrong digest ends the session**: the closing `PUT` is 400 `DIGEST_INVALID`, and the
  session's `GET` afterwards is 404 `BLOB_UPLOAD_INVALID`. Gitea, GitLab and Nexus's `docker`
  format keep the session open; Nexus's `oci` format and Quay end it too.
- **Delete a repository**: after `/v2` deletes of every manifest, an emptied repository's tag
  list is 200 with `"tags":[]`. The REST API's repository delete makes it 404 ([Delete](#delete)).
- **Routing words**: misroutes again, as on 2026-11-11 (then 13, 12, 1 and 15 misses of 20);
  [goharbor/harbor#24363](https://github.com/goharbor/harbor/issues/24363) is not fixed in
  v2.15.4.
- **Chunk size limit and the session's status**, which the column had *not measured* and "the
  test": none met at 8 MiB in one `PATCH`, and 204 with `Range`. The tag list is ordered byte
  by byte. The 2 GiB blob of [Blob redirect, ranges, sizes,
  headers](#blob-redirect-ranges-sizes-headers) stands.
- Every other cell is the column's. Harbor's challenge names no scope, so Gitea's extra 401
  before each request ([gitea.md](gitea.md#the-probe-again-2026-11-13-850)) does not happen
  here: 13 of the 483 requests were 401s.

## What the provider must do differently

1. **Done in #357:** read a quota refusal as one. Over a project's storage quota Harbor answers the blob `PUT`
   and the manifest `PUT` with 403 `DENIED` "… will exceed the configured upper limit of 1.0
   MiB"; the provider makes it `ErrUnauthorized`, "the registry rejected Lajstrom's own
   credentials", and the client gets 502: the raw frontend's `PUT` answers `Bad Gateway`, the
   facade's blob `PUT` 502 `UNKNOWN` (measured, [Quotas](#quotas)). It is the registry
   refusing to store, not bad credentials. Since #357 its words, "exceed … limit", make it
   `provider.ErrLimit`: the client gets `403` (on `/v2` `403 DENIED`) naming the quota, and the
   log a warning ([operations.md](../operations.md#a-registrys-limits-and-quotas)).
2. **Done in #73, for type `harbor`:** list repositories through Harbor's REST API when reindex (#29) needs a listing. A project
   robot cannot read `_catalog` (401 with a Basic challenge; even the admin's Bearer token is
   refused, and only the admin with Basic auth gets it), so the probe sets `catalog=false` and
   `ListRepositories` is `ErrUnsupported`. Reindex calls it and, on Harbor, falls back to the
   `oci_repos` rows (#37), which a lost database loses; a REST listing would let it start from
   nothing. `GET /api/v2.0/projects/<project>/repositories`
   answers the robot (permission `repository:list`) with `page`, `page_size` (at most 100),
   `q=name=~<prefix>`, `X-Total-Count` and `Link`; the names carry the project. Type `harbor`
   lists so ([Type harbor](#type-harbor)); type `generic` still reads `oci_repos`.
3. **Done in #73, for type `harbor`:** say at startup what an empty prefix means on Harbor. Without one, the referrers
   probe answers 400 `BAD_REQUEST` "invalid repository name: lajstrom-probe" (a one-component
   name), `Capabilities` fails, `serve` logs "registry capabilities unknown; serving anyway",
   and every repository is refused with 401 "project raw not found". The configuration needs
   `prefix: <project>/`; the 400 could be turned into a message that says so. Type `harbor`
   refuses a configuration without a prefix, and a project that does not exist is an error at the
   start and at each publish whose hint says to create it or to set `harbor.createProject`.
4. Optional: read 412 `PRECONDITION` as a conflict by its code. Today a push to an immutable tag
   and the delete of a manifest under one become `ErrConflict` only because Harbor's message
   says "configured as immutable" (`mentionsImmutable`).
5. Bound a name at 245 bytes, the project included, not 255. Harbor answers an upload session
   in a longer name with a 500 ([The probe](#the-probe-2026-11-14-850)). The provider gives
   every registry but ECR 255 bytes less the prefix (`maxNameLen` in
   internal/provider/generic/capabilities.go, whose comment says Harbor keeps 255). So with
   `prefix: lajstrom-it/` a name of 234 to 243 bytes below the prefix passes Lajstrom's `400`
   ([quirks.md](quirks.md#case-and-length)) and meets Harbor's 500 instead. A Harbor entry for
   `MaxNameLength` in the vendor table would close it. **Done in #1041:** the vendor table
   bounds a name on Harbor at 245 bytes, the prefix included (`vendorNameLengths`), so with
   `prefix: lajstrom-it/` the names below it get 233 and a longer one is Lajstrom's `400
   NAME_INVALID` with the limit. Not measured on Harbor again.
6. **Done in #1041:** read the 412 `VIOLATE_FOREIGN_KEY_CONSTRAINT` to the delete of a manifest an
   image index lists as a refusal of this one delete, `ErrConflict`, by its code (`deniedOne` in
   internal/provider/generic/repository.go), as Zot's 405 and GitLab B's 409 are. A client then
   gets what it gets on those: the facade's `DELETE` of the manifest is `409 DENIED` "the
   registry refused to delete … : another manifest refers to it", where it was `412
   BLOB_UPLOAD_INVALID`, and the API's version delete is `409`, where it was `502`. Not measured
   on Harbor again.

Nothing else: the token service, the referrers API, tags, uploads (the cross-replica one too),
mounts, ranges and the digest delete work as the provider expects. The out-of-order chunk is
Harbor's ([Uploads](#uploads)).

## Type harbor

`registries[].type: harbor` (#73) is the generic provider's `/v2` below one project, as above,
and Harbor's REST API (`/api/v2.0`) for what `/v2` does not say or do
([internal/provider/harbor](../../internal/provider/harbor)). The project is the first component
of `prefix`, which the type requires; the rest of the prefix is a path inside the project. The
robot account of `auth` answers `/v2` as for type `generic` and goes to the REST API as Basic
credentials, to the registry's host only (no redirect is followed).

```yaml
registries:
  - name: harbor
    type: harbor
    url: https://harbor.example.com
    prefix: lajstrom/                     # the project; lajstrom/prod/ for a path inside it
    auth: {username: "robot$lajstrom+lajstrom", passwordEnv: HARBOR_ROBOT_PASSWORD}
    harbor:
      createProject: false                # true: create a missing project (a system robot's)
```

| | type `generic` | type `harbor` |
| --- | --- | --- |
| The project | not checked: a missing one is `401` on `/v2`, read as Lajstrom's credentials refused, and a publish is `502` | `HEAD /api/v2.0/projects?project_name=<project>`, which any account may ask, at the start and at a publish, once per 10 minutes; a missing one is an error in the log and `502` for a publish, with a hint to create it or to set `harbor.createProject`, which creates it, private, with Harbor's default quota |
| Listing, for reindex | none: `_catalog` is for system administrators, and reindex reads `oci_repos` | `GET /api/v2.0/projects/<project>/repositories?q=name=~<prefix>&sort=name&page_size=100`, every page; `catalog` is true when the robot may list (`repository:list`), else false, as type `generic` |
| Repository delete | unsupported | `DELETE /api/v2.0/projects/<project>/repositories/<name below the project, / as %252F>`; `412` (a tag an immutability rule holds) is a conflict |
| Immutability rules | not read; a refusal is a conflict all the same | counted at the start: one that is not disabled makes `immutableTags` true, and a warning says what such a rule refuses (the delete of a manifest under a tag it matches, the move of such a tag, the repository delete) |
| Name length | 245 bytes with the project once the probe has told Harbor by its challenge, else 255 | 245 bytes with the project |
| A quota, an immutable tag, a manifest an index lists | the generic provider's: `provider.ErrLimit` (`403`), `ErrConflict` (`409`) | the same: `/v2` is the generic provider's |

Neither type creates or changes an immutability rule, a quota, a tag retention rule or a robot:
they are the Harbor administrator's. Tag retention is not read: keep it off the project, or off
the repositories under the prefix ([operations.md](../operations.md#the-registrys-own-rules)).
`capabilities.catalog` and `capabilities.immutableTags` in the configuration replace what the
REST API says, and the provider then does not ask it.

### The robot account

A project robot of the project, with five permissions and no more:

| Resource | Actions | What Lajstrom does with it |
| --- | --- | --- |
| `repository` | `pull`, `push` | `/v2`: the reads, and every publish (Harbor's token service grants the scope's `pull` and `push` from them) |
| `repository` | `delete` | `/v2`: the delete of a version's manifest by digest, prune; the REST API's repository delete |
| `repository` | `list` | the listing; without it `catalog` is false, a warning says so, and reindex reads `oci_repos` |
| `immutable-tag` | `list` | optional: the rules at the start; without it `immutableTags` stays false and an info line says so |

No `artifact`, `tag` or `project` permission is needed: a version is deleted through `/v2`, and the
project is asked about with the `HEAD` any account may send.

```sh
curl -u "$A" -H 'Content-Type: application/json' -X POST $H/api/v2.0/robots -d '{"name":"lajstrom","level":"project","duration":-1,
  "permissions":[{"kind":"project","namespace":"lajstrom","access":[
    {"resource":"repository","action":"pull"},{"resource":"repository","action":"push"},
    {"resource":"repository","action":"delete"},{"resource":"repository","action":"list"},
    {"resource":"immutable-tag","action":"list"}]}]}' | jq -r .secret > robot.secret
```

`harbor.createProject: true` needs more. A project robot cannot create a project: Harbor answers
`403 FORBIDDEN` "Only system level robot can create project", which the provider reads as
`provider.ErrUnauthorized` with a hint. A system robot can, with `project: create` and the five
permissions on every project (`"namespace": "*"`), which then hold in the project it creates:

```json
{"name":"lajstrom","level":"system","duration":-1,"permissions":[
  {"kind":"system","namespace":"/","access":[{"resource":"project","action":"create"}]},
  {"kind":"project","namespace":"*","access":[{"resource":"repository","action":"pull"},
    {"resource":"repository","action":"push"},{"resource":"repository","action":"delete"},
    {"resource":"repository","action":"list"},{"resource":"immutable-tag","action":"list"}]}]}
```

Such a robot reaches every project of the Harbor, where a project robot reaches one: the default,
`false`, keeps Lajstrom's credentials to its project. With Harbor's "project creation" set to
administrators only, the system robot's `project: create` is what counts.

Rotation is the operator's: Harbor refreshes a robot's secret (the UI's "Refresh secret", or
`PATCH /api/v2.0/robots/<id>`), and a robot that expires stops working on its day. Lajstrom reads
`passwordEnv` at the start: a new secret takes a restart of each replica, as for any registry.

### Deletes and the garbage collector

A version's delete and prune delete its manifest through `/v2` by digest (`202`): Harbor deletes
the artifact with every tag that names it and its referrers, which it keeps as accessories
([Delete](#delete)). The REST API's artifact delete does the same and is not used. A repository
delete goes through the REST API. Neither frees space: Harbor keeps the blobs until its garbage
collector runs, a job of jobservice that a system administrator schedules (Administration, Clean
Up; `POST /api/v2.0/system/gc/schedule`). Lajstrom never starts it, and its robot has no
permission to. The collector keeps an untagged referrer, with or without `delete_untagged`
([Garbage collection](#garbage-collection)).

### Measured, 2026-11-15 (#73)

Harbor v2.15.4 from [harbor.sh](#run-it), as on 2026-11-14, Lajstrom at the change of #73, the
project `lajstrom-it` and a project robot `robot$lajstrom-it+ci` with the five permissions above
and no others:

- `go test -tags integration -run TestIntegrationHarbor ./internal/provider/harbor/`
  ([Commands](#commands)) passes. As the robot: the start's line `listRepositories=true
  immutabilityRules=0`, `MaxNameLength` 221 below `lajstrom-it/it-<8 hex>/`; four repositories
  pushed and listed through three pages of two (`page_size` lowered by the test); a repository
  delete, then `404` as `ErrNotFound`; a version's `/v2` delete; a project that does not exist, a
  `RejectedError` of `404` whose message does not name it. As the administrator, in projects of
  its own: `createProject`; an immutability rule `**` (`immutableTags` true, the move of a tag and
  the delete of its manifest `ErrConflict`, the repository delete `412`, `ErrConflict`); a quota of
  1 MiB (a 1.5 MiB blob `ErrLimit`, the message without the project or Harbor's numbers).
- With curl, as the robot: `POST /api/v2.0/projects` is `403` "Only system level robot can create
  project"; `HEAD /api/v2.0/projects?project_name=` is `404` for a project that does not exist and
  `200` for one that does, the robot's or not; the repositories of a project that does not exist
  are `403 FORBIDDEN`, not `404`, since Harbor checks the permission first. As a system robot with
  `project: create` and the five permissions on `*`: the `POST` is `201`, its listing of the new
  project `200`, and its token for a repository there holds `pull`, `push` and `delete`.
- Through Lajstrom, `serve` with a repo `raw/scratch` on `prefix: lajstrom-it/smoke/`: two files
  uploaded, then the database replaced by an empty one and `lajstrom reindex --repo raw/scratch`
  run. Type `harbor` found both: `ociRepositories=2 manifests=2 added=2`. Type `generic`, on the same
  Harbor and the same empty database, found none, `ociRepositories=0`, and warned to name them
  with `--attach`.
- `lajstrom registry probe --registry harbor` on that configuration: "What the probe finds"
  `referrers=true catalog=true`, "Overrides needed" none (the `_catalog`'s `401` asks for no
  `catalog: false`, since the provider lists with the REST API).

## Projects, robots and create on push

Harbor's token service grants a scope only inside a project the account may use; outside one
the token comes back with no actions, and the registry answers with the reason:

```text
GET /service/token?service=harbor-registry&scope=repository:lajstrom-it/raw/a/b/c:pull,push,delete   (robot)
    access [{"type":"repository","name":"lajstrom-it/raw/a/b/c","actions":["pull","push","delete"]}], exp - iat = 1800
GET /service/token?…&scope=repository:nosuchproject/x:pull,push&scope=registry:catalog:*            (robot)
    access [{…"nosuchproject/x","actions":[]},{"type":"registry","name":"catalog","actions":[]}]
GET /service/token?…&scope=repository:library/x:pull,push                                          (robot)
    actions ["pull"]: library is public

POST /v2/lajstrom-it/raw/a/b/c/blobs/uploads/    202, then PUT …&digest=   201   (config, layer)
PUT  /v2/lajstrom-it/raw/a/b/c/manifests/v1      201   Docker-Content-Digest, Location
POST /v2/nosuchproject/x/blobs/uploads/          401 {"errors":[{"code":"UNAUTHORIZED","message":"project nosuchproject not found: …"}]}   (robot and admin; empty Www-Authenticate)
POST /v2/toplevel/blobs/uploads/                 400 {"errors":[{"code":"BAD_REQUEST","message":"bad request: invalid repository name: toplevel"}]}
POST /v2/lajstrom-it/blobs/uploads/              400 … invalid repository name: lajstrom-it
```

A repository is created by its first manifest, at any depth below the project:
`lajstrom-it/raw/a/b/c` and `lajstrom-it/maven/repo/org/example/group/artifact/sub/1/2/3/4/5`
both took their manifest and appear in the REST API. A project is not created on push. Tokens
live 30 minutes; oras-go fetches a new one on the next 401. Basic auth with the robot straight
on `/v2/` works too (200), though `/v2/` challenges with `Bearer
realm="http://localhost:5005/service/token",service="harbor-registry"`. The realm is the
registry's own host name, which oras-go accepts on loopback ([gitlab.md](gitlab.md#bearer-tokens)
had to move its test token server for that). `POST …/blobs/uploads/?digest=` with the bytes,
a monolithic upload in one request, answers 202 and opens a session instead.

## Referrers

The OCI 1.1 referrers API, in Harbor's core. A subject `S` tagged `v1` in
`lajstrom-it/raw/ref/pkg`, then two referrers by digest, without a tag, one with
`artifactType` and one with only a config media type:

```text
PUT manifests/sha256:b98ffa7a…   201   OCI-Subject: sha256:a7763216…   (artifactType application/vnd.lajstrom.test.signature.v1)
PUT manifests/sha256:740e198c…   201   OCI-Subject: sha256:a7763216…   (config application/vnd.lajstrom.test.sbom.v1+json, no artifactType)
GET referrers/sha256:a7763216…   200   Content-Type: application/vnd.oci.image.index.v1+json, X-Total-Count: 2
    {"manifests":[{"artifactType":"application/vnd.lajstrom.test.signature.v1","digest":"sha256:b98ffa7a…","size":670,"annotations":{…}},
                  {"artifactType":"application/vnd.lajstrom.test.sbom.v1+json","digest":"sha256:740e198c…","size":545,"annotations":null}]}
GET referrers/sha256:a7763216…?artifactType=application/vnd.lajstrom.test.signature.v1
                                 200   OCI-Filters-Applied: artifactType, X-Total-Count: 2, one manifest
GET referrers/sha256:a7763216…?n=1   200, both manifests, no Link
GET referrers/<zero digest> in lajstrom-it/lajstrom-probe (no such repository)   200 {"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}
GET referrers/<zero digest> in lajstrom-probe (no project)                        400 BAD_REQUEST invalid repository name: lajstrom-probe
GET referrers/sha256:abc         400 BAD_REQUEST unsupported digest sha256:abc: invalid checksum digest length
```

The artifact type of a referrer without one is its config media type, as the spec asks;
`X-Total-Count` counts before the filter; `n` is ignored, so the provider's page of 100 is one
answer. A referrer whose subject does not exist is taken (201, `OCI-Subject`) and listed. No
`sha256-<digest>` tag is written: `oras attach` used the API, `oras discover` listed the
referrer, and `oras repo tags` shows the subject's tag alone. Harbor's own view is the same:
`GET /api/v2.0/projects/lajstrom-it/repositories/raw%252Fref%252Fpkg/artifacts?with_accessory=true`
lists the subject with `"accessories":[{"type":"subject.accessory","digest":"sha256:b98ffa7a…"},…]`,
and `…/artifacts/<subject>/accessories` the two. Referrers are not listed as artifacts of their
own; the REST API wants the repository name below the project with `/` encoded twice (`%252F`).

## Catalog and listing

`_catalog` is Harbor's to system administrators, and only with Basic auth:

```text
GET /v2/_catalog?n=2   no credentials          401  Www-Authenticate: Basic realm="harbor"
GET /v2/_catalog?n=2   robot, Basic            401  {"errors":[{"code":"UNAUTHORIZED","message":"unauthorized to list catalog: …"}]}
GET /v2/_catalog?n=2   robot, Bearer           401  the same (the token holds no catalog action)
GET /v2/_catalog?n=2   admin, Bearer           401  the same, though the token holds registry:catalog:*
GET /v2/_catalog?n=2   admin, Basic            200  Link: </v2/_catalog?last=lajstrom-it%2Fcat%2Fr2&n=2>; rel="next"  {"repositories":["lajstrom-it/cat/r1","lajstrom-it/cat/r2"]}
GET <that Link>        admin, Basic            200  Link: …last=lajstrom-it%2Fcat%2Fr4…   {"repositories":["lajstrom-it/cat/r3","lajstrom-it/cat/r4"]}
GET /v2/_catalog?n=0   admin, Basic            200  {"repositories":[]}
GET /v2/_catalog       admin, Basic            200  all 8; n=100000 the same
```

The probe gets the 401 and sets `catalog=false`. The REST API answers the robot:

```text
GET /api/v2.0/projects/lajstrom-it/repositories?page=1&page_size=2
    200  Link: </api/v2.0/projects/lajstrom-it/repositories?page=2&page_size=2>; rel="next"   X-Total-Count: 8   ["lajstrom-it/cat/r5","lajstrom-it/cat/r4"]
GET …?page=2&page_size=2
    200  Link: <…page=1…>; rel="prev" , <…page=3…>; rel="next"   ["lajstrom-it/cat/r3","lajstrom-it/cat/r2"]
GET …?q=name%3D~cat%2F&sort=name&page_size=2
    200  Link: <…page=2&page_size=2&q=name=~cat/&sort=name>; rel="next"   X-Total-Count: 5   ["lajstrom-it/cat/r1","lajstrom-it/cat/r2"]
GET …?page_size=101   422 {"errors":[{"code":"UNPROCESSABLE_ENTITY","message":"validation failure list:\npage_size in query should be less than or equal to 100"}]}
GET /api/v2.0/repositories?page_size=2   200 for the robot too
```

Newest first unless `sort=name`. `tags/list` paginates as distribution does:

```text
GET /v2/lajstrom-it/cat/r1/tags/list?n=2          Link: </v2/lajstrom-it/cat/r1/tags/list?last=v2&n=2>; rel="next"   {"tags":["v1","v2"]}
GET …?n=2&last=v2                                 Link: …last=v4…   {"tags":["v3","v4"]}
GET …?n=2&last=v4                                 no Link           {"tags":["v5"]}
GET …?n=100000                                    200, all five
GET /v2/lajstrom-it/nope/x/tags/list              404 {"errors":[{"code":"NOT_FOUND","message":"repository lajstrom-it/nope/x not found"}]}
```

## Delete

```text
DELETE /v2/lajstrom-it/curl/del/manifests/t1                 400 {"errors":[{"code":"UNSUPPORTED","message":"unsupported digest t1: invalid checksum digest format"}]}
DELETE /v2/lajstrom-it/curl/del/manifests/sha256:<t2>        202   GET manifests/t2 and by digest afterwards: 404 NOT_FOUND "artifact … not found"
DELETE the same digest again                                 404
DELETE /v2/lajstrom-it/curl/del/blobs/sha256:<t2's layer>    202
DELETE /v2/lajstrom-it/curl/del/tags/reference/t3            401 UNAUTHORIZED "unrecognized request: DELETE …"
DELETE /api/v2.0/projects/lajstrom-it/repositories/curl%252Fdel/artifacts/t3            200 (robot)   GET t3: 404
DELETE /api/v2.0/projects/lajstrom-it/repositories/curl%252Fdel/artifacts/t4/tags/t4    200 (robot)   GET t4: 404, by digest: 200 (untagged)
DELETE /api/v2.0/projects/lajstrom-it/repositories/curl%252Fdel                         200 (robot)   tags/list: 404
DELETE /v2/lajstrom-it/raw/ref/pkg/manifests/sha256:<S>      202   both referrers of S: 404; referrers of S: []
```

A digest delete removes the artifact with all its tags. **The referrers of a subject go with
it**: Harbor keeps them as accessories of the subject and deletes them together. Lajstrom's
provider deletes by digest only, and the integration test deletes the referrer first. A tag is
deleted through the REST API, not through `/v2/`; a repository too (the provider's
`DeleteRepository` says unsupported).

## Immutable tags

Immutability rules belong to a project: `POST /api/v2.0/projects/2/immutabletagrules` (admin;
the robot gets 403 `FORBIDDEN` without the `immutable-tag` permission) with a rule for every
repository and every tag:

```json
{"disabled":false,"action":"immutable","template":"immutable_template",
 "tag_selectors":[{"kind":"doublestar","decoration":"matches","pattern":"**"}],
 "scope_selectors":{"repository":[{"kind":"doublestar","decoration":"repoMatches","pattern":"**"}]}}
```

`a` tagged `v1` before the rule, then, in `lajstrom-it/imm/pkg`:

```text
PUT  manifest b as v1                412 {"errors":[{"code":"PRECONDITION","message":"Failed to process request due to 'imm/pkg:v1' configured as immutable."}]}
PUT  manifest a as v1 again          412 the same
PUT  manifest a by digest            201
PUT  manifest b as v2                201
PUT  manifest c by digest            201
PUT  manifest a as v1-copy           201   (a second tag for a tagged manifest)
DELETE manifests/sha256:<a>          412 {"errors":[{"code":"PRECONDITION","message":"the tag v1-copy configured as immutable, cannot be deleted"}]}
DELETE manifests/sha256:<c>          202   (untagged)
DELETE manifests/v1                  400 UNSUPPORTED (by tag, as without rules)
DELETE /api/v2.0/…/artifacts/v1      412 the same message, robot and admin
DELETE /api/v2.0/…/artifacts/v1/tags/v1   412 "the tag v1 configured as immutable, cannot be deleted"
PUT  a referrer of a, by digest      201   OCI-Subject;   DELETE it: 202
PUT  an index as sha256-<a> (the tag schema), when that tag exists    412 … 'imm/pkg:sha256-e49d1862…' configured as immutable.
```

`ImmutableTags` should read **412 with the code `PRECONDITION`** and a message "… configured as
immutable". The provider's `classifyResponse` makes it `ErrConflict` today through the word
"immutable", for the push and for the delete. As on GitLab, the same manifest pushed again
under its own tag is refused, not idempotent; Lajstrom's ingest resolves the tag first and
writes nothing on a repeat. `…/artifacts?with_immutable_status=true` marks each tag
`"immutable":true`. Referrers are untagged and stay deletable; the tag-schema index could not be
updated under such a rule, which does not matter while the referrers API is there.

## Quotas

A project with a storage quota of 1 MiB, `lajstrom-quota` (admin, `"storage_limit":1048576`):

```text
512 KiB blob, its manifest           201, 201   quota used 524728 of 1048576
768 KiB blob, PUT                    403 {"errors":[{"code":"DENIED","message":"adding 768.0 KiB of storage resource, which when updated to current usage of 512.4 KiB will exceed the configured upper limit of 1.0 MiB."}]}
its manifest                         403 DENIED "adding 768.4 KiB of storage resource, … will exceed the configured upper limit of 1.0 MiB."
1.5 MiB blob, POST + PUT             403
1.5 MiB blob, POST + PATCH           202 (Range: 0-1572863): a PATCH is not checked
the 512 KiB manifest as a second tag 201, used unchanged
```

The quota is checked when a blob upload closes and when a manifest is pushed, before the
registry looks for the blobs (a manifest of a blob that was never uploaded is the same 403, not
`MANIFEST_BLOB_UNKNOWN`). A blob mounted from another project is not counted: the admin
mounted the 2 GiB blob of `lajstrom-it` into `lajstrom-quota` (`POST
…/blobs/uploads/?mount=…&from=lajstrom-it/big/blob`, 201) and pushed a manifest of it (201),
and the usage grew by the 442 bytes of the manifest.

Through Lajstrom, on `lajstrom-it` with a limit of its usage plus 64 KiB:

```text
curl -T q128k.bin localhost:8085/raw/quota/q128k.bin     502 Bad Gateway
    serve log: raw: request failed … provider: unauthorized (HTTP 403, DENIED)
curl -T q16k.bin  localhost:8085/raw/quota/q16k.bin      201
oras push … localhost:8085/oci/conformance/quota:1 big.bin   Error response from registry: unknown: Bad Gateway
    serve log: facade: request failed … PUT …/blobs/uploads/… status=502 … provider: unauthorized (HTTP 403, DENIED)
```

The first item of [What the provider must do differently](#what-the-provider-must-do-differently).

## Uploads

`TestIntegrationUploads/chunk out of order` fails: Harbor's registry appends a `PATCH` at the
end of the session whatever its `Content-Range` says, where distribution 3 and the OCI spec
answer 416:

```text
PATCH abc, Content-Range: 0-2                202  Range: 0-2
PATCH x,   Content-Range: 1-1                202  Range: 0-3
PATCH y,   Content-Range: 7-7                202  Range: 0-4
PUT ?digest=sha256(abcxy)                    201
the second PATCH on distribution 3.1.2       416 {"errors":[{"code":"RANGE_INVALID","message":"invalid content range"}]}
```

A client that resumes at a wrong offset is caught only by the digest of the closing `PUT`. The
session across providers, the step that failed against [GitLab's test token
server](gitlab.md#bearer-tokens) ("request body is not rewindable"), passes here, and so does
the facade's `TestIntegrationUploadAcrossReplicas`: the second provider's `GET` of the session
before its `PATCH` fetches the token. So the second item of gitlab.md's list does not hold for
every token registry; why it failed there was not examined again. Cross-repository mounts
inside the project are 201.

## Blob redirect, ranges, sizes, headers

- No redirect on filesystem storage: `GET` of a blob is 200 with the bytes, `Accept-Ranges:
  bytes`, `Docker-Content-Digest`; `HEAD` the same; `Range: bytes=0-4` is 206 with
  `Content-Range: bytes 0-4/13`, a range past the end 416 `bytes */13`, two ranges a 206
  `multipart/byteranges`. A missing blob is 404 `BLOB_UNKNOWN`. S3 storage
  (`storage_service` in `harbor.yml`, with `redirect.disable`) was not run.
- A 2 GiB blob in one `PUT` (chunked, from `/dev/zero`) was 201 in 15 s; `HEAD` says
  `Content-Length: 2147483648`. No blob limit was met.
- Manifests: 4 MiB. A manifest of 4194304 bytes is 201, of 4194305 bytes 413
  `{"errors":[{"code":"REQUEST_ENTITY_TOO_LARGE","message":"request entity too large"}]}`: the
  same limit as Lajstrom's `server.maxManifestSize` default.
- A manifest `GET` or `HEAD` needs an OCI type in `Accept`: without one, with `*/*` or with
  the Docker v2 type alone, Harbor answers 404 `MANIFEST_UNKNOWN` "OCI manifest found, but
  Accept header does not support OCI manifests", by tag and by digest. oras-go and the provider
  send one; the facade passes the client's.
- `Docker-Content-Digest` on the blob `PUT`, the manifest `PUT`, `HEAD` and `GET`;
  `OCI-Subject` on the `PUT` of a manifest with a subject. `GET /v2/` (with a token) is 200
  `{}` with `Docker-Distribution-Api-Version: registry/2.0` and nothing that names Harbor.
- No rate limit: no 429 in the curl runs, three runs of the test suites and the conformance
  suite. Harbor has none of its own.

## Media types

Nothing in our manifest profile is refused: `artifactType`, the empty config with its `data`,
`application/vnd.lajstrom.*` layers, annotations. The oras pushes of the [raw](../quickstart/oras.md)
and [deb](../quickstart/apt.md) quick starts, straight to Harbor with the robot:

```text
$ oras push --plain-http --artifact-type application/vnd.lajstrom.raw.v1 … localhost:5005/lajstrom-it/raw/assets/docs/hello.txt:ec5ba820a095a4d6 docs/hello.txt:application/vnd.lajstrom.raw.file.v1
Pushed [registry] localhost:5005/lajstrom-it/raw/assets/docs/hello.txt:ec5ba820a095a4d6
ArtifactType: application/vnd.lajstrom.raw.v1
$ oras push --plain-http --artifact-type application/vnd.lajstrom.deb.v1 … localhost:5005/lajstrom-it/apt/ubuntu/world:2.0-1__jammy_2fmain_2famd64 \
    world_2.0-1_amd64.deb:application/vnd.lajstrom.deb.package.v1 metadata.json:application/vnd.lajstrom.deb.metadata.v1+json
Pushed [registry] localhost:5005/lajstrom-it/apt/ubuntu/world:2.0-1__jammy_2fmain_2famd64
ArtifactType: application/vnd.lajstrom.deb.v1
```

The same raw push again succeeds. Harbor files both as artifacts of type `UNKNOWN` with
`artifact_type` `application/vnd.lajstrom.raw.v1` and `…deb.v1`, the annotations shown in its
UI.

## Garbage collection

The question of #37: does a referrer stored as cosign stores it, a manifest with `subject` and
no tag, survive the collector? Harbor's GC is a job of jobservice, run by hand here (`POST
/api/v2.0/system/gc/schedule`, `"type":"Manual"`); it does not run by itself unless scheduled.
In `lajstrom-it/gc/pkg`: `M`, a manifest tagged `v1`; `R`, a referrer of `M` by digest, no tag;
`N`, a manifest by digest, no tag, no subject; `T`, a manifest listed by an index tagged
`sha256-<M>` (the tag schema); `U`, a blob no manifest refers to. Harbor keeps every blob
younger than its time window, 2 h (`GC_TIME_WINDOW_HOURS` of core, "for testing/debugging" in
the source), so the experiment ran twice, the second time with a new set and the window at 0:

| | window 2 h, `delete_untagged: false` | 2 h, `true` | 0, `false` | 0, `true` |
| --- | --- | --- | --- | --- |
| `R`, untagged referrer of a tagged subject | kept | **kept** | kept | **kept**, and its layer |
| `N`, untagged, no subject | kept | deleted (404) | kept | deleted, and its layer |
| `U`, unreferenced blob | kept (inside the window) | kept | deleted | deleted |
| `M`, `T` and the index | kept | kept | kept | kept |

```text
GC 3  delete_untagged false, time_window 2   "no need to execute GC as there is no non referenced artifacts."
GC 5  delete_untagged true,  time_window 2   "delete the untagged artifact: … RepositoryName(lajstrom-it/gc/pkg) … Digest:(sha256:a5f6e8ca…)"   (N)
GC 7  delete_untagged false, time_window 0   "1 blobs and 0 manifests eligible for deletion"   (U)
GC 8  delete_untagged true,  time_window 0   "delete the untagged artifact: … (sha256:c875eda0…)", "1 blobs and 1 manifests are actually deleted"   (N)
```

**An untagged referrer survives Harbor's GC, with or without `delete_untagged`**: the option
deletes the untagged artifacts Harbor lists, and a referrer is not one of them but an accessory
of its subject (the REST API shows it under the subject only). It goes when its subject goes
([Delete](#delete)). Left out of the request, `delete_untagged` is true (`gc.deleteUntagged =
true` in the job's source; not run). The window protects blobs uploaded before their manifest;
Lajstrom's longest gap (an upload session of the facade lives an hour after its
last request) is inside the default 2 h. `time_window` in the request is ignored: the API sets
it from core's environment. `docker compose` recreating core with the variable, or
`LAJSTROM_HARBOR_GC_TIME_WINDOW_HOURS=0 deploy/compose/harbor/harbor.sh up`, sets it.

## Our tests against it

`LAJSTROM_TEST_REGISTRY=localhost:5005`, `LAJSTROM_TEST_REGISTRY_USERNAME='robot$lajstrom-it+ci'`,
the robot's secret in `LAJSTROM_TEST_REGISTRY_PASSWORD`, `LAJSTROM_TEST_POSTGRES_DSN` of the dev
stack, `go test -race -count=1 -tags integration`: the whole suite with
`LAJSTROM_TEST_REGISTRY_PREFIX=lajstrom-it/`, then `-run 'TestIntegration|TestAptGet' -v ./...`
with the prefix, without it, and with the prefix and the immutability rule `**`. The apt tests
run apt-get in `ubuntu:22.04`, `ubuntu:24.04` and `debian:12` against Lajstrom on Harbor.

| Test | prefix `lajstrom-it/` | no prefix | prefix, immutability rule `**` |
| --- | --- | --- | --- |
| provider `TestIntegration` | pass; `list repositories` skipped (no catalog) | **fail**: `Capabilities`, the probe's 400 | **fail**: delete, `provider: conflict (HTTP 412, PRECONDITION)` |
| provider `TestIntegrationUploads` | **fail**: chunk out of order accepted; the other six steps pass, the session across providers among them | **fail**: start upload, 401 | the same as with the prefix |
| ingest `TestIntegration` | pass | **fail**: 401 | pass |
| raw `TestIntegration` | pass | **fail**: 401 | **fail**: `DELETE` 409 |
| facade: uploads across replicas, abandoned monolithic, pass-through, raw push, blob reads | pass (S3 redirect skipped) | **fail**: 502, the backend's 401 | pass |
| `TestAptGet`, `TestAptGetInstall/registry`, `TestAptGetSigned` | pass | **fail**: 401 | pass |
| conformance v1.1.1 through the facade, an `oci` repo | pull 23, push 26 pass; discovery 10 pass, 5 fail | not run | not run |

With the prefix the whole suite passed, the Postgres tests included, but for the out-of-order
chunk. The failures, by owner:

- **Harbor's:** the out-of-order chunk ([Uploads](#uploads)).
- **The configuration's:** without a prefix nothing is inside a project; Harbor wants
  `prefix: <project>/` (item 3 of the list).
- **The policy's, read rightly:** with the rule every delete of a tagged manifest is refused,
  and the raw route answers 409.
- **Ours, the facade:** the five discovery failures are those of Zot, distribution and GitLab:
  the referrers API of #28 (the facade answered 501 then) and `OCI-Subject` in the references
  setup, which Harbor sends and the pass-through dropped. The facade serves both since #28; the
  suite was not run against Harbor again.
- **Ours, the provider:** the quota's 403 read as bad credentials, a 502 for the client
  (item 1).

The serve log of the conformance run has no warning or error; the facade's `/v2/_catalog`
answers from Lajstrom's index (`{"repositories":["oci/conformance/a"]}`), not from Harbor.

## Commands

Harbor as in [Run it](#run-it). The project, the robot (its secret goes to a file and stays
out of the terminal), the quota project:

```sh
H=http://localhost:5005 A="admin:$HARBOR_ADMIN_PASSWORD"
curl -u "$A" -H 'Content-Type: application/json' -X POST $H/api/v2.0/projects \
  -d '{"project_name":"lajstrom-it","metadata":{"public":"false"}}'
umask 077; curl -s -u "$A" -H 'Content-Type: application/json' -X POST $H/api/v2.0/robots -d '{"name":"ci","level":"project","duration":-1,
  "permissions":[{"kind":"project","namespace":"lajstrom-it","access":[
    {"resource":"repository","action":"pull"},{"resource":"repository","action":"push"},{"resource":"repository","action":"delete"},
    {"resource":"repository","action":"list"},{"resource":"repository","action":"read"},
    {"resource":"artifact","action":"delete"},{"resource":"artifact","action":"list"},{"resource":"artifact","action":"read"},
    {"resource":"tag","action":"list"},{"resource":"tag","action":"create"},{"resource":"tag","action":"delete"},
    {"resource":"accessory","action":"list"}]}]}' | jq -r .secret > robot.secret
curl -u "$A" -H 'Content-Type: application/json' -X POST $H/api/v2.0/projects \
  -d '{"project_name":"lajstrom-quota","metadata":{"public":"false"},"storage_limit":1048576}'
```

A token and a request with it; the probes with curl:

```sh
TOKEN=$(curl -s -u "robot\$lajstrom-it+ci:$(cat robot.secret)" \
  "$H/service/token?service=harbor-registry&scope=repository:lajstrom-it/raw/ref/pkg:pull,push,delete" | jq -r .token)
curl -si -H "Authorization: Bearer $TOKEN" $H/v2/lajstrom-it/raw/ref/pkg/referrers/sha256:<subject>
curl -si -X PUT -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/vnd.oci.image.manifest.v1+json' \
  --data-binary @m.json $H/v2/lajstrom-it/raw/ref/pkg/manifests/v1
curl -si -u "robot\$lajstrom-it+ci:$(cat robot.secret)" "$H/api/v2.0/projects/lajstrom-it/repositories?page=1&page_size=2"
curl -u "$A" -H 'Content-Type: application/json' -X POST $H/api/v2.0/projects/2/immutabletagrules -d @rule.json
curl -u "$A" -H 'Content-Type: application/json' -X PUT $H/api/v2.0/quotas/2 -d '{"hard":{"storage":-1}}'
```

oras 1.3.6 from its image on the host network, logged in with the robot:

```sh
oras() { docker run --rm -i --network host -v "$PWD:/w" -w /w ghcr.io/oras-project/oras:v1.3.6 "$@"; }
oras login --plain-http -u 'robot$lajstrom-it+ci' --password-stdin localhost:5005 < robot.secret
```

The garbage collector, and its log:

```sh
curl -u "$A" -H 'Content-Type: application/json' -X POST $H/api/v2.0/system/gc/schedule \
  -d '{"schedule":{"type":"Manual"},"parameters":{"delete_untagged":false,"dry_run":false,"workers":1}}'
curl -s -u "$A" "$H/api/v2.0/system/gc?page_size=1" | jq '.[0] | {id, job_status, job_parameters}'
curl -s -u "$A" $H/api/v2.0/system/gc/<id>/log
```

The integration tests of type `harbor`: as the project robot, and as the administrator, which
makes projects of its own and deletes them:

```sh
LAJSTROM_TEST_HARBOR=http://localhost:5005 LAJSTROM_TEST_HARBOR_PROJECT=lajstrom-it \
LAJSTROM_TEST_HARBOR_USERNAME='robot$lajstrom-it+ci' LAJSTROM_TEST_HARBOR_PASSWORD="$(cat robot.secret)" \
LAJSTROM_TEST_HARBOR_ADMIN_USERNAME=admin LAJSTROM_TEST_HARBOR_ADMIN_PASSWORD="$HARBOR_ADMIN_PASSWORD" \
  go test -race -count=1 -tags integration -run TestIntegrationHarbor -v ./internal/provider/harbor/
```

The tests and the conformance suite (`lajstrom-harbor.yaml`: the example configuration with
`listen: 127.0.0.1:8085`, the registry `local` at `http://localhost:5005` with
`prefix: lajstrom-it/`, `username: robot$lajstrom-it+ci` and `passwordEnv:
HARBOR_ROBOT_PASSWORD`, no repos):

```sh
LAJSTROM_TEST_REGISTRY=localhost:5005 LAJSTROM_TEST_REGISTRY_PREFIX=lajstrom-it/ \
LAJSTROM_TEST_REGISTRY_USERNAME='robot$lajstrom-it+ci' LAJSTROM_TEST_REGISTRY_PASSWORD="$(cat robot.secret)" \
LAJSTROM_TEST_POSTGRES_DSN='postgres://lajstrom:lajstrom@localhost:5432/lajstrom?sslmode=disable' \
  go test -race -count=1 -tags integration ./...
HARBOR_ROBOT_PASSWORD="$(cat robot.secret)" bin/lajstrom serve --config lajstrom-harbor.yaml &
HARBOR_ROBOT_PASSWORD="$(cat robot.secret)" bin/lajstrom admin repo create oci/conformance --registry local --config lajstrom-harbor.yaml
OCI_ROOT_URL=http://localhost:8085 test/conformance/run.sh
```

# First public use of the name "Lajstrom"

A dated log of the first public use of the name *Lajstrom* for this software project, kept as
evidence for possible future trademark purposes. All times are UTC, on **2026-11-05**.

Append new entries at the end; do not edit past entries except to fix links.

## Timeline

| Time (UTC) | Event | Public evidence | Snapshot |
| --- | --- | --- | --- |
| 19:08:09Z | GitHub organization `lajstrom` created | <https://github.com/lajstrom>, `created_at` in the [org API](https://api.github.com/orgs/lajstrom) | [github-org-lajstrom.json](first-use/github-org-lajstrom.json) |
| 19:11:07Z | First commit `e26e19d` of `lajstrom/lajstrom` (commit time) | <https://github.com/lajstrom/lajstrom/commit/e26e19d63e5be22e0581b46cb5d72cfe723f6f71> | [proxy-golang-org-lajstrom-v0.0.1.json](first-use/proxy-golang-org-lajstrom-v0.0.1.json) (`Time`, `Origin.Hash`) |
| 19:11:09Z | Repository `lajstrom/lajstrom` created on GitHub | `created_at` in the [repository API](https://api.github.com/repos/lajstrom/lajstrom) | [github-repo-lajstrom.json](first-use/github-repo-lajstrom.json) |
| ~19:11Z | Tag `v0.0.1` pushed (annotated tag dated 19:11:16Z; repository `pushed_at` 19:11:18Z) | <https://github.com/lajstrom/lajstrom/tree/v0.0.1> | [github-repo-lajstrom.json](first-use/github-repo-lajstrom.json) |
| 19:11:43Z | Go module `github.com/lajstrom/lajstrom@v0.0.1` first fetched by proxy.golang.org, as recorded by the module index | <https://proxy.golang.org/github.com/lajstrom/lajstrom/@v/v0.0.1.info>, <https://index.golang.org/index?since=2026-11-05T19:00:00Z> | [proxy-golang-org-lajstrom-v0.0.1.json](first-use/proxy-golang-org-lajstrom-v0.0.1.json), [index-golang-org-lajstrom.json](first-use/index-golang-org-lajstrom.json) |
| 19:23:50Z | PyPI package `lajstrom` 0.0.1 published | <https://pypi.org/project/lajstrom/0.0.1/> | [pypi-lajstrom-0.0.1.json](first-use/pypi-lajstrom-0.0.1.json) |
| 19:28:07Z | crates.io crate `lajstrom` 0.0.1 published | <https://crates.io/crates/lajstrom> | [crates-io-lajstrom.json](first-use/crates-io-lajstrom.json) |
| 19:33:28Z | npm package `lajstrom` created (staged placeholder `0.0.0-stage`), `0.0.1` published at 19:34:45Z; npm organization `lajstrom` created | <https://www.npmjs.com/package/lajstrom>, <https://www.npmjs.com/org/lajstrom> | [npm-lajstrom.json](first-use/npm-lajstrom.json) |
| 19:37:46Z | Docker Hub account `lajstrom` created | <https://hub.docker.com/u/lajstrom> | [docker-hub-user-lajstrom.json](first-use/docker-hub-user-lajstrom.json) |
| 2026-11-05 | Domain `lajstrom.dev` registered by the project owner | <https://lajstrom.dev> | none, see notes |
| 20:39:09Z | Domain `lajstrom.dev` registered: the `registration` event of its RDAP record, 2026-11-05T20:39:09.010Z (added 2026-11-12, see notes) | <https://lajstrom.dev>, <https://pubapi.registry.google/rdap/domain/lajstrom.dev> | [rdap-lajstrom-dev.json](first-use/rdap-lajstrom-dev.json) |

## Snapshots

The files in [`first-use/`](first-use/) are responses of the public APIs listed below, fetched on
2026-11-05 between 20:17Z and 20:21Z, without authentication, and pretty-printed:

| File | Source |
| --- | --- |
| `github-org-lajstrom.json` | `GET https://api.github.com/orgs/lajstrom` |
| `github-repo-lajstrom.json` | `GET https://api.github.com/repos/lajstrom/lajstrom` |
| `proxy-golang-org-lajstrom-v0.0.1.json` | `GET https://proxy.golang.org/github.com/lajstrom/lajstrom/@v/v0.0.1.info` |
| `index-golang-org-lajstrom.json` | the `github.com/lajstrom/lajstrom` record from `GET https://index.golang.org/index?since=2026-11-05T19:00:00Z&limit=2000` |
| `pypi-lajstrom-0.0.1.json` | `GET https://pypi.org/pypi/lajstrom/0.0.1/json` |
| `crates-io-lajstrom.json` | `GET https://crates.io/api/v1/crates/lajstrom` (with a descriptive `User-Agent`, as crates.io requires) |
| `npm-lajstrom.json` | `GET https://registry.npmjs.org/lajstrom` |
| `docker-hub-user-lajstrom.json` | `GET https://hub.docker.com/v2/users/lajstrom/` |

Personal data was redacted and replaced with `"[redacted]"`; nothing else was changed:

- `npm-lajstrom.json`: the `email` fields of `maintainers` and `_npmUser`.
- `crates-io-lajstrom.json`: the display `name` of the publishing user in `published_by` and
  `audit_actions` (the GitHub login `gecube` is kept).

## Notes

- The `Time` field of the proxy.golang.org `.info` response is the commit time of the tagged
  commit (19:11:07Z), not the time the proxy fetched it. The first fetch by the proxy is the
  `Timestamp` recorded by index.golang.org (19:11:43Z).
- The npm organization has no public JSON API; its page is linked instead.
- `lajstrom.dev`: at the time the snapshots were taken (about 20:21Z) the `.dev` registry RDAP
  service (`https://pubapi.registry.google/rdap/domain/lajstrom.dev`) returned 404 and the domain
  had no NS records yet. A snapshot of the RDAP record should be added here once the registration
  is visible.
- `lajstrom.dev`, added on 2026-11-12: the RDAP record is visible now. Its `registration` event
  is 2026-11-05T20:39:09.010Z, after the snapshots of 20:17Z to 20:21Z, hence the 404 then; the
  last row of the timeline has it. `rdap-lajstrom-dev.json` is
  `GET https://pubapi.registry.google/rdap/domain/lajstrom.dev`, fetched on 2026-11-12 at
  08:34:47Z, without authentication, and pretty-printed. It holds the registrar's contacts and
  no personal data, so nothing was redacted.

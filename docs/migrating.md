# Coming from Verdaccio, Nexus or Artifactory

Lajstrom imports nothing from another registry's storage. Packages move through what a proxy repo
already does, in three steps:

1. A **proxy repo** whose upstream is the old registry, with its credentials.
2. A **prefetch**: `lajstrom admin repo prefetch` fetches the versions of a list of names through
   the proxy, each as a client's first `npm install` would fetch it. The tarballs come into the
   registry byte for byte, checked against the old registry's checksums, with their origin in
   the manifest.
3. The **switch**: the proxy goes offline and the old registry is switched off. The proxy serves
   what it holds, with the dist-tags and deprecations as the old registry had them.

This page is for npm. pypi and maven get the same steps when their proxies come (#67, #68).
Verdaccio is what the steps were run against (`test/e2e/migrate-npm.sh`). The Nexus and
Artifactory commands below follow their documentation and were not run here.

Another way, publishing every tarball to a hosted repo again, is in
[quickstart/npm.md](quickstart/npm.md#coming-from-verdaccio). It keeps the bytes too, but it
needs the tarballs on disk, it sets each version's publish time to the day of the move, and the
dist-tags and deprecations have to be set again by hand.

## 1. The upstream and the proxy repo

The old registry becomes an upstream in the configuration, with the token of a user that may
read every package. Verdaccio's and Nexus's npm token, and an Artifactory access token, are a
bearer token: `auth.tokenFile`, a file that holds it. A registry that wants a user and a password
takes `auth.username` and `auth.passwordFile` (HTTP Basic). The file is read when `serve` starts.

```yaml
upstreams:
  - name: old
    format: npm
    url: "https://verdaccio.internal.example/"     # Verdaccio
    # url: "https://nexus.example/repository/npm-private/"            # Nexus, the hosted npm repository
    # url: "https://acme.jfrog.io/artifactory/api/npm/npm-local/"     # Artifactory, the local npm repository
    auth:
      tokenFile: /etc/lajstrom/old-registry.token
```

Point it at the repository that holds your own packages, not at a group or virtual repository
that also proxies npmjs.org: what Lajstrom does not need to move is easier to leave out than to
sort out later. Then the proxy repo, by an admin:

```sh
curl -s -X POST "$LAJSTROM/api/v1/repos" \
  -d '{"format": "npm", "name": "legacy", "registry": "local", "kind": "proxy", "upstream": {"name": "old"}}'
```

The repo can serve clients from here on: a version a client asks for is fetched then.

## 2. The list of names

A prefetch takes package names; npm has no standard list of every package of a registry, so the
list is made from the old registry:

- **Verdaccio:** `storage/.verdaccio-db.json` lists the packages published to it, and not those
  it cached from an uplink: `jq -r '.list[]' storage/.verdaccio-db.json > names.txt`.
- **Nexus:** the search API of the repository, `GET /service/rest/v1/search?repository=npm-private`,
  a page at a time with `continuationToken`. An npm component's `group` is its scope without the
  `@`: `jq -r '.items[] | if .group then "@\(.group)/\(.name)" else .name end' | sort -u`.
- **Artifactory:** the folders of the local repository, `GET /artifactory/api/storage/npm-local`.
  A scope is a folder (`@acme`) with a folder per package.

The file has one name a line; blank lines and what follows `#` are skipped, and a name twice goes
once.

## 3. The prefetch

`lajstrom admin repo prefetch` is a client of the server's API ([api.md](api.md#prefetch-a-proxy-repo-post-apiv1reposformatnameprefetch)):
the server fetches, with its credentials for the old registry. It needs a token with `manage` on
the repo, a maintainer's.

```sh
export LAJSTROM_URL=https://lajstrom.example LAJSTROM_TOKEN_FILE=~/.lajstrom-token
lajstrom admin repo prefetch npm/legacy --from-file names.txt --all-versions --report report.json
```

```text
fetched  @acme/widgets 1.0.0
fetched  @acme/widgets 1.1.0
fetched  @acme/widgets 2.0.0-rc.1
held     @acme/tools 3.2.0
failed   @acme/broken 0.1.0: 502 proxy: npm/legacy: broken-0.1.0.tgz of upstream old does not match its sha512
npm/legacy: 3 names: 3 fetched, 1 held, 0 shared, 1 failed
```

- **Which versions.** `--all-versions` takes every version the old registry lists. Without it, a
  prefetch takes the versions the dist-tags name (`latest`, `next`, …), which is enough for
  `npm install <name>` and `<name>@<tag>`, and the rest comes on demand while the old registry
  is still there. Prefetch everything that a lockfile may name before the switch.
- **What happens to each.** The same as on a client's miss: the claim, so that two replicas fetch
  a version once; the download, bounded by the repo's `maxFileSize`; the check against the
  strongest checksum the packument gives (`dist.integrity`, else `dist.shasum`); the ingest, with
  `lajstrom.origin.url` and `lajstrom.origin.checksum` in the manifest; an audit row
  `proxy.fetch`, by the token's user. A version the repo holds is `held` and costs one index
  lookup. The full and the abbreviated packument of each name are cached too, so that the repo
  answers `npm install` once it is offline.
- **Failures** are a line each with the status a client would have had. A run that stops (the
  server down, a signal) records the names it did not reach as failed. Run again from the
  report to try only the failures:

  ```sh
  lajstrom admin repo prefetch npm/legacy --from-file report.json --failed --report report.json
  ```

- **Batches.** The names go 20 a request (`--batch`, at most 100). A request returns when every
  version of its names has ended, and fetches 4 versions at a time; a long list takes as long as
  the old registry takes to send it.

Check what came, before the switch: the dist-tags and the integrity of a package as both serve
it, and the count of `proxy.fetch` rows.

```sh
diff <(npm view @acme/widgets dist-tags time --json --registry https://verdaccio.internal.example/) \
     <(npm view @acme/widgets dist-tags time --json --registry "$LAJSTROM/npm/legacy/")
curl -s "$LAJSTROM/api/v1/audit?repo=npm/legacy&action=proxy.fetch&limit=500" | jq '.events | length'
```

## 4. The switch

```sh
curl -s -X PATCH "$LAJSTROM/api/v1/repos/npm/legacy" -d '{"upstream": {"offline": true}}'
```

An offline proxy asks its upstream nothing ([proxy-and-group.md](spec/proxy-and-group.md) §4.4):
it serves the versions it holds and the packuments it cached, whatever their age. The old
registry can be stopped. Then the clients: `.npmrc` names the new repo (`registry=` or
`@acme:registry=`) with a token of Lajstrom's. The tarballs are the same bytes, so the
`integrity` of lockfiles holds; their `resolved` URLs still name the old registry, which npm's
`replace-registry-host` setting redirects.

What an offline proxy does not do:

- **It takes no publish.** New versions go to a hosted repo, and one URL serves both: a group
  `[hosted, legacy]` with the private scope in `group.merge`, so that a new version in the hosted
  repo does not hide the old versions of the same name
  ([quickstart/npm.md](quickstart/npm.md#10-a-group-one-url-for-both), proxy-and-group.md §8.2).

  ```sh
  curl -s -X POST "$LAJSTROM/api/v1/repos" -d '{"format": "npm", "name": "internal", "registry": "local"}'
  curl -s -X POST "$LAJSTROM/api/v1/repos" \
    -d '{"format": "npm", "name": "all", "kind": "group", "group": {"members": ["internal", "legacy"], "merge": ["@acme/*"]}}'
  ```

- **Its dist-tags live in a cache.** They are the old registry's packuments, kept in the index
  (`proxy_meta`). The versions are in the registry and survive anything the index does, but a
  purge of the upstream cache, or an index rebuilt from the registry, loses the packuments: the
  repo then lists what it holds, with `latest` the highest version, and its other dist-tags and
  its deprecations are gone. `npm dist-tag` cannot set them again on a proxy.

## Users and tokens

None carry over: an admin makes the users again (`lajstrom admin user create`, or
`/api/v1/users`) and binds them to the repos, and every `.npmrc` gets a new token from
`npm login`. A machine gets a deploy token of the repo.

# Quick start: an npm repo

An npm repo serves packages to npm, pnpm and yarn 4, as Verdaccio does: `npm publish` puts a
version in, `npm install` resolves from the repo's packuments, and `npm dist-tag`, `npm
deprecate` and `npm unpublish` change a package. Every version is one OCI artifact in the
registry behind Lajstrom, its tarball and the version document npm sent as layers; dist-tags and
deprecations are a journal of small manifests beside the versions, so that `lajstrom reindex`
restores them. A proxy repo serves registry.npmjs.org and keeps every tarball it fetched in the
registry too, and a group repo serves both under one URL. The endpoints are specified in
[http-routing.md](../spec/http-routing.md) §3 (npm) and §6, the format in
[ADR 0012](../adr/0012-npm-profile-routes-and-dist-tags-journal.md), the proxy and the group in
[ADR 0013](../adr/0013-proxy-and-group-repos.md) and
[proxy-and-group.md](../spec/proxy-and-group.md). The output shown is that of one run of this
page from top to bottom, with npm 11.20.0, pnpm 10.34.9 and yarn 4.18.1 (npm's notice of a newer
npm left out); `test/e2e/npm.sh` runs the same steps with checks ([the end-to-end
script](#the-end-to-end-script)).

## 1. Start the dev stack, make a repo and a user

You need Go, Docker and `curl`; npm, pnpm and yarn run in `node:26.13.0-slim` (Node 26, the LTS,
with npm 11), the image of `test/e2e/npm.sh`, so nothing of Node is needed on the host. `crane`
(section 11) is optional.

```sh
make dev-up                        # Zot, distribution and Postgres: docs/dev-stack.md
make run                           # serves deploy/compose/lajstrom.example.yaml on :8080
```

In another terminal:

```sh
cfg=deploy/compose/lajstrom.example.yaml
bin/lajstrom admin repo create npm/internal --registry local --config $cfg
bin/lajstrom admin user create --name alice --config $cfg     # asks for a password, or takes it with --password-stdin
bin/lajstrom admin binding create --principal user:alice --role writer --mask npm/internal --config $cfg
```

```json
{"format":"npm","name":"internal","registry":"local","kind":"hosted","anonymousRead":false,"policy":null,"createdAt":"2026-11-10T17:42:35.460885Z","updatedAt":"2026-11-10T17:42:35.460885Z"}
{"uid":"01a1c68d-f8e4-7d05-8285-172d039ddc0b","name":"alice","kind":"local","disabled":false,"createdAt":"2026-11-10T17:42:35.492707Z"}
{"id":1,"principal":"user:01a1c68d-f8e4-7d05-8285-172d039ddc0b","name":"alice","role":"writer","repoMask":"npm/internal","createdAt":"2026-11-10T17:42:35.505045Z"}
```

The binding lets alice publish to `npm/internal`, install from it and move its dist-tags: the
role `writer` is `read` and `write` on the repos its mask names. `npm unpublish` is `delete`,
which only `maintainer` has (section 6); a user who only installs gets `reader`. Without a
binding the repo answers `404`, as a repo that does not exist would. Nothing is open: without
credentials every request is `401`.

## 2. npm login and `.npmrc`

A container on the host's network, where `localhost:8080` is Lajstrom (Linux and OrbStack):

```sh
docker run --rm -it --network host node:26.13.0-slim bash
```

In the container, the repo becomes npm's registry, and `npm login` asks for alice's user name and
password:

```sh
npm config set registry http://localhost:8080/npm/internal/
npm login
npm whoami
cat ~/.npmrc
```

```text
npm notice Log in on http://localhost:8080/npm/internal/
Username: alice
Password:
Logged in on http://localhost:8080/npm/internal/.
alice
registry=http://localhost:8080/npm/internal/
//localhost:8080/npm/internal/:_authToken=ljs_pat_…
```

npm tries its web login first, which Lajstrom answers `404`, and falls back to the prompt. The
password is checked as a Basic password is, failed logins are throttled, and the answer is a new
[personal access token](../auth.md#tokens) of alice, valid 90 days, named
`npm login npm/internal <date>`: npm keeps it as `_authToken` for the repo's URL, and sends it as
a bearer on every request. `npm login` and `npm adduser` never create a user: users are an
admin's (`lajstrom admin user`, `/api/v1/users`).

## 3. Publish

A scoped package, `@acme/widgets`:

```sh
mkdir /widgets && cd /widgets
npm init -y --scope=@acme
echo 'module.exports = () => "widgets 1.0.0";' > index.js
npm publish
```

```text
npm notice package: @acme/widgets@1.0.0
…
npm notice Publishing to http://localhost:8080/npm/internal/ with tag latest and default access
+ @acme/widgets@1.0.0
```

The same version again is refused by npm itself, which reads the packument first:

```sh
npm publish
```

```text
npm error You cannot publish over the previously published versions: 1.0.0.
```

A client that sends it anyway (npm 10 does) has the same tarball accepted again, which changes
nothing, and another tarball refused with `409`: a version is immutable. Two more versions, the second under the dist-tag `beta`, which leaves
`latest` where it is:

```sh
npm version minor
echo 'module.exports = () => "widgets 1.1.0";' > index.js
npm publish
npm version 2.0.0-beta.0
echo 'module.exports = () => "widgets 2.0.0-beta.0";' > index.js
npm publish --tag beta
npm view @acme/widgets dist-tags versions
```

```text
v1.1.0
+ @acme/widgets@1.1.0
v2.0.0-beta.0
+ @acme/widgets@2.0.0-beta.0
dist-tags = { beta: '2.0.0-beta.0', latest: '1.1.0' }
versions = [ '1.0.0', '1.1.0', '2.0.0-beta.0' ]
```

What the server checks, each a `400` naming the rule ([http-routing.md](../spec/http-routing.md),
npm, Publish):

- the tarball is the package: a gzipped tar with one top directory whose `package.json` has the
  name and version published, and the `integrity` and `shasum` npm computed;
- the version document npm sends says what the tarball's `package.json` says about what an
  install runs: `dependencies`, `optionalDependencies`, `peerDependencies`,
  `bundleDependencies`, `bin` and the install scripts. A packument cannot hide a dependency or a
  script the tarball has ("manifest confusion");
- a name npm accepts for a new package, and a SemVer version without build metadata;
- one version and one tarball per publish: `npm publish --provenance` sends a second attachment,
  which is refused.

`access` (`--access public`, `restricted`) is ignored: who reads a package is the repo's bindings.
Unscoped packages are published the same way.

## 4. Install

From an empty cache, in a new project:

```sh
mkdir /app && cd /app && npm init -y >/dev/null
npm install --cache /tmp/empty-cache @acme/widgets
node -e 'console.log(require("@acme/widgets")())'
npm install @acme/widgets@beta
node -e 'console.log(require("@acme/widgets")())'
```

```text
added 1 package, and audited 2 packages in 106ms

found 0 vulnerabilities
widgets 1.1.0

changed 1 package, and audited 2 packages in 107ms

found 0 vulnerabilities
widgets 2.0.0-beta.0
```

The packument is rendered once per change and kept in the index database; it answers
`If-None-Match` with `304`, and `Cache-Control: no-cache` makes clients see a publish at once. A
tarball is the registry's blob, streamed through Lajstrom, or a `307` to the registry's storage
when it redirects. `npm audit`, which `npm install` runs, gets no advisories from a hosted repo:
it knows none for its own packages.

## 5. dist-tags

```sh
npm dist-tag add @acme/widgets@1.0.0 stable
npm dist-tag ls @acme/widgets
npm install @acme/widgets@stable
node -e 'console.log(require("@acme/widgets")())'
npm dist-tag rm @acme/widgets stable
npm dist-tag rm @acme/widgets latest
```

```text
+stable: @acme/widgets@1.0.0
beta: 2.0.0-beta.0
latest: 1.1.0
stable: 1.0.0

changed 1 package, and audited 2 packages in 113ms

found 0 vulnerabilities
widgets 1.0.0
-stable: @acme/widgets@1.0.0
npm error code E400
npm error 400 Bad Request - DELETE http://localhost:8080/npm/internal/-/package/@acme%2fwidgets/dist-tags/latest - format: invalid: the dist-tag latest is never removed; point it at another version
```

Each change is one entry of the package's journal in the registry (section 11), and an audit row
`npm.dist-tag` with the entry's digest. `latest` can be
moved, never removed; without a `latest` entry (a version pushed with `oras push`), `latest` is
the highest release.

## 6. Deprecate and unpublish

```sh
npm deprecate @acme/widgets@1.0.0 "use 1.1.0 or later"
npm view @acme/widgets@1.0.0 deprecated
mkdir /old && cd /old && npm init -y >/dev/null
npm install @acme/widgets@1.0.0
npm deprecate @acme/widgets@1.0.0 ""
```

```text
npm notice deprecating @acme/widgets@1.0.0 with message "use 1.1.0 or later"
use 1.1.0 or later
npm warn deprecated @acme/widgets@1.0.0: use 1.1.0 or later

added 1 package, and audited 2 packages in 103ms

found 0 vulnerabilities
npm notice undeprecating @acme/widgets@1.0.0
```

A deprecation is a journal entry too, `write` like a dist-tag. Unpublishing deletes, which a
writer may not:

```sh
npm unpublish @acme/widgets@2.0.0-beta.0
```

```text
npm error code E403
npm error 403 403 Forbidden - PUT http://localhost:8080/npm/internal/@acme%2fwidgets/-rev/7-28c9c352683c5a23206ce136e903ae4d - format: forbidden: delete on npm/internal
npm error 403 In most cases, you or one of your dependencies are requesting a package version that is forbidden by your security policy, or on a server you do not have access to.
```

On the host, alice becomes a maintainer of the repo:

```sh
bin/lajstrom admin binding create --principal user:alice --role maintainer --mask npm/internal --config $cfg
```

and in the container:

```sh
npm unpublish @acme/widgets@2.0.0-beta.0
npm view @acme/widgets dist-tags versions
```

```text
- @acme/widgets@2.0.0-beta.0
dist-tags = { latest: '1.1.0' }
versions = [ '1.0.0', '1.1.0' ]
```

The version's manifest is deleted from the registry, as a `DELETE` on `/v2` deletes it, with its
referrers (signatures), and the dist-tag `beta` that named it goes with it. `npm unpublish
@acme/widgets --force` deletes every version and the journal (section 12). Unlike npmjs.org,
Lajstrom lets the same version be published again after an unpublish. The registry must allow
deletes (Zot, and distribution with `REGISTRY_STORAGE_DELETE_ENABLED=true`); one that does not
answers `501` and nothing changes.

## 7. Tokens

A build machine publishes and installs with a deploy token of the repo: it reaches this repo
only, with the scopes it was given. Its secret is printed this once:

```sh
bin/lajstrom admin token create --repo npm/internal --name ci --scopes read,write --config $cfg | jq -r .secret > npm-token
docker run --rm -it --network host -e NPM_TOKEN="$(cat npm-token)" node:26.13.0-slim bash
```

In that container, the token goes in `.npmrc` as `_authToken`, from the environment, so that the
file can be committed:

```sh
cat > ~/.npmrc <<'EOF'
registry=http://localhost:8080/npm/internal/
//localhost:8080/npm/internal/:_authToken=${NPM_TOKEN}
EOF
npm whoami
npm view @acme/widgets version
```

```text
token:2
1.1.0
```

`npm whoami` names a deploy token `token:<id>`. In alice's container, `npm logout` revokes the
token `npm login` made, and removes it from `.npmrc`:

```sh
npm logout
npm whoami
```

```text
npm error code ENEEDAUTH
npm error need auth This command requires you to be logged in.
npm error need auth You need to authorize this machine using `npm adduser`
```

On the host:

```sh
bin/lajstrom admin token list --user alice --config $cfg
bin/lajstrom admin token list --repo npm/internal --config $cfg
```

```text
ID  KIND  NAME                                         SCOPES  EXPIRES               LAST USED             REVOKED               HASH
1   pat   npm login npm/internal 2026-11-10T17:42:35Z  all     2027-02-08T17:42:35Z  2026-11-10T17:42:36Z  2026-11-10T17:42:39Z  d8902041
ID  KIND    NAME  SCOPES      EXPIRES  LAST USED             REVOKED  HASH
2   deploy  ci    read,write  never    2026-11-10T17:42:39Z  -        43eed50c
```

`npm token list`, `create` and `revoke` are `404`: tokens are managed with `lajstrom admin token`
or the [API](../api.md#tokens), which also make personal access tokens narrowed to `read`.
`npm access`, `owner`, `team`, `star` and `profile` are `404` as well.

## 8. pnpm and yarn

Node 25 and later come without corepack, so pnpm and yarn are installed with npm; from npmjs.org
here, or through the proxy repo of section 9.

**pnpm** reads the same `.npmrc` and hands `login`, `whoami`, `dist-tag`, `deprecate` and
`unpublish` to npm. In the container with the deploy token:

```sh
npm install -g --registry=https://registry.npmjs.org/ pnpm@10 @yarnpkg/cli-dist@4
mkdir /p && cd /p && npm init -y >/dev/null
pnpm add @acme/widgets
pnpm whoami
mkdir /gadgets && cd /gadgets && npm init -y --scope=@acme >/dev/null
echo 'module.exports = () => "gadgets 1.0.0";' > index.js
pnpm publish --no-git-checks
```

```text
added 2 packages in 2s
Packages: +1
+
Progress: resolved 1, reused 0, downloaded 1, added 1, done

dependencies:
+ @acme/widgets 1.1.0

Done in 316ms using pnpm v10.34.9
token:2
npm notice package: @acme/gadgets@1.0.0
…
npm notice Publishing to http://localhost:8080/npm/internal/ with tag latest and default access
+ @acme/gadgets@1.0.0
```

**yarn 4** has a configuration of its own, `.yarnrc.yml`, and three settings matter for a private
registry:

- `npmAlwaysAuth: true`: without it yarn sends no credentials for an unscoped package, and does
  not try again when it gets `401`. It goes in the registry's entry of `npmRegistries`, where
  `yarn npm login` puts the token: yarn reads it there, not at the top level, for a registry that
  has an entry;
- `npmMinimalAgeGate: 0`: by default yarn does not install a version published in the last day
  (1440 minutes). `npmPreapprovedPackages: ["@acme/*"]` lifts the gate for your own scopes only;
- `unsafeHttpWhitelist`: the hosts yarn may reach over `http`, here `localhost`.

In alice's container:

```sh
npm install -g --registry=https://registry.npmjs.org/ @yarnpkg/cli-dist@4
cat > ~/.yarnrc.yml <<'EOF'
npmRegistryServer: "http://localhost:8080/npm/internal"
npmRegistries:
  "http://localhost:8080/npm/internal":
    npmAlwaysAuth: true
npmMinimalAgeGate: 0
unsafeHttpWhitelist: ["localhost"]
EOF
mkdir /y && cd /y && npm init -y >/dev/null
yarn npm login
yarn npm whoami
yarn add @acme/widgets @acme/gadgets
yarn node -e 'console.log(require("@acme/widgets")(), require("@acme/gadgets")())'
```

```text
➤ YN0000: Logging in to http://localhost:8080/npm/internal
✔ Username: · alice
✔ Password: · ****
➤ YN0000: Successfully logged in
➤ YN0000: alice
➤ YN0000: Done in 0s 14ms
➤ YN0000: · Yarn 4.18.1
➤ YN0000: ┌ Resolution step
➤ YN0085: │ + @acme/gadgets@npm:1.0.0, @acme/widgets@npm:1.1.0
➤ YN0000: └ Completed
➤ YN0000: ┌ Fetch step
➤ YN0013: │ 2 packages were added to the project (+ 1.86 KiB).
➤ YN0000: └ Completed
➤ YN0000: ┌ Link step
➤ YN0000: └ Completed
➤ YN0000: · Done in 0s 57ms
widgets 1.1.0 gadgets 1.0.0
```

`yarn npm login` writes the token into `~/.yarnrc.yml`, under the registry's entry:

```yaml
npmRegistries:
  "http://localhost:8080/npm/internal":
    npmAlwaysAuth: true
    npmAuthToken: ljs_pat_…
```

A build machine puts `npmAuthToken: "${NPM_TOKEN}"` there instead, with a deploy token. yarn's
dist-tags and its publish, which wants the project installed first:

```sh
yarn npm tag add @acme/gadgets@1.0.0 stable
yarn npm tag list @acme/gadgets
yarn npm tag remove @acme/gadgets stable
mkdir /gizmos && cd /gizmos && npm init -y --scope=@acme >/dev/null
echo 'module.exports = () => "gizmos 1.0.0";' > index.js
yarn install && yarn npm publish
```

```text
➤ YN0000: Tag stable added to version 1.0.0 of package @acme/gadgets
├─ @acme/gadgets@latest → 1.0.0
└─ @acme/gadgets@stable → 1.0.0
➤ YN0000: Tag stable removed from package @acme/gadgets
…
➤ YN0000: Publishing to http://localhost:8080/npm/internal with tag latest
➤ YN0000: Package archive published
```

yarn has no `deprecate` and no `unpublish`: npm does both against the same repo.

## 9. A proxy of npmjs.org

A proxy repo serves an upstream registry and keeps every tarball it fetched as an artifact in the
registry, so that installs keep working when the upstream is slow or gone. The upstream belongs
to the operator, in the configuration: copy the example to `deploy/compose/lajstrom.yaml`, which
`make run` prefers, and replace its `upstreams: []` with:

```yaml
upstreams:
  - name: npmjs
    format: npm
    url: "https://registry.npmjs.org/"
```

Restart `make run`, then on the host:

```sh
cfg=deploy/compose/lajstrom.yaml
bin/lajstrom admin repo create npm/npmjs --registry local --kind proxy --upstream '{"name": "npmjs", "deny": ["@acme/*"]}' --config $cfg
bin/lajstrom admin binding create --principal user:alice --role reader --mask npm/npmjs --config $cfg
```

```json
{"format":"npm","name":"npmjs","registry":"local","kind":"proxy","anonymousRead":false,"policy":null,"upstream":{"name":"npmjs","metadataTTL":"10m","negativeTTL":"2m","offline":false,"allow":[],"deny":["@acme/*"]},"createdAt":"2026-11-10T17:42:47.292804Z","updatedAt":"2026-11-10T17:42:47.292804Z"}
{"id":3,"principal":"user:01a1c68d-f8e4-7d05-8285-172d039ddc0b","name":"alice","role":"reader","repoMask":"npm/npmjs","createdAt":"2026-11-10T17:42:47.351797Z"}
```

`deny: ["@acme/*"]` keeps the proxy from ever asking npmjs.org for your own scope, so that a
public package of the same name cannot stand in for a private one. In alice's container, the
proxy becomes the registry and `@acme` keeps the hosted repo; one `npm login` per repo URL:

```sh
npm config set registry http://localhost:8080/npm/npmjs/
npm login
npm login --scope=@acme --registry=http://localhost:8080/npm/internal/
cat ~/.npmrc
mkdir /app3 && cd /app3 && npm init -y >/dev/null
npm install --cache /tmp/empty-cache-3 chalk@4.1.2 @acme/widgets
npm ls --all
```

```text
Logged in on http://localhost:8080/npm/npmjs/.
Logged in to scope @acme on http://localhost:8080/npm/internal/.
registry=http://localhost:8080/npm/npmjs/
//localhost:8080/npm/npmjs/:_authToken=ljs_pat_…
//localhost:8080/npm/internal/:_authToken=ljs_pat_…
@acme:registry=http://localhost:8080/npm/internal/

added 7 packages, and audited 8 packages in 586ms

2 packages are looking for funding
  run `npm fund` for details

found 0 vulnerabilities
app3@1.0.0 /app3
+-- @acme/widgets@1.1.0
`-- chalk@4.1.2
  +-- ansi-styles@4.3.0
  | `-- color-convert@2.0.1
  |   `-- color-name@1.1.4
  `-- supports-color@7.2.0
    `-- has-flag@4.0.0
```

On the first request for a tarball, the proxy downloads it, checks it against the `integrity`
of the upstream's packument, and publishes it to the registry through the same path as a
`npm publish`, with the audit row `proxy.fetch`; the client gets it from the registry. Every
`dist.tarball` names Lajstrom, never npmjs.org. The next install of the same version, from any
machine, is served from the registry; two replicas that miss the same tarball at once download
it once. The packuments are cached in the index database for `metadataTTL` (10 minutes), then
revalidated; when npmjs.org does not answer, the cached packument is used however old it is.

The denied scope:

```sh
npm view @acme/widgets --@acme:registry=http://localhost:8080/npm/npmjs/
```

```text
npm error code E403
npm error 403 403 Forbidden - GET http://localhost:8080/npm/npmjs/@acme%2fwidgets - format: forbidden: npm/npmjs does not mirror "@acme/widgets": deny: @acme/*
npm error 403 In most cases, you or one of your dependencies are requesting a package version that is forbidden by your security policy, or on a server you do not have access to.
```

The repo's `upstream` settings change with `PATCH /api/v1/repos/npm/npmjs` ([api.md](../api.md#change-patch-apiv1reposformatname)):
`metadataTTL`, `negativeTTL` (2 minutes for a name npmjs.org does not have), `offline` (never ask
the upstream, serve what is held), `allow` and `deny`; `DELETE
/api/v1/repos/npm/npmjs/upstream-cache` drops the cached packuments
([api.md](../api.md#purge-a-proxys-cache-delete-apiv1reposformatnameupstream-cachenamepackage)).
The upstream's URL and credentials (`auth.tokenFile`, or `username` and `passwordFile`) are only
in the configuration ([configuration.md](../configuration.md#upstreams)).

`npm audit`, and the audit that `npm install` runs, are forwarded to npmjs.org: the proxy sends
the request on with its own credentials for the upstream, never the client's, and answers what
npmjs.org answers ([http-routing.md](../spec/http-routing.md), npm, Audit). The answer is not
cached.

```sh
npm audit
```

```text
found 0 vulnerabilities
```

npm sends the name and version of every package of the tree to the URL of `registry=`, those of
other registries too, here `@acme/widgets`. The proxy leaves out the names its `deny` refuses,
which never reach npmjs.org, and asks nothing when no name is left. pnpm's audit sends npm 6's
dependency tree, which cannot lose a package without what hangs below it: through this proxy it
is `501` when the tree names an `@acme` package.

## 10. A group: one URL for both

A group repo serves the hosted repo and the proxy under one URL, one `registry=` line for
everything ([proxy-and-group.md](../spec/proxy-and-group.md) §8). It holds nothing of its own:
it reads its members in order, and a name the hosted member holds hides every version the
upstream has of that name, so a public package that takes the name of a private one is never
installed in its place. On the host:

```sh
bin/lajstrom admin repo create npm/all --kind group --group '{"members": ["internal", "npmjs"]}' --config $cfg
bin/lajstrom admin binding create --principal user:alice --role reader --mask npm/all --config $cfg
```

```json
{"format":"npm","name":"all","kind":"group","anonymousRead":false,"group":{"members":["internal","npmjs"],"merge":[]},"createdAt":"2026-11-10T17:43:51.774870Z","updatedAt":"2026-11-10T17:43:51.774870Z"}
{"id":4,"principal":"user:01a1c68d-f8e4-7d05-8285-172d039ddc0b","name":"alice","role":"reader","repoMask":"npm/all","createdAt":"2026-11-10T17:43:51.877126Z"}
```

`read` on the group is all a reader needs: through it, alice reads both members whatever her
bindings on them. Writes through the group go to its hosted member and need the permission
there: alice publishes to `npm/all` because she is a `writer` on `npm/internal`. Only the admin
role chooses a group's members ([rbac.md](../spec/rbac.md) §7). In alice's container, the group
replaces both lines of section 9:

```sh
npm config set registry http://localhost:8080/npm/all/
npm config delete @acme:registry
npm login
cat ~/.npmrc
mkdir /app4 && cd /app4 && npm init -y >/dev/null
npm install --cache /tmp/empty-cache-4 chalk@4.1.2 @acme/widgets
npm ls --all
npm view @acme/widgets dist-tags versions
npm view chalk@4.1.2 dist.tarball
```

```text
npm notice Log in on http://localhost:8080/npm/all/
Username: alice
Password:
Logged in on http://localhost:8080/npm/all/.
registry=http://localhost:8080/npm/all/
//localhost:8080/npm/npmjs/:_authToken=ljs_pat_…
//localhost:8080/npm/internal/:_authToken=ljs_pat_…
//localhost:8080/npm/all/:_authToken=ljs_pat_…

added 7 packages, and audited 8 packages in 506ms

2 packages are looking for funding
  run `npm fund` for details

found 0 vulnerabilities
app4@1.0.0 /app4
+-- @acme/widgets@1.1.0
`-- chalk@4.1.2
  +-- ansi-styles@4.3.0
  | `-- color-convert@2.0.1
  |   `-- color-name@1.1.4
  `-- supports-color@7.2.0
    `-- has-flag@4.0.0
dist-tags = { latest: '1.1.0' }
versions = [ '1.0.0', '1.1.0' ]
http://localhost:8080/npm/all/chalk/-/chalk-4.1.2.tgz
```

Every link names the group; a tarball comes from the member that holds it, and the proxy
fetches through the group what it does not hold yet, its audit row `proxy.fetch` with
`"via": "npm/all"`. The packument of a name is merged from the members that have it, the hosted
member's versions and dist-tags first; nothing is cached for the group itself, so a publish to
the hosted repo is seen at once. `npm audit` through the group goes to npmjs.org without the
names the hosted repo holds, which never leave the server. pnpm's audit is `501` through a
group: it sends npm 6's dependency tree, and a private package cannot be cut out of a tree
without the packages below it.

A publish, `npm dist-tag`, `npm deprecate` and `npm unpublish` through the group are the hosted
repo's:

```sh
cd /widgets
npm version 1.2.0
echo 'module.exports = () => "widgets 1.2.0";' > index.js
npm publish
npm view @acme/widgets dist-tags --registry http://localhost:8080/npm/internal/
```

```text
v1.2.0
npm notice package: @acme/widgets@1.2.0
…
npm notice Publishing to http://localhost:8080/npm/all/ with tag latest and default access
+ @acme/widgets@1.2.0
{ latest: '1.2.0' }
```

The audit rows are `npm/internal`'s, `push` and `npm.dist-tag`, with `"via": "npm/all"`. A group
without a hosted member answers a write `405`. The hosted name hides the upstream only while the
hosted repo has it: before the first publish of a name, the group asks the upstream for it, so
`deny` on the proxy (section 9) is what keeps a private scope from ever being looked up there.
`group.merge` lists names whose versions are merged across members even when the hosted repo
holds them ([api.md](../api.md#the-resource)).

## 11. In the registry

Each version is a manifest under `npm/internal/<scope>/<name>`, tagged with the version; each
change of a dist-tag or a deprecation is a manifest tagged `_j<seq>`, eight digits, which no
version can be:

```sh
crane ls localhost:5000/npm/internal/acme/widgets
crane manifest localhost:5000/npm/internal/acme/widgets:_j00000003 | jq .annotations
crane ls localhost:5000/npm/npmjs/chalk
```

```text
1.0.0
1.1.0
1.2.0
_j00000001
_j00000002
_j00000003
_j00000004
_j00000005
_j00000006
_j00000007
_j00000008
_j00000009
```

```json
{
  "lajstrom.format": "npm",
  "lajstrom.name": "@acme/widgets",
  "lajstrom.npm.op": "add",
  "lajstrom.npm.seq": "3",
  "lajstrom.npm.tag": "beta",
  "lajstrom.npm.version": "2.0.0-beta.0",
  "lajstrom.repo": "internal",
  "lajstrom.schema": "1",
  "lajstrom.uploaded-by": "user:01a1c68d-f8e4-7d05-8285-172d039ddc0b",
  "org.opencontainers.image.created": "2026-11-10T17:42:36Z"
}
```

```text
4.1.2
```

No tag is ever moved: the current dist-tags are the journal read in order, and an entry stays
when the version it named is unpublished. `bin/lajstrom reindex --repo npm/internal --config $cfg`
rebuilds the index of the repo from these manifests, the dist-tags and deprecations included
([operations.md](../operations.md#reindex)).

## 12. Clean up

In alice's container, as maintainer, through the group:

```sh
npm unpublish @acme/widgets --force
npm unpublish @acme/gadgets --force
npm unpublish @acme/gizmos --force
```

```text
npm warn using --force Recommended protections disabled.
- @acme/widgets
npm warn using --force Recommended protections disabled.
- @acme/gadgets
npm warn using --force Recommended protections disabled.
- @acme/gizmos
```

and on the host, `rm npm-token`. What the proxy fetched stays in the registry; its versions are
deleted as a hosted repo's are, with `delete` on the proxy repo
(`DELETE /api/v1/repos/npm/npmjs/packages/<name>/versions/<version>`).

## Coming from Verdaccio

What a Verdaccio `config.yaml` says, and where it goes:

| Verdaccio | Lajstrom |
| --- | --- |
| `storage: ./storage` | the OCI registry of the repo (`registries[]`, `repos[].registry`): nothing is kept on a disk of Lajstrom's. The index (SQLite or Postgres) is derived from the registry, and `lajstrom reindex` rebuilds it |
| `uplinks: {npmjs: {url: https://registry.npmjs.org/}}` | an entry of `upstreams[]` in the configuration, and a proxy repo that names it (`kind: proxy`, `upstream.name`): section 9 |
| `uplinks.*.maxage` | the proxy repo's `upstream.metadataTTL` (10 minutes) |
| `uplinks.*.timeout`, `max_fails`, `fail_timeout` | `upstreams[].timeout` and `retries`; an upstream that fails leaves the cached packuments in use, however old |
| `uplinks.*.auth` | `upstreams[].auth.tokenFile`, or `username` and `passwordFile` |
| `uplinks.*.cache: false` | none: a proxy keeps every tarball it serves |
| `packages: {'@acme/*': {access, publish, unpublish}}` | bindings on a repo: `reader` installs, `writer` also publishes and moves dist-tags and deprecations, `maintainer` also unpublishes. A binding names a repo, not a pattern of package names: a scope that needs other people gets a repo of its own |
| `$all`, `$anonymous` | the repo's `anonymousRead`, with `auth.anonymousRead: true` in the configuration |
| `$authenticated` | none: each user gets a binding |
| `packages: {'**': {proxy: npmjs}}` | a group repo of the hosted repo and the proxy, one URL for both (section 10); unlike Verdaccio, a name the hosted repo holds hides the upstream's versions of it |
| a private scope without `proxy` | `deny: ["@acme/*"]` on the proxy: it never asks the upstream for those names |
| `auth: {htpasswd: {file, max_users}}` | local users made by an admin, `lajstrom admin user` or `/api/v1/users`; `npm adduser` never creates one, as `max_users: -1` |
| `security.api.jwt` | personal access tokens, which `npm login` makes, and deploy tokens of a repo for machines: listed, narrowed and revoked through `lajstrom admin token` and the API |
| `listen`, `url_prefix`, `VERDACCIO_PUBLIC_URL` | `server.listen`; `server.externalURL`, the base of every `dist.tarball` |
| `max_body_size` | the repo's `policy.maxFileSize` for the tarball; the rest of a publish is at most 4 MiB |
| `logs` | `log.level` and `log.format`; every change is also a row of the audit log, `/api/v1/audit` |

Not there:

- a web UI (a separate one is planned, [lajstrom-ui](https://github.com/lajstrom/lajstrom-ui)),
  and npm's web login: npm falls back to the password prompt by itself;
- self-registration;
- permissions per package name inside a repo;
- `npm token`, `access`, `owner`, `team`, `star` and `profile`: `404`;
- `npm publish --provenance`: refused;
- `pnpm audit` through a group: `501`, and through a proxy when it names a denied package;
- `notify` webhooks, and plugins of any kind: the protocols are Lajstrom's own;
- an import of Verdaccio's `storage`: publish the tarballs to the hosted repo again.

## The end-to-end script

`test/e2e/npm.sh` runs sections 2 to 11 with checks, in `node:26.13.0-slim` with pnpm 10.34.9
and yarn 4.18.1, against repos, a user and deploy tokens it makes through the API: `npm login`,
`whoami`, `publish` of an unscoped and a scoped package, `install` from an empty cache,
`dist-tag add`, `ls` and `rm`, `deprecate`; pnpm with the deploy token as `_authToken`; `yarn npm
login`, `publish`, `tag` and `add`; `npm install chalk@4.1.2` twice through a proxy repo, each
tarball fetched once; a group of both, through which npm, pnpm and yarn each publish a scoped
package that depends on `chalk@4.1.2` and install it from an empty cache, a public name in the
hosted repo hiding the upstream's versions, and `group.merge`; `npm unpublish` refused to a
writer, then done by a maintainer; the hosted repo deleted, made again and reindexed, every
dist-tag and deprecation back; `npm logout`. It is the demo of M4 ([roadmap.md](../roadmap.md)).
It needs a server whose configuration has the upstream `npmjs`, as `make e2e` writes one
([testing.md](../testing.md#end-to-end)):

```sh
make e2e E2E=npm
```

By hand against `make run`, it runs as an admin whose name and password are in
`LAJSTROM_E2E_USER` and `LAJSTROM_E2E_PASSWORD`, and `NPM_E2E_UPSTREAM=` (empty) leaves the proxy
out when the configuration has no upstream, as the example has none; the group then has the
hosted repo only:

```sh
LAJSTROM_E2E_USER=admin LAJSTROM_E2E_PASSWORD=… NPM_E2E_UPSTREAM= test/e2e/npm.sh http://localhost:8080
```

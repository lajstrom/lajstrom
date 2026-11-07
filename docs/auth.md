# Authentication

Who a request comes from, and how Lajstrom finds out. This page describes local users, their
passwords, HTTP Basic on every route, the bearer tokens of the OCI facade, the bootstrap of the
first admin and `whoami`. What a principal may do is RBAC's ([rbac.md](spec/rbac.md), #45);
until then every route is open, and authentication only tells who is asking. The model is
[ADR 0007](adr/0007-rbac-model.md); where the token signing key lives is
[ADR 0010](adr/0010-bearer-token-signing-key.md).

## Principals

A principal is who performs a request. Its canonical form names it in role bindings, in the
audit log, in publication sessions and in the access log ([rbac.md](spec/rbac.md) §1):

| Principal | Form | From |
| --- | --- | --- |
| local user | `user:<name>` | HTTP Basic with the user's password (this page) |
| deploy token | `token:<id>` | HTTP Basic `deploy:<token>` (#44) |
| anonymous | `anonymous` | a request without credentials |

A personal access token acts as its user (#44); OIDC users and groups come after the MVP.

There are no users in the configuration file, by decision: users live in the index database
with the rest of the control plane, are created with `lajstrom admin user` (and with the users
API of #47), and are the same on every replica.

## Local users and passwords

A user name is 1 to 64 bytes of lowercase letters, digits, `.`, `_` and `-`, starting with a
letter or a digit. It is what HTTP Basic carries before the colon and what `user:<name>` binds,
so it has no colon, no space and no upper case. `deploy` is reserved: `deploy:<token>` is how a
deploy token authenticates (#44). A password is 8 to 1024 bytes of UTF-8, any characters.

Passwords are stored as argon2id hashes in `users.password_hash`
([index-schema.md](spec/index-schema.md)), in the PHC string format:

```text
$argon2id$v=19$m=19456,t=2,p=1$<16-byte salt>$<32-byte key>
```

The parameters are the first recommendation of the OWASP Password Storage Cheat Sheet: 19 MiB
of memory, 2 passes, 1 thread. One verification takes about 17 ms on an Apple M4 Pro and
reserves 19 MiB while it runs. Each hash carries its parameters, so they can change in a later
version and the hashes made before still verify; the comparison runs in constant time. The
password and the hash are never logged, printed or answered.

### The verification cache

`apt` and `pip` send their credentials with every request, and argon2id on every request would
cost a CPU core per 60 requests a second. So a verified password is remembered for 5 minutes,
up to 10 000 verifications per replica, the oldest dropped first. The cache is keyed by a sha256
of the user name and the secret, never the secret itself, and holds the stored hash it was
verified against.

The user is still read from the database on every request with credentials, and a remembered
verification counts only while the user's stored hash is the one it was verified against. So:

- `lajstrom admin user disable` refuses the user's credentials from the next request on, on
  every replica;
- `lajstrom admin user set-password` makes the old password fail at the next request, and the
  new one is verified once and remembered;
- a wrong password is never remembered: each try costs one verification.

A user that does not exist costs one verification too, against a fixed hash, so that "no such
user" and "wrong password" take the same time and the answer does not tell which. Rate limiting
of failed logins comes with #130.

## HTTP Basic on every route

Every route reads the `Authorization` header, the frontends (`/raw/`, `/pypi/`, `/apt/`,
`/maven/`), the OCI facade (`/v2/`), the management API (`/api/`) and the system endpoints
(`/-/healthz`, `/-/readyz`, `/-/version`) alike:

- **No header**: the request goes on as `anonymous`. What anonymous may do is RBAC's to decide;
  in this version everything is open, so the dev stack works without credentials.
- **`Basic`** ([RFC 7617](https://www.rfc-editor.org/rfc/rfc7617); the first colon splits the
  user name from the password): the credentials are checked, and the request goes on as
  `user:<name>`. Credentials that are refused (an unknown user, a disabled one, a wrong
  password) or that do not decode are answered at once with `401`, whatever the route: a client
  that sends credentials learns at once that they are wrong. The challenge is
  `WWW-Authenticate: Basic realm="lajstrom"`, except under `/v2/`, where it is the bearer
  challenge of the token flow (below) and the client learns it at `/v2/token`, which itself
  answers with the Basic challenge.
- **`Bearer`** and any other scheme: the request goes on as anonymous with the header
  untouched. The OCI facade verifies the bearer of `/v2` ([below](#bearer-tokens-on-v2)).

The body of the `401` follows the route:

| Route | Body |
| --- | --- |
| `/v2/` | `{"errors":[{"code":"UNAUTHORIZED","message":"credentials are needed: the ones sent were refused"}]}`, the error format of the distribution spec |
| `/api/` | `{"error":{"code":"FORBIDDEN","message":"credentials are needed: the ones sent were refused"}}`, the envelope of [api.md](api.md) |
| elsewhere | `credentials are needed: the ones sent were refused`, plain text |

The access log line of every request has `principal`: the canonical form, `anonymous` for a
request without credentials and for the `401`s above, the token's subject for a request with
a valid bearer. The `Authorization` header is never logged, and neither is a token.

### The 401/404/403 rule

[rbac.md](spec/rbac.md) §3 decides what a denial answers so that it does not reveal whether a
repo exists: `401` with the challenge for `anonymous` (the bearer challenge under `/v2/`, Basic
elsewhere), `404` for a principal without any access to the repo, `403` for one that can read
the repo but lacks the action. Before RBAC (#45) the Authorizer denies nothing, and the `401`s
are: credentials that are refused, on every route; `whoami` without credentials; `GET /v2/`
without credentials; and, under `/v2/`, a bearer that does not verify or does not name the
repository. A bearer that names the repository without the action is `403`. The rest of the
rule comes with #45.

## Bearer tokens on `/v2`

OCI clients (`docker`, `oras`, `crane`, `helm`) authenticate with the docker token flow
([rbac.md](spec/rbac.md) §4): `GET /v2/` answers `401` with a challenge that names a realm, the
client gets a token from the realm with the credentials it has, or none, and sends the token as
a bearer on every `/v2` request. Lajstrom is its own token server: the realm is its
`/v2/token`, which takes the same HTTP Basic credentials as every other route.

### The flow

From a run against `bin/lajstrom serve` on the example configuration, with `server.listen` and
`server.externalURL` moved to port 18080 and the master key set; the tokens are cut after their
first characters:

```text
$ curl -si http://localhost:18080/v2/
HTTP/1.1 401 Unauthorized
Content-Type: application/json
Docker-Distribution-Api-Version: registry/2.0
Www-Authenticate: Bearer realm="http://localhost:18080/v2/token",service="localhost:18080"

{"errors":[{"code":"UNAUTHORIZED","message":"authentication required: get a token from the realm of the challenge"}]}

$ curl -s -u alice 'http://localhost:18080/v2/token?service=localhost:18080&scope=repository:raw/scratch/hello:pull,push'
{"token":"eyJhbGciOiJFU…","access_token":"eyJhbGciOiJFU…","expires_in":1200,"issued_at":"2026-11-07T21:48:23Z"}

$ jq -R 'split(".") | .[0:2] | map(gsub("-"; "+") | gsub("_"; "/") | @base64d | fromjson)' <<<"$TOKEN"
[
  {
    "alg": "ES256",
    "kid": "4f2155f3dddad8dc8682a11dc282cd36c70306478d4c3bf50c1e5761be4cfc79",
    "typ": "JWT"
  },
  {
    "iss": "localhost:18080",
    "sub": "user:alice",
    "aud": [
      "localhost:18080"
    ],
    "exp": 1794089303,
    "nbf": 1794088103,
    "iat": 1794088103,
    "jti": "pkoI1THcQvFGXuIEYB50xw",
    "access": [
      {
        "type": "repository",
        "name": "raw/scratch/hello",
        "actions": [
          "pull",
          "push"
        ]
      }
    ]
  }
]

$ curl -si -H "Authorization: Bearer $TOKEN" http://localhost:18080/v2/raw/scratch/hello/tags/list
HTTP/1.1 200 OK
Content-Type: application/json
Docker-Distribution-Api-Version: registry/2.0

{"name":"raw/scratch/hello","tags":["7ab1ebd2573ff075","d2c2335ae7f9acbc"]}

$ curl -si -X POST -H "Authorization: Bearer $PULL_ONLY" http://localhost:18080/v2/raw/scratch/hello/blobs/uploads/
HTTP/1.1 403 Forbidden
Content-Type: application/json
Docker-Distribution-Api-Version: registry/2.0

{"errors":[{"code":"DENIED","message":"the bearer token does not grant push on raw/scratch/hello"}]}

$ curl -si -H "Authorization: Bearer $TOKEN" http://localhost:18080/v2/raw/scratch/other/tags/list
HTTP/1.1 401 Unauthorized
Content-Type: application/json
Docker-Distribution-Api-Version: registry/2.0
Www-Authenticate: Bearer realm="http://localhost:18080/v2/token",service="localhost:18080",scope="repository:raw/scratch/other:pull",error="insufficient_scope"

{"errors":[{"code":"UNAUTHORIZED","message":"the bearer token grants nothing on raw/scratch/other: get a token with its scope"}]}

$ curl -si -u alice:wrong http://localhost:18080/v2/token
HTTP/1.1 401 Unauthorized
Content-Type: application/json
Www-Authenticate: Basic realm="lajstrom"

{"errors":[{"code":"UNAUTHORIZED","message":"credentials are needed: the ones sent were refused"}]}
```

`$PULL_ONLY` is a token asked for `scope=repository:raw/scratch/hello:pull`.

### The token endpoint

`GET /v2/token?service=<service>&scope=<scope>&scope=…` issues a token for the principal of
the request: the user of the Basic credentials (refused ones are `401` with the Basic
challenge, from the authentication middleware), or `anonymous` without credentials. The
`user:<PAT>` and `deploy:<token>` credentials of #44 come the same way. Only `GET`: the OAuth2
`POST` form is not implemented (`405`), and the clients above use `GET`.

- **Scopes**: each `scope` parameter is `repository:<format>/<repo>/<path>:<actions>`, several
  parameters or several scopes in one separated by spaces. The actions are a comma list of
  `pull`, `push` and `delete`, `*` meaning the three; any other action is ignored. A scope of
  another type, a name that is not `<format>/<repo>/<path>` of an existing repo, and a scope
  with no action left are dropped, as the distribution token flow expects; a repository asked
  for twice gets the union. One scope of another type is kept: `registry:catalog:*`, which
  `crane catalog` and `oras repo ls` ask for to list `/v2/_catalog`.
- **Reduction**: for each scope, each action is kept when the Authorizer allows it to the
  principal on `<format>/<repo>`: `pull` is `read`, `push` is `write`, `delete` is `delete`
  ([rbac.md](spec/rbac.md) §2). Without an Authorizer, the state until RBAC (#45), every
  requested action is granted, to the anonymous principal too: the facade is open, and stays
  open. `serve` says so at startup, at warn:

  ```text
  level=WARN msg="authorization is not enforced until RBAC (#45): every principal, anonymous included, may do everything on /v2 and the frontends, and /v2/token therefore grants every requested scope"
  ```

  With #45, a reader asking for `pull,push` gets `pull` (O-12), and a principal that may not
  read a repo gets no scope for it.
- **Service**: the configured one (below). A `service` parameter that names another is a
  client talking to another registry, not an error: it is ignored, and the token carries the
  configured service.
- **Answer**: `200` with `{"token": "<jwt>", "access_token": "<jwt>", "expires_in": 1200,
  "issued_at": "<RFC 3339>"}` and `Cache-Control: no-store`. The access log line has the
  principal. No audit row is written for a token: one per `docker pull` would be noise, and the
  pushes and deletes the token allows are audited themselves.
- **Rate limiting**: none yet. #130 throttles the token endpoint per client address, with the
  failed logins per account; it belongs in the server's middleware in front of the
  authentication of the Basic credentials, so that a flood of token requests costs no argon2id
  work.

### What a token says

| Part | Value |
| --- | --- |
| header `alg` | the algorithm of the signing key: `ES256` (ECDSA P-256, the default) or `EdDSA` (Ed25519), `keys.tokenAlgorithm` |
| header `kid` | the fingerprint of the signing key: the lowercase hexadecimal sha256 of its public key (PKIX DER) |
| header `typ` | `JWT` |
| `iss`, `aud` | the service |
| `sub` | the principal: `user:alice`, `anonymous`, later `token:<id>` |
| `iat`, `nbf` | the time of issue, to the second |
| `exp` | `iat` plus 20 minutes |
| `jti` | 16 random bytes, base64url |
| `access` | `[{"type": "repository", "name": "<format>/<repo>/<path>", "actions": [...]}]`, sorted by type and name |

The realm and the service come from `server.externalURL` when it is set: the realm is
`<externalURL>/v2/token`, the service its host, with the port when it has one
(`https://art.example.com` gives `art.example.com`). Without it they come from the request's
`Host` over `http://`, which is what the dev stack runs; production sets `server.externalURL`.
Replicas behind one edge share one `Host` anyway, so a token one of them issues is for the
service of the others (H-12).

### The bearer on `/v2`

Every `/v2` request but `/v2/token` may carry `Authorization: Bearer <jwt>`. The facade looks
up the key its `kid` names and accepts the algorithm of that key and nothing else, never
`none`, never HMAC; then the signature, `exp` (required), `nbf` and `iat` with 60 seconds of
clock skew, `iss` and `aud` against the service, and `sub`, which must be a principal. A token
that fails any of them is `401` with the challenge; the reason is logged at debug, with the
`kid` and never the token, and is not answered.

A request with a valid bearer runs as the token's principal, in the request and in the access
log, and is decided by the token's `access`, not by the Authorizer:

- a repository the token does not name is `401` with the challenge, its `scope` and
  `error="insufficient_scope"`: the client gets a token with that scope and retries. A
  cross-repo mount names both repositories (`scope="repository:<target>:pull,push
  repository:<from>:pull"`), which is how docker mounts from another repository;
- an action the token does not grant on a repository it names is `403 DENIED`: the token
  endpoint already refused it, and a wider token would not help (O-12);
- `GET /v2/` is `200 {}` with any valid bearer, as with Basic credentials, and `401` with the
  challenge to the anonymous principal;
- `GET /v2/_catalog` lists, with `registry:catalog:*`, the repositories the principal may read,
  and otherwise the repositories the token grants `pull` on.

A request without a bearer runs as before: as the Basic user or anonymous, decided by the
Authorizer (none until #45: everything allowed), except that its `401`s carry the bearer
challenge, with the scope the request needed: `pull` for `GET` and `HEAD`, `pull,push` for the
uploads and a manifest `PUT`, `delete` for `DELETE`.

### The signing key

One key signs the tokens of every replica ([ADR 0010](adr/0010-bearer-token-signing-key.md)): a
row of `keys` with the kind `jwt` and no repo ([index-schema.md](spec/index-schema.md)), its
private key encrypted with the master key as the apt keys are, its fingerprint the `kid`.
`serve` loads it before it listens, or makes it when there is none, and logs it:

```text
level=INFO msg="bearer tokens are signed with the key in the database" fingerprint=34caa8a7856565fad3b7d97dae05d51b0dace8adadc744f31de8d9162a2bdbf5 algorithm=ES256 keys.masterKeyEnv=LAJSTROM_MASTER_KEY
```

A key that does not decrypt (a database restored under another master key) stops `serve` with
an error that names it: start with the right master key, or retire the key with
`lajstrom admin key drop --token` and the next start makes a new one. Each replica keeps the
verification keys in memory and reloads them at most once a second, so a key rotated or dropped
on another node counts everywhere within a second; for that second a replica may refuse a token
of the new key, which the client gets again. Rotation, drop and export are in
[operations.md](operations.md#the-bearer-token-key).

Without a master key (`keys.masterKeyEnv` empty, or the variable not set) `serve` signs with an
ephemeral key in memory and says so at startup:

```text
level=WARN msg="bearer tokens are signed with an ephemeral key: they do not survive a restart and are not shared between replicas; set keys.masterKeyEnv"
```

That is enough for one process, the dev stack and the quick starts, which need no new variable;
a client whose token the next start refuses gets another.

### What a token does not say

A token is trusted for its lifetime, as the distribution token flow expects. A user disabled or
a binding removed takes effect at once on the frontends and at the next `/v2/token`, and within
20 minutes on `/v2` for a token already issued (the consequence in ADR 0010). The token names
repositories by name, so a repo deleted is unknown at once (`NAME_UNKNOWN`). An operator who
needs every outstanding token gone now runs `lajstrom admin key rotate --token --drop`: within a
second no replica accepts a token of the old key, and clients get new tokens with their
credentials. Whether the facade should instead ask the Authorizer on every `/v2` request is
#45's decision.

### Clients

`oras login` and `docker login` work as with any registry; on the dev stack, which is plain
HTTP on loopback, `oras` needs `--plain-http` and docker treats `localhost` as an insecure
registry by itself:

```sh
oras login --plain-http localhost:8080 -u alice
docker login localhost:8080 -u alice
```

`oras` and docker get a token when the facade challenges them: at `login`, which asks
`GET /v2/`, and on any `401`. While the facade is open (#45) their pushes and pulls are not
challenged and go without a token, as anonymous; nothing changes for them on the dev stack.

`test/e2e/oci-token.sh` runs the flow end to end (the challenge, a token, its claims, a changed
token, a token without push, `oras login`, push and pull, a rotation and a drop):

```text
ok   GET /v2/: 401 WWW-Authenticate: Bearer realm="http://localhost:18080/v2/token",service="localhost:18080"
ok   admin user create --name e2e-token-20261107214723-22564 --admin --password-stdin
ok   GET /v2/token -u e2e-token-20261107214723-22564 scope=repository:raw/scratch/hello:pull,push: 200, expires_in 1200, issued_at 2026-11-07T21:47:23Z
ok   claims: {"header":{"alg":"ES256","kid":"7510eb6690413c0dfca82391e00b126fd21cb737d5e9f3f2034a0c3f63dfd402","typ":"JWT"},"payload":{"iss":"localhost:18080","sub":"user:e2e-token-20261107214723-22564","aud":["localhost:18080"],"exp":1794089243,"nbf":1794088043,"iat":1794088043,"jti":"6F-DwcnnDZoyn-3VoptjIQ","access":[{"type":"repository","name":"raw/scratch/hello","actions":["pull","push"]}]}}
ok   GET /v2/raw/scratch/hello/tags/list with the token: 200
ok   the token with one byte changed: 401, WWW-Authenticate: Bearer realm="http://localhost:18080/v2/token",service="localhost:18080",scope="repository:raw/scratch/hello:pull"
ok   POST /v2/raw/scratch/hello/blobs/uploads/ with a pull-only token: 403 DENIED: the bearer token does not grant push on raw/scratch/hello
ok   oras login localhost:18080 -u e2e-token-20261107214723-22564: Login Succeeded
ok   oras push localhost:18080/raw/scratch/hello:d2c2335ae7f9acbc: Digest: sha256:824822b5759552cf0c8756ad414915c0bf7836d5b461ad680d720a5c62d7f451
ok   oras pull localhost:18080/raw/scratch/hello:d2c2335ae7f9acbc: the same bytes
ok   admin key rotate --token: 5ebc303f7ee99e4833c1c959f0e8ba3bbcd5c164d64370eff0cc3600e89e5372; the old token (kid 7510eb6690413c0d…) still 200; a new token has kid 5ebc303f7ee99e48…
ok   admin key rotate --token --drop: 4f2155f3dddad8dc8682a11dc282cd36c70306478d4c3bf50c1e5761be4cfc79; the token of 5ebc303f7ee99e48… is 401, a new one 200
PASS oci-token e2e against http://localhost:18080 as e2e-token-20261107214723-22564
```

## Bootstrap: the first admin

The first user is created from the command line, without HTTP, against the database of the
configuration ([rbac.md](spec/rbac.md) §6). Run `lajstrom migrate` first, or start `serve` once:

```sh
LAJSTROM_ADMIN_PASSWORD='…' lajstrom admin user create --name admin --admin --config lajstrom.yaml
```

```json
{"name":"admin","kind":"local","disabled":false,"createdAt":"2026-11-07T12:00:00Z"}
```

`--admin` also creates the binding `(user:admin, admin, */*)`: everything on every repo, the
system and the audit log. The binding is evaluated by RBAC (#45); the row is there from the
start. The password comes from, in this order:

1. `LAJSTROM_ADMIN_PASSWORD`, when it is set;
2. standard input with `--password-stdin`, the whole of it without the trailing newline
   (`printf '%s\n' "$PASSWORD" | lajstrom admin user create --name ci --password-stdin`);
3. a prompt without echo, twice, when standard input is a terminal.

With none of them the command fails and names the three. The other commands:

```sh
lajstrom admin user list                    # NAME, KIND, DISABLED, CREATED
lajstrom admin user disable alice           # refused from the next request on
lajstrom admin user enable alice
lajstrom admin user set-password alice      # the same three password sources
```

A name that exists is refused (`user alice already exists`, exit status 1). Every change is an
audit event ([rbac.md](spec/rbac.md) §5) with the user as its subject: `user.create` (its
details say whether the user is an admin), `user.password`, `user.disable`, `user.enable`, by
`anonymous` with the source `cli`. `disable` of a disabled user and `enable` of an enabled one
change nothing and write no event. Everything after the bootstrap goes through `/api/v1` once
#47 brings the users API.

## whoami

`GET /api/v1/whoami` answers the principal of the request. Every principal may ask who it is;
no authorizer is consulted.

```sh
curl -s -u admin http://localhost:8080/api/v1/whoami
```

```json
{"principal":"user:admin","kind":"user","name":"admin"}
```

Without credentials it is `401` with the Basic challenge and
`{"error":{"code":"FORBIDDEN","message":"credentials are needed"}}`.

## What comes next

| Issue | What |
| --- | --- |
| #44 | Personal access tokens (`user:<PAT>`, acting as the user) and deploy tokens (`deploy:<token>`, the principal `token:<id>`), in the same Basic credentials |
| #45 | RBAC: the bindings decide what each principal may do, anonymous gets `401` with the challenge where it may not read, and the rule above holds everywhere |
| #47 | The users API: `GET/POST /api/v1/users`, `PATCH`, `POST …/password`, with the same rules and audit events as `lajstrom admin user` |
| #130 | Rate limiting of failed logins |

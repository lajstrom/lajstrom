# Authentication and authorization

Who a request comes from, how Lajstrom finds out, and what the principal may do. This page
describes local users, their passwords, HTTP Basic on every route, the roles and bindings that
decide every request ([Authorization](#authorization)), the bearer tokens of the OCI facade,
personal access tokens and deploy tokens, the bootstrap of the first admin and `whoami`. The
normative rules are [rbac.md](spec/rbac.md); the model is [ADR 0007](adr/0007-rbac-model.md);
where the token signing key lives is [ADR 0010](adr/0010-bearer-token-signing-key.md).

## Principals

A principal is who performs a request. Its canonical form names it in role bindings, in the
audit log, in publication sessions and in the access log ([rbac.md](spec/rbac.md) §1):

| Principal | Form | From |
| --- | --- | --- |
| local user | `user:<uid>` | HTTP Basic with the user's password; HTTP Basic or a bearer with a personal access token of the user ([Tokens](#tokens)) |
| deploy token | `token:<id>` | HTTP Basic `deploy:<token>` ([Tokens](#tokens)) |
| anonymous | `anonymous` | a request without credentials |

A personal access token acts as its user: its principal is the user's, narrowed by the token's
scopes. A user may come from an OIDC login, and its groups from the login's claims
([OIDC login and groups](#oidc-login-and-groups)).

There are no users in the configuration file, by decision: users live in the index database
with the rest of the control plane, are created with `lajstrom admin user` and with the users
API ([api.md](api.md#users)), and are the same on every replica.

## Local users and passwords

A user name is 1 to 64 bytes of lowercase letters, digits, `.`, `_` and `-`, starting with a
letter or a digit. It is what HTTP Basic carries before the colon, so it has no colon, no space and no upper case. `deploy` is reserved: `deploy:<token>` is how a
deploy token authenticates. A password is 8 to 1024 bytes of UTF-8, any characters, that does
not start with `ljs_pat_` or `ljs_dep_`: a secret with either prefix is a token's, never a
password (a password set before #44 that starts with one no longer logs in; set another).

### The uid and the name

A user is its **uid**, a UUID v7 made when the user is created and never changed
([rbac.md](spec/rbac.md) §1): the principal is `user:<uid>`, and bindings, sessions, tokens and
`/v2` tokens hold the uid. The name is what a person types: HTTP Basic, the CLI and
`--principal user:<name>` take it and Lajstrom looks the uid up. A name has the form of a uid
never, so `user:<s>` is unambiguous.

- `lajstrom admin user rename <name> <new name>` changes the name only: the user keeps its
  uid, its bindings and its tokens; the old name logs in no more from the next request.
- `lajstrom admin user delete <name>` revokes the user's tokens, deletes its bindings and keeps
  the row, disabled, under the name `~<uid>`, so that the audit log still names it; `admin user
  list` and `GET /api/v1/users` show the live users only. The name is free again at once; a
  user created with it gets another uid and none of the old rights.
- `delete` and `disable` refuse the last enabled user with an admin binding (exit status 1;
  `409` over the API), so that somebody can still manage the server.
- `admin user list` shows NAME, UID, KIND, DISABLED, CREATED; `admin binding list` shows the
  principal with its USER name. `GET /api/v1/whoami` answers both: `{"principal": "user:<uid>",
  "kind": "user", "name": "<name>", "uid": "<uid>"}`.

A `/v2` token issued before #182, whose `sub` is `user:<name>`, is refused with 401; the client
logs in again and gets one with the uid.

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
user" and "wrong password" take the same time and the answer does not tell which. A failed
verification counts against the account ([Failed logins](#failed-logins)).

### Failed logins

A refused password is a guess, and costs a replica one argon2id verification, about 17 ms of a
core. So each failed verification counts against its account for a window of one minute, which
opens with the first failure: a wrong password, and a user that is unknown, disabled or has no
password, which cost the same verification. With 10 failures in its window, an account's
passwords are no longer checked until the window closes: every request with Basic credentials
that carry a password for it is answered `429 Too Many Requests` with `Retry-After`, the seconds
left in the window, and no challenge. A correct password is answered `429` too, so the answer
tells nothing about the password; after the window it works, and the count starts over.

- **The account** is the user's uid: a rename keeps the count. A name that no user has is
  counted by the name, the same way, so that the throttle does not tell whether a user exists:
  its `429` comes after the same 10 failures, for the same time, with the same body, and costs
  no verification either.
- **A throttle, not a lockout.** Nothing needs unlocking: it lifts by itself when the window
  closes. Someone who keeps sending wrong passwords for an account keeps its passwords
  answered `429` for as long as they do, a minute at a time; they get no more than 10 guesses a
  minute per replica, and never lock the user out of the tokens.
- **Tokens are not throttled.** A personal access token or a deploy token is 256 random bits,
  checked by a lookup without argon2id, and cannot be guessed; it works while its user's
  password is under throttle, which is one more reason for CI to use one ([In CI](#in-ci)).
- **A remembered verification** ([the cache](#the-verification-cache)) is not a failure, and
  not a way around the throttle: the throttle is asked first.
- **Tries in parallel** for one account, or from one address, are verified one after the
  other, and the throttle is asked again before each: a burst of wrong passwords gets the
  budget and `429` for the rest, not the budget plus the burst.

**Per client address**, two more throttles bound what one address can do across accounts, and
what it can make a replica spend:

- **Failed logins**: each failure counts against the client address too, 30 a minute, whatever
  the accounts; beyond, every password from the address is answered `429` until its minute has
  passed, before the user is read. The budget is above an account's, so that one account under
  throttle does not throttle the people behind the same address.
- **The token endpoint**: 100 requests a second per address to `/v2/token`, with credentials
  or without; beyond, `429` until the second has passed. It is checked before the Basic
  credentials, so that a flood of token requests costs no argon2id.

The client address is the one [ADR 0011](adr/0011-client-address-behind-a-proxy.md) defines:
the peer of the connection, or the client that `X-Forwarded-For` names when the peer is in
`server.trustedProxies` ([configuration.md](configuration.md#server)). An IPv6 address counts as
its /64. **With an empty `server.trustedProxies`, the per-address throttles are off**, and the
startup log says so: behind an edge every client would share the edge's address, and a
`docker pull` storm or a broken CI job would throttle everyone at once. The exception is
`server.tls`: Lajstrom terminates TLS itself, the peer is the client, and the per-address
throttles are on with it (#248), as the startup log says; an L4 balancer in front that hides the
client would make its address everyone's, so TLS then belongs at an L7 edge with the list. The
per-account throttle needs no address and is always on.

**Per replica.** The counts live in the memory of each replica, at most 10 000 keys per
throttle, and a restart clears them. Behind a balancer, N replicas are N budgets: up to 10 × N
guesses a minute for an account. The bound across replicas is the edge's
([operations.md](operations.md#failed-logins-and-the-edge)). The budgets are constants of
`internal/auth`, not settings.

**When a throttle is full**, a new key takes the place of a window that has closed, or, when
every window is still open, of the open window opened first that has not reached its budget.
A window that throttles its account is never dropped while it is open, so failures of other
names cannot end it: an account under throttle stays under throttle through a flood of
failures of names that no user has. When all 10 000 windows are open and at their budget, a
new key is not counted until a window closes: the throttle fails open. Getting there takes
100 000 failed logins within a minute on one replica, each an argon2id verification, about
1 700 core-seconds; failing closed instead would let such a flood lock every other account
out of the replica for a minute.

The body of the `429` follows the route, like the `401`: under `/v2/`
`{"errors":[{"code":"TOOMANYREQUESTS","message":"too many failed logins: try again after Retry-After seconds"}]}`
(`too many token requests` on `/v2/token` over its budget), under `/api/`
`{"error":{"code":"UNAVAILABLE","message":"…"}}`, plain text elsewhere. Each is logged at info
as `throttled` with the `throttle` (`account`, `address` or `token`), the `account` for the first
(`user:<uid>`, or `unknown` for a name that no user has; never the name typed, in case it was a
password) and the client `address`, and counted in `lajstrom_auth_throttled_total`.

## HTTP Basic on every route

Every route reads the `Authorization` header, the frontends (`/raw/`, `/pypi/`, `/apt/`,
`/maven/`, `/npm/`), the OCI facade (`/v2/`), the management API (`/api/`) and the system
endpoints (`/-/healthz`, `/-/readyz`, `/-/version`) alike:

- **No header**: the request goes on as `anonymous`, which may read a repo with `anonymousRead`
  when `auth.anonymousRead` allows it, and nothing else: the system endpoints answer it,
  `GET /api/v1/repos` lists the repos it may read (none by default: `{"repos":[]}`), and
  everywhere else it gets `401` with the challenge, so that the client sends credentials
  ([Authorization](#authorization)).
- **`Basic`** ([RFC 7617](https://www.rfc-editor.org/rfc/rfc7617); the first colon splits the
  user name from the password): the credentials are checked, and the request goes on as
  `user:<uid>`, or as `token:<id>` for a deploy token. The secret decides what is checked: one
  that starts with `ljs_pat_` is a personal access token of the user, one that starts with
  `ljs_dep_` a deploy token, anything else the user's password ([Tokens](#tokens)). Credentials
  that are refused (an unknown user, a disabled one, a wrong password, a token unknown, revoked,
  expired or of another user) or that do not decode are answered at once with `401`, on every
  route but npm's legacy login (below): a client that sends credentials learns at once that they
  are wrong. The challenge is `WWW-Authenticate: Basic realm="lajstrom"`, except under `/v2/`,
  where it is the bearer challenge of the token flow (below) and the client learns it at
  `/v2/token`, which itself answers with the Basic challenge.
- **`Bearer ljs_pat_…`** and **`Bearer ljs_dep_…`** on every route but `/v2`: a personal access
  token or a deploy token, checked as in Basic credentials, and refused the same way. npm sends
  its `_authToken` so, and yarn its `npmAuthToken` ([ADR 0012](adr/0012-npm-profile-routes-and-dist-tags-journal.md)).
- **Any other `Bearer`**, any bearer under `/v2`, and any other scheme: the request goes on as
  anonymous with the header untouched. The OCI facade verifies the bearer of `/v2`, which is
  its own JWT and nothing else ([below](#bearer-tokens-on-v2)).
- **npm's legacy login**, `PUT /npm/<repo>/-/user/org.couchdb.user:<name>`, whatever its
  header: the request goes on as anonymous with the header untouched, and the npm frontend
  checks the user name and the password of its body ([Tokens](#tokens)).

The body of the `401` follows the route:

| Route | Body |
| --- | --- |
| `/v2/` | `{"errors":[{"code":"UNAUTHORIZED","message":"credentials are needed: the ones sent were refused"}]}`, the error format of the distribution spec |
| `/api/` | `{"error":{"code":"UNAUTHENTICATED","message":"credentials are needed: the ones sent were refused"}}`, the envelope of [api.md](api.md) |
| elsewhere | `credentials are needed: the ones sent were refused`, plain text |

The access log line of every request has `principal`: the canonical form, `anonymous` for a
request without credentials and for the `401`s above, `user:<owner>` for a personal access
token, `token:<id>` for a deploy token, the token's subject for a request with a valid bearer
on `/v2`. The `Authorization` header is never logged, and neither is a token or its hash.

## Authorization

Every request is decided by RBAC ([rbac.md](spec/rbac.md) §3): the bindings of the principal in
the index, behind the scopes of its token. There is no configuration without it; the dev stack
and the quick starts log in too.

### Roles

| Role | On the repos its mask names |
| --- | --- |
| `reader` | `read`: `GET` and `HEAD` on the frontends, `pull` on `/v2`, the repo in the lists |
| `writer` | `read`, `write`: uploads, `push` |
| `maintainer` | `read`, `write`, `delete` and `manage`: deleting versions, changing the repo, its deploy tokens, reindexing it |
| `admin` | everything on every repo, and the system: users, bindings, creating a repo, every user's tokens, the audit log |

A `maintainer` on `*/*` has every repo but not the system; the system needs the `admin` role.

### Bindings

A binding is `(principal, role, mask)`. The principal is `user:<name>` or `user:<uid>` of an
existing user, stored as `user:<uid>`, or `group:<name>`, a group of OIDC logins, which need not
have members ([Groups and group bindings](#groups-and-group-bindings)); the mask is `<format>/<repo>`, each segment a name or `*`: `raw/assets` exactly, `raw/*` every raw
repo, `*/payments` the repo `payments` of every format, `*/*` everything. The repo need not
exist: a binding may come before its repo, and outlives it. The rights of a principal are the
union of its bindings. A deploy token has no bindings (its repo and scopes are its rights), and
neither has `anonymous` (a repo's `anonymousRead` is).

`lajstrom admin binding` manages them, straight against the database of the configuration, and
`/api/v1/bindings` over HTTP ([api.md](api.md#bindings)). Like every `lajstrom admin` command on
this page, it reads the configuration of `--config`, else of `LAJSTROM_CONFIG`, else
`lajstrom.yaml` in the current directory; on the dev stack, give it the file `make run` serves
(`export LAJSTROM_CONFIG=deploy/compose/lajstrom.example.yaml`, or your copy
`deploy/compose/lajstrom.yaml`):

```text
$ lajstrom admin binding create --principal user:alice --role writer --mask 'raw/*'
{"id":2,"principal":"user:01a1c136-51a6-7883-a350-b1cf6000a23f","name":"alice","role":"writer","repoMask":"raw/*","createdAt":"2026-11-09T16:48:44.981899Z"}
$ lajstrom admin binding create --principal user:alice --role reader --mask '*/payments'
{"id":3,"principal":"user:01a1c136-51a6-7883-a350-b1cf6000a23f","name":"alice","role":"reader","repoMask":"*/payments","createdAt":"2026-11-09T16:48:44.997609Z"}
$ lajstrom admin binding list
ID  USER   PRINCIPAL                                  ROLE    MASK        CREATED
1   admin  user:01a1c136-5180-7b5e-95ce-428e99e68887  admin   */*         2026-11-09T16:48:44Z
2   alice  user:01a1c136-51a6-7883-a350-b1cf6000a23f  writer  raw/*       2026-11-09T16:48:44Z
3   alice  user:01a1c136-51a6-7883-a350-b1cf6000a23f  reader  */payments  2026-11-09T16:48:44Z
$ lajstrom admin binding create --principal user:alice --role admin --mask 'raw/*'
lajstrom: repoMask: an admin binding is on */*: the admin role is every repo and the system
$ lajstrom admin binding create --principal token:3 --role writer --mask raw/assets
lajstrom: principal: token:3 cannot be bound: a binding is for user:<name> or group:<name>; anonymous reads through a repo's anonymousRead, a deploy token gets its repo and scopes
$ lajstrom admin binding create --principal user:alice --role writer --mask 'raw/*'
lajstrom: binding user:alice writer on raw/* exists already
$ lajstrom admin binding delete 1
lajstrom: binding 1 is the last admin binding of an enabled user: create another first (lajstrom admin binding create, or admin user create --admin)
```

(A run against a new SQLite database after `admin user create --name admin --admin` and
`--name alice`.) The principal is stored with the user's uid and shown with its name.
`list --principal user:alice` lists one principal's. `delete <id>` refuses the last admin
binding of an enabled user, so that somebody can still manage the server. Every change is an
audit event, `binding.create` or `binding.delete`, with the principal (`user:<uid>`) as its
subject and the id, role and mask in the details.

### How a request is decided

For the principal of the request and an action on `<format>/<repo>`, in this order:

1. the credential first: a personal access token with scopes keeps only those actions, an
   admin's too, so that an admin's `read` token does not write;
2. an `admin` binding allows everything;
3. a deploy token reaches its own repo, with the actions of its scopes, never `delete` or
   `manage`;
4. the bindings whose mask matches the repo allow the actions of their roles;
5. `anonymous`, and a user its bindings do not allow, may `read` a repo whose `anonymousRead` is
   on, when `auth.anonymousRead` is on too: both switches, and they are off by default (a
   deploy token stays on its repo). The server's switch is configuration
   ([configuration.md](configuration.md)); a repo's is `--anonymous-read` of
   `lajstrom admin repo create`, `repos[].anonymousRead` for a repo the configuration creates,
   or `PATCH /api/v1/repos/<format>/<name>` with `{"anonymousRead": true}` for one that exists
   ([api.md](api.md));
6. anything else is denied.

A change counts from the next request, on every replica: a binding created or deleted, through
the CLI or the API, a repo's `anonymousRead`, a user disabled. Each replica keeps the
bindings in memory and reads one counter of the index, `auth_version`, once per request; a
change moves it, and the next request reloads. `auth.anonymousRead` is configuration and
changes with a restart. On `/v2` a token, once issued, holds until it expires
([What a token does not say](#what-a-token-does-not-say)).

### The 401/404/403 rule

A denial does not tell whether a repo exists ([rbac.md](spec/rbac.md) §3):

- `anonymous` gets `401` with the challenge (the bearer challenge under `/v2/`, Basic
  elsewhere), on a repo it may not read and on one that does not exist alike;
- a principal with no access to the repo at all gets `404`, the answer of a repo that does not
  exist;
- `403` is for a principal that can read the repo and lacks the action.

So a user whose only binding is `reader` on `pypi/*` gets `404` on `raw/assets` and on
`raw/missing`, and a reader of `raw/assets` gets `403` for a `PUT` there. The same holds on
every route: rbac.md §3 lists the others that could tell (creating a repo, bindings, the lists,
`/v2` with a bearer) and why they do not. `test/e2e/rbac.sh` checks the matrix of
[acceptance.md](acceptance.md) S-02 with real requests, on the raw frontend and on `/v2`.

## Bearer tokens on `/v2`

OCI clients (`docker`, `oras`, `crane`, `helm`) authenticate with the docker token flow
([rbac.md](spec/rbac.md) §4): `GET /v2/` answers `401` with a challenge that names a realm, the
client gets a token from the realm with the credentials it has, or none, and sends the token as
a bearer on every `/v2` request. Lajstrom is its own token server: the realm is its
`/v2/token`, which takes the same HTTP Basic credentials as every other route, and the same
credentials in the OAuth2 form that containerd posts.

### The flow

From a run against `bin/lajstrom serve` on the example configuration, with `server.listen` and
`server.externalURL` moved to port 18140 and the master key set, after `alice`, a `writer` on
`raw/*`, had uploaded two versions of `raw/scratch/hello`. `-u alice` asks for her password; the
tokens are cut after their first characters, and the answers are shown without the headers every
answer has (`Content-Length`, `Date`, `X-Request-Id` and the security headers):

```text
$ curl -si http://localhost:18140/v2/
HTTP/1.1 401 Unauthorized
Content-Type: application/json
Docker-Distribution-Api-Version: registry/2.0
Www-Authenticate: Bearer realm="http://localhost:18140/v2/token",service="localhost:18140"

{"errors":[{"code":"UNAUTHORIZED","message":"authentication required: get a token from the realm of the challenge"}]}

$ curl -s -u alice 'http://localhost:18140/v2/token?service=localhost:18140&scope=repository:raw/scratch/hello:pull,push'
{"token":"eyJhbGciOiJFUz…","access_token":"eyJhbGciOiJFUz…","expires_in":1200,"issued_at":"2026-11-11T07:42:32Z"}

$ TOKEN=$(curl -s -u alice 'http://localhost:18140/v2/token?service=localhost:18140&scope=repository:raw/scratch/hello:pull,push' | jq -r .token)
$ PULL_ONLY=$(curl -s -u alice 'http://localhost:18140/v2/token?service=localhost:18140&scope=repository:raw/scratch/hello:pull' | jq -r .token)
$ jq -R 'split(".") | .[0:2] | map(gsub("-"; "+") | gsub("_"; "/") | @base64d | fromjson)' <<<"$TOKEN"
[
  {
    "alg": "ES256",
    "kid": "b8fd560ff5782aa2d1fc6159e37462bf282f01c050b9745697e58881636910a9",
    "typ": "JWT"
  },
  {
    "iss": "localhost:18140",
    "sub": "user:01a1c980-f66c-7291-8415-5d7775f62732",
    "aud": [
      "localhost:18140"
    ],
    "exp": 1794384152,
    "nbf": 1794382952,
    "iat": 1794382952,
    "jti": "STvrSMUNZ5mj8RZ_LWu4OQ",
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

$ curl -si -H "Authorization: Bearer $TOKEN" http://localhost:18140/v2/raw/scratch/hello/tags/list
HTTP/1.1 200 OK
Content-Type: application/json
Docker-Distribution-Api-Version: registry/2.0

{"name":"raw/scratch/hello","tags":["14282e94937ee86e","6a13a3f389e37acd"]}

$ curl -si -X POST -H "Authorization: Bearer $PULL_ONLY" http://localhost:18140/v2/raw/scratch/hello/blobs/uploads/
HTTP/1.1 403 Forbidden
Content-Type: application/json
Docker-Distribution-Api-Version: registry/2.0

{"errors":[{"code":"DENIED","message":"the bearer token does not grant push on raw/scratch/hello"}]}

$ curl -si -H "Authorization: Bearer $TOKEN" http://localhost:18140/v2/raw/scratch/other/tags/list
HTTP/1.1 401 Unauthorized
Content-Type: application/json
Docker-Distribution-Api-Version: registry/2.0
Www-Authenticate: Bearer realm="http://localhost:18140/v2/token",service="localhost:18140",scope="repository:raw/scratch/other:pull",error="insufficient_scope"

{"errors":[{"code":"UNAUTHORIZED","message":"the bearer token grants nothing on raw/scratch/other: get a token with its scope"}]}

$ curl -si -u alice:wrong http://localhost:18140/v2/token
HTTP/1.1 401 Unauthorized
Content-Type: application/json
Www-Authenticate: Basic realm="lajstrom"

{"errors":[{"code":"UNAUTHORIZED","message":"credentials are needed: the ones sent were refused"}]}
```

`$PULL_ONLY` is a token asked for `scope=repository:raw/scratch/hello:pull`: the push it does not
grant is `403`, while a repository the token does not name is `401` with the scope to ask for.

### The token endpoint

`GET /v2/token?service=<service>&scope=<scope>&scope=…` issues a token for the principal of
the request: the user of the Basic credentials (refused ones are `401` with the Basic
challenge, from the authentication middleware), or `anonymous` without credentials. A
personal access token (`user:<PAT>`) and a deploy token (`deploy:<token>`) come the same way,
and their scopes narrow the token (below).

`POST /v2/token` issues the same token for the same credentials sent in the OAuth2 form of the
password grant ([RFC 6749](https://www.rfc-editor.org/rfc/rfc6749#section-4.3) §4.3), which
containerd posts before it tries `GET` whenever it has a secret (#908). docker, oras, crane
and helm use `GET` for a user name and a password. Any other method is `405`.

- **The form**: an `application/x-www-form-urlencoded` body (`; charset=utf-8` and other
  parameters allowed) of at most 8 KiB, with `grant_type=password`, `username`, `password`,
  `service` and `scope`. `scope` may repeat and hold several scopes separated by spaces, as
  `GET`'s does; containerd sends one with all of its scopes. `client_id` (containerd's is
  `containerd-client`), `access_type` and any other field are ignored. The scopes, the
  service, the reduction and the answer are those of `GET` below.
- **The credential** is `username` and `password`, checked as the user name and the secret of
  Basic credentials are: a password, a personal access token of the user, or a deploy token
  under any user name. They count in the same throttles of failed logins as Basic credentials,
  on `GET /v2/token` and on every other route. An `Authorization` header on the `POST` is not
  read. There is no anonymous `POST`: anonymous gets its token with `GET`.
- **The form is read first**, before the credential is checked, so that a request the form
  refuses costs no verification and counts as no failed login. Its refusals are OAuth2's
  ([RFC 6749](https://www.rfc-editor.org/rfc/rfc6749#section-5.2) §5.2),
  `{"error": "<code>", "error_description": "<why>"}` with `Cache-Control: no-store`:
  - another `grant_type`, `refresh_token` included, is `400` `unsupported_grant_type`, since
    Lajstrom issues no refresh token;
  - another media type, a form without `grant_type`, `username` or `password`, or with one of
    them or `service` more than once, is `400` `invalid_request`;
  - a body over 8 KiB is `413` `invalid_request`, refused before it is read when its
    `Content-Length` says so.
- **What `GET` answers too** is answered as for `GET`: refused credentials are `401` with the
  Basic challenge, a throttle is `429`, more than 100 repositories `400` with `UNSUPPORTED`.
- **containerd's fallback.** After a `400`, `401` or `404` to its `POST`, containerd tries `GET`
  with Basic. A wrong password therefore costs it two failed logins per token, the `POST`'s
  and the `GET`'s. An identity token alone (`identityToken` in Talos, `identitytoken` in a
  docker `config.json`) is posted as `grant_type=refresh_token`, then sent as Basic with an
  empty user name. A deploy token passes that way, since its user name is not checked; a
  personal access token does not, since it is refused under any name but its user's. Give
  containerd a user name and a password ([guides/talos.md](guides/talos.md)).

- **Scopes**: each `scope` parameter is `repository:<format>/<repo>/<path>:<actions>`, several
  parameters or several scopes in one separated by spaces. The actions are a comma list of
  `pull`, `push` and `delete`, `*` meaning the three; any other action is ignored. A scope of
  another type, a name that is not `<format>/<repo>/<path>`, and a scope with none of the three
  actions are dropped, as the distribution token flow expects; a repository asked for twice
  gets the union. The scope of a repo that does not exist stays, with no action (below). One scope of another type is kept: `registry:catalog:*`, which
  `crane catalog` and `oras repo ls` ask for to list `/v2/_catalog`; a personal access token
  whose scopes do not include `read` does not get it, since the catalog is listed by its user's
  bindings. A request may name at most 100 repositories: more is `400` with `UNSUPPORTED`,
  before any repo is looked up (docker asks for one or two, oras and helm for a few).
- **Reduction**: for each scope, each action is kept when the credential allows it and the
  Authorizer allows it to the principal on `<format>/<repo>`: `pull` is `read`, `push` is
  `write`, `delete` is `delete` ([rbac.md](spec/rbac.md) §2). The credential first: a personal
  access token with scopes keeps the actions of its scopes, a deploy token those of its scopes
  on its own repo and nothing elsewhere, and neither ever gets `delete`; so a PAT with `read`
  gets a token with `pull` only, and its push is `403 DENIED` (S-03). The bindings next: a
  reader asking for `pull,push` gets `pull` (O-12). A principal that may not read a repo gets
  its scope with `"actions": []`, as for a repo that does not exist: the client can decode the
  token, and the two must not differ in it ([rbac.md](spec/rbac.md) §3, §4).
- **Service**: the configured one (below). A `service` parameter that names another is a
  client talking to another registry, not an error: it is ignored, and the token carries the
  configured service.
- **Answer**: `200` with `{"token": "<jwt>", "access_token": "<jwt>", "expires_in": 1200,
  "issued_at": "<RFC 3339>"}` and `Cache-Control: no-store`. The access log line has the
  principal. No audit row is written for a token: one per `docker pull` would be noise, and the
  pushes and deletes the token allows are audited themselves.
- **Rate limiting**: 100 requests a second per client address, `GET` and `POST` alike, then
  `429` with `Retry-After` and `TOOMANYREQUESTS`, checked before the credentials and before
  the form is read, so that a flood costs no argon2id; the passwords it carries, in Basic
  credentials or in the form, count in the throttles of failed logins
  ([Failed logins](#failed-logins)). Off without `server.trustedProxies`, unless `server.tls`
  is set.

### What a token says

| Part | Value |
| --- | --- |
| header `alg` | the algorithm of the signing key: `ES256` (ECDSA P-256, the default) or `EdDSA` (Ed25519), `keys.tokenAlgorithm` |
| header `kid` | the fingerprint of the signing key: the lowercase hexadecimal sha256 of its public key (PKIX DER) |
| header `typ` | `JWT` |
| `iss`, `aud` | the service |
| `sub` | the principal: `user:<uid>` (a user, with a password or a personal access token), `token:<id>` (a deploy token), `anonymous` |
| `iat`, `nbf` | the time of issue, to the second |
| `exp` | `iat` plus 20 minutes |
| `jti` | 16 random bytes, base64url |
| `access` | `[{"type": "repository", "name": "<format>/<repo>/<path>", "actions": [...]}]`, sorted by type and name |

The realm and the service come from `server.externalURL`: the realm is
`<externalURL>/v2/token`, the service its host, with the port when it has one
(`https://art.example.com` gives `art.example.com`). `server.externalURL` is required (#248) but
for the dev stack (`server.insecureHTTP: true`), where without it they come from the request's
`Host`, over `https://` for a request that came over TLS and `http://` otherwise.
Replicas behind one edge share one `Host` anyway, so a token one of them issues is for the
service of the others (H-12).

### The bearer on `/v2`

Every `/v2` request but `/v2/token` may carry `Authorization: Bearer <jwt>`. The facade looks
up the key its `kid` names and accepts the algorithm of that key and nothing else, never
`none`, never HMAC; then the signature, `exp` (required), `nbf` and `iat` with 60 seconds of
clock skew, `iss` and `aud` against the service, and `sub`, which must be a principal, a user by
its uid. A token
that fails any of them is `401` with the challenge; the reason is logged at debug, with the
`kid` and never the token, and is not answered.

A request with a valid bearer runs as the token's principal, in the request and in the access
log, and is decided by the token's `access`, not by the Authorizer:

- a repository the token does not name is `401` with the challenge, its `scope` and
  `error="insufficient_scope"`: the client gets a token with that scope and retries. A
  cross-repo mount names both repositories (`scope="repository:<target>:pull,push
  repository:<from>:pull"`), which is how docker mounts from another repository. The facade
  asks the token before it looks the repo up, so this is the answer whether the repo exists or
  not;
- a repository the token names with no action is the answer of a repo that does not exist:
  `404 NAME_UNKNOWN`, and `401` with the challenge to `anonymous`. A mount from such a source
  is a plain upload session (`202`), as from a source that does not exist;
- an action the token does not grant on a repository it names with others is `403 DENIED`: the
  token endpoint already refused it, and a wider token would not help (O-12);
- `GET /v2/` is `200 {}` with any valid bearer, as with Basic credentials, and `401` with the
  challenge to the anonymous principal;
- `GET /v2/_catalog` lists, with `registry:catalog:*`, the repositories the principal may read,
  and otherwise the repositories the token grants `pull` on.

A request without a bearer runs as on the other routes: as the Basic user or anonymous, decided
by the bindings ([Authorization](#authorization)). `GET /v2/_catalog` without one is `401` with
the challenge of `registry:catalog:*` to the anonymous principal, as `GET /v2/` is: `oras repo
ls` asks for a token only when it is challenged, and would otherwise list what anonymous may
read, after `oras login` too. The `401`s of a request without a bearer carry the bearer
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
on another node counts everywhere within a second. A token verifies at once on the replica that
issued it; for that second another replica may refuse a token of the new key, which the client
gets again. Rotation, drop and export are in
[operations.md](operations.md#the-bearer-token-key).

Without a master key (neither `keys.masterKeyFile` nor `keys.masterKeyEnv` set, or the variable
not set) `serve` signs with an ephemeral key in memory and says so at startup, with the key's
fingerprint and the reason:

```text
level=WARN msg="bearer tokens are signed with an ephemeral key: they do not survive a restart and are not shared between replicas; set keys.masterKeyEnv or keys.masterKeyFile" fingerprint=1e61d8cdea41763d44aa1bd5e91ae2b396d1943e90efc605d8c9d78cc24dbd9d algorithm=ES256 reason="keys: no master key: keys.masterKeyEnv is not set"
```

That is enough for one process, the dev stack and the quick starts, which need no new variable;
a client whose token the next start refuses gets another.

### What a token does not say

A token is trusted until its `exp`, as the distribution token flow expects: the facade checks
the signature, the expiry and the `access` claim on every `/v2` request, and does not evaluate
the bindings again ([rbac.md](spec/rbac.md) §4, the consequence in ADR 0010).
Whatever changed after a token was issued counts on `/v2` at the client's next `/v2/token`:

| Change | Frontends and `/api/v1` | `/v2/token` | `/v2`, with a token issued before the change |
| --- | --- | --- | --- |
| a binding created or deleted | the next request, on every replica | the next token | at the token's `exp`: 20 minutes at most |
| a repo's `anonymousRead` changed | the next request, on every replica | the next token | 20 minutes at most |
| a user disabled | the next request, on every replica | the next token | 20 minutes at most |
| a personal access or deploy token revoked, or expired | the next request, on every replica | the next token | 20 minutes at most |
| a repo deleted | the next request: `404` | the next token | at once: `NAME_UNKNOWN`, the token names the repository |
| `lajstrom admin key rotate --token --drop` | nothing | tokens of the new key | within a second: every token of the old key is refused |

Evaluating the bindings on every `/v2` request would close the window for a binding and for
`anonymousRead` only. The token does not say which credential it was issued for (`sub` is
`user:<uid>` for a password and for a personal access token alike), so a revoked token would
still count until `exp`, and each change would have a rule of its own.

To take access away on `/v2` at once, remove it first (the binding, the token, the user) and
then end every outstanding token:

```sh
lajstrom admin key rotate --token --drop
```

Within a second no replica accepts a token of the old key. Every client of `/v2` gets a `401`
on its next request, asks `/v2/token` again with its credentials, and gets a token of the new
key under the bindings of that moment; a client whose access is gone gets none. The rotation
ends the tokens of every principal, not only of the one whose access changed: it is the
emergency brake, not the everyday path ([operations.md](operations.md#the-bearer-token-key)).
Without a master key `serve` signs with an ephemeral key and the token commands fail
([above](#the-signing-key)); a restart of `serve` ends every token instead.

### Clients

`oras login` and `docker login` work as with any registry; on the dev stack, which is plain
HTTP on loopback, `oras` needs `--plain-http` and docker treats `localhost` as an insecure
registry by itself:

```sh
oras login --plain-http localhost:8080 -u alice
docker login localhost:8080 -u alice
```

`oras` and docker get a token when the facade challenges them: at `login`, which asks
`GET /v2/`, and on any `401`. A client that has not logged in gets the anonymous token, which
reads what anonymous may read and nothing else.

`test/e2e/oci-token.sh` runs the flow end to end (the challenge, a token, its claims, a changed
token, a token without push, `oras login`, push and pull, a rotation and a drop); from `make e2e` on
2026-11-09:

```text
ok   GET /v2/: 401 WWW-Authenticate: Bearer realm="http://127.0.0.1:44240/v2/token",service="127.0.0.1:44240"
ok   admin user create --name e2e-token-20261109162343-11444 --admin --password-stdin
ok   GET /v2/token -u e2e-token-20261109162343-11444 scope=repository:raw/scratch/hello:pull,push: 200, expires_in 1200, issued_at 2026-11-09T16:23:43Z
ok   claims: {"header":{"alg":"ES256","kid":"87ac72679a2c991a36a7e1eae7a7de9b337cf55274a72713df04e86c1fcfbc12","typ":"JWT"},"payload":{"iss":"127.0.0.1:44240","sub":"user:01a1c11f-69de-71e1-82db-9900a32cae11","aud":["127.0.0.1:44240"],"exp":1794242623,"nbf":1794241423,"iat":1794241423,"jti":"3NMyHgY4e_en8wOkuxJSsw","access":[{"type":"repository","name":"raw/scratch/hello","actions":["pull","push"]}]}}
ok   GET /v2/raw/scratch/hello/tags/list with the token: 404
ok   the token with one byte changed: 401, WWW-Authenticate: Bearer realm="http://127.0.0.1:44240/v2/token",service="127.0.0.1:44240",scope="repository:raw/scratch/hello:pull"
ok   POST /v2/raw/scratch/hello/blobs/uploads/ with a pull-only token: 403 DENIED: the bearer token does not grant push on raw/scratch/hello
ok   oras login 127.0.0.1:44240 -u e2e-token-20261109162343-11444: Login Succeeded
ok   oras push 127.0.0.1:44240/raw/scratch/hello:e7ec39e723694e49: Digest: sha256:864d1368ca1bb546333d26e5d65d2aed40e96ff23d359dee7ceea4e8fac13d21
ok   oras pull 127.0.0.1:44240/raw/scratch/hello:e7ec39e723694e49: the same bytes
ok   admin key rotate --token: 879fe137c3e5499884bbb18fa469794b47580003581007615c7e13100ad2f4b5; the old token (kid 87ac72679a2c991a…) still 200; a new token has kid 879fe137c3e54998…
ok   admin key rotate --token --drop: 136159cb982e126067d6101eb00baddcd1c6fc715b4bf262f2b90424b871324e; the token of 879fe137c3e54998… is 401, a new one 200
PASS oci-token e2e against http://127.0.0.1:44240 as e2e-token-20261109162343-11444
```

## Tokens

Two kinds of token stand in for a password ([rbac.md](spec/rbac.md) §1):

- A **personal access token** (PAT) acts as its user: the principal is `user:<uid>`, with the
  user's rights, narrowed by the token's `scopes` when it has any. It is what goes into
  `auth.conf`, `~/.pypirc` and `settings.xml` instead of a password: it expires, it is revoked
  alone, and it tells nothing about the password.
- A **deploy token** is a principal of its own, `token:<id>`, bound to exactly one repo with
  `read`, `write` or both: the credential of a CI job.

### The secret

`ljs_pat_` or `ljs_dep_`, then 43 characters of `0-9a-zA-Z`: 32 random bytes written as a
number in base 62 and padded, so that every secret has 51 characters, none that a URL, a shell
or a netrc file treats specially, one double-click selects it, and the prefix tells the kind,
to the authenticator and to a secret scanner.

The index keeps the lowercase hexadecimal sha256 of the whole secret, prefix included
(`tokens.hash`), and finds the token by it. The secret is in the answer of its creation once,
the API's `201` or the CLI's JSON line, and never again: it is not stored, logged, audited or
answered. Every later view shows `hashPrefix`, the first 8 characters of the hash, to tell
tokens apart (X-03). A lost secret is not recovered: create another token and revoke the old.

A token is checked without argon2id and without a cache: 256 random bits are not guessed, so
the lookup of the hash is the check, one query for the token and one for its user or repo, a
miss costing what a hit costs.

### Rules

| | Personal access token | Deploy token |
| --- | --- | --- |
| principal | `user:<owner's uid>` | `token:<id>` |
| bound to | its user | one repo |
| `scopes` | none (the user's full rights), or `read`, `write` or both | `read`, `write` or both, required; `delete`, `manage` and anything else are `400` |
| expiry | required: 90 days when not given, at most 365 days ahead (`400` beyond, naming the bound) | optional: none is never |
| `name` | 1 to 64 bytes of printable UTF-8, unique among the user's live tokens (`409`) | the same, among the repo's live tokens |
| created with | a password only (`403` with a token, #249) | a password, or a personal access token without scopes, of a user with `manage` on the repo |

**A personal access token is created with a password**, never with a token: a request whose
credential is a personal access token, with scopes or without, the admin's too, or a deploy
token gets `403` on `POST /api/v1/users/<name>/tokens` before its body is read. A stolen token
would otherwise mint tokens that last up to a year and outlive its revocation. An OIDC login
mints one itself ([OIDC login and groups](#oidc-login-and-groups)); `npm login` checks the password (#308), and
`lajstrom admin token create` needs no credential, as it runs against the database. A personal
access token without scopes still lists and revokes its user's tokens. CI that wants a fresh personal access token asks
for it with a password, or gets a deploy token instead ([In CI](#in-ci)).

A live token is neither revoked nor expired. A scope grants its action and nothing else:
`write` does not include `read`, so a client that pushes through `/v2`, and reads blobs before
it writes them, needs both. The bounds are constants, not configuration.

Expiry and revocation count at the next request, on every replica: the token is read from the
database on every request. A token is expired once `expiresAt` is past, without leeway.
`lastUsedAt` is the time of the last request with the token, written at most once a minute per
token and replica; a write that fails is logged at debug and refuses nothing. A PAT is refused
while its user is disabled, and a repo deleted takes its deploy tokens with it.

### What a token may do

The scopes narrow what the bindings give, on every route and at `/v2/token`:

- a PAT without scopes is its user: what the user's bindings allow;
- a PAT with scopes gets `read` with `read` and `write` with `write`, and never `delete` or
  `manage`; anything else is `403` (S-03, X-14);
- a deploy token is confined to its repo: on any other it is `404`, the answer of a repo that
  does not exist, since it has no access there at all (S-04); on its repo it gets `read` and
  `write` per its scopes, and `403` for an action outside them, `delete` and `manage` above all:
  `DELETE` of a version, `PATCH` or `DELETE` of the repo, its deploy tokens (X-14).
  `GET /api/v1/repos` lists its repo only;
- the PATs of a user are the user's own to list, create and revoke, or the admin's, and are
  created with a password only ([Rules](#rules)). The deploy tokens of a repo need `manage` on
  it. A deploy token or a PAT with scopes asking for either is `403`;
- at `/v2/token` the same rules narrow the token ([above](#the-token-endpoint)); `/v2` itself
  takes that token and no other bearer.

### Where the secret goes

- **curl**: `curl -u alice:ljs_pat_… https://art.example.com/raw/scratch/file`, or the same in
  a config file, `curl -K` with `user = "alice:ljs_pat_…"`, to keep it off the command line. A
  deploy token: `-u deploy:ljs_dep_…`.
- **The management API**: `Authorization: Bearer ljs_pat_…` or `Bearer ljs_dep_…`, or Basic,
  `alice:ljs_pat_…` and `deploy:ljs_dep_…`.
- **apt**: `/etc/apt/auth.conf.d/lajstrom.conf`, mode 0600, one line per repo, with a personal
  access token or with a deploy token (apt takes the first line whose `machine` matches):

  ```text
  machine art.example.com/apt/ubuntu-internal login alice password ljs_pat_…
  machine art.example.com/apt/ubuntu-internal login deploy password ljs_dep_…
  ```

  apt sends the credentials of a `machine` without a scheme over `https` only; for plain `http`
  (the dev stack) write the scheme: `machine http://localhost:8080/apt/e2e`. Without it,
  `apt-get update` warns `Credentials for localhost:8080/apt/e2e match, but the protocol is not
  encrypted. Annotate with http:// to use.` and gets `401`.
- **pip and twine**: `~/.pypirc` with `username = alice` and `password = ljs_pat_…`, or
  `username = __token__` (or any name) and a deploy token.
- **oras, docker, helm**: `oras login -u alice --password-stdin art.example.com` with the token
  on standard input; the client gets its bearer from `/v2/token` with the token as its
  password, so the narrowing holds on `/v2`. A deploy token logs in as `deploy`.
- A client that insists on a user name with a deploy token may send any: only the secret
  counts.

### In CI

A pipeline gets a deploy token of its repo with `write`, plus `read` when its client reads
before it writes: an oras or docker push through `/v2`, which asks for its blobs first, and a
Maven deploy, which reads `maven-metadata.xml` first; curl and twine need `write` only. It gets
the token from a secret of the CI system (a GitHub Actions secret, a masked and protected GitLab
CI/CD variable), and hands it to curl on standard input, so that it is neither in the job's
command lines nor in its log:

```yaml
# GitHub Actions
- name: Upload
  env:
    LAJSTROM_DEPLOY_TOKEN: ${{ secrets.LAJSTROM_DEPLOY_TOKEN }}
  run: |
    printf 'user = "deploy:%s"\n' "$LAJSTROM_DEPLOY_TOKEN" |
      curl -fsS -K - -T dist/app.tar.gz "https://art.example.com/raw/releases/app/${GITHUB_REF_NAME}/app.tar.gz"
```

```yaml
# GitLab CI
upload:
  rules:
    - if: $CI_COMMIT_TAG
  script:
    - printf 'user = "deploy:%s"\n' "$LAJSTROM_DEPLOY_TOKEN" | curl -fsS -K - -T dist/app.tar.gz "https://art.example.com/raw/releases/app/${CI_COMMIT_TAG}/app.tar.gz"
```

The same secret goes into the client's own file where the client has one: `settings.xml`
(`<username>deploy</username>`, `<password>${env.LAJSTROM_DEPLOY_TOKEN}</password>`), twine's
`TWINE_USERNAME=deploy` and `TWINE_PASSWORD`, `oras login -u deploy --password-stdin`.

### Creating, listing, revoking

Through the API ([api.md](api.md#tokens)):

```sh
curl -s -u alice -X POST https://art.example.com/api/v1/users/alice/tokens \
  -H 'Content-Type: application/json' -d '{"name": "laptop", "scopes": ["read"], "expiresIn": "30d"}'
```

```json
{"id":7,"kind":"pat","user":"alice","name":"laptop","scopes":["read"],"expiresAt":"2026-12-07T12:00:00Z","lastUsedAt":null,"createdAt":"2026-11-07T12:00:00Z","revokedAt":null,"hashPrefix":"c68464d8","secret":"ljs_pat_…"}
```

Or from the command line, straight against the database of the configuration:

```sh
lajstrom admin token create --user alice --name laptop [--scopes read,write] [--expires 90d|2027-01-31T00:00:00Z]
lajstrom admin token create --repo raw/assets --name ci --scopes write [--expires 30d]
lajstrom admin token list --user alice         # or --repo raw/assets
lajstrom admin token revoke 7
```

`create` prints the token as JSON on one line with its secret, and nothing else on standard
output; `list` prints a table, `SCOPES` `all` for a PAT without scopes; `revoke` prints nothing,
and changes nothing for a token revoked already:

```text
ID  KIND  NAME    SCOPES  EXPIRES               LAST USED             REVOKED  HASH
7   pat   laptop  read    2026-12-07T12:00:00Z  2026-11-07T12:03:11Z  -        c68464d8
```

### Audit

`token.create` (details `id`, `kind`, `name`, `scopes`, `expiresAt`, and `createdBy`, the
credential of the request: `password`, or `token:<id>` for a token, which a deploy token made
with a personal access token has; none from the CLI) and `token.revoke` (`id`, `kind`, `name`),
by the principal of the request (`anonymous` from the CLI), with the source and the request id. The subject is `user:<owner>` for a PAT and `<format>/<repo>` for a
deploy token, whose repo is also the row's `repo`. Never the secret, never its hash. Revoking a
revoked token writes nothing. The use of a token is not audited, as reads are not; `lastUsedAt`
tells when it was last used.

### End to end

`test/e2e/tokens.sh` runs the above with real clients: a PAT created through the API, used as
Basic on a raw `PUT` and as a bearer on `whoami`; an apt repo with a `.deb` built in
`ubuntu:24.04` and uploaded with the PAT; `apt-get update && apt-get install` in
`ubuntu:24.04` with a PAT (`login <user>`) and then a deploy token with `read` (`login deploy`)
in `auth.conf.d`, each token's `lastUsedAt` showing that apt sent it, and a token that does not
exist refused; the deploy token's `404` and `403`; a deploy token with `write`; `oras login`
and push with the PAT; a PAT with `read` refused a push on the frontend and through its `/v2`
token; revocation, expiry and an expiry over a year. `twine upload` with a deploy token, the
rest of #44's criterion, is not in the script: `test/e2e/pypi.sh` uploads with a password. It
was run by hand on 2026-11-11, twine 7.0.0 in `debian:13` with a deploy token with `write`:
from `~/.pypirc` with `username = __token__`, and with `TWINE_USERNAME=deploy` and
`TWINE_PASSWORD`, both uploaded; the same token is refused by `pip install`, which reads.

From `E2E='auth tokens oci-token rbac' make e2e` on 2026-11-09, against the dev stack's Zot,
after RBAC (#45) and the uid (#182). The `whoami` line shows the principal and the name that the
script checks, not the whole answer, which has `kind` and `uid` too
([The uid and the name](#the-uid-and-the-name)):

```text
ok   admin user create --name e2e-tokens-20261109162333-19002 --admin --password-stdin
ok   POST /api/v1/users/e2e-tokens-20261109162333-19002/tokens -u e2e-tokens-20261109162333-19002: 201, Location /api/v1/users/e2e-tokens-20261109162333-19002/tokens/1, id 1, hashPrefix 535bb986, scopes all, expiresAt 2027-02-07T16:23:34.004851Z, the secret ljs_pat_… shown once
ok   GET /api/v1/users/e2e-tokens-20261109162333-19002/tokens: hashPrefix 535bb986, no secret
ok   PUT /raw/scratch/e2e-tokens-20261109162333-19002/hello.txt -u e2e-tokens-20261109162333-19002:<PAT>: 201 sha256:b66ea0c7e94806507b93fd5084ab7cdd795b0f40e140d9be7ff6cc7ae764343e
ok   GET /api/v1/whoami with Authorization: Bearer <PAT>: 200 {"principal":"user:01a1c11f-4347-7c22-bee9-195d60bbb247","name":"e2e-tokens-20261109162333-19002"}
ok   apt/e2e exists
ok   dpkg-deb --build in ubuntu:24.04, PUT /apt/e2e/upload?suite=noble -u e2e-tokens-20261109162333-19002:<PAT>: 201 sha256:5d6817acada8e4a16274916c0f787d161bf7dcc9e3b0ececf383ea6281cf3506
ok   GET /apt/e2e/key.asc: 200, the source is signed-by it
ok   POST /api/v1/repos/apt/e2e/deploy-tokens with the PAT: 201, id 3, hashPrefix 4e69750f, scopes read, expiresAt never
ok   apt-get update && apt-get install e2e-tokens-20261109162333-19002 in ubuntu:24.04 [signed-by=/etc/apt/keyrings/e2e.asc], auth.conf login e2e-tokens-20261109162333-19002 password <pat 2, scopes read>: installed; lastUsedAt 2026-11-09T16:23:35.217286Z
ok   apt-get update && apt-get install e2e-tokens-20261109162333-19002 in ubuntu:24.04 [signed-by=/etc/apt/keyrings/e2e.asc], auth.conf login deploy password <deploy 3, scopes read>: installed; lastUsedAt 2026-11-09T16:23:36.871001Z
ok   apt-get update with a deploy token that does not exist: fails, 401  Unauthorized
ok   PUT /raw/scratch/… -u deploy:<deploy token 3, read on apt/e2e>: 404 (no access at all): index: not found: repository raw/scratch
ok   PUT /apt/e2e/upload -u deploy:<deploy token 3, read>: 403: format: forbidden: write on apt/e2e: auth: outside the credential's scopes: the deploy token has read
ok   deploy token 4 with write on raw/scratch, as gitlab-ci-token:<token>: PUT 201, DELETE 403, PATCH /api/v1/repos/raw/scratch 403 FORBIDDEN, PUT to apt/e2e 404
ok   oras login 127.0.0.1:44240 -u e2e-tokens-20261109162333-19002 --password-stdin <PAT>: Login Succeeded (Basic on /v2/token)
ok   oras push 127.0.0.1:44240/raw/scratch/t:f3f65760c1fa8ba1: Digest: sha256:f4bd917e0e7d6a41c9c016f6d79cf5c19116fe7f44320b1463db3308f0175679
ok   GET /v2/token -u e2e-tokens-20261109162333-19002:<PAT> scope=repository:raw/scratch/t:pull,push: sub user:01a1c11f-4347-7c22-bee9-195d60bbb247, actions pull,push
ok   PUT /raw/scratch/… -u e2e-tokens-20261109162333-19002:<PAT 5, scopes read>: 403: format: forbidden: write on raw/scratch: auth: outside the credential's scopes: the personal access token has read
ok   GET /v2/token -u e2e-tokens-20261109162333-19002:<read PAT> scope=…:pull,push: actions [pull]; POST /v2/raw/scratch/t/blobs/uploads/ with it: 403 DENIED: the bearer token does not grant push on raw/scratch/t
ok   DELETE /api/v1/users/e2e-tokens-20261109162333-19002/tokens/1: 204; the next requests with it, bearer and Basic: 401; revokedAt 2026-11-09T16:23:39.83724Z
ok   a PAT with expiresAt 2026-11-09T16:23:41Z: 200 at once, 401 three seconds later
ok   a PAT with expiresAt 2027-12-14T16:23:39Z: 400 expiresAt: a personal access token expires at most 365 days ahead, at 2027-11-09T16:23:43Z at the latest
ok   server log: principal=user:01a1c11f-4347-7c22-bee9-195d60bbb247 (38 lines), principal=token:3 (5 lines), no secret
PASS tokens e2e against http://127.0.0.1:44240 as e2e-tokens-20261109162333-19002
```

Without the master key apt repos are not signed, and the two apt steps run with
`[trusted=yes]`; the rest is the same:

```text
ok   GET /apt/e2e/key.asc: 404, the server does not sign: [trusted=yes]
ok   apt-get update && apt-get install e2e-tokens-20261107230011-31449 in ubuntu:24.04 [trusted=yes], auth.conf login e2e-tokens-20261107230011-31449 password <pat 5, scopes read>: installed; lastUsedAt 2026-11-07T23:00:12.35067Z
ok   apt-get update && apt-get install e2e-tokens-20261107230011-31449 in ubuntu:24.04 [trusted=yes], auth.conf login deploy password <deploy 6, scopes read>: installed; lastUsedAt 2026-11-07T23:00:13.504948Z
```

## Bootstrap: the first admin

There is no admin password in the environment or in the configuration. A password there is
applied once, on the first start (Grafana's `admin_password`, Harbor's `harbor_admin_password`,
GitLab's `GITLAB_ROOT_PASSWORD`), and after the first change of the password the Secret keeps a
value that looks real and is not. The first admin is made with a one-time token instead, which can
never be mistaken for a live credential (decided on #440). There are two ways:

- **Over HTTP, with the bootstrap token.** For an operator who reaches the server and not its
  database: a Helm install, a laptop in front of a cluster.
- **Against the database, with `lajstrom admin user create --admin`.** For an operator who runs
  commands next to the database, with the server's configuration. It is also the recovery when
  every admin is lost ([A lost password](operations.md#a-lost-password)).

### The bootstrap token

You make the token, 32 to 256 bytes without whitespace, and give it to `serve` as a file:
`auth.bootstrap.tokenFile` (`LAJSTROM_BOOTSTRAP_TOKEN_FILE`), a Kubernetes Secret or a Compose
secret mounted as a file.

```sh
openssl rand -hex 32 > bootstrap-token
```

At start, while no enabled user has an admin binding, `serve` makes the token of the file valid
on every replica, before it listens, and logs that it is valid, never the token:

```text
level=INFO msg="no admin yet: the bootstrap token of auth.bootstrap.tokenFile is valid; exchange it with lajstrom admin bootstrap" auth.bootstrap.tokenFile=/run/secrets/lajstrom-bootstrap-token madeBy=lajstrom-7d9c6b5f4-x2k8q:1 madeAt=2026-11-11T09:02:26.275Z expiresAt=2026-11-12T09:02:26.275Z
```

`serve` never makes a token and never logs one: logs go to central logging and live long, and
whoever reads them within the token's lifetime could claim the first admin. Without the file
(the key empty, or a file that does not exist) there is no token, and a warning says how to make
the first admin:

```text
level=WARN msg="no admin yet, and no bootstrap token: auth.bootstrap.tokenFile is not set. Write a token of your own (openssl rand -hex 32) to a file, name it in auth.bootstrap.tokenFile, restart and run lajstrom admin bootstrap; or, with the database, run lajstrom admin user create --admin"
```

A file that cannot be read, or a token that is too short, too long or not one word, stops
`serve`. The index keeps the sha256 of the token, never the token (`bootstrap_token`,
[index-schema.md](spec/index-schema.md)). It is valid for `auth.bootstrap.ttl`, 24 hours by
default; used or expired, it is not made valid again: write a new token to the file and restart.
A new token in the file replaces the old one at the next start. Of replicas that start together,
one stores the token.

### Exchanging it

```sh
lajstrom admin bootstrap --server https://art.example.com --name admin
```

```text
Bootstrap token:
Password:
Repeat the password:
{"uid":"01a1c9a1-8909-7932-b435-6fc48e1e252b","name":"admin","kind":"local","disabled":false,"createdAt":"2026-11-11T08:02:49.225264Z"}
```

The command sends `POST /api/v1/bootstrap` ([api.md](api.md#the-first-admin-post-apiv1bootstrap)),
which needs no credentials: the token is the credential. It needs no database either. The token
comes from `--token-file`, else from a prompt without echo; the password from standard input with
`--password-stdin`, else from a prompt, twice; the server from `--server`, else `server.listen`
of the configuration on loopback, as for `lajstrom healthcheck`. `--password-stdin` needs
`--token-file`: standard input holds the password, so the token cannot be prompted for. Without a
terminal:

```sh
printf '%s' "$PASSWORD" | lajstrom admin bootstrap --server https://art.example.com --name admin \
  --token-file /run/secrets/bootstrap-token --password-stdin
```

The new user has the binding `(user:<uid>, admin, */*)`, made in the transaction that uses the
token, so that the token makes one admin whichever replica gets it. Then:

- every request to `POST /api/v1/bootstrap` is `404`, whatever its token; the command says that an
  admin exists already;
- a start while the file of `auth.bootstrap.tokenFile` exists logs a warning: `the bootstrap token
  is set but no longer valid, an admin exists: remove auth.bootstrap.tokenFile and its file`.

A wrong token is `403`; 10 wrong tokens from one client address within a minute make its next
requests `429` with `Retry-After`, the right token too, and leave the other addresses alone. The
count is per replica and per client address, like the per-address throttles of
[Failed logins](#failed-logins), and like them it is off without `server.trustedProxies` or
`server.tls`: no address reaches it, and one count for every client would let anyone's 10 wrong
tokens a minute refuse the operator's right one. Wrong tokens are then `403` however many come,
and the right one is never refused for them. That bounds no guessing, which a random token of
32 bytes or more, as `openssl rand` makes, does not need; the edge's request limit is the bound
that holds ([operations.md](operations.md#failed-logins-and-the-edge)). The right token, used or
expired, is `403` with a message that says so. A name or a password that breaks its rule, or a
name that exists, leaves the token valid. The audit event is `user.create` by `anonymous` with
the source `bootstrap`, the client address and the request id.

### Against the database

`lajstrom admin user create` works on the database of the configuration, without HTTP
([rbac.md](spec/rbac.md) §6). Run `lajstrom migrate` first, or start `serve` once:

```sh
lajstrom admin user create --name admin --admin --config lajstrom.yaml
```

```json
{"uid":"01a1c155-57ca-7c43-a38e-9595784a4098","name":"admin","kind":"local","disabled":false,"createdAt":"2026-11-07T12:00:00Z"}
```

`--admin` also creates the binding `(user:<uid>, admin, */*)`: everything on every repo, the
system and the audit log ([Authorization](#authorization)). It works after `lajstrom migrate` or
the first start of `serve`, not before, and needs no bootstrap token. The password comes from:

1. standard input with `--password-stdin`, the whole of it without the trailing newline
   (`printf '%s\n' "$PASSWORD" | lajstrom admin user create --name ci --password-stdin`);
2. a prompt without echo, twice, when standard input is a terminal.

With neither the command fails and names both. `LAJSTROM_ADMIN_PASSWORD` is not read: a command
that finds it set fails and says to unset it. The other commands:

```sh
lajstrom admin user list                    # NAME, UID, KIND, DISABLED, CREATED
lajstrom admin user disable alice           # refused from the next request on
lajstrom admin user enable alice
lajstrom admin user set-password alice      # the same two sources, or --password-file <file>
```

`set-password --password-file` reads the password from a file, the way `--password-stdin` reads
standard input: the whole of it without the trailing newline. It is for the recovery Job of the
Helm chart, whose image has no shell to put a Secret on standard input
([helm.md](deploy/helm.md#a-lost-password)).

A name that exists is refused (`user alice already exists`, exit status 1). Every change is an
audit event ([rbac.md](spec/rbac.md) §5) with the user as its subject: `user.create` (its
details say whether the user is an admin), `user.password`, `user.disable`, `user.enable`, by
`anonymous` with the source `cli` and the host name of the machine the command ran on (`host`).
`disable` of a disabled user and `enable` of an enabled one change nothing and write no event.
Everything after the bootstrap can go through `/api/v1` ([api.md](api.md#users)).

### How others do it

| Product | First admin | Lost admin password |
| --- | --- | --- |
| Teleport | `tctl users add` prints a one-time invite URL, valid 1 hour by default, 48 at most ([tctl](https://goteleport.com/docs/reference/cli/tctl/)) | `tctl users reset` prints a reset URL, valid 8 hours by default |
| Grafana | `admin_password`, "set once on first-run", `admin` by default ([configuration](https://grafana.com/docs/grafana/latest/setup-grafana/configure-grafana/)) | `grafana cli admin reset-admin-password` against the database ([CLI](https://grafana.com/docs/grafana/latest/administration/cli/)) |
| GitLab | a random password in `/etc/gitlab/initial_root_password`, removed after 24 hours; `GITLAB_ROOT_PASSWORD` on the first install only ([install](https://docs.gitlab.com/install/package/ubuntu/)) | `gitlab-rake "gitlab:password:reset"` or the Rails console ([reset](https://docs.gitlab.com/security/reset_user_password/)) |
| Gitea | `gitea admin user create --admin` ([CLI](https://docs.gitea.com/administration/command-line/)) | `gitea admin user change-password` |
| Harbor | `harbor_admin_password`, used on the first start only ([harbor.yml](https://goharbor.io/docs/main/install-config/configure-yml-file/)) | no documented way; an admin may not reset its own password ([reset](https://goharbor.io/docs/main/administration/managing-users/reset-user-password/)) |
| Vault | the root token of `vault operator init`, which does not expire ([tokens](https://developer.hashicorp.com/vault/docs/concepts/tokens)) | `vault operator generate-root` with a quorum of key holders and a one-time password ([generate-root](https://developer.hashicorp.com/vault/docs/commands/operator/generate-root)) |
| Lajstrom | a one-time token that you make, from a file, exchanged with `lajstrom admin bootstrap`, never logged; or `admin user create --admin` against the database | `admin user set-password`, or `admin user create --admin`, against the database ([operations.md](operations.md#a-lost-password)) |

## whoami

`GET /api/v1/whoami` answers the principal of the request. Every principal may ask who it is;
no authorizer is consulted.

```sh
curl -s -u admin http://localhost:8080/api/v1/whoami
```

```json
{"principal":"user:01a1c980-f635-7729-bd3a-067fb89ea59f","kind":"user","name":"admin","uid":"01a1c980-f635-7729-bd3a-067fb89ea59f"}
```

Without credentials it is `401` with the Basic challenge and
`{"error":{"code":"UNAUTHENTICATED","message":"credentials are needed"}}`. With a personal access
token it is the token's user; with a deploy token,
`{"principal":"token:3","kind":"token","name":"3"}`.

## OIDC login and groups

One OpenID Connect identity provider (IdP) can log people in: Keycloak, Dex, Okta, Entra ID,
Google, GitLab, any IdP with the authorization code flow ([rbac.md](spec/rbac.md) §8). A login
ends in a [personal access token](#tokens) of the person's user, so every client keeps working
as it does with any other token: docker, pip, npm, mvn and apt cannot run a browser flow, and
they never see a token of the IdP. The IdP's groups become principals, `group:<name>`, which
bindings can name.

### Setting it up

At the IdP, register a confidential client (OAuth 2.0 client with a secret) whose redirect URI
is `<server.externalURL>/-/oidc/callback`, with the scopes `openid`, `profile`, and the scope
that puts the groups in the ID token (`groups` for Dex and Keycloak's group mapper; Entra ID
and Okta name theirs). Then:

```yaml
server:
  externalURL: https://art.example.com
auth:
  oidc:
    issuer: https://idp.example.com/realms/main
    clientID: lajstrom
    clientSecretFile: /run/secrets/oidc-client-secret
    # scopes: [openid, profile, email, groups]   the default
    # usernameClaim: preferred_username           the default
    # groupsClaim: groups                         the default; "" reads no groups
    # tokenLifetime: 24h                          the default
```

The keys are in [configuration.md](configuration.md#auth). `serve` logs `OIDC login on` with the
issuer and the client ID at start, and reads nothing from the IdP until the first login, so it
starts while the IdP is down. The issuer must be https, but with `server.insecureHTTP`, the dev
stack.

### Logging in

In a browser, open `https://art.example.com/-/oidc/login`. After the IdP's login, the page shows
a personal access token once, with how to use it:

```sh
docker login art.example.com -u alice              # the token as the password
pip install --index-url https://alice:<token>@art.example.com/pypi/internal/simple/ requests
```

From a terminal, or on a machine without a browser, `lajstrom login` runs the device flow: it
prints a page and a code, you open the page in a browser on any machine, confirm the code and log
in, and it prints the token:

```sh
$ lajstrom login --server https://art.example.com --output token | docker login art.example.com -u alice --password-stdin
To log in, open this page in a browser and confirm the code BCDF-GHJK:

    https://art.example.com/-/oidc/device?user_code=BCDF-GHJK

Waiting for the login (10 minutes)...
Logged in to art.example.com as alice; the token expires at 2026-11-13T22:00:00Z.
```

Without `--output token` it prints one JSON line with the server, the user, the token and its
expiry. It writes no file: the token goes where you put it.

The first login of a person creates their user, of kind `oidc` and without a password, named by
the claim `usernameClaim`. The user is the identity's, the `iss` and `sub` of its ID token, not
the name's: a later login keeps the user and its name even if the claim changed, and a name that
another user has already refuses the first login (`the name alice is taken by another user`):
an admin renames or deletes that user. An identity is never joined to an existing local user by
its name, so that whoever holds a name at the IdP does not take over the local user of that
name, the first admin included.

The token of a login expires after `tokenLifetime`, 24 hours by default, and has no scopes: it
is its user. An OIDC user has no password, so it cannot create tokens with the API; it logs in
again for a new one, and lists and revokes its tokens as any user.

To keep a person out, **disable** their user (`lajstrom admin user disable alice`): their tokens
stop at the next request, and their logins are refused. Deleting the user lets the identity go,
and the next login makes a new user.

### Groups and group bindings

Each login replaces the user's groups with those of the claim `groupsClaim`, and they count
until the token of that login expires. A group removed at the IdP therefore stops granting at
the person's next login, on every replica, for every token of the user, and at the latest when
the login's token expires. A name that is not 1 to 256 bytes of printable text is dropped.

A binding names a group as it names a user, and the group need not have members yet:

```console
$ lajstrom admin binding create --principal group:platform --role maintainer --mask '*/*'
$ lajstrom admin binding create --principal group:payments-dev --role writer --mask '*/payments'
$ curl -s -u alice:<token> https://art.example.com/api/v1/whoami
{"principal":"user:01a1…","kind":"user","name":"alice","uid":"01a1…","groups":["payments-dev","platform"]}
```

A user's rights are its own bindings plus those of its current groups; a token's scopes narrow
both. A group's `admin` binding makes its members admins, but it is not "an admin" for the rule
that keeps the last admin binding of an enabled user (`409`), nor for the bootstrap: a group can
lapse without anyone changing a binding.

### What is checked

The login uses the authorization code flow with PKCE (S256) and a nonce; its state is single
use, ten minutes, kept as a hash in the index so that any replica finishes it, and bound to the
browser by a cookie, so that a link to someone else's callback logs nobody in. The ID token must
be signed with an asymmetric key of the IdP's JWKS, by an algorithm of that key (never `none` or
an HMAC), be issued by the configured issuer to this client (`iss`, `aud`, `azp`), and be valid
within 60 s of skew. The keys are cached; a new `kid` reloads them at most every 30 seconds. No
code, state, ID token or personal access token is logged. Every login writes `user.login` to the
audit log with the issuer, the subject and the groups, a first login `user.create`, and the token
`token.create` with `createdBy: oidc`. The [threat model](security/threat-model.md) has the flows.

Not yet: token exchange for CI (a GitHub Actions or GitLab OIDC token for a short-lived token,
proposed on #69; meanwhile a pipeline uses a [deploy token](#in-ci)), and joining an existing
local user to an identity.

## What comes next

Token exchange for CI and linking a local user to an identity (#69). Nothing else of
authentication is open.

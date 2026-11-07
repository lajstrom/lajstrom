# Authentication

Who a request comes from, and how Lajstrom finds out. This page describes local users, their
passwords, HTTP Basic on every route, the bootstrap of the first admin and `whoami`. What a
principal may do is RBAC's ([rbac.md](spec/rbac.md), #45); until then every route is open, and
authentication only tells who is asking. The model is [ADR 0007](adr/0007-rbac-model.md).

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
of failed logins comes with #50.

## HTTP Basic on every route

Every route reads the `Authorization` header, the frontends (`/raw/`, `/pypi/`, `/apt/`,
`/maven/`), the OCI facade (`/v2/`), the management API (`/api/`) and the system endpoints
(`/-/healthz`, `/-/readyz`, `/-/version`) alike:

- **No header**: the request goes on as `anonymous`. What anonymous may do is RBAC's to decide;
  in this version everything is open, so the dev stack works without credentials.
- **`Basic`** ([RFC 7617](https://www.rfc-editor.org/rfc/rfc7617); the first colon splits the
  user name from the password): the credentials are checked, and the request goes on as
  `user:<name>`. Credentials that are refused (an unknown user, a disabled one, a wrong
  password) or that do not decode are answered at once with `401` and
  `WWW-Authenticate: Basic realm="lajstrom"`, whatever the route: a client that sends
  credentials learns at once that they are wrong.
- **`Bearer`** and any other scheme: the request goes on as anonymous with the header
  untouched. The OCI facade reads the bearer of `/v2` with #26.

The body of the `401` follows the route:

| Route | Body |
| --- | --- |
| `/v2/` | `{"errors":[{"code":"UNAUTHORIZED","message":"credentials are needed: the ones sent were refused"}]}`, the error format of the distribution spec |
| `/api/` | `{"error":{"code":"FORBIDDEN","message":"credentials are needed: the ones sent were refused"}}`, the envelope of [api.md](api.md) |
| elsewhere | `credentials are needed: the ones sent were refused`, plain text |

The access log line of every request has `principal`: the canonical form, `anonymous` for a
request without credentials and for the `401`s above. The `Authorization` header is never
logged.

### The 401/404/403 rule

[rbac.md](spec/rbac.md) §3 decides what a denial answers so that it does not reveal whether a
repo exists: `401` with the Basic challenge for `anonymous`, `404` for a principal without any
access to the repo, `403` for one that can read the repo but lacks the action. Before RBAC
(#45) nothing is denied, and two `401`s exist: credentials that are refused, on every route, and
`whoami` without credentials. The rest of the rule comes with #45.

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
| #26 | Bearer tokens for `/v2`: the token endpoint checks Basic credentials with the same authenticator and issues a JWT; the facade validates the bearer on every `/v2` request |
| #44 | Personal access tokens (`user:<PAT>`, acting as the user) and deploy tokens (`deploy:<token>`, the principal `token:<id>`), in the same Basic credentials |
| #45 | RBAC: the bindings decide what each principal may do, anonymous gets `401` with the challenge where it may not read, and the rule above holds everywhere |
| #47 | The users API: `GET/POST /api/v1/users`, `PATCH`, `POST …/password`, with the same rules and audit events as `lajstrom admin user` |
| #50 | Rate limiting of failed logins |

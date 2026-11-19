# Authentication and authorization

Who a request comes from, how Lajstrom finds out, and what the principal may do. This page
describes local users, their passwords, HTTP Basic on every route, the roles and bindings that
decide every request ([Authorization](#authorization)), the bearer tokens of the OCI facade,
personal access tokens and deploy tokens, service accounts, the bootstrap of the first admin and
`whoami`. The
normative rules are [rbac.md](spec/rbac.md); the model is [ADR 0007](adr/0007-rbac-model.md);
where the token signing key lives is [ADR 0010](adr/0010-bearer-token-signing-key.md).

## Principals

A principal is who performs a request. Its canonical form names it in role bindings, in the
audit log, in publication sessions and in the access log ([rbac.md](spec/rbac.md) §1):

| Principal | Form | From |
| --- | --- | --- |
| local user | `user:<uid>` | HTTP Basic with the user's password; HTTP Basic or a bearer with a personal access token of the user ([Tokens](#tokens)) |
| service account | `user:<uid>` | HTTP Basic or a bearer with a personal access token of the account only ([Service accounts](#service-accounts)) |
| deploy token | `token:<id>` | HTTP Basic `deploy:<token>` ([Tokens](#tokens)) |
| anonymous | `anonymous` | a request without credentials |
| client certificate | `cert:spiffe://…`, `cert:<bundle>/<name>` | a certificate of a configured CA, on `GET /v2/token` ([Node and workload identity](#node-and-workload-identity)) |
| Kubernetes service account | `k8s:<cluster>/<namespace>/<service account>` | its token as the password of `/v2/token` ([Node and workload identity](#node-and-workload-identity)) |
| CI job | `ci:<issuer>/<project>` | a CI token, minted at `/-/oidc/token` for the OIDC token of GitHub Actions or GitLab CI ([Token exchange for CI](#token-exchange-for-ci)) |

A personal access token acts as its user: its principal is the user's, narrowed by the token's
scopes. A user may come from an OIDC login, and its groups from the login's claims
([OIDC login and groups](#oidc-login-and-groups)).

There are no users in the configuration file, by decision: users live in the index database
with the rest of the control plane, are created with `lajstrom admin user` and with the users
API ([api.md](api.md#users)), and are the same on every replica.

## Local users and passwords

A user name is 1 to 64 bytes of lowercase letters, digits, `.`, `_` and `-`, starting with a
letter or a digit. It is what HTTP Basic carries before the colon, so it has no colon, no space and no upper case. `deploy` is reserved: `deploy:<token>` is how a
deploy token authenticates. A password is at least `auth.passwords.minLength` characters (8 by
default) and at most 1024 bytes of UTF-8, any characters unless the rules ask for letters and
digits ([Password rules](#password-rules-your-own-password-change-at-first-use)), that does
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
- `admin user list` shows NAME, UID, KIND, DISABLED, CREATED, OWNER (a service account's,
  [Service accounts](#service-accounts)); `admin binding list` shows the
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
  minute per replica, and never lock the user out of the tokens. The lockout, which locks a
  password for longer and across the replicas, is a setting of its own ([The
  lockout](#the-lockout)), and the throttles stay beside it.
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
([operations.md](operations.md#failed-logins-and-the-edge)), or the lockout's, which counts in
the index. The budgets are constants of `internal/auth`, not settings.

**What is recorded.** Every refused credential, a password, a token or a certificate, is an
`authentication refused` line at info with the account's uid, never the name as it was typed;
a throttle that trips, of an account or of an address, is a `login.block` row of the audit log,
at most 60 a minute per replica, and so is a lock of the lockout
([operations.md](operations.md#failed-logins-and-the-edge), [rbac.md](spec/rbac.md) §1, §5).

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

### The lockout

With `auth.lockout.duration` set, the wrong passwords of an account are counted in the index,
across the replicas, and the 10th in a row locks the account's password for that duration
([rbac.md](spec/rbac.md) §1, Local passwords; [ADR 0028](adr/0028-local-passwords-off-or-locked-out.md)).
It is `0` by default, which counts nothing and locks nothing, or from `5m` to `24h`; `30m` is PCI
DSS 8.3.4's ([configuration.md](configuration.md#auth)).

- **What counts.** A wrong password of a live, enabled user that has one, on any route that
  takes a password: Basic credentials, `/v2/token`, `npm login`, `POST /api/v1/login`, and the
  current password of a user's own change ([below](#password-rules-your-own-password-change-at-first-use)). Ten is a
  constant, the most PCI DSS allows. A password that verifies sets the count back to `0`. Names
  that no user has, disabled users and users without a password are not counted: their
  passwords never verify, and the throttles above bound them.
- **A locked password** is refused whatever it is, as a wrong one is: `401`, after one
  verification against the fixed hash that a name no user has costs, so that neither the answer
  nor its time tells a locked account from a wrong password or from a name no user has. The
  lock is read with the user, before [the cache](#the-verification-cache), so a remembered
  password does not get past it. A refusal while locked counts against the throttles, and
  neither lengthens the lock nor counts toward the next one.
- **The message.** While the lockout is set, every `401` that refuses a user name and a
  password says what locks one, the same for every account and every reason, on every route
  that takes one: `the user name or the password was refused: 10 wrong passwords in a row lock
  a password for 30m`. A person whose password stops working learns why; nobody learns more than
  the configuration says.
- **Only the password.** The user's personal access tokens, its OIDC logins and the bearer
  tokens of `/v2` keep working: whoever knows a user name can stop that user's password, and
  nothing else. CI that uses a token is not stopped. The last enabled admin is locked like
  anyone; the command line unlocks it.
- **The end of a lock.** It ends after the duration, or earlier by `POST
  /api/v1/users/<name>/unlock` ([api.md](api.md#unlock-post-apiv1usersuserunlock)), by `lajstrom
  admin user unlock <name>`, or by an admin's new password for the user, each with a
  `user.unlock` audit row when a lock was in force. Check who asks before you unlock: that is PCI
  DSS 8.3.4's "until the user's identity is confirmed".
- **What is recorded.** A lock is a `login.block` row with the `limit` `lockout`, `failures` 10
  and `until`, once per lock whatever the replicas, and a `password locked` line at info. The
  users API and `lajstrom admin user list` show a locked user's `lockedUntil`.
- **Replicas and upgrades.** The count holds across the replicas. A replica with the setting
  `0`, or of a release before migration 0036, neither counts nor reads a lock, so set it once
  every replica runs a release that knows it; a change takes a restart. A restore of a snapshot
  unlocks every user.

### Password rules, your own password, change at first use

Where local passwords stay, `auth.passwords` sets their rules ([rbac.md](spec/rbac.md) §1, Local
passwords; [configuration.md](configuration.md#auth)); where they are off, the IdP's rules apply,
and none of this.

- **The rules.** A password has at least `auth.passwords.minLength` characters, `8` by default
  and `12` for PCI DSS 8.3.6, counted as Unicode code points of the password as it was sent, which
  is not normalized: four CJK characters are four, not the twelve bytes they take. It is at most
  1024 bytes of UTF-8 and does not start with `ljs_pat_` or `ljs_dep_`. With
  `auth.passwords.requireLettersAndDigits` it holds a letter and a digit, of any script (Unicode
  categories L and Nd): `пароль пароль 7` passes, `correct horse battery` does not. The rules are
  checked wherever a password is set: `POST /api/v1/users`, both forms of `POST …/password`,
  `POST /api/v1/bootstrap`, `lajstrom admin user create` and `set-password`. A password that
  breaks one is `400`, exit status 1 on the command line, naming the rule and its setting, never
  the password. A password set before keeps working, since a hash cannot be checked: to have
  everyone choose one under a raised rule, set `mustChange` on them (below).
- **Your own password.** A user changes its password with its current one, which is the
  credential of the request: `POST /api/v1/users/<name>/password` with `{"currentPassword",
  "password"}` ([api.md](api.md#password-post-apiv1usersuserpassword)). The current password is
  checked as Basic credentials are, with the throttles and the lockout: a wrong one is `401` with
  the message of every refused password, and counts as a failure. The request needs no other
  credential, and its `Authorization` is ignored. The new password follows the rules and is none
  of the user's **last four passwords**, the current one and the three before it, each compared
  by argon2id (about 70 ms): one of them is `400`, `password: was used before`. The change is
  `204`, and the user's tokens are not touched. An admin's new password is not compared with the
  history, so that the admin does not learn which passwords the user had, but the hash it
  replaces goes into the history all the same.
- **Change at first use.** With `auth.passwords.changeAtFirstUse` (`true` for PCI DSS 8.3.5),
  every password that is not the user's own change, an admin's create or new password, through
  the API or on the command line, and the bootstrap's, gives the user `mustChange`. Until the user
  changes it, its password authenticates that change and nothing else: once it has verified,
  every other request with it is `403`, on every route, `/v2/token`, `npm login` and `POST
  /api/v1/login` included, with the message `the password must be changed before it is used for
  anything else: change it with lajstrom change-password, or POST /api/v1/users/{user}/password
  with currentPassword and password`, and it mints no token. A wrong password is `401` as always,
  so that only the holder of the password learns it. The user's tokens keep working. Without the
  setting, an admin's new password clears `mustChange`. Whatever the setting, an admin sets or
  clears it with `PATCH /api/v1/users/<name>` and `{"mustChange": true}` or `false`
  ([api.md](api.md#change-patch-apiv1usersuser)): to have a user choose a password under a raised
  rule, or every 90 days (PCI DSS 8.3.9; there is no maximum age). The users API answers
  `mustChange: true`, and `lajstrom admin user list` `(must change its password)`.
- **What is recorded.** Every change of a password is a `user.password` row, by the admin, or by
  the user itself for its own change, with `mustChange: true` in its details when the change left
  it set; a create under `changeAtFirstUse` has it in its `user.create` row too, and an admin's
  `PATCH` is a `user.mustchange` row. A password that a rule or the history refuses writes no
  row; a wrong current password is a failed login.
- **Upgrades.** Migration 0037 gives every user `mustChange` `false` and an empty history: the
  first change after it compares with the current password only. A replica of a release before
  it knows no user's own change and takes a password whose `mustChange` is set, and refuses a
  configuration with `auth.passwords`: set the keys once every replica runs a release that knows
  them. A snapshot keeps `mustChange` and the history.

From a terminal, `lajstrom change-password` makes the user's own change against a server, as
`lajstrom login` logs in ([Logging in](#logging-in)): it needs no configuration but the server's
address, and no credential but the passwords it asks for.

```console
$ lajstrom change-password alice --server https://art.example.com
Current password:
New password:
Repeat the new password:
The password of alice is changed.
```

It prompts without echo on a terminal; otherwise it reads the current password from the first
line of standard input and the new one from the second, and nothing else, so that a script pipes
them in. It takes no password as a flag. Without `--server` it talks to `server.listen` of the
configuration on loopback, as `lajstrom admin bootstrap` does, with the pin of
`server.tls.certFile` when there is one. A refused current password, a new one that breaks a
rule or that was used before, and a throttled account exit with status 1 and the server's
status and message.

### Turning local passwords off

With `auth.localPasswords: false`, no password is a credential, an admin's included: people log
in through the IdP ([OIDC login and groups](#oidc-login-and-groups)), whose lockout, password
rules and MFA apply, and get their personal access tokens from `lajstrom login`
([rbac.md](spec/rbac.md) §1, Local passwords). `serve` refuses the setting while
`auth.oidc.issuer` is empty.

- Every password is `401` before the user is read, on every route that takes one, with the
  message `passwords are off on this server: use a personal access token, or log in through the
  IdP with lajstrom login`, the same for every name. No argon2id runs and no throttle counts.
- Personal access tokens, deploy tokens, CI tokens, client certificates and Kubernetes tokens
  work as before.
- No password is set: a `password` in `POST /api/v1/users` and `POST …/password` are `400`, and
  `lajstrom admin user create` (but with `--service`) and `set-password` fail, naming the
  setting. The stored passwords are kept, refused; `true` makes them credentials again.
- The bootstrap token is not made valid ([Bootstrap](#bootstrap-the-first-admin)).
- The break-glass path is the command line against the database: `lajstrom admin token create
  --user <admin>` makes an admin a token while the IdP is down; under the
  [admin mode](#administrative-access-with-a-second-factor) such a token reaches no admin role,
  and the command line does the admin's work itself.
- What the API made only for a password, a personal access token through
  `POST /api/v1/users/<name>/tokens`, a service account's token and a deploy token over a mask,
  is made with a web UI's fresh sign-in through OIDC in its place ([Rules](#rules), #1323).
- `GET /api/v1/info` leaves `password` out of `authModes`, so that a UI shows no password form.

The lockout does nothing while local passwords are off, and neither do the keys of
`auth.passwords`, which are accepted: the IdP's rules apply.

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
- **`DPoP ljs_pat_…`** on `/api/` only ([RFC 9449](https://www.rfc-editor.org/rfc/rfc9449)):
  the token of a web UI's sign-in, bound to a key of the browser, with a proof of that key in the
  header `DPoP` ([api.md](api.md#the-dpop-bound-token)). A refused proof or token is `401` with
  `WWW-Authenticate: DPoP error="invalid_dpop_proof"` or `error="invalid_token"`, with
  `error_description="idle"` for a token unused for longer than `auth.login.idleTimeout`, which
  the server then revokes ([rbac.md](spec/rbac.md) §10, #1277). Such a token
  is refused in Basic credentials and as a bearer, on every route; `DPoP` outside `/api/` is
  refused too.
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
| `attacher` | `read`, `attach`: signatures, SBOMs, scan reports and other referrers pushed through `/v2` to the versions of the repo, and no version (#89) |
| `writer` | `read`, `write`: uploads, `push`; `write` holds `attach` |
| `maintainer` | `read`, `write`, `delete` and `manage`: deleting versions, changing the repo, its deploy tokens, reindexing it |
| `admin` | everything on every repo, and the system: users, bindings, creating a repo, every user's tokens, the audit log |

A `maintainer` on `*/*` has every repo but not the system; the system needs the `admin` role.
`attach` is for a job that signs or scans what others publish: with it alone, `cosign attest`,
`cosign sign` and `oras attach` go through, and a manifest that is no referrer is `403` naming
`write` ([rbac.md](spec/rbac.md) §2).

### Bindings

A binding is `(principal, role, mask)`. The principal is `user:<name>` or `user:<uid>` of an
existing user, stored as `user:<uid>`, or `group:<name>`, a group of OIDC logins, which need not
have members ([Groups and group bindings](#groups-and-group-bindings)), or a `cert:` or `k8s:`
principal of a client certificate or a Kubernetes service account, or a pattern of them
([Node and workload identity](#node-and-workload-identity)); the mask is `<format>/<repo>`, each segment a name or `*`: `raw/assets` exactly, `raw/*` every raw
repo, `*/payments` the repo `payments` of every format, `*/*` everything. The repo need not
exist: a binding may come before its repo, and outlives it. The rights of a principal are the
union of its bindings. A deploy token has no bindings (its repo, or its mask, and its scopes
are its rights), and neither has `anonymous` (a repo's `anonymousRead` is).

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
lajstrom: principal: token:3 cannot be bound: a binding is for user:<name>, group:<name>, cert:<name> or k8s:<name>; anonymous reads through a repo's anonymousRead, a deploy token gets its repo and scopes
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
subject and the id, role and mask in the details. With `auth.approval: second-admin`, a binding
of the role `admin` or on `*/*` made through the API waits for a second admin, and the command
line makes it at once ([Changes that wait for a second admin](#changes-that-wait-for-a-second-admin)).

### How a request is decided

For the principal of the request and an action on `<format>/<repo>`, in this order:

1. the credential first: a personal access token with scopes keeps only those actions, an
   admin's too, so that an admin's `read` token does not write;
2. an `admin` binding allows everything;
3. a deploy token reaches its own repo, or the repos its mask matches, with the actions of its
   scopes, never `delete` or `manage`;
4. the bindings whose mask matches the repo allow the actions of their roles;
5. `anonymous`, and a user its bindings do not allow, may `read` a repo whose `anonymousRead` is
   on, when `auth.anonymousRead` is on too: both switches, and they are off by default (a
   deploy token stays on its repo or its mask). The server's switch is configuration
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
  `read`, `write` and `attach`, one or more: the credential of a CI job; `read` and `attach` that
  of a scanner ([guides/sbom-and-scans.md](guides/sbom-and-scans.md)). Or bound to a mask of repos, `oci/*` or
  `*/talos`, which only an admin creates: the credential of a fleet of nodes
  ([Over a mask](#a-deploy-token-over-a-mask)).

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
the lookup of the hash is the check, one query for the token and one for its user or repo
(none for a mask), a miss costing what a hit costs.

### Rules

| | Personal access token | Deploy token | Deploy token over a mask |
| --- | --- | --- | --- |
| principal | `user:<owner's uid>` | `token:<id>` | `token:<id>` |
| bound to | its user | one repo | a mask (`oci/*`, `*/talos`), the repos created later included |
| `scopes` | none (the user's full rights), or `read`, `write` and `attach`, one or more | `read`, `write` and `attach`, one or more, required; `delete`, `manage` and anything else are `400`; a group's deploy token reads only | the same |
| expiry | required: 90 days when not given, at most 365 days ahead (`400` beyond, naming the bound); a service account's by `auth.serviceAccounts.maxTokenTTL` instead ([Service accounts](#service-accounts)) | optional: none is never; with `auth.deployTokens.maxTTL`, the bound when not given, at most the bound ahead (`400` beyond, naming the bound; below) | the same |
| `name` | 1 to 64 bytes of printable UTF-8, unique among the user's live tokens (`409`) | the same, among the repo's live tokens | the same, among the live tokens over the same mask |
| created with | a password, or a web UI's fresh sign-in through OIDC in its place (`403` with any other token, #249, #1323) | a password, or a personal access token without scopes, of a user with `manage` on the repo | the password of a user with the `admin` role, or its fresh sign-in (`403` with any other token, #911, #1323) |

**A personal access token is created with a password**, never with a token: a request whose
credential is a personal access token, with scopes or without, the admin's too, or a deploy
token gets `403` on `POST /api/v1/users/<name>/tokens` before its body is read. A stolen token
would otherwise mint tokens that last up to a year and outlive its revocation. The one token
that stands for the password is a web UI's sign-in through OIDC whose person authenticated at
the IdP within the last 10 minutes, bound to the browser's key (#1323): the UI signs in again
with `prompt=login`, and a sign-in that is older is asked for more, `401` with `max_age=600`
([api.md](api.md#asking-for-more)). The same holds for a deploy token over a mask, and so an
OIDC user, who has no password, makes its tokens through the API too. The token made has
`createdBy` `token:<id>` and `signIn` in its `token.create` row. An OIDC login
mints one itself ([OIDC login and groups](#oidc-login-and-groups)); `npm login` checks the password (#308), and
so does the sign-in of a web UI, [`POST /api/v1/login`](api.md#sign-in-post-apiv1login), whose
token expires after `auth.login.tokenLifetime` (12 hours) and is bound to a key of the browser
(DPoP), so that it works nowhere but `/api/` with a proof of that key; `lajstrom admin token create` needs
no credential, as it runs against the database. A personal
access token without scopes still lists and revokes its user's tokens. CI that wants a fresh personal access token asks
for it with a password, or gets a deploy token instead ([In CI](#in-ci)).

A live token is neither revoked nor expired. A scope grants its action and nothing else:
`write` does not include `read`, so a client that pushes through `/v2`, and reads blobs before
it writes them, needs both. The bounds of a person's personal access tokens are constants, not
configuration; those of a service account's ([Service accounts](#service-accounts)) and of
deploy tokens are configuration.

**A deploy token's lifetime** is bounded by `auth.deployTokens.maxTTL`
([configuration.md](configuration.md)), none by default (#1286). With a bound, a token of a repo
or over a mask created without `--expires`, `expiresAt` or `expiresIn` expires at the bound, and
an expiry beyond it is `400`, naming the field, the key and the latest expiry it allows, through
the API and on the command line alike, which reads the bound of the configuration it is given.
The bound counts when a token is created: a token made before it was set or lowered keeps its
expiry, or none, until it is revoked; raising it lengthens no token. Such a token, live, with no
expiry or expiring more than the bound after its creation, is answered with `beyondMaxTTL: true`
in every view of the API, so that `GET /api/v1/tokens?kind=deploy&live=true` lists what is left
to replace ([api.md](api.md#every-token-get-apiv1tokens)), and `serve` logs at warn at start how
many there are. Without a bound, a deploy token created without an expiry never expires.

Expiry and revocation count at the next request, on every replica: the token is read from the
database on every request. A token is expired once `expiresAt` is past, without leeway.
`lastUsedAt` is the time of the last request with the token, less than a minute behind it: a
request writes it when the value read with the token is a minute old or more, and a write only
moves it forward, so it is written at most once a minute per token on all replicas together. A
write that fails is logged at debug, refuses nothing, and is made again at the next request. A PAT is refused
while its user is disabled, and a repo deleted takes its deploy tokens with it, but not a token
over a mask, which names no repo.

### What a token may do

The scopes narrow what the bindings give, on every route and at `/v2/token`:

- a PAT without scopes is its user: what the user's bindings allow;
- a PAT with scopes gets `read` with `read`, `write` with `write`, `attach` with `attach` or
  `write`, and never `delete` or `manage`; anything else is `403` (S-03, X-14);
- a deploy token is confined to its repo: on any other it is `404`, the answer of a repo that
  does not exist, since it has no access there at all (S-04); on its repo it gets `read`,
  `write` and `attach` per its scopes, `write` holding `attach`, and `403` for an action outside them, `delete` and `manage` above all:
  `DELETE` of a version, `PATCH` or `DELETE` of the repo, its deploy tokens (X-14).
  `GET /api/v1/repos` lists its repo only;
- a deploy token over a mask is a deploy token on every repo its mask matches, and `404`
  elsewhere: a repo created later that the mask matches is reached from its creation on.
  `GET /api/v1/repos` lists the repos of its mask;
- the PATs of a user are the user's own to list, create and revoke, or the admin's, and are
  created with a password only ([Rules](#rules)). The deploy tokens of a repo need `manage` on
  it, and the deploy tokens over a mask the `admin` role, which creates them with its password
  only. A deploy token or a PAT with scopes asking for any of them is `403`;
- at `/v2/token` the same rules narrow the token ([above](#the-token-endpoint)); `/v2` itself
  takes that token and no other bearer. A `push` asked for by a principal that has `attach` and
  not `write` is granted as `attach`: the token's access lists `pull` and `attach`, and `/v2`
  takes with it blob uploads and referrers only.

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

On GitHub Actions and GitLab CI a job needs no stored secret at all: it trades the OIDC token
its platform gives it for a CI token of minutes ([Token exchange for CI](#token-exchange-for-ci)).
Elsewhere, or without `auth.ci`, a pipeline gets a deploy token of its repo with `write`, plus `read` when its client reads
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

### A deploy token over a mask

A node sends one credential per registry host: Talos has one `RegistryAuthConfig` per host,
containerd one `registry.configs."<host>".auth`, Kubernetes one entry per host in an
`imagePullSecrets` secret. A cluster that pulls through Lajstrom reads several repos of that
host, `oci/ghcr`, `oci/k8s` and `oci/talos` say, so a deploy token of one repo does not fit, and
a personal access token expires within a year on every node. A deploy token over a mask covers
them all with one secret that need not expire (#911):

```sh
lajstrom admin token create --mask 'oci/*' --name talos-nodes --scopes read | jq -r .secret
```

or, as an admin, `POST /api/v1/deploy-tokens` with `{"repoMask": "oci/*", "name": "talos-nodes",
"scopes": ["read"]}` ([api.md](api.md#deploy-tokens-over-a-mask)). The node logs in as `deploy`
with the secret as its password, on the frontends and at `/v2/token`, as with any deploy token.

- The mask has the grammar of a binding's ([Bindings](#bindings)): `<format>/<repo>`, each
  segment a name or `*` for any. `oci/*` is every `oci` repo, `*/talos` the repo `talos` of every
  format, `oci/k8s` one repo, `*/*` everything.
- **A mask matches the repos created after the token too.** A new `oci` repo is readable with a
  token over `oci/*` from its creation on, and so by every node that holds it. Choose the mask
  for the repos the nodes need and no more: name the repos so that one mask covers them
  (`*/talos`, `oci/*` on a server whose `oci` repos are all for the nodes), or give each
  cluster an exact mask per repo where it has a credential per host anyway.
- With `auth.approval: second-admin`, one whose scopes hold `write`, made through the API, waits
  for a second admin: the `POST` answers `202` with a request and the secret, which works once
  another admin approves the request ([Changes that wait for a second admin](#changes-that-wait-for-a-second-admin)).
- Only the `admin` role creates one, **with its password**: a `maintainer`, even on `*/*`,
  gets `403`, and so does a request whose credential is a token, a personal access token with
  scopes or without, the admin's too, or a deploy token, before its body is read. A stolen admin
  token must not mint a token that needs no expiry and outlives it (decided on #911,
  2026-11-14, as #249 decided for personal access tokens). An admin without a password, an
  OIDC user, makes one with `lajstrom admin token create --mask`.
- `lajstrom admin token create --mask` asks for no password: like every `lajstrom admin`
  command on this page, it writes the database of the configuration, and whoever holds the
  database's credentials is the operator. Its `token.create` row has the source `cli`.
- A repo deleted does not take it with it.
- It is listed, and revoked, by an admin: `GET /api/v1/deploy-tokens` and `DELETE
  /api/v1/deploy-tokens/<id>`, or `lajstrom admin token list --mask` and `revoke`. A repo's own
  list (`GET …/repos/<format>/<repo>/deploy-tokens`) does not show the tokens over a mask that
  match it.
- Everything else is a deploy token's: `read` and `write` only, never `delete` or `manage`,
  the expiry optional, the secret shown once, revoked from the next request on, `token.create`
  and `token.revoke` in the audit log. With `write`, it may write through a group whose hosted
  member the mask matches too.
- During a rolling update to the release that brings it, a replica of the release before
  refuses it (`401`): it works once every replica runs the new release.

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
lajstrom admin token create --mask 'oci/*' --name talos-nodes --scopes read
lajstrom admin token list --user alice         # or --repo raw/assets, or --mask 'oci/*'
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
with a personal access token has; none from the CLI; and `maxTTL`, the seconds of
`auth.deployTokens.maxTTL` or `auth.serviceAccounts.maxTokenTTL` for a deploy token or a service
account's token created under that bound, none without one) and `token.revoke` (`id`, `kind`,
`name`; and `reason` `idle`, `lastUsedAt` and `idleTimeout` for a web UI's sign-in that the server
revoked as idle, by `anonymous` with the source `idle`: [rbac.md](spec/rbac.md) §10),
by the principal of the request (`anonymous` from the CLI), with the source and the request id. The subject is `user:<owner>` for a PAT and `<format>/<repo>` for a
deploy token, whose repo is also the row's `repo`; for a deploy token over a mask it is the mask,
and the row has no `repo`. Never the secret, never its hash. Revoking a
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

## Service accounts

A service account (#1020, [ADR 0021](adr/0021-service-accounts.md), [rbac.md](spec/rbac.md) §1) is
a user for a system: a release pipeline, a mirror job, a deployment controller. Some companies
require one per system, registered with a person or a team who answers for it, instead of a
person's account or a deploy token with nobody behind it. It is a user of kind `service`:

- it has **no password** and no OIDC identity, and nothing logs it in. It authenticates with its
  personal access tokens only, as Basic `<name>:ljs_pat_…` or `Authorization: Bearer ljs_pat_…`,
  wherever a person's token works: the frontends, `/v2/token`, the management API;
- its principal is `user:<uid>`, as every user's: it is bound with
  [Bindings](#bindings) like a person, renamed, disabled, enabled and deleted like one, and the
  audit log names it;
- it has an **owner**, required: a person, `user:<name>`, or a group of the OIDC logins,
  `group:<name>` ([Groups and group bindings](#groups-and-group-bindings)). A group need not have
  members yet. The owner answers for the account and gets no right on it by owning it: what the
  account may do is its bindings, and who manages it is the admin role.

```sh
lajstrom admin user create --name ci-release --service --owner group:platform
lajstrom admin binding create --principal user:ci-release --role writer --mask 'pypi/*'
lajstrom admin token create --user ci-release --name github-actions --scopes read,write
lajstrom admin user list --kind service
lajstrom admin user set-owner ci-release user:alice
```

Through the API, `POST /api/v1/users` with `{"name": "ci-release", "kind": "service", "owner":
"group:platform"}`, `GET /api/v1/users?kind=service`, `PATCH /api/v1/users/ci-release` with
`{"owner": "user:alice"}` ([api.md](api.md#users)); the tokens as every user's,
`POST /api/v1/users/ci-release/tokens`.

**Its tokens** are personal access tokens: without scopes they are the account's full rights,
with `read`, `write` or both they narrow its bindings, so one job gets a read token and another a
read-write token of the same account. The account has no password, and a personal access token
is created with a password only ([Rules](#rules)), so its tokens are created by an admin with the
admin's password, or on the command line; a token of the account lists and revokes the account's
tokens, when it has no scopes, and never creates one.

**Their lifetime** is bounded by `auth.serviceAccounts.maxTokenTTL`
([configuration.md](configuration.md)), 365 days by default, instead of a person's 90 days by
default and 365 at most: a token created without `--expires` or `expiresIn` expires at the bound,
and an expiry beyond it is `400`, naming the key. `0` bounds nothing: such a token then never
expires, as a deploy token without `auth.deployTokens.maxTTL`. Lowering the bound changes
nothing for the tokens made before: revoke them to apply it. Those beyond the bound are marked
`beyondMaxTTL` and counted at start, as deploy tokens are ([Rules](#rules)).

**The owner.** What disabling the owner does is `auth.serviceAccounts.onOwnerDisabled`. With
`flag`, the default, it changes nothing for the account, which keeps working: the users API
answers `ownerDisabled: true`, and `admin user list` shows `user:alice (disabled)`. Give the
account another owner, or disable it. With `disable`, disabling the owner disables its enabled
service accounts too, in the same transaction, each a `user.disable` row with `owner` in its
details. Enabling the owner again does not enable them: enable each one that should run again
(`lajstrom admin user enable ci-release`), after giving it another owner if the owner left.
Deleting a user who still owns a live service account is `409` with either value, naming the
accounts: hand them over with `set-owner`, or delete them, first. A group owner is never
disabled: it has no state in Lajstrom.

**Admins.** A service account may have the admin role, but it does not count for the rule that
keeps one enabled admin (a person must remain), nor for the bootstrap: it has no password and
cannot make itself a token once its own expire. That rule never refuses disabling or deleting a
service account.

**Audit.** Its requests are rows of `user:<uid>` with its name, and `GET /api/v1/audit` adds
`"principalKind": "service"` to them. Its creation is `user.create` with `kind` and `owner` in
the details, and a change of its owner `user.owner`, with `from` and `owner`.

**During a rolling update** to the release that brings service accounts, a replica of the release
before takes a service account for a user without a password: its tokens with an expiry work
there, and a token without one, which only the bound `0` makes, is refused (`401`). With the bound
`0`, create expiring tokens until every replica runs the new release, or update the replicas
first.

## Inactive users

PCI DSS 8.2.6 asks that an account inactive for 90 days be disabled ([rbac.md](spec/rbac.md) §1,
Inactive users; [compliance.md](compliance.md)). Lajstrom does it at the account's next
authentication, with no task that looks for inactive users:

```yaml
auth:
  disableInactiveUsersAfter: 2160h # 90 days; 0, the default, disables nobody; else at least 24h
```

**The last activity.** Every user has `lastSeenAt` (the users API, `lajstrom admin user list`):
the time of its last successful authentication, with its password (HTTP Basic, `docker login`'s
`/v2/token`, `npm login`, a web UI's sign-in), a personal access token of its own, or an OIDC
login. A service account authenticates with its tokens only, so their use is its activity. It is
written as a token's `lastUsedAt` is, when the value read with the user is a minute old or more,
so it is less than a minute behind. A `/v2` bearer is issued for an authentication, which counts;
its own use does not. An admin's change of the user is not the user's activity. Recording an
activity moves neither the user's revision nor its `ETag`.

**Inactive.** A user is inactive once the setting has passed since its last activity, or since
its creation when it has none. The users API answers `inactive: true` for it, `lajstrom admin
user list` writes `(inactive)` after its LAST SEEN, and `serve` warns at start how many there
are, naming the last enabled admin when it is among them. Read the list before turning the setting
on, or before lowering it: the users it reaches are inactive at once.

**At its next authentication** a credential of an inactive user that verifies is refused, `401` as
a disabled user's (a password counts as a failed login), and the user is disabled: a
`user.disable` row by `anonymous` with `source: inactivity`, the client's address and the request
id, and `reason: inactive`, `lastSeenAt` and the setting as `after` in its details. A wrong
password or token disables nobody. The page of an OIDC login says that the account was disabled
for inactivity, and that an admin can enable it; the IdP's sign-in does not make the user active.
With `auth.serviceAccounts.onOwnerDisabled: disable`, its service accounts are disabled with it,
as with every disabling. From the moment the setting has passed, no credential of the user is
taken on any replica; the row says `disabled` from the first one tried.

**Coming back.** `lajstrom admin user enable alice` (or `PATCH` with `"disabled": false`) sets
`lastSeenAt` to the time of the enabling, so that alice's next request does not disable her again.

**The last enabled admin** is never disabled so: its credential is taken, its activity recorded,
and a warning is logged. A service account's and a group's admin bindings do not count for that
rule: an inactive service account with the admin role is disabled as any.

**Upgrading** to the release that brings it (migration 0034): a user with a password counts from
the upgrade, since its password logins left no record before; a service account or an OIDC user
counts from the last use of its tokens or its last login, so one unused for longer than the
setting is inactive at once. A snapshot taken before 0034 is restored alike. During a rolling
update, a replica of the release before records no activity and disables nobody, and a user
enabled there may be disabled again at its first authentication on a newer one: enable it again
there.

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

With `auth.localPasswords: false` ([Turning local passwords off](#turning-local-passwords-off))
neither makes an admin who could log in, so neither is taken: `serve` makes no bootstrap token
valid, `POST /api/v1/bootstrap` is `404`, and `admin user create` sets no password. The first
admin is a person: log in through the IdP once, so that the user exists, then, against the
database, `lajstrom admin binding create --principal user:<name> --role admin --mask '*/*'`.
`serve` says so at start while no admin exists ([rbac.md](spec/rbac.md) §6). The same holds under
`auth.admin.requireMFA` ([Administrative access with a second factor](#administrative-access-with-a-second-factor)):
a local admin would never reach the role, so the first admin is an OIDC user given its binding
on the command line, and `admin user create --admin` makes a binding without effect.

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
lajstrom admin user list                    # NAME, UID, KIND, DISABLED, CREATED, OWNER; --kind service
lajstrom admin user disable alice           # refused from the next request on
lajstrom admin user enable alice
lajstrom admin user set-password alice      # the same two sources, or --password-file <file>
lajstrom admin user unlock alice            # ends the lock of auth.lockout.duration
```

`set-password --password-file` reads the password from a file, the way `--password-stdin` reads
standard input: the whole of it without the trailing newline. It is for the recovery Job of the
Helm chart, whose image has no shell to put a Secret on standard input
([helm.md](deploy/helm.md#a-lost-password)).

A name that exists is refused (`user alice already exists`, exit status 1). Every change is an
audit event ([rbac.md](spec/rbac.md) §5) with the user as its subject: `user.create` (its
details say whether the user is an admin), `user.password`, `user.disable`, `user.enable`,
`user.unlock` (with the end of the lock it ended, `lockedUntil`), by `anonymous` with the source
`cli` and the host name of the machine the command ran on (`host`).
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
expiry. It writes no file: the token goes where you put it. A local user with a password changes
it from a terminal the same way, with `lajstrom change-password <name> --server <url>`
([Password rules, your own password, change at first use](#password-rules-your-own-password-change-at-first-use)).

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
and the next login makes a new user. With `auth.oidc.refresh.interval`, a person disabled or
removed at the IdP loses their logins within the interval without it
([Asking the IdP again](#asking-the-idp-again)).

### A web UI

The web UI ([lajstrom-ui](https://github.com/lajstrom/lajstrom-ui)) signs a person in through
the same login, and the token goes to the UI instead of a page
([rbac.md](spec/rbac.md) §10). The UI starts the login with `return_to`, one of its pages, the
challenge of a PKCE verifier it keeps, and `dpop_jkt`, the thumbprint of its DPoP key; the
callback sends the browser back to that page with a one-time code in the fragment, `#code=…`,
and the UI trades the code and the verifier, with a DPoP proof of that key, for a token bound to
the key at [`POST /api/v1/login`](api.md#sign-in-post-apiv1login). The token expires after
`auth.login.tokenLifetime` (12 hours), or `tokenLifetime` when that is shorter, and signing out
revokes it.

`return_to` must be on the origin of `server.externalURL`, a UI at `/ui/` behind the same edge,
or of [`server.corsOrigins`](configuration.md#server), a UI served from elsewhere: the login
redirects to no other site. The code is useless without the verifier, which never leaves the
UI's tab, and without the key, which never leaves the browser, and it is redeemed once, within
the 10 minutes of the login. The IdP needs nothing
more: its redirect URI stays `<server.externalURL>/-/oidc/callback`.

The token keeps when and how the person authenticated at the IdP: the ID token's `auth_time`,
`amr` and `acr`, each when the IdP sends it, which the views of the token answer as `signIn`. A
step that needs a fresh sign-in (a [CI trust policy that
publishes](#policies-in-the-configuration-or-through-the-api), a token made in place of a
password) takes a token whose `auth_time` is at most 10 minutes old; one that needs a second
factor (the same policies, the [admin mode](#administrative-access-with-a-second-factor)) one
whose `amr` or `acr` shows one: by default `mfa`, or two kinds of factor such as `pwd` and `otp`;
with `auth.oidc.mfa.amr` or `auth.oidc.mfa.acr` set ([configuration.md](configuration.md#auth)),
one of their values, such as `[hwk, swk]` for `amr`, or the `acr` your IdP gives a flow with a
second factor. A sign-in short of a step is asked for more: `401` with
`WWW-Authenticate: DPoP error="insufficient_user_authentication"`, and `max_age=600` when it is
not fresh ([api.md](api.md#asking-for-more)). The UI then signs in again with `prompt=login`
beside `return_to`: Lajstrom asks the IdP for `prompt=login` and `max_age=600`, so that the
person authenticates again, with their second factor if the IdP asks for one. Lajstrom sends no
`acr_values`: the IdP's policy for the client decides how a person authenticates.

### Asking the IdP again

A login asks the IdP once, and its token then lasts `tokenLifetime`: a person disabled or removed
at the IdP keeps it until it expires, unless an admin disables their user in Lajstrom too
([Offboarding a person](compliance.md#offboarding-a-person)). With `auth.oidc.refresh.interval`
([#1282](https://github.com/lajstrom/lajstrom/issues/1282),
[ADR 0037](adr/0037-asking-the-idp-again-about-each-login.md), [rbac.md](spec/rbac.md) §8),
`serve` asks the IdP again about each login, every interval, with the refresh token of the
login's code exchange, and ends the login when the IdP refuses it:

```yaml
auth:
  oidc:
    scopes: [openid, profile, email, groups, offline_access]  # where the IdP needs it
    refresh:
      interval: 15m  # 0, the default, is off; 1m to less than tokenLifetime
keys:
  masterKeyFile: /run/secrets/lajstrom-master-key  # seals the refresh tokens
```

- **What is kept.** The callback keeps the login's refresh token in the index, sealed with the
  [master key](operations.md#the-master-key), one per login: it is never logged, audited, shown or
  put in a snapshot, a database backup holds it sealed, and `lajstrom admin key rewrap` seals it
  again. The setting needs a master key: without one `serve` refuses the configuration. While a
  replica does not hold the key that seals, its callback answers `503` with `Retry-After: 30`
  before the code exchange. The mint of the login's token, at the callback, at the device's poll
  or for a web UI, joins it to the token.
- **Who asks.** Every 30 seconds each replica claims the logins that are due, with a claim per
  login in the index, so that one replica at a time asks about one login, and calls the IdP's
  token endpoint at most 4 at once, 10 seconds each at most. A login is due the interval after
  its mint, then after each answer, less up to a tenth of the interval at random, so that the
  logins of one morning spread out. There is no leader and no other service: the replicas share
  the logins through the claims.
- **The IdP confirms.** The login goes on. A new refresh token, when the IdP rotates them, replaces
  the old one, and an ID token in the answer, checked as at a login but for the nonce and of the
  login's identity, replaces the user's groups when they differ, with a `user.groups` row in the
  audit log. Nothing else moves: the token's expiry, its sign-in, the user's last activity.
- **The IdP refuses** with `invalid_grant`, and nothing else is a refusal: the person was disabled
  or removed, signed out, or their session at the IdP ended. That login's token is revoked, with a
  `token.revoke` row by `anonymous`, `source: refresh` and `reason: refused`. The user stays
  enabled, and its other tokens are as they were; each login of a person removed at the IdP is
  refused at its own refresh, all within the interval. The person's next request with that token
  is `401`, a bearer of `/v2` issued for it before lasts its 20 minutes, and a new login fails at
  the IdP.
- **The IdP fails** (down, slow, `429`, `5xx`, `invalid_client`, an answer that does not parse, an
  ID token that does not verify): nothing ends. The login is asked again after 1, 2, 4 … minutes
  up to the interval, and the error is logged at warn by its kind. While the IdP does not answer,
  logins last their lifetime, as without the setting, and new logins fail.
- **A login without a refresh token** (an IdP that issued none) works as before and lasts its
  lifetime: the callback logs `the IdP issued no refresh token`, its `user.login` row says
  `refresh: false`, and `serve` warns at start when the IdP's discovery document lists no
  `refresh_token` grant. A disabled user's logins are refreshed too, so that enabling the user
  brings back no login the IdP ended.
- **Off** (`0`), no login keeps a refresh token, and `serve` deletes the ones it kept at its start.

What it does not end, and the offboarding procedure still does: the personal access tokens a
person made through a web UI's fresh sign-in, the tokens an admin made for them, the logins whose
IdP issued no refresh token, and every login while the IdP does not answer. The cost is one
request to the IdP's token endpoint per live login and interval: 1,000 logins at `15m` are about
1.2 a second. `lajstrom_oidc_refreshes_total` counts the answers by outcome
([Metrics](operations.md#metrics)).

Lajstrom adds no scope and no parameter of its own: the IdP must issue refresh tokens to the
client. Set it up at your IdP as below, then log in once and check that the `user.login` row says
`refresh: true`, and, after the interval, that the IdP's answer kept the groups (an IdP whose
refreshed ID token lacks the groups claim drops them at the first refresh):

- **Keycloak.** It issues a refresh token without `offline_access`, bound to the person's browser
  session: the login then ends with that session (SSO Session Max, 10 hours by default), and
  Lajstrom's refreshes keep the session from idling out for every client of the realm. Prefer
  `offline_access` (the client scope of that name, assigned to the client): an offline session of
  its own (Offline Session Idle, 30 days by default), which a user disabled or removed, or the
  offline session revoked, ends. The realm's Revoke Refresh Token makes each refresh token single
  use. The groups mapper must add the groups to the ID token.
- **Okta.** Allow the Refresh Token grant on the app and add `offline_access` to the scopes. The
  app's refresh token rotation, with its grace period, and the authorization server's access
  policy, which sets a refresh token's lifetime, are Okta's; a deactivated user is refused. The
  refreshes count against the org's rate limit of the token endpoint.
- **Entra ID.** Add `offline_access`. It answers a new refresh token at each refresh; a disabled or
  deleted account, and sessions an admin revoked, are refused. The groups claim of the ID token is
  as the app's token configuration sets it, and a user in more groups than the token holds gets an
  overage claim, so no groups, as at a login.
- **Google.** It issues refresh tokens only for its own parameter `access_type=offline`, which
  Lajstrom does not send: Google's logins are not asked about again, each logs the warning, and
  they stay bounded by `tokenLifetime`.
- **Dex.** Add `offline_access`; it rotates refresh tokens at each use. At a refresh it asks its
  connector again where the connector can (LDAP, OIDC, GitHub among them); a connector that cannot,
  SAML for one, does not see a removal upstream.

## Administrative access with a second factor

With `auth.admin.requireMFA: true` ([configuration.md](configuration.md#auth); off by default;
PCI DSS 8.4.1), an `admin` binding, a user's own or a group's, counts only for the token of a web
UI's sign-in through OIDC, sent with a proof of the browser's key, whose sign-in shows a second
factor ([A web UI](#a-web-ui); [rbac.md](spec/rbac.md) §12, [ADR 0029](adr/0029-administrative-access-with-a-second-factor.md)).
The sign-in need not be fresh: its second factor counts for the token's lifetime,
`auth.login.tokenLifetime`, and `auth.login.idleTimeout` ends it sooner. Every other credential
keeps the principal's other bindings and loses `admin`, on every route, `/v2` included:

- **A password**, in Basic credentials or in a web UI's sign-in, and **a personal access token**,
  `lajstrom login`'s included: an admin whose only binding is `admin` does with them what
  anonymous does, and pushes nothing, reads no private repo, and `/v2/token` issues its bearer
  without the role. An admin who publishes gets a binding for it (a `maintainer` on `*/*` reaches
  every repo, never `system` nor `audit`); `admin retention` and `admin repo prefetch`, which go
  over HTTP, need one too.
- **A service account's and a local user's `admin` bindings** count for nothing: neither ever
  signs in through OIDC. They stay, and count again when the mode is off. `serve` warns at start
  how many there are, and, when no enabled OIDC user and no group has an `admin` binding, that no
  request can reach the role.
- **No admin from a terminal through the API**: `lajstrom login`'s token is a bearer of a day,
  bound to no key. The command line against the database, `lajstrom admin user|binding|token|ci-policy`,
  is unchanged: it is the break-glass path while the IdP is down. A token it makes reaches no
  admin role; using the API without the IdP takes turning the mode off, a restart recorded as a
  `config.change` row.
- **The first admin** is an OIDC user given its binding on the command line: the bootstrap token
  is not made valid ([Bootstrap](#bootstrap-the-first-admin)), and `serve` refuses the mode
  without `auth.oidc.issuer`.
- **Passwords and the lockout** are not concerned: a locked password locks no admin out, since no
  password reaches the role.

On a route of the admin role, an admin whose web UI sign-in shows no second factor is asked for
more, `401` with `insufficient_user_authentication` ([api.md](api.md#asking-for-more)); any other
credential of an admin gets `403`, naming `auth.admin.requireMFA` and the kind of the credential.
A principal without an `admin` binding gets the `403` of [the 401/404/403 rule](#the-401404403-rule),
and repo routes answer by the bindings that count, so the mode tells nothing of which repos
exist. `GET /api/v1/info` answers `adminMFA: true`, and the caller's `adminNeedsMFA: true` when
its `admin` binding is left out for the request's credential, so that a UI offers a sign-in with a
second factor ([api.md](api.md#server-information-get-apiv1info)). Every control-plane change made
with a web UI's sign-in has `signIn`, `{token, authTime, amr, acr}`, in its audit row's details:
an admin's change shows how the admin signed in at the IdP. During a rolling update, a replica of
the release before refuses a configuration with the key, and gives the role as before while it
runs: the mode counts once every replica runs a release that knows it. [operations.md](operations.md#administrative-access-with-a-second-factor)
has the steps to turn it on.

### Groups and group bindings

Each login replaces the user's groups with those of the claim `groupsClaim`, and they count
until the token of that login expires. A group removed at the IdP therefore stops granting at
the person's next login, or, with `auth.oidc.refresh.interval`, at the next refresh of one of
their logins whose answer has an ID token ([Asking the IdP again](#asking-the-idp-again)), on
every replica, for every token of the user, and at the latest when the login's token expires. A
name that is not 1 to 256 bytes of printable text is dropped.

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
code, state, ID token, refresh token or personal access token is logged. Every login writes
`user.login` to the audit log with the issuer, the subject and the groups (and `refresh` with
`auth.oidc.refresh.interval`), a first login `user.create`, and the token `token.create` with
`createdBy: oidc`. The [threat model](security/threat-model.md) has the flows.

Not yet: token exchange for CI (a GitHub Actions or GitLab OIDC token for a short-lived token,
proposed on #69; meanwhile a pipeline uses a [deploy token](#in-ci)), and joining an existing
local user to an identity.

## Approval of privileged changes

Two settings, both off by default, tie a change of the control plane to its approval
([rbac.md](spec/rbac.md) §13, [ADR 0030](adr/0030-approval-of-privileged-changes.md); PCI DSS
7.2.3 and 8.2.4): a second admin approves the changes that grant the most, and every change
carries a reason, the reference of its approval in your records.

### A reason with each change

Every change of the control plane through `/api/v1` takes `changeReason`: a ticket or a sentence,
1 to 1024 bytes of UTF-8 without control characters, as `--reason` (or `LAJSTROM_REASON`) on the
command line. It is a field of the JSON body, or `?changeReason=` on a route without one (a
`DELETE`, an unlock, a webhook's new secret, an approval, a rejection), never both; it is kept in
the change's audit row as `details.changeReason`, never `reason`, which a policy rule and a deny
keep with their own meaning:

```console
$ curl -s -u admin:<password> -X POST https://art.example.com/api/v1/bindings \
    -d '{"principal": "user:alice", "role": "writer", "repoMask": "pypi/*", "changeReason": "CHG-1042"}'
$ curl -s -u admin:<password> -X DELETE 'https://art.example.com/api/v1/bindings/7?changeReason=CHG-1043'
```

With `auth.requireReason: true`, a change without one is `400` naming `changeReason`, after the
role is checked and before anything is written, and the commands of `lajstrom admin user`,
`binding`, `token`, `repo`, `webhook`, `ci-policy` and `policy rule` that write exit with status
`2` without a reason (`admin policy rule create`, whose `--reason` is the rule's, takes
`LAJSTROM_REASON`). The changes are a repo's create, change and delete; every user route of the
admin role; a binding's create and delete; the tokens made or revoked for another user or a
service account, a repo's deploy tokens and those over a mask; a webhook's create, change, delete
and new secret; a policy rule's create and delete, a deny and its removal; a CI trust policy's
create, change, renewal, approval and delete; a request's approval and rejection. A person's own
tokens and password, the bootstrap, the writes of artifacts, a reindex and a purge of a proxy's
cache need none. `GET /api/v1/info` answers `requireReason`, so that a UI asks for one.

### Changes that wait for a second admin

With `auth.approval: second-admin`, three changes made through the API wait for a second admin,
and no other:

- a binding of the role `admin`, whatever its principal: a user, a group, a service account;
- a binding on the mask `*/*`, whatever its role;
- a deploy token over a mask whose scopes hold `write`.

The route checks what it checks for the change (the role, the credential, the body, the `409` of
a binding that exists or a name that is taken), then answers `202 Accepted` with a **request** in
its place, and its `Location`, `/api/v1/approvals/<id>`. A request grants nothing: no binding or
token exists until it is approved. The same change pending twice is `409`. A deploy token's
secret is made with the request and answered once in the `202`; it is no credential (`401`, an
unknown token) until the approval makes the token with it, with the expiry asked, and a request
rejected or expired leaves nothing to revoke. The approver never sees the secret.

```console
$ curl -s -u admin:<password> -X POST https://art.example.com/api/v1/bindings \
    -d '{"principal": "user:carol", "role": "admin", "repoMask": "*/*", "changeReason": "CHG-1050"}'
{"id":12,"kind":"binding","change":{"principal":"user:01a1…","role":"admin","repoMask":"*/*"},
 "changeReason":"CHG-1050","state":"pending","requestedBy":"user:01a0…",
 "requestedAt":"2026-11-17T09:00:00Z","expiresAt":"2026-11-24T09:00:00Z"}
$ curl -s -u root:<password> -X POST 'https://art.example.com/api/v1/approvals/12/approve?changeReason=CHG-1050'
{"id":12,…,"state":"approved","decidedBy":"user:01a2…","decidedAt":"…","decisionReason":"CHG-1050","result":"binding:31"}
```

- **Who requests** (#1393, [ADR 0035](adr/0035-second-admin-is-another-person.md)): a request
  is made with what its approval takes (below), the admin's password or a web UI's fresh sign-in
  through OIDC, never a token. `POST /api/v1/bindings` of a binding that waits is `403` with a
  token, a service account's included, and asks a sign-in that is not fresh for more (`401`); a
  deploy token over a mask took no token already. So a token that one admin made for another
  admin, or for a service account with `admin`, neither requests nor approves, whoever made it
  and whenever, and a service account, which has neither a password nor a sign-in, requests
  nothing: a person requests its change, or the command line makes it. Scripts and Terraform
  that request these changes with a token get `403`: they send a password, or the operator makes
  the change on the command line.
- **The approval** is an admin's other than the requester, compared by uid, with what a deploy
  token over a mask takes: the admin's password, or a web UI's fresh sign-in through OIDC in its
  place ([Rules](#rules)), never another token; under `auth.admin.requireMFA`
  ([Administrative access with a second factor](#administrative-access-with-a-second-factor)),
  a fresh sign-in with a second factor. It
  checks again what the request checked, and that the requester is still an enabled user, not a
  service account, with the admin role: one that fails is `409` naming it (the requester
  disabled or demoted, a service account's request made on a release before #1393, the
  principal deleted, the binding or the name made meanwhile, an expiry beyond a bound lowered
  meanwhile), and the request stays pending. It makes the change, which counts from the next
  request on, on every replica.
- **A rejection** is any admin's, with any credential that reaches the role; the requester's is a
  withdrawal.
- **An expiry**: a request still pending 7 days after it was made expires; each replica looks once
  a minute. A request decided or expired is `409` to another decision.
- **Who sees them**: `GET /api/v1/approvals[?state=pending]` and `GET /api/v1/approvals/<id>`,
  the admin role ([api.md](api.md#approvals)), and `lajstrom admin approval list` on the command
  line. `GET /api/v1/info` answers an admin `pendingApprovals`, the number it could approve
  ([Telling the second admin](#telling-the-second-admin)).
- **The command line**, `lajstrom admin binding create`, `user create --admin` and
  `token create --mask`, makes the change at once whatever the setting, and `user set-password`
  sets an admin's password: it is the operator's, the break-glass path, and the way to the
  second admin of a server that has one.
- **Creating a token for a service account**, or for another user, does not wait, whatever its
  bindings, so that rotating one takes one admin; such a token makes no request (above).
- **A user made an admin**: `POST /api/v1/users` with `admin: true` is `400`, naming the
  setting, and makes no user: make the user without `admin`, then request its `admin` binding
  with `POST /api/v1/bindings`, which waits (#1376). `lajstrom admin user create --admin` still
  makes both at once.
- **Another admin's password** (#1393): an admin's new password,
  `POST /api/v1/users/<name>/password` without `currentPassword`, is `400`, naming the setting,
  for a user who holds `admin`, by an `admin` binding of its own or of a group whose membership
  has not lapsed, enabled or disabled, whatever the admin mode, and whoever asks, the user itself
  included; it writes no row. An admin who knew another's password would sign in as it and
  approve its own requests. A person who holds
  `admin` changes its own password with `currentPassword`
  ([Your own password](#password-rules-your-own-password-change-at-first-use)), and
  `lajstrom admin user set-password` is the break-glass path; an admin who forgot its password
  may also have its `admin` binding deleted, its password set, and the binding requested again,
  which a second admin approves. A user without `admin` gets a new password as before.
- **What stays as it was**: enabling a disabled admin, a service account included, an unlock,
  `mustChange`, a rename and a change of owner are one admin's changes, made at once: under the
  two rules above, none gives an admin a credential of another that requests or approves. A
  password that an admin set while its user held no `admin` (a create, a new password) is still
  known to that admin once a second admin approves the user's `admin` binding: that approver
  decides, and the `user.create` or `user.password` row by another principal shows who set it.

With `second-admin`, `serve` warns at start when fewer than two enabled users have an `admin`
binding that counts (under the admin mode, OIDC users only): no request could be approved.
Turning the setting on leaves the bindings, the tokens and the passwords set before as they are:
an admin's password that another admin set stays known to that admin, so have each admin change
its own once the setting is on. Turning it to `none` makes every change at once again, with any
credential, and an admin sets any user's password as before; the requests made before stay, to
be approved, rejected or to expire. `GET /api/v1/info` answers `approval`, so that a UI expects
the `202`. The rules of #1393 take no setting and no migration: during a rolling update, a
replica of the release before still takes a token's request and sets an admin's password, so
reject the requests made during the update and read its `user.password` rows if that matters.
The approval of a CI trust policy that writes is `auth.ci.apiWriteApproval`, apart
([Token exchange for CI](#token-exchange-for-ci)).

### Telling the second admin

Lajstrom sends no mail and keeps no address of a person (#1336,
[ADR 0038](adr/0038-telling-the-second-admin.md)). Two things tell an admin that a request waits:

- **Webhook events.** Each step of a request's life is an event of the webhooks
  ([operations.md](operations.md#webhooks)): `approval.requested` when a request is made,
  `approval.approved`, `approval.rejected` (a rejection or the requester's withdrawal) and
  `approval.expired`. Each is written with its audit row, in its transaction: it is sent once its
  step commits, and never for a step that failed. The expiry's is written by the replica that marks
  the request expired, so it comes once, whatever the replicas. An event goes only to the
  webhooks of the admin, a mask with a `*` that covers the change's: a binding of `admin` and a
  binding on `*/*` to those of `*/*`, a deploy token over `oci/*` to those of `*/*` and `oci/*`; a
  repo's webhook gets none. It holds the request as `GET /api/v1/approvals/<id>` answers it and
  the names of its users (the requester, the decider, a binding's principal), never the deploy
  token's secret or its hash, nor the credential or the sign-in of either side.
- **A count.** `GET /api/v1/info` answers `pendingApprovals` while `caller.admin` is `true`: the
  requests pending, not expired, and not the caller's own. Any other caller gets none, so that
  nobody without the role learns that a request waits. A web UI shows it while the person acts; it
  asks without its token while the person does nothing, and gets no count then.

Route the events where the admins read them: a webhook of `*/*` with the four types, or the four
added to one you have, to a chat or a mail gateway that verifies the signature. A webhook whose
`events` is empty gets them from the upgrade on. Both hold whatever `auth.approval` says, so the
requests still pending once it is set back to `none` are counted, and their end is sent. During a
rolling update, a replica of the release before writes no event for what it requests, decides or
expires, answers no count, and refuses the new types in a webhook's `events` (`400`).

```json
{
  "specversion": "1.0",
  "id": "01a3c136-51a6-7883-a350-b1cf6000a23f",
  "type": "dev.lajstrom.approval.requested",
  "source": "https://art.example.com/api/v1/approvals",
  "subject": "approval:12",
  "time": "2026-11-17T09:00:00Z",
  "datacontenttype": "application/json",
  "data": {
    "schema": 1, "action": "approval.request", "audit": 4310,
    "actor": {"principal": "user:01a0…", "name": "admin"},
    "approval": {"id": 12, "kind": "binding", "change": {"principal": "user:01a1…", "role": "admin", "repoMask": "*/*"},
      "changeReason": "CHG-1050", "state": "pending", "requestedBy": "user:01a0…",
      "requestedAt": "2026-11-17T09:00:00Z", "expiresAt": "2026-11-24T09:00:00Z"},
    "names": {"user:01a0…": "admin", "user:01a1…": "carol"}
  }
}
```

### Audit

A request is `approval.request`, by the requester, subject `approval:<id>`, with its `kind`, its
`change` (a deploy token's without its hash) and `expiresAt`. Its approval is
`approval.approve`, by the approver, with the request's `kind`, `change` and `requestedBy` and the
approver's `changeReason`. Each has `credential`, `password` or the sign-in's `token:<id>`, as a
token's creation has `createdBy`: the proof that each side was a person's (#1393); a request made
on a release before has none, and a request or a password refused writes no row. The change
writes its own `binding.create` or `token.create` beside it, by the approver, with `approval`
`{id, requestedBy, requestedAt}`, the request's `changeReason` and, for a token, the request's
`createdBy`. A rejection is `approval.reject`, an expiry `approval.expire` by `anonymous` with
`source: expiry`. Each has `signIn` when it was made with a web UI's sign-in. Each of these four
rows is also the webhook event of its type, written in its transaction (above); the event takes
`requestId` from the row's details, and nothing else of them. Requests are kept, decided or not,
and are not in a snapshot.

## The access report and its review

PCI DSS 7.2.4 asks that user accounts and their privileges are reviewed every six months, and that
management acknowledges that access remains appropriate; 7.2.5.1 asks the same for system
accounts ([rbac.md](spec/rbac.md) §14, [ADR 0032](adr/0032-access-report-and-its-review.md)).
Lajstrom answers what such a review reads, in one document, and records that it took place.

### The report

`GET /api/v1/access-report` (the admin role) and `lajstrom admin access-report` (against the
database) answer **who has what at one moment**, read in one read-only transaction:

- `users`: every live user, a disabled one too, as the users API answers it, with its `principal`,
  its current `groups` and when each lapses ([Groups and group bindings](#groups-and-group-bindings)),
  its `bindings`, its own and its groups' (those have `via`, the group), and its live personal
  access tokens; a service account with its owner and `ownerDisabled`;
- `groups`: every group that has a binding, with its bindings and its `members`, the users whose
  current groups name it;
- `workloads`: every `cert:` and `k8s:` principal or pattern that has a binding
  ([Node and workload identity](#node-and-workload-identity)); Lajstrom keeps no last use of them;
- `deployTokens`: every live deploy token, of a repo and over a mask, with `principal`
  `token:<id>`;
- `ciPolicies`: every CI trust policy, of the configuration and of the API, an expired one too;
- `anonymous`: the repos that anonymous reads, while `auth.anonymousRead` is on;
- `settings`, what the flags read of the configuration, and `generatedAt`, the database's time of
  the read.

A token is answered as the tokens API answers it, with its `hashPrefix`: the report holds no
password, no secret and no hash, and no webhook (whose URL may hold a credential). Requests that
wait for a second admin grant nothing and are not in it. Reading it is not audited, as no read is;
the access log has it.

**The flags** are on each entry and each token, in this order: `admin` (an `admin` binding, its
own or a current group's), `allRepos` (a binding on `*/*`, a deploy token over `*/*`, a CI grant
on `*/*`), `adminWithoutEffect` (the `admin` binding of a service account or a local user under
`auth.admin.requireMFA`, which counts for nothing), `noExpiry` (a token or a CI trust policy
without an expiry), `unused` (no use for more than `unusedDays`), `beyondMaxTTL`, `ownerDisabled`
and `inactive` ([Inactive users](#inactive-users)). A reader tolerates a flag it does not know. A
user's last use is its `lastSeenAt`, a token's its `lastUsedAt`, either its creation when it has
none; `unusedDays` is 90 by default (PCI DSS 8.2.6), from 1 to 3650.

**The scope**: `all`, the default; `people`, the users of kind `local` and `oidc` and the groups,
for 7.2.4; `systems`, the service accounts, the workloads, the deploy tokens and the CI trust
policies, for 7.2.5.1, which your risk analysis may review at another frequency. A section out of
the scope is left out of the document; `anonymous` is in every scope. No other filter: a review
names one document.

**One document, one sha256.** The document is compact JSON in a canonical form: the keys of every
object in byte order, its lists in fixed orders (`users` by name, `groups`, `workloads` and
`members` by principal, tokens by id, `ciPolicies` by name, `bindings` by role, mask and `via`),
its times in UTC to the second, and a line feed at the end. The same rows at the same time are
the same bytes, from the API and from the command line, so that a review names the document by
the sha256 of its bytes. It is not paged; it grows with the rows, a few hundred bytes a binding or
a token. It changes with every use of a token (`lastUsedAt`): keep the copy you reviewed.

```console
$ lajstrom admin access-report --scope people > access-2026-h2.json
sha256:5f3c…e9a1
```

The command line reads the database as every `lajstrom admin` command does, so it works under
the admin mode, where no terminal reaches the admin role through the API, and while the IdP is
down. It writes the document on standard output and `sha256:<hex>` on standard error. `--format
csv` writes a table for a spreadsheet instead, one line per binding, token and CI grant, with the
columns `principal`, `kind`, `name`, `item`, `role`, `mask`, `scopes`, `expiresAt`, `lastUsedAt`,
`via` and `flags` (the entry's and the item's, joined by `;`), and on standard error the sha256 of
the JSON document of the same read, which is the one a review names; a cell that a spreadsheet
would take for a formula (it starts with `=`, `+`, `-`, `@`) gets a `'` before it. It writes
nothing, and its host's clock must agree with the database's within 60 seconds, as for every
command against the database ([operations.md](operations.md#clocks)).

### The review

`POST /api/v1/access-reviews` records that a person reviewed a report, as an audit row
`access.review` by that person:

```console
$ curl -s -u admin:<password> -X POST https://art.example.com/api/v1/access-reviews -d '{
    "sha256": "5f3c…e9a1", "generatedAt": "2026-11-18T10:00:00Z", "scope": "people",
    "note": "Removed bob (left in October) and two unused tokens, CHG-2210.\nAcknowledged by the CISO, 2026-11-20."}'
{"id":5120,"at":"2026-11-20T09:14:02.118374Z","principal":"user:01a0…","principalName":"admin",
 "action":"access.review","subject":"sha256:5f3c…e9a1","ip":"10.0.4.7",
 "details":{"generatedAt":"2026-11-18T10:00:00Z","note":"Removed bob …","requestId":"…","scope":"people",
 "sha256":"5f3c…e9a1","source":"api"},"state":"ok"}
```

- **Who**: the admin role, with any credential that reaches it; under `auth.admin.requireMFA`, a
  web UI's sign-in with a second factor, whose `signIn` the row keeps. No second admin and no
  `changeReason`: a review changes no access, and the note says what was done.
- **The body**: `sha256`, the 64 lowercase hexadecimal digits of the document's sha256, without
  `sha256:`; its `generatedAt` and `scope` (`all` when absent); a `note` of 1 to 4096 bytes of
  UTF-8 without control characters but line feeds: what was found and done, who in management
  acknowledged, the ticket. A field out of its bounds is `400` naming it, and nothing is written;
  so is a `generatedAt` more than 60 seconds after the server's time.
- **What it checks**: the form, not the report. Lajstrom keeps no report, and could not make the
  same one again, since every use of a token changes it: the evidence is the row and the document
  you keep with the ticket, whose sha256 the row names.
- **The answer** is `201` with the row as `GET /api/v1/audit` answers it. Past reviews are
  `GET /api/v1/audit?action=access.review`, newest first. The command line records none: its rows
  name nobody, and a review is a person's.

## Node and workload identity

A node or a pod can pull from Lajstrom with no secret of Lajstrom's stored on it
([rbac.md](spec/rbac.md) §9, [ADR 0025](adr/0025-node-and-workload-identity.md), #1019):

- a **node** presents a client certificate that your CA signed, as containerd and Talos can;
- a **pod** pulls with its own service account: the kubelet hands its credential provider a
  short-lived token of the pod's service account (KEP-4412), which Lajstrom verifies against the
  cluster's issuer.

Both are credentials of `/v2/token` only, which is all containerd needs: it asks there for the
bearer of every pull. No other route reads them. Each gives a principal of its own, which
bindings name like a user, with `reader`, `writer` or `maintainer` (never `admin`):

| Credential | Principal |
| --- | --- |
| a certificate of a SPIFFE bundle | `cert:spiffe://<trust domain>/<path>`, its SPIFFE ID |
| a certificate of a `cn` or `dns` bundle | `cert:<bundle>/<CN or first DNS name>` |
| a Kubernetes service-account token | `k8s:<cluster>/<namespace>/<service account>` |

A fleet is bound by a **pattern**, a principal whose last segment is `*`: it matches every
principal of its kind below it, at any depth.

```sh
lajstrom admin binding create --principal 'cert:spiffe://cluster-a/node/*' --role reader --mask 'oci/*'
lajstrom admin binding create --principal 'k8s:prod/ci/*' --role reader --mask 'oci/k8s'
lajstrom admin binding create --principal 'k8s:prod/ci/builder' --role writer --mask 'oci/builds'
```

`cert:spiffe://cluster-a/*` would also match the workloads of the trust domain, not only its
nodes: bind the narrowest prefix that names what you mean. These principals have no row, no
token and no owner; they are not users, deploy tokens or the service accounts of #1020, and the
access log, the audit log and `lajstrom.uploaded-by` name each node and each service account by
itself.

### Client certificates

Give Lajstrom the CAs whose certificates count, one bundle each:

```yaml
auth:
  clientCertificates:
    bundles:
      - name: cluster-a                 # a SPIRE or cert-manager CA that writes SPIFFE IDs
        caFile: /etc/lajstrom/cluster-a-ca.pem
        trustDomain: cluster-a          # identity: spiffe, the default
      - name: corp                      # a plain CA: the principal is cert:corp/<CN>
        caFile: /etc/lajstrom/corp-ca.pem
        identity: cn
```

A `spiffe` bundle takes the leaf's one URI SAN, which must be a SPIFFE ID of its trust domain; a
`cn` bundle the subject's CN, a `dns` bundle the first DNS name. The bundles are tried in order;
the first whose CAs verify the certificate, for client authentication, decides. The files are
read at start: a new CA takes a restart. The keys are in
[configuration.md](configuration.md#auth).

**Where the certificate comes from:**

- **Lajstrom terminates TLS** (`server.tls`): with a bundle, the listener asks every client for a
  certificate, naming the bundles' CAs. A client without one connects as before. A Go client,
  containerd among them, sends its certificate only when its chain names one of those CAs as its
  issuer, and goes on anonymous otherwise; a certificate that is sent and that no bundle signed
  fails the handshake.
- **Behind an edge**: the edge asks for the certificate and forwards it in a header, and Lajstrom
  reads that header only from the peers of `server.trustedProxies`. Set
  `auth.clientCertificates.forwardedHeader` to its name; [reverse-proxy.md](reverse-proxy.md#client-certificates)
  has nginx and Envoy. Lajstrom verifies the certificate again against its bundles, so the edge
  need not verify it, only forward it, and it must drop a header that a client sent.

**On the node.** containerd, in `/etc/containerd/certs.d/<host>/hosts.toml` (the `ca` only for a
private CA of Lajstrom's own certificate):

```toml
server = "https://lajstrom.example.com"

[host."https://lajstrom.example.com/v2/oci/k8s"]
  capabilities = ["pull", "resolve"]
  override_path = true
  ca = "/etc/containerd/certs.d/lajstrom.example.com/ca.crt"
  client = [["/etc/containerd/certs.d/lajstrom.example.com/node.crt", "/etc/containerd/certs.d/lajstrom.example.com/node.key"]]
```

Talos, beside the mirrors of [the Talos guide](guides/talos.md) §3, since 1.12 (PEM):

```yaml
apiVersion: v1alpha1
kind: RegistryTLSConfig
name: lajstrom.example.com
clientIdentity:
  cert: |-
    -----BEGIN CERTIFICATE-----
    …
  key: |-
    -----BEGIN EC PRIVATE KEY-----
    …
```

or in `machine.registries` (base64 of the PEM):

```yaml
machine:
  registries:
    config:
      lajstrom.example.com:
        tls:
          clientIdentity:
            crt: LS0tLS1CRUdJTi…
            key: LS0tLS1CRUdJTi…
```

The node needs no `auth` for the host: containerd asks `/v2/token` with a `GET` and no
credential, over the same TLS configuration, and gets the bearer of its certificate's principal.
To check a certificate by hand:

```sh
curl -s --cert node.crt --key node.key 'https://lajstrom.example.com/v2/token?scope=repository:oci/k8s/pause:pull' | jq -r .token | cut -d. -f2 | base64 -d
{"access":[{"type":"repository","name":"oci/k8s/pause","actions":["pull"]}],"sub":"cert:spiffe://cluster-a/node/w1",…}
```

With `log.level: debug`, Lajstrom logs `client certificate` with the principal and the bundle,
or `client certificate refused` with the reason.

**Revocation.** There is none: Lajstrom reads no CRL and asks no OCSP responder, and a
certificate counts until its `notAfter`. Keep lifetimes short where something renews them (SPIRE,
cert-manager's CSI driver: a day or less). To cut a node off sooner, remove its binding (from its
next `/v2/token` on, 20 minutes at most for a bearer it holds), or rotate the bundle's CA. On
Talos the certificate lives in the machine configuration and lasts until you patch it
(`talosctl patch machineconfig`, applied without a reboot): bind such nodes one by one, not by a
pattern, where you may need to cut one off before its certificate ends. The gain over a deploy
token is that the key never crosses the wire, not a shorter life.

### Kubernetes service-account tokens

The kubelet (Kubernetes 1.34 and later; 1.33 with the feature gate
`KubeletServiceAccountTokenForCredentialProviders`) can hand an image credential provider a token
of the pod's service account, for an audience its configuration names. `lajstrom
credential-provider` is that provider: it answers the token as the password of the image's
registry, and Lajstrom's `/v2/token` takes it for a cluster it knows. The images the node pulls
for itself (kubelet, etcd, the Talos installer) are not pulled for a pod: they need a client
certificate or a [deploy token](#a-deploy-token-over-a-mask).

Give Lajstrom the cluster:

```yaml
server:
  externalURL: https://lajstrom.example.com
auth:
  kubernetes:
    clusters:
      - name: prod
        issuer: https://kubernetes.default.svc.cluster.local   # kubectl get --raw /.well-known/openid-configuration | jq -r .issuer
        caFile: /etc/lajstrom/prod-ca.pem    # the cluster's CA, for an issuer on the API server
        # audience: https://lajstrom.example.com   the default, server.externalURL
```

Lajstrom reads the issuer's discovery document and its JWKS, without credentials, at the first
token; the keys are cached as the OIDC login's. An EKS, GKE or AKS issuer is public. The API
server of your own cluster serves them to the service accounts only, so let anyone read them,
and make the issuer reachable from Lajstrom:

```sh
kubectl create clusterrolebinding service-account-issuer-discovery \
  --clusterrole=system:service-account-issuer-discovery --group=system:unauthenticated
```

An API server whose `AuthenticationConfiguration` limits anonymous requests to some paths, as
Talos's does (`/livez`, `/readyz`, `/healthz`), needs `/.well-known/openid-configuration` and
`/openid/v1/jwks` among them too ([the Talos guide](guides/talos.md) §3.2).

The kubelet asks the API server for a token of Lajstrom's audience, which a node may do only when
RBAC allows it (`ServiceAccountNodeAudienceRestriction`, which Kubernetes 1.37 enforced in the
Talos spike):

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: lajstrom-image-pull-audience}
rules:
  - verbs: ["request-serviceaccounts-token-audience"]
    apiGroups: [""]
    resources: ["https://lajstrom.example.com"]   # the audience; resourceNames would narrow it to service accounts
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: lajstrom-image-pull-audience}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: lajstrom-image-pull-audience}
subjects: [{kind: Group, name: "system:nodes", apiGroup: rbac.authorization.k8s.io}]
```

Without it, the kubelet logs `is not authorized to request tokens for audience` and pulls with
no credential of the pod's.

On each node, put the `lajstrom` binary, under the provider's name, in the kubelet's provider
directory and configure it (`--image-credential-provider-config`,
`--image-credential-provider-bin-dir`; on Talos, a `KubeCredentialProviderConfig` document and the
directory `/usr/local/lib/kubelet/credentialproviders`, the guide's §3.2):

```yaml
apiVersion: kubelet.config.k8s.io/v1
kind: CredentialProviderConfig
providers:
  - name: lajstrom                 # the file in the provider directory
    args: [credential-provider]
    apiVersion: credentialprovider.kubelet.k8s.io/v1
    matchImages: ["lajstrom.example.com"]
    defaultCacheDuration: 1m
    tokenAttributes:
      serviceAccountTokenAudience: https://lajstrom.example.com   # the cluster's audience
      requireServiceAccount: true
      cacheType: Token
```

A pod names the image on Lajstrom's host, not through a mirror: the kubelet asks the provider of
the registry the image names. A token is the pod's for one image pull; the provider tells the
kubelet to reuse it until a minute before it expires. Lajstrom checks its signature by the issuer's keys, its `iss`, its
audience, its `exp` (the legacy tokens of Secrets, which have none, are refused) and that its
`sub` names the service account of its `kubernetes.io` claim. A token that does not verify is
`401` and counts as no failed login; an issuer whose keys never loaded is `503`. Lajstrom does
not ask the cluster whether the pod still exists: the token counts until it expires.

To check by hand, with a token of the audience:

```sh
TOKEN=$(kubectl -n ci create token builder --audience https://lajstrom.example.com)
curl -s -u "k8s:$TOKEN" 'https://lajstrom.example.com/v2/token?scope=repository:oci/k8s/pause:pull' | jq -r .token | cut -d. -f2 | base64 -d
{"access":[{"type":"repository","name":"oci/k8s/pause","actions":["pull"]}],"sub":"k8s:prod/ci/builder",…}
```

Both were measured on a Talos cluster, with Lajstrom terminating TLS and behind nginx
([the Talos guide](guides/talos.md) §3.1 and §3.2).

**Not yet:** certificates and service-account tokens on any route but `/v2/token`; CRL and OCSP;
SPIFFE federation.

## Token exchange for CI

A job of GitHub Actions or GitLab CI publishes with no Lajstrom secret stored in CI: it posts the
OIDC token its platform issues it to `POST /-/oidc/token` (RFC 8693 token exchange) and gets a
**CI token**, `ljs_ci_…`, that lives 15 minutes, for the principal `ci:<issuer>/<project>`
([rbac.md](spec/rbac.md) §11, [ADR 0026](adr/0026-token-exchange-for-ci.md)). Trust policies in
the configuration decide which tokens are taken and what their CI token may do; nothing is
stored per job but the audit row of the exchange.

### Setting it up

Name the platforms and write a policy per thing a project may do:

```yaml
server:
  externalURL: https://art.example.com
auth:
  ci:
    issuers:
      - name: github                       # the principal is ci:github/<owner>/<repo>
        platform: github
        issuer: https://token.actions.githubusercontent.com
      - name: gitlab                       # ci:gitlab/<group>/…/<project>
        platform: gitlab
        issuer: https://gitlab.com         # or your GitLab's URL
        # audience: https://art.example.com   the default, server.externalURL
    policies:
      - name: app-release                  # tags of acme/app, in its release environment
        issuer: github
        claims:
          repository_id: "123456789"       # gh api repos/acme/app --jq .id
          ref: refs/tags/v*
          environment: release
        grants:
          - {role: writer, mask: pypi/internal}
          - {role: writer, mask: oci/acme}
      - name: acme-read                    # every repository of the acme organization, every ref
        issuer: github
        claims:
          repository_owner_id: "987654"    # gh api orgs/acme --jq .id
        grants:
          - {role: reader, mask: "*/*"}
      - name: platform-main                # protected main of the projects of a GitLab group
        issuer: gitlab
        claims:
          namespace_id: "42"               # the group's ID, on its page
          ref: main
          ref_protected: "true"
        grants:
          - {role: maintainer, mask: "oci/*"}
```

A token is taken when one or more policies of its issuer match it: every claim a policy names
must have one of its values. Its CI token gets the grants of every policy that matched. The
claims a policy may name:

| | GitHub Actions | GitLab CI |
| --- | --- | --- |
| the project, by id (one required) | `repository_id`, `repository_owner_id` | `project_id`, `namespace_id` |
| names, beside an id | `repository`, `repository_owner`, `actor` | `project_path`, `namespace_path`, `user_login` |
| patterns: `*` any run but `/`, `**` any run | `ref`, `environment`, `workflow_ref`, `job_workflow_ref` | `ref`, `ref_path`, `environment`, `ci_config_ref_uri` |
| values | `ref_type`, `ref_protected`, `event_name`, `runner_environment`, `repository_visibility`, `actor_id` | `ref_type`, `ref_protected`, `environment_protected`, `pipeline_source`, `runner_environment`, `deployment_tier`, `project_visibility`, `user_id` |

GitHub's `ref` is the full ref, `refs/heads/main` or `refs/tags/v1.2.3`; GitLab's `ref` is the
short name, `main`, and its `ref_path` the full one. A value may be a list, any of which matches:
`event_name: [push, workflow_dispatch]`.

The rules that keep a policy from publishing more than you meant, each refused at start with the
policy and the key named:

- **An id, not only a name.** A deleted organization's, user's or group's name can be registered
  again by anyone, and their repository of the old name would match. Name the project by its id;
  a name beside it is checked too.
- **A ref or an environment to write.** A policy that grants `writer` or `maintainer` names `ref`,
  `ref_path` or `environment`: without one, every branch, and every pull request of the
  repository, publishes. A policy that only reads may match every ref, so that the jobs of pull
  requests read your internal packages.
- **A pull request never writes.** Whatever its conditions, a policy that writes does not match a
  GitHub token of `pull_request`, `pull_request_target`, `pull_request_review` or
  `pull_request_review_comment`, nor a GitLab pipeline of `merge_request_event` or
  `external_pull_request_event`: such a job may run a fork's code with the base project's
  identity. GitHub gives a fork's `pull_request` no token at all; `pull_request_target` runs on
  the base's `ref` and is the one that matters.
- **No `*` alone,** no `*` outside the claims of a ref, an environment or a workflow, and no claim
  the table does not list.

What remains is yours to pin: a workflow of a protected ref that runs untrusted code
(`workflow_run`, `issue_comment`) has the base's ref and event. Use an `environment` with
required reviewers or deployment branches, `job_workflow_ref` to name the one workflow that
publishes (`acme/app/.github/workflows/release.yml@refs/tags/*`), and `runner_environment:
github-hosted` (or `gitlab-hosted`) to keep self-hosted runners out.

GitHub Enterprise Server is `platform: github` with its issuer, `https://<host>/_services/token`;
a GitLab of your own behind a private CA takes a `caFile`. A `ci:` principal takes no binding
(`400`): its rights are its policies' grants.

### Policies in the configuration, or through the API

A policy lives in one of two places, each its owner (ADR 0015):

- **The configuration**, `auth.ci.policies`: reviewed in git with the deployment, changed with a
  restart; `serve` logs each one with its claims and grants at start. The API shows them with
  `source: config` and refuses to change or delete them (`409`).
- **The API**, for the admin role: `/api/v1/ci/policies` ([api.md](api.md#ci-trust-policies)) or
  `lajstrom admin ci-policy` on the database. The same rules hold, `400` naming the field, and the
  issuer must be one of the configuration. A change counts from the next request on, on every
  replica, without a restart: each create, change and delete moves `auth_version`, which every
  replica reads before an exchange and before a request with a CI token. Each is an audit row,
  `ci.policy.create`, `ci.policy.update` (with the `previous` policy) or `ci.policy.delete`, with
  the policy's issuer, claims and grants, and an event of the webhooks. The command line makes the
  policies that read, lists them all, and deletes any.

```sh
curl -fsS -u admin -X POST https://art.example.com/api/v1/ci/policies -H 'Content-Type: application/json' -d '{
  "name": "lib-read", "issuer": "github",
  "claims": {"repository_id": ["555"]},
  "grants": [{"role": "reader", "mask": "pypi/internal"}]}'
lajstrom admin ci-policy create --name lib-read --issuer github --claim repository_id=555 --grant reader:pypi/internal
lajstrom admin ci-policy list
```

**A policy of the API that publishes takes more.** One that grants `writer` or `maintainer`
(made, changed, or renewed) needs:

- **A fresh sign-in with a second factor**: the admin signs in to a web UI through OIDC again,
  with the UI's step-up (`/-/oidc/login?…&prompt=login`, which asks your IdP for `prompt=login` and
  `max_age=600`), and makes the change within 10 minutes, with the token of that sign-in. Your IdP
  must put `auth_time` in its ID tokens, and `amr` or `acr` must show a second factor: `mfa`, or
  two kinds of factor such as `pwd` and `otp`, or `hwk` and `pin`, or the values of
  `auth.oidc.mfa` ([A web UI](#a-web-ui)). A web UI's sign-in short of it is asked for more, `401`
  with `insufficient_user_authentication` ([api.md](api.md#asking-for-more)). A password, a
  personal access token, `lajstrom login`'s token, a deploy token and the command line never
  count, `403`: they make the policies that read only.
- **A second admin**, with `auth.ci.apiWriteApproval: second-admin` (the default): the policy
  waits, `pending`, and grants nothing until another admin, signed in the same way, approves the
  revision they read (`POST …/<name>/approve` with `If-Match`). The admin who made the change
  cannot approve it. On an install with one admin, set `none`, or declare the policies that publish
  in `auth.ci.policies`, where the review of the configuration's history is the second pair of
  eyes.
- **Repos you allow**: each mask it writes to must be within `auth.ci.apiWriteRepos` (empty by
  default: no policy of the API publishes). Publishing to other repos is the configuration's.
- **An expiry**: 180 days at most, the default. Renew it before (`POST …/<name>/renew`, the same
  sign-in and approval; the policy keeps working until the renewal is approved). From its
  `expiresAt` on it grants nothing, its CI tokens included, and it shows `state: expired`.

```yaml
auth:
  ci:
    apiWriteRepos: ["pypi/internal", "oci/*"]
    apiWriteApproval: second-admin
```

Every create, approval, change, renewal, expiry and delete of a policy of the API is an audit row
(`ci.policy.create`, `.approve`, `.update`, `.renew`, `.expire`, `.delete`, with the sign-in that
made it) and an event of the webhooks of the admin (`ci.policy.created` and the others,
[webhooks](spec/webhooks.md)): point a webhook of `*/*` at your alerting. Whoever steals an admin's
token or password alone can add a policy that reads, not one that publishes. A snapshot carries the
policies of the API, never the configuration's.

### GitHub Actions

The job asks GitHub for a token of Lajstrom's audience, exchanges it, and hands the CI token to
its clients, masked in the log:

```yaml
permissions:
  id-token: write     # the OIDC token
  contents: read
jobs:
  release:
    runs-on: ubuntu-latest
    environment: release
    steps:
      - uses: actions/checkout@v4
      - name: Lajstrom token
        run: |
          oidc=$(curl -fsS -H "Authorization: Bearer $ACTIONS_ID_TOKEN_REQUEST_TOKEN" \
            "$ACTIONS_ID_TOKEN_REQUEST_URL&audience=https://art.example.com" | jq -r .value)
          token=$(printf %s "$oidc" | curl -fsS https://art.example.com/-/oidc/token \
            -d grant_type=urn:ietf:params:oauth:grant-type:token-exchange \
            -d subject_token_type=urn:ietf:params:oauth:token-type:jwt \
            --data-urlencode subject_token@- | jq -r .access_token)
          echo "::add-mask::$token"
          echo "LAJSTROM_TOKEN=$token" >> "$GITHUB_ENV"
      - name: Upload the wheel
        run: twine upload --repository-url https://art.example.com/pypi/internal/ dist/*
        env:
          TWINE_USERNAME: __token__
          TWINE_PASSWORD: ${{ env.LAJSTROM_TOKEN }}
      - name: Push the image
        run: |
          echo "$LAJSTROM_TOKEN" | docker login art.example.com -u ci --password-stdin
          docker push art.example.com/oci/acme/app:${GITHUB_REF_NAME}
```

The audience must be the issuer's `audience`, `server.externalURL` by default: a token of
GitHub's default audience, the owner's URL, is refused. A failed exchange says why: `no trust
policy matches the token: issuer github, repository acme/app, ref refs/heads/feature,
environment none, event_name push`.

### GitLab CI

The job declares the token in `id_tokens`, with Lajstrom's audience:

```yaml
release:
  image: python:3.13
  rules:
    - if: $CI_COMMIT_BRANCH == "main"
  id_tokens:
    LAJSTROM_ID_TOKEN:
      aud: https://art.example.com
  script:
    - apt-get update -qq && apt-get install -qq -y jq
    - |
      LAJSTROM_TOKEN=$(printf %s "$LAJSTROM_ID_TOKEN" | curl -fsS https://art.example.com/-/oidc/token \
        -d grant_type=urn:ietf:params:oauth:grant-type:token-exchange \
        -d subject_token_type=urn:ietf:params:oauth:token-type:jwt \
        --data-urlencode subject_token@- | jq -r .access_token)
    - TWINE_USERNAME=__token__ TWINE_PASSWORD="$LAJSTROM_TOKEN" twine upload --repository-url https://art.example.com/pypi/internal/ dist/*
    - echo "$LAJSTROM_TOKEN" | docker login art.example.com -u ci --password-stdin
```

### What a CI token does

- It is a credential wherever a deploy token is: the password under any user name on every route
  and on `/v2/token` (docker, oras, helm, twine, Maven's `settings.xml`, curl's `-u`), and
  `Authorization: Bearer` outside `/v2` (npm's `_authToken`).
- It may do what the grants of its policies give, as a deploy token over a mask does: `404` on a
  repo no grant's mask matches, `403` for an action its roles lack there. It never reaches the
  system resources, and it creates no token: a personal access token, a deploy token of a repo
  (with `maintainer` too) and one over a mask are `403`.
- It lives `auth.ci.tokenLifetime`, 15 minutes by default and 20 at most; a job that runs longer
  exchanges again, with a new OIDC token where its platform gives one. It is not revoked alone:
  delete its policy through the API (from the next request on), or remove it from the
  configuration and restart, or `lajstrom admin key rotate --token --drop`, which ends every
  bearer of `/v2` too. A bearer `/v2/token` issued on it lives
  its own 20 minutes.

### Audit

Each exchange that mints a token is a `token.exchange` row of the `ci:` principal: the issuer,
the policies, the token's id (`details.token`), its expiry, and the claims of the table with the
run's (`run_id`, `run_attempt`, `sha` on GitHub; `pipeline_id`, `job_id`, `sha` on GitLab), never
a token nor GitLab's `user_email`. The rows of the job's pushes, deletes and fetches carry
`details.ciToken`, the same id, through the bearer of `/v2/token` too, and its manifests
`lajstrom.uploaded-by: ci:<issuer>/<project>`. The row says `outcome: minted`.

A refused exchange is a `token.exchange` row of `outcome: refused` with its `reason` and the
issuer, and, for a token that verified, its `ci:` principal and the claims that say who and where
(the ids, the project, `ref`, `ref_type`, `environment`, the event); a token that did not verify is
`anonymous` and tells nothing, its claims being anyone's. Anyone can get a valid token from a
repository of their own, so the rows are rate-limited on each replica: one a minute for a project
(`<issuer>/<project id>`) or, for a token that did not verify, for a client address, the next row
carrying the refusals held back as `suppressed`. A job that retries against a missing policy writes
a row a minute; every refusal is logged at info.

```sh
curl -s -u admin "https://art.example.com/api/v1/audit?principal=ci:github/acme/app" | jq -c '.events[] | [.action, .repo, .details.ciToken // .details.token]'
```

**Not yet:** narrowing a CI token (`scope`), revoking one alone, other CI platforms, and the
registries' own routes for trusted publishing (`pypa/gh-action-pypi-publish`, `npm publish`
without a token), which can sit on the same policies later.

## What comes next

Linking a local user to an identity (#69). Nothing else of authentication is open.

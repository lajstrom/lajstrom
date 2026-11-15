# 0021. Service accounts: users of kind service, with an owner and long-lived tokens

- Status: proposed
- Date: 2026-11-15
- Deciders: gecube

## Context

Many companies require that a system authenticates with a registered service account, not with
a person's account nor with a token that no account stands behind (#1020). Such an account has
an owner who answers for it, the audit log names it, and it outlives the person who set it up.
GitLab has service accounts at the group and instance level for this; Harbor has robot
accounts.

What Lajstrom has ([rbac.md](../spec/rbac.md)):

- A **local user** has a password and personal access tokens, which expire within 365 days, 90
  by default, and are created with a password only, never with a token (#249).
- An **OIDC user** (§8) has no password; its logins mint its tokens.
- A **deploy token** is bound to one repo or, since #911, to a mask, with no principal behind it
  and no bindings. It serves a fleet of nodes, but it is not an account, and the compliance
  processes that ask for an account with an owner do not take it.
- A **group** (§8) is a `group:<name>` of the claims of OIDC logins. It has no row and no state:
  it exists through the identities that name it, and lapses with them.
- `users.kind` is a column already, `local` or `oidc`, and no code decides anything by it.

A service account is close to an OIDC user, a user without a password. Beyond that it needs an
owner, a way to get tokens, and a token lifetime that a system can live with. The issue leaves
open: whether its tokens can be narrowed below its bindings, whether it is listed with the
users, who may own it, what its owner's going does to it, how long its tokens live, and who
creates it.

## Considered options

### 1. The principal

- **A. A user of kind `service`**, principal `user:<uid>` as every user. Bindings, sessions, a
  bearer's `sub`, `lajstrom.uploaded-by`, the audit rows and a webhook's actor keep one form,
  which the release before reads. The kind shows where people read: the users API and the
  answers of the audit log.
- **B. A principal of its own, `service:<uid>`.** An audit row says `service:` by itself, but
  every place that reads `user:` takes a second form, and a replica of the release before
  refuses or skips the bindings, sessions and rows of the new prefix during a rolling update.

### 2. The scopes of its tokens

- **A. Personal access tokens of the account** (`tokens.kind` `pat`) with their scopes: `read`,
  `write` or both narrow its bindings as they narrow a person's. One CI job gets a read token,
  another a read-write one, of the same account.
- **B. Tokens with the account's full rights only.** A job that only pulls would hold a
  credential that pushes.

### 3. Where it is listed

- **A. With the users**, `GET /api/v1/users`, whose resource has `kind` already, filtered by
  `?kind=service`. Its name is in the names of the users anyway (Basic carries it), and every
  route of a user works for it.
- **B. A collection of its own**, `/api/v1/service-accounts`: clearer to browse, but it doubles
  every route of a user (get, rename, disable, delete, tokens) over the same rows.

### 4. Who may own it

- **A. A user or a group**: a live person (local or OIDC, not a service account), or a
  `group:<name>` of §8. A group suits the team that runs a system: the account does not follow
  one person.
- **B. A user only**: simpler, but the account changes hands with every reorganisation.

A group has no state in Lajstrom, so only a user owner is ever disabled or deleted.

### 5. When its owner is disabled or deleted

- **A. Flag.** A disabled owner changes nothing for the account, which the API marks
  `ownerDisabled`; an admin gives it another owner or disables it. Deleting a user who owns a
  live service account is `409`, naming the accounts: an account's owner is always a live user
  or a group, and the admin decides who takes the account when the person is deleted.
- **B. Disable with the owner.** Disabling or deleting the owner disables its service accounts in
  the same transaction. Compliance needs no follow-up, but a person's leave or departure stops
  the systems the person owned, at a moment nobody chose for them.
- **C. A switch**, `auth.serviceAccounts.disableWithOwner`, off by default: A unless the operator
  asks for B.

### 6. The lifetime of its tokens

- **A. A bound of its own, `auth.serviceAccounts.maxTokenTTL`, `0` (the default) for none.** A
  token created without an expiry gets the bound, or none when there is no bound, as a deploy
  token; one created with an expiry must be within the bound. People's tokens keep 90 days by
  default and 365 at most.
- **B. The people's bound.** A system's token is then replaced by hand every year at least,
  which is what the issue asks to avoid.

### 7. Who creates it

- **A. The admin role**, with any credential that reaches it, as for a local user, and the
  operator on the command line. Its tokens, as every personal access token, take a password,
  the admin's, or the command line: the account has no password and never mints its own.
- **B. The admin role and the owner of a group.** It needs a rule for what an owner may do,
  which comes with the groups that own accounts, later.

## Decision

Option A for every question; 5 is the one to weigh. **A service account is a user of kind
`service` with a required owner, listed with the users, authenticated by personal access tokens
that their scopes narrow and a bound of its own limits, and created by the admin role.**

- **What it is.** A user of kind `service`: no password, no OIDC identity, no login of any kind.
  It authenticates with its personal access tokens only, as Basic `<name>:<token>` or `Bearer
  <token>`. Its name is a user name, unique among the live users; it is renamed, disabled,
  enabled, deleted and bound as every user. Setting its password is `400`.
- **The owner** is required: `user:<name>` or `user:<uid>` of a live user that is not a service
  account, kept as `user:<uid>`, or `group:<name>`. The admin role changes it (`PATCH
  /api/v1/users/<name>` with `owner`, `lajstrom admin user set-owner`), an audit row
  `user.owner`. Owning grants nothing: the owner answers for the account and gets no right on
  it, nor on its tokens.
- **Its tokens** are personal access tokens, with or without scopes. The admin role creates them
  with its password, or the operator on the command line; they are listed and revoked as every
  user's, by the admin role and by the account's own token without scopes.
- **Their lifetime.** `auth.serviceAccounts.maxTokenTTL`, a duration, `0` by default: no bound,
  and a token created without an expiry never expires. With a bound, a token created without an
  expiry expires at the bound, and an expiry beyond it is `400`. A bound lowered later counts
  for the tokens created after it.
- **Its owner disabled or deleted:** option A. Deleting the owner of a live service account is
  `409`, naming the accounts; a disabled owner is the answer's `ownerDisabled`.
- **Listing.** `GET /api/v1/users?kind=service`, `lajstrom admin user list --kind service`. The
  resource adds `owner`, `ownerName` (a user owner's name) and `ownerDisabled`.
- **Admins.** A service account may have any binding, `admin` included, but its `admin` binding
  does not count for the last-admin rule nor for the bootstrap, as a group's does not: without a
  password it cannot make a token once its own expire. That rule never refuses disabling or
  deleting it.
- **Audit.** The principal of its requests is `user:<uid>`, with its name, as for every user;
  the answers of `GET /api/v1/audit` add `principalKind` `service`. Its `user.create` row has
  `kind` and `owner` in the details.
- **Schema.** One column, `users.owner`, text, NULL for every kind but `service`, in migration
  `0026_users_owner.sql`, which only adds. A snapshot of the control plane carries it with the
  other columns of `users`.

## Consequences

- A system gets an account of its own with an owner, its own name in the audit log, and tokens
  that need not expire and are narrowed per job.
- No principal form is added: nothing that stores or reads `user:<uid>` changes. During a rolling
  update the release before takes a service account for a user without a password: its tokens
  with an expiry work there, and one without is refused (`401`), as that release refuses every
  personal access token without an expiry. That release would also let an admin set its password
  and count it as an admin, until the update ends.
- With no bound by default, a service account's token lives until it is revoked. An operator who
  wants rotation sets `maxTokenTTL`.
- Deleting a person can take one more step: hand their service accounts to someone else, or
  delete them.
- Later, with no change of the schema: the owner of a group managing the group's service accounts
  and their tokens; option B or C of question 5, if the maintainers prefer it.

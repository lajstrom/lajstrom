# 0027. Inactive users: a last activity per user, and a disabling at the next authentication

- Status: proposed; A and B at authentication decided on #1283 (2026-11-16)
- Date: 2026-11-16
- Deciders: gecube

## Context

PCI DSS v4.0.1 8.2.6: "Inactive user accounts are removed or disabled within 90 days of
inactivity" (#1283, [compliance.md](../compliance.md)). What Lajstrom has
([rbac.md](../spec/rbac.md) §1, §8):

- A token's `last_used_at`, written at most once a minute per token and replica, for the person
  who reads a token list; a failed write is logged and refuses nothing.
- An OIDC login sets its identity's `login_at` and writes a `user.login` row.
- A password's use leaves nothing but a line of the access log.
- `lajstrom admin user disable` and `PATCH /api/v1/users/<name>`, by hand.

The replicas share one database and have no leader: a task that should run once runs on a
claim of the database, as the expiry of a CI trust policy does (ADR 0026). The authenticator
reads the user's row for every credential, so that a disabling counts from the next request on
every replica.

## Considered options

**Recording.**

- **A. `users.last_seen_at`**, written on every successful authentication of the user, as a
  token's `last_used_at` is.
- Reading the audit log, or the tokens' `last_used_at`: a password's use is in neither.

**Acting.**

- **B1. A task of `serve`** that disables, every hour say, the users whose last activity is older
  than the setting. The row says `disabled` on the day, but one replica must run it under a claim,
  and it runs whether anybody authenticates or not.
- **B2. A check at authentication.** A credential of a user inactive beyond the setting is
  refused, and the user disabled then. No task, no claim; the row says `disabled` from the first
  credential tried after the setting has passed.
- **C. A report only**, the review disables: the 90 days then depend on how often the review
  runs, and 7.2.4's runs every six months.

## Decision

A and B2 (the maintainer, 2026-11-16, on #1283): `users.last_seen_at`, and, with
`auth.disableInactiveUsersAfter` set (`0` by default; `2160h` for PCI DSS), a credential of a
user inactive beyond it is refused and the user is disabled then, with a `user.disable` row whose
`reason` is `inactive`. The users API and the access report (#1279) show `inactive`, computed
from `last_seen_at`, before that. A service account counts the use of its tokens; the last
enabled admin is never disabled. rbac.md §1 says what holds; these are the defaults proposed for
what the decision left open:

1. **Activity** is any successful authentication of the user: a password, a personal access
   token (a web UI's included), an OIDC login. It is recorded to the minute, as #1277 records a
   token's use: when the value that the authentication read with the row is NULL or a minute
   old, by a write that only moves it forward.
2. **The upgrade** (migration 0034): a user with a password counts from the migration, since its
   password logins left no trace; a user without one, from the latest use of its tokens and its
   OIDC logins.
3. **The refusal** comes after the credential has verified, and is a disabled user's: `401`
   without its reason. The row is by `anonymous`, `source` `inactivity`, as the expiry of a CI
   trust policy is by `anonymous`, `source` `expiry`.
4. **Enabling** a user restarts its clock.
5. **An OIDC login** of an inactive user is refused like any credential, whatever the IdP says.
6. **The last enabled admin** is not refused: its credential is taken, with a warning.
7. **A user owner's service accounts** follow `auth.serviceAccounts.onOwnerDisabled`, as with
   every disabling.

## Consequences

- From the moment the setting has passed, no credential of the user is taken on any replica,
  which is what 8.2.6 asks of a disabled account. `users.disabled` turns true only at the first
  credential tried after it, so a reviewer reads `inactive` beside `disabled`. A user that never
  tries again stays enabled and `inactive`, and its credentials stay refused, until an admin
  disables or deletes it.
- No task and no claim: one column, read with the row that the authentication reads already, and
  at most one write a minute per active user across the replicas.
- A person back after the setting needs an admin, through OIDC too: `lajstrom admin user enable`.
- An upgrade makes no user with a password inactive. A service account or an OIDC user whose
  recorded use is older than the setting is inactive at once, so an operator reads `lastSeenAt`
  before turning the setting on.
- A replica of the release before records nothing: a long rollback leaves the records behind,
  and the users enabled during it are disabled again at their first authentication on a newer
  replica when their record is older than the setting.

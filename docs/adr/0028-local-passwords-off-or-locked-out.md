# 0028. Local passwords: off where an IdP logs people in, else a lockout counted in the index

- Status: proposed; C with A decided on #1280 (2026-11-16)
- Date: 2026-11-17
- Deciders: gecube

## Context

PCI DSS v4.0.1 8.3.4: "Invalid authentication attempts are limited by: Locking out the user ID
after not more than 10 attempts. Setting the lockout duration to a minimum of 30 minutes or until
the user's identity is confirmed" (#1280, [compliance.md](../compliance.md)). What Lajstrom has
([auth.md](../auth.md#failed-logins), [rbac.md](../spec/rbac.md) §1):

- A throttle, not a lockout: 10 failed passwords a minute per account and 30 per client address,
  then `429` for the rest of the minute. It lifts by itself, and tokens are not throttled.
- Counted in each replica's memory: N replicas are N budgets. A counter shared in the index was
  weighed on #282 and not taken (2026-11-12): a database round trip on every password check and a
  hot row under attack, for a bound N times tighter.
- OIDC (#69): a person who logs in through the IdP has no password in Lajstrom, and the IdP's
  lockout applies.

## Considered options

- **A. `auth.lockout.duration`**, the failures counted in the index across the replicas, the lock
  ended by the duration or by `lajstrom admin user unlock`. Anyone who knows a user name can keep
  that user's password locked; a write per failure.
- **B. The same lockout per replica, in memory.** Cheap, but 10 × N guesses per lock, and a
  restart unlocks: an assessor may not accept it.
- **C. No local passwords for people**, `auth.localPasswords: false`: every password refused,
  people log in through the IdP, and `lajstrom admin` against the database is the break-glass
  path. It needs an IdP, and settles most of the password rules of 8.3.5 to 8.3.9 too (#1281).

## Decision

C, with A for installs without an IdP (the maintainer, 2026-11-16, on #1280), both off by
default. This revisits #282 knowingly: the throttles stay per replica, as #282 decided, and the
lockout beside them counts in the index. rbac.md §1, Local passwords, says what holds; these are
the defaults proposed for what the decision left open:

1. **Turning passwords off** refuses a password on every route that takes one, before a user is
   read: `401` with a message that says so, the same for every name. No password is set either
   (`400`; the command line fails), and the stored hashes stay, refused. `serve` refuses it
   without `auth.oidc.issuer`. The bootstrap token is not made valid; the break-glass path is
   `lajstrom admin token create --user`.
2. **The count** is of the wrong passwords in a row of an enabled user that has one, a column of
   `users` (migration 0036), written in one statement; a verified password sets it back. Names
   that no user has are not counted in the index: no write per guessed name, no table of names.
3. **A locked account answers as a wrong password**: `401` without the reason, after one
   verification against the fixed hash, so that a lock tells nobody whether the user exists or is
   locked.
4. **The lock is of the password**: tokens, OIDC logins and bearers keep working.
5. **The throttles stay**, per replica, beside the lockout.
6. **Ten is a constant**, PCI DSS's most; the duration is `0`, or from `5m` to `24h`.
7. **An unlock** is the API's (`POST /api/v1/users/<name>/unlock`, the admin role) as well as the
   command line's, and an admin's new password unlocks too, each a `user.unlock` row. A lock is a
   `login.block` row with the `limit` `lockout`, one per lock.

## Consequences

- An account's password gets at most 10 guesses per lock across every replica, instead of 10 × N
  a minute: with `30m`, 480 a day.
- Anyone who knows a user name can keep that user's password locked, an admin's included. The
  user's tokens and OIDC logins keep working, and the command line unlocks; the person learns of
  the lock from an admin, since the answer does not say it.
- A failure is a write of the user's row, at most 10 per lock. A password check reads nothing
  more, since the authentication reads the row anyway. While the setting is `0`, nothing is
  written.
- With passwords off, a person's password in a CI job or a package client's configuration stops
  working: it takes a personal access token of an OIDC login. The routes that take an admin's
  password (a token through the API, a service account's token, a deploy token over a mask) are
  then the command line's, as they already are for an OIDC admin.
- A replica of the release before neither counts nor reads a lock, during a rolling update.

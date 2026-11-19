# 0037. Asking the IdP again: each login's sealed refresh token, a refusal ends the login

- Status: proposed; B decided on #1282 (2026-11-16), its defaults proposed (#1282)
- Date: 2026-11-19
- Deciders: gecube

## Context

PCI DSS v4.0.1 8.2.5: "Access for terminated users is immediately revoked." SOC 2 CC6.2: "user
system credentials are removed when user access is no longer authorized."

An OIDC login asks the IdP once and ends in a personal access token of Lajstrom, which lasts
`auth.oidc.tokenLifetime` (24 hours by default), or the shorter of it and
`auth.login.tokenLifetime` (12 hours) for a web UI; its groups count as long
([rbac.md](../spec/rbac.md) §8, §10). rbac.md §8 says "no request but a login asks the IdP
anything". So a person disabled or removed at the IdP keeps the tokens of their logins until each
expires, unless someone disables the user in Lajstrom too.

#1282 listed three options: **A**, SCIM 2.0, which the IdP pushes; **B**, ask the IdP again with
each login's refresh token, sealed, every N minutes, and end the login's tokens when it refuses;
**C**, a procedure, the offboarding of `docs/compliance.md`, and a short token lifetime. The
maintainer decided on 2026-11-16: C at once, done in #1299, then B as the feature; A stays a
possible later option. This record is the contract of B. It leaves open:

1. which logins, the setting, its default and its bounds, and a login whose IdP issues no
   refresh token;
2. what is stored, where, and whether snapshots, backups and the rewrap carry it;
3. which replica refreshes, the load on the IdP, and its jitter;
4. the IdP down, against the IdP refusing;
5. what a refusal ends, its audit row, and what the person sees;
6. what a success changes: the groups, the token's expiry;
7. refresh tokens that the IdP rotates at each use;
8. what each IdP needs.

## Considered options

1. **Which logins.** (a) Every login while OIDC is on; (b) a setting, off by default, as
   `auth.disableInactiveUsersAfter` and `auth.login.idleTimeout` are.
2. **What is stored.** (a) A row per login, in a table of its own; (b) a row per user, the
   refresh token of its latest login; (c) columns of `tokens`.
3. **Who refreshes.** (a) A lease: one replica refreshes every login; (b) the beat of `replicas`,
   every 30 seconds; (c) a claim per row: any replica takes the rows that are due, one at a time
   each.
4. **The IdP down.** (a) A login keeps its token until it expires, as without B; (b) a login that
   the IdP has not confirmed for a grace period is ended.
5. **What a refusal ends.** (a) The token of the login refused; (b) the tokens of every login of
   the user; (c) every token of the user; (d) the user, disabled.
6. **What a success changes.** (a) Nothing but the next due time; (b) the groups too, from the
   ID token of the answer; (c) the token's expiry too, so that a login lasts as long as the IdP
   confirms it.

## Decision

Proposed: 1b, 2a, 3c, 4a, 5a and 6b. rbac.md §8 (Asking the IdP again) says what holds, and
index-schema.md has the table.

1. **A setting, `auth.oidc.refresh.interval`**, `0` (off) by default, `15m` for PCI DSS 8.2.5,
   from `1m` to less than `auth.oidc.tokenLifetime`. It takes a master key. Lajstrom adds no
   scope: the operator adds `offline_access` where the IdP needs it. A login whose IdP issued no
   refresh token lasts its lifetime, as before, with a warning and `refresh: false` in its
   `user.login` row.
2. **A row per login in `oidc_refresh`** (migration 0042), the refresh token sealed with the
   master key (`oidc_refresh:<id>` as additional data) and sealed again by a rewrap. Not in a
   snapshot; a backup holds it as sealed. With the setting `0`, `serve` deletes every row at its
   start.
3. **A claim per row**, of 2 minutes, by any replica every 30 seconds, at most 4 calls at once
   per replica. A login is due `interval` after its mint, then after each refresh, each time less
   up to a tenth of the interval at random. A replica that seals nothing claims none.
4. **Only `invalid_grant` ends a login.** Any other answer, or none, is an error: tried again
   after 1, 2, 4 … minutes up to the interval, and the login keeps its token.
5. **A refusal revokes the token of that login**, once, with a `token.revoke` row by `anonymous`,
   `source` `refresh`, `reason` `refused`. The user's other tokens and the user are untouched.
6. **A success confirms the login**, keeps the new refresh token when the IdP rotated it, and
   replaces the identity's groups from the answer's ID token when they differ (a `user.groups`
   row). It moves neither the token's expiry nor its sign-in, and is no activity of the user.

## Consequences

- **A rule changes.** rbac.md §8 said that no request but a login asks the IdP anything; with the
  setting, `serve` asks it again about each login, apart from any request. No request waits for
  the IdP: the refresh runs in the background, and a request reads only `tokens.revoked_at`, as
  before.
- **Lajstrom keeps a credential of the IdP**, one per live login: sealed with the master key, in
  no log, audit row, answer or snapshot, and of no use without the client's secret. A leak of
  the database and of the master key together gives the IdP's refresh tokens with the rest.
- **The window** between a removal at the IdP and the end of the person's logins is the interval
  and a minute, and 20 minutes more on `/v2` for a bearer issued before (rbac.md §4), where it was
  `auth.oidc.tokenLifetime`.
- **What B does not end**, and the offboarding procedure still does: the personal access tokens
  that an OIDC user made itself through a web UI's fresh sign-in (§12), those an admin made for
  the user, a login whose IdP issued no refresh token, and every login while the IdP does not
  answer.
- **The load** is one request to the token endpoint per live login and interval: 1,000 logins at
  `15m` are about 1.2 a second. The jitter spreads the logins of one morning; the backoff of an
  error keeps an outage from turning into a storm when the IdP comes back.
- **A login may end earlier than its lifetime**, when the person's session at the IdP ends first:
  a login lasts the shorter of the two.
- **A rotated refresh token lost** (a replica stopped between the IdP's answer and its write) is
  refused at the next refresh at an IdP that takes each once: the person logs in again.
- **Groups** removed at the IdP count from the next refresh of one of the user's logins that has an
  ID token, not from the next login only. An IdP whose refreshed ID token lacks the groups claim
  drops the user's groups at the first refresh: a test login shows it.
- **During a rolling update** a replica of the release before refuses `auth.oidc.refresh` as an
  unknown key, so the key is set once every process runs the release with migration 0042; until
  then no login keeps a refresh token.

**What each IdP needs** (to be checked against each IdP's documentation in the implementation):

- **Keycloak** issues a refresh token without `offline_access`, bound to the browser's SSO
  session: a login then ends at its SSO Session Max (10 hours by default), and Lajstrom's refresh
  keeps the session from idling out, for every client of the realm. With `offline_access` it
  issues an offline token, of an offline session of its own (Offline Session Idle, 30 days by
  default): the better choice. Its realm setting Revoke Refresh Token makes each token single-use.
- **Okta** needs the Refresh Token grant on the app and the scope `offline_access`. The app's
  rotation, with its grace period for the old token, and the authorization server's policy, which
  sets a refresh token's lifetime, are Okta's. The refreshes count against the org's rate limit of
  the token endpoint.
- **Entra ID** needs `offline_access`, and answers a new refresh token at each refresh. A disabled
  or deleted account, and sessions that an admin revoked, are refused. The `groups` claim is as the
  app's token configuration sets it for the ID token; a user in more groups than the token holds
  gets an overage claim instead, and so no groups, as at a login.
- **Google** issues a refresh token only for its own parameter `access_type=offline`, which
  Lajstrom does not send: Google's logins are not refreshed, and each logs the warning.
- **Dex** needs `offline_access`, and rotates refresh tokens at each use. At a refresh it asks its
  connector again where the connector can (LDAP, OIDC, GitHub among them); a connector that cannot,
  SAML for one, does not see a removal upstream.

SCIM (A) stays possible later. It would tell Lajstrom of a removal at once, also of a person with
no live login, and B would still bound what a lost SCIM call leaves open.

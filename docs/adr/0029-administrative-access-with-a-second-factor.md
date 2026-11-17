# 0029. Administrative access with a second factor: the admin role only for a web UI's OIDC sign-in

- Status: proposed; A decided on #1276 (2026-11-16); #1323 proposed with it
- Date: 2026-11-17
- Deciders: gecube

## Context

PCI DSS v4.0.1 8.4.1: "MFA is implemented for all non-console access into the CDE for personnel
with administrative access", and 2.2.7's notes count "browser-based interfaces and application
programming interfaces (APIs)" as such access. 8.5.1: MFA systems are "not susceptible to replay
attacks" and "cannot be bypassed by any users, including administrative users unless
specifically documented, and authorized by management on an exception basis, for a limited time
period" (#1276, [compliance.md](../compliance.md)). What Lajstrom has
([rbac.md](../spec/rbac.md) §1, §3, §8, §10, §11):

- The admin role is reached with a password in Basic credentials, with a personal access token,
  with the token of any OIDC login, and with a service account's token. A second factor, if any,
  is the IdP's, at an OIDC login; an IdP that asks for one at every login covers that path only.
- The web UI's sign-in (#1170): a token bound to the browser's DPoP key, which keeps the
  `auth_time` and the `amr` of its OIDC login (#937). A CI trust policy of the API that writes
  needs such a sign-in, fresh and with a second factor (`auth.FreshSignIn`), and the approval of
  a second admin. Nothing reads `acr`, and nothing else reads the sign-in.
- A personal access token through `POST /api/v1/users/<name>/tokens`, a service account's token
  and a deploy token over a mask are made with a password and never with a token (#249, #911).
  An OIDC user has no password, and with `auth.localPasswords: false` (ADR 0028) no one has one:
  the API cannot make them at all, and an operator without access to the database cannot either
  (#1323).
- A refusal of §11's fresh sign-in is the generic `403`, which a UI tells from one of RBAC only by
  its message (#1296).

## Considered options

For #1276:

- **A. An admin mode for OIDC with MFA.** The admin role counts only for a web UI's sign-in
  through OIDC whose ID token meets the `amr`/`acr` conditions; passwords, personal access tokens
  and service accounts keep their other bindings and lose `admin`. Admins need the IdP and a
  browser; automation with admin rights moves to the command line against the database or to
  narrower bindings.
- **B. A built-in second factor for local users**, TOTP or WebAuthn: seeds sealed in the index,
  enrolment, recovery codes, a UI. Package clients cannot carry it. Large.
- **C. A step-up per action**: every admin write needs a sign-in with a second factor of the last
  N minutes. An admin's reads stay single-factor.
- **D. Documentation only.** Nothing in Lajstrom enforces or shows it.

For #1323: a fresh sign-in through OIDC in place of the password on the routes that ask for one,
or the command line only, as ADR 0028 left it.

For the answer of a step that needs more (#1296): RFC 9470's `401` with
`WWW-Authenticate: … error="insufficient_user_authentication"`, or a `403` with a new error code.

## Decision

A (the maintainer, 2026-11-16, on #1276), off by default, sharing the checks of the ID token with
#937; with it, as #1323 proposes, a fresh sign-in through OIDC in place of the password. rbac.md
§10 and §12 say what holds; these are the defaults proposed for what the decision left open:

1. **Keys.** `auth.admin.requireMFA` turns the mode on. What a second factor is, for the mode and
   for every step of #937 alike, is `auth.oidc.mfa.amr` and `auth.oidc.mfa.acr`, lists of the
   values taken: one of the token's `amr` in the first, its `acr` in the second, each that is set;
   with neither set, #937's rule (`mfa`, or methods of two kinds of factor of RFC 8176). The token
   of a sign-in keeps `acr` beside `auth_time` and `amr` (migration 0038), and keeps `amr` and
   `acr` when the ID token has no `auth_time`: such a sign-in shows its factors and is never
   fresh. The conditions are read at each request, so that a change of them reaches the tokens
   already made. Lajstrom asks the IdP for no `acr_values`.
2. **No freshness for the role.** The sign-in's second factor counts for the token's lifetime,
   which `auth.login.tokenLifetime` (at most 24 hours) and `auth.login.idleTimeout` bound.
3. **Only the web UI's sign-in counts**, the token bound to a DPoP key. `lajstrom login`'s token
   is a bearer of a day that whoever copies it can replay, and does not count: no admin works
   through the API from a terminal. The command line against the database is unchanged and is the
   break-glass path; a token it makes reaches no admin role, and using the API without the IdP
   takes turning the mode off, a restart recorded as `config.change`.
4. **The first admin** is an OIDC user given its binding on the command line: the bootstrap token
   is not made valid, and `serve` refuses the mode without `auth.oidc.issuer`.
5. **What the other credentials keep.** An admin's personal access token keeps the other bindings
   only: an admin with no other binding cannot push or read a private repo with it. A service
   account's and a local user's `admin` bindings stay and count for nothing; `serve` warns at
   start. The last-admin rule is unchanged.
6. **The answers.** On a route of the admin role, a web UI's sign-in through OIDC without a second
   factor is asked for more: `401` with `WWW-Authenticate: DPoP
   error="insufficient_user_authentication"` (RFC 9470), and `max_age=600` where a step needs a
   fresh sign-in; any other credential of an admin is `403`, saying what the role needs. The same
   answer serves §11's steps, which settles the choice of #1296. `GET /api/v1/info` answers
   `adminMFA` and, for the caller, `adminNeedsMFA`, and the views of a token answer its sign-in.
7. **Audit.** Every control-plane change made with a web UI's sign-in has `signIn`, `{token,
   authTime, amr, acr}`, in its row's details.
8. **A fresh sign-in in place of a password** (#1323): `POST /api/v1/users/<name>/tokens` and
   `POST /api/v1/deploy-tokens` take a web UI's sign-in through OIDC of the last 10 minutes, as
   §11 does, where they take a password. A second factor is needed only where the role needs one,
   under the mode: a password has none either.

## Consequences

- Under the mode, administrative access through the API and the UI needs the IdP, a second factor
  there, and a browser. An admin's password and tokens reach what its other bindings give, and
  scripts that used an admin's token for publishing, retention or a prefetch need a binding of
  their own.
- The command line against the database stays the way in when the IdP is down, guarded by
  whoever holds the database's credentials; turning the mode off is the other, recorded.
- A server whose IdP sends neither `amr` nor `acr` cannot use the mode until the IdP is set to
  send one; the same holds for #937's write policies already.
- A UI tells a step-up from any other refusal by the challenge alone, and knows from `info` and
  the token's `signIn` when to sign in again first. A client of §11's routes that read the `403`
  reads a `401` with the challenge instead.
- With passwords off, the web UI and the API make a service account's token and a deploy token
  over a mask again; a stolen sign-in token is of no use without its key, and of the last 10
  minutes.
- A replica of the release before refuses a configuration with the new keys, and gives the role
  as before while it runs: the mode counts once every replica runs a release that knows it.

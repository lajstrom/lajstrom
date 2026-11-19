# 0035. A second admin is another person: no token requests, no admin's password set by another

- Status: proposed (#1393)
- Date: 2026-11-19
- Deciders: gecube

## Context

[ADR 0030](0030-approval-of-privileged-changes.md) makes the changes that grant the most wait for
an admin other than the requester, compared by uid (#1285), and #1376 closed one way past it,
`POST /api/v1/users` with `admin: true`. #1393 found three more, each through another identity
that one admin controls, while `auth.admin.requireMFA` is off ([rbac.md](../spec/rbac.md) §12):

1. **A token made for another user.** The admin role makes a personal access token for any user,
   another admin and a service account with `admin` included (rbac.md §1). Such a token files a
   request in that user's name, and the admin who made it approves it with its own password: the
   approval compares the requester and the approver by uid, and takes a service account with
   `admin` as a requester. What waits does not depend on the credential, so a service account's
   token makes no such change at once; ADR 0030's "a token of a service account does not wait"
   is about creating one.
2. **Another admin's password.** An admin sets it, signs in as that admin, and approves its own
   requests.
3. **Enabling** a disabled user who keeps its `admin` binding; with 2, a second approver.

A request is made today with any credential that reaches the admin role; its approval takes a
password or a web UI's fresh sign-in (ADR 0030, 4). A CI trust policy that writes takes a fresh
sign-in with a second factor for its change and for its approval alike (rbac.md §11).

## Considered options

For the tokens:

- **A. Refuse** creating a token for another user who holds `admin`, `400`, the command line
  instead. It leaves the tokens made before their user held `admin`, by an admin who then had a
  second admin approve the binding, and those made before the release; and rotating the token of
  a service account with `admin` takes the command line.
- **B. Wait**: creating such a token is a request. The gaps of A, and a rotation takes two admins.
- **C. Record who made each token** (a column, so a migration) and refuse an approver who made
  the requester's token. The tokens made before the migration and on the command line have none.
- **D. A request takes what its approval takes**: a password or a fresh sign-in, never a token.
  No token files a request, whoever made it and whenever; a service account files none.

For the passwords:

- **E. Refuse** an admin's new password for a user who holds `admin`, `400`: the person changes
  its own, and the command line is the break-glass path.
- **F. Wait**: the approver then approves that one admin knows another's password.
- **G. Mark the account** so that it does not approve until its user changes the password: the
  admin who set it changes it as the user, with the password it set.

For enabling: **H.** no rule once D and E hold; **I.** refuse; **J.** wait, a new kind of request.

## Decision

Proposed: D, E and H. rbac.md §13 (Who requests, Another admin's password, What else stays) says
what holds.

1. **A request is made with the admin's password or a web UI's fresh sign-in** through OIDC,
   under the admin mode with a second factor, as its approval is: `POST /api/v1/bindings` of a
   binding that waits is `403` with a token; `POST /api/v1/deploy-tokens` took no token already.
   A token so gives the admin who made it no second voice: it neither requests nor approves.
   Creating a token for another user, a service account with `admin` included, stays one admin's
   and does not wait, so that rotating one takes one admin.
2. **The approval refuses a request whose requester is a service account** (`409`): one made on
   a release before.
3. **An admin's new password for a user who holds `admin`**, by its own binding or a group's whose
   membership has not lapsed, enabled or disabled, is `400` naming the setting, whoever asks, the
   user itself included, so that a token made for it sets none. An unlock and `mustChange` are as
   before: neither gives anyone the password.
4. **Enabling** a user who holds `admin` stays one admin's: under 1 and 3 it gives no admin a
   credential of that user.
5. **Audit**: `approval.request` and `approval.approve` have `credential`, `password` or the
   sign-in's `token:<id>`; a refusal writes no row.
6. **No setting and no migration**: the rules hold under `auth.approval: second-admin`; with
   `none` nothing changes.

## Consequences

- A script or a Terraform provider that requests an `admin` binding or a binding on `*/*` with a
  token gets `403` under `second-admin`, where ADR 0030 answered `202`: it sends a password, or
  the operator makes the binding on the command line. A service account with `admin` requests
  nothing.
- An admin who forgot its password gets a new one on the command line, or by having its `admin`
  binding deleted, its password set and the binding requested again, which a second admin
  approves.
- A password that an admin set before its user held `admin` (a create, a new password) stays known
  to that admin after a second admin approves the binding: the approver decides, and the audit
  log shows who set it. Only a credential that the person makes alone closes that, as an OIDC
  login under the admin mode does (ADR 0029).
- During a rolling update, a replica of the release before still takes a token's request and
  sets an admin's password.

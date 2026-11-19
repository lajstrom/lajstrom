# 0030. Approval of privileged changes: a second admin for what grants most, a reason with each

- Status: proposed; B with C decided on #1285 (2026-11-16)
- Date: 2026-11-17
- Deciders: gecube

## Context

PCI DSS v4.0.1 7.2.3: "Required privileges are approved by authorized personnel." 8.2.4: the
addition, deletion and modification of user IDs and authentication factors are "authorized with
the appropriate approval" and "implemented with only the privileges specified on the documented
approval". SOC 2 CC6.3 and CC8.1 ask for segregation of duties and approved changes (#1285,
[compliance.md](../compliance.md)). What Lajstrom has ([rbac.md](../spec/rbac.md) §3, §5, §11):

- One admin makes users, bindings (`admin` ones included), service accounts and deploy tokens
  over a mask alone; each change is an audit row naming who made it.
- For a CI trust policy of the API that writes, #937 decided a second admin's approval: the policy
  row waits, `state` `pending`, until an admin other than the one who made it approves it with a
  fresh sign-in with a second factor and the `If-Match` of its revision; each replica marks the
  policies whose expiry passed, once a minute.
- The command line takes `--reason` (`LAJSTROM_REASON`) into `details.changeReason` (#1284); the
  API takes none.
- ADR 0015 (accepted, not built) declares bindings in git, where a review is the approval.

## Considered options

- **A. ADR 0015's declarative bindings**: the review in git is the approval. Large, and the API's
  bindings stay unapproved.
- **B. Changes that a second admin approves**, as #937's write policies, for an `admin` binding, a
  binding on `*/*`, a deploy token over a mask with `write` and a service account with `admin`;
  `auth.approval: second-admin | none`.
- **C. A reason on every control-plane change**, through the API and the command line, required
  with `auth.requireReason: true`.

For the storage of what waits under B:

- **A state on the objects**, as `ci_policies` has: `bindings`, `tokens` and `users` get a
  `pending` state, and every reader of them leaves those rows out: the evaluation's snapshot, the
  last-admin rule, the bootstrap, the authentication of a token, the lists, the snapshot.
- **A table of requests**: the change is kept as a request, and its approval makes it, in
  `bindings` or `tokens` as any change does.

## Decision

B with C (the maintainer, 2026-11-16, on #1285), both off by default; ADR 0015 stays the
long-term path. rbac.md §13 says what holds; these are the defaults proposed for what the decision
left open:

1. **A table of requests**, `approvals` (migration 0039). Nothing that reads `bindings` or
   `tokens` changes, so a reader that forgets a filter cannot grant what waits: a state on the
   objects would put that filter in every reader of the two tables, now and later. What #937's
   mechanism gives is kept: a request that grants nothing, an approver other than the requester,
   one decision made of two at once, an expiry marked once a minute by one replica.
2. **What waits**: a binding of the role `admin`, whatever its principal (a service account's is
   "a service account with admin"), a binding on `*/*`, and a deploy token over a mask with
   `write`, through the API. The route answers `202` with the request. A token of a service
   account does not wait, so that rotating one takes one admin. `POST /api/v1/users` with
   `admin: true`, which makes the user's `admin` binding with the user, is `400` naming the
   setting instead, so that the binding is requested with `POST /api/v1/bindings` (#1376).
3. **A deploy token's secret** is made with the request and answered to the requester, once; the
   request keeps its hash, and the approval makes the token with it. The approver never sees the
   secret, and a request rejected or expired leaves nothing to revoke.
4. **The approver** is an admin other than the requester, with what a deploy token over a mask
   takes: a password, or a web UI's fresh sign-in through OIDC (ADR 0029), never another token;
   under the admin mode, with a second factor. A request never changes, so no `If-Match`.
5. **A rejection** by any admin, the requester's being a withdrawal; **an expiry** after 7 days,
   a constant. Turning the setting to `none` leaves the requests that wait to be decided or to
   expire.
6. **The command line** makes the changes at once whatever the setting: it is the operator's,
   the break-glass path, and the way to the second admin of a server that has one.
7. **The reason** is `changeReason`, in the body or the query, 1 to 1024 bytes as `--reason`, on
   the changes of the control plane but a person's own credentials, the bootstrap and the writes
   of artifacts. `auth.requireReason: true` makes it required, there and on the command line.
8. **Audit**: `approval.request`, `.approve`, `.reject` and `.expire`, and the change's own row
   with `approval` and the request's `changeReason`. Requests are not in a snapshot, and no
   webhook event is sent for them.

## Consequences

- With `second-admin`, an install needs two admins who can sign in, two OIDC users under the
  admin mode; `serve` warns when it has fewer. An install with one admin keeps `none`, or makes
  these changes on the command line, or, later, declares them in git (ADR 0015).
- A client of `POST /api/v1/bindings` or `POST /api/v1/deploy-tokens`, a script or a Terraform
  provider, gets `202` and no binding or working token for these changes while the setting is
  `second-admin`: it must wait for the approval, or the operator makes those changes on the
  command line. One of `POST /api/v1/users` with `admin: true` gets `400`, and no user.
- The deploy token's secret is in the requester's hands before it works; a request that nobody
  approves within 7 days is dropped, and the secret never works.
- `auth.requireReason` makes every control-plane change of the API and of the command line take a
  reason, which breaks the scripts that do not send one.
- The approval of a CI trust policy (`auth.ci.apiWriteApproval`, default `second-admin`) stays
  apart, on its own row and with its own setting.
- A replica of the release before refuses a configuration with the new keys, and makes every
  change at once while it runs.

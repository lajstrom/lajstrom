# 0038. Telling the second admin: webhook events of a request's life, a count for admins

- Status: proposed (#1336)
- Date: 2026-11-19
- Deciders: gecube

## Context

With `auth.approval: second-admin` ([ADR 0030](0030-approval-of-privileged-changes.md),
[rbac.md](../spec/rbac.md) §13), an `admin` binding, a binding on `*/*` and a deploy token over a
mask with `write` wait in `approvals` until a second admin approves them, for 7 days at most.
Nothing tells the second admin that a request waits: it finds it only in
`GET /api/v1/approvals`, `lajstrom admin approval list` or a web UI. A request that nobody looks
at expires, and the change is asked for again. ADR 0030 decided that "no webhook event is sent"
for requests.

Lajstrom has webhooks ([webhooks.md](../spec/webhooks.md)): signed CloudEvents from an outbox
written in the transaction of each audit row, delivered by any replica, again until the receiver
answers. A webhook of a mask with a `*` is the admin's, and the events of a CI trust policy, whose
approval is the same kind of step (rbac.md §11), already go only to such webhooks.
`GET /api/v1/info` answers a UI what it needs, and, with a credential, whether the caller reaches
the admin role. Lajstrom keeps no address of a person and sends no mail.

#1336 proposes webhook events for the life of a request, `approval.request`, `.approve`,
`.reject` and `.expire`, on the existing webhooks, and a count of the pending requests for admins
in the UI, with no mail sent by Lajstrom.

## Considered options

1. **How an admin hears of a request.**
   - (a) Webhook events on the existing webhooks, which the operator routes to a chat or a mail.
   - (b) Mail sent by Lajstrom: an SMTP setting, and an address for each admin, which Lajstrom
     does not keep.
   - (c) A notification URL of its own in the configuration, beside the webhooks.
   - (d) Nothing: the admins look at the list.
2. **Which webhooks get the events.** (a) The admin's, a mask with a `*` that covers the change's
   mask, as for a CI trust policy; (b) those of `*/*` only; (c) every webhook whose mask matches,
   a repo's included.
3. **What an event holds.** (a) The request as the API answers it, and the names of its users;
   (b) the request's id only, which the receiver reads with an admin's credential.
4. **The expiry's event.** (a) Written by the replica whose mark of the request wins, in its
   transaction; (b) none, since a receiver can count down from `expiresAt`.
5. **A reminder** before the expiry: (a) none; (b) an event a day before.
6. **The count for a UI.** (a) A field of `GET /api/v1/info`, for a caller with the admin role;
   (b) a route of its own; (c) none: the UI pages the list.

## Decision

Proposed: 1a, 2a, 3a, 4a, 5a and 6a. rbac.md §13 (Telling the second admin) and webhooks.md §1
and §3 say what holds.

1. **Four types**, as the past tense of the audit actions, as the other types are:
   `approval.requested`, `approval.approved`, `approval.rejected` (a rejection or a withdrawal)
   and `approval.expired`, each with `data.action` its row's action (`approval.request`, …).
   Each is written with its audit row, in its transaction.
2. **Only the admin's webhooks** that cover the change's mask get them: a binding of `admin` and
   one on `*/*` reach the webhooks of `*/*`; a deploy token over `oci/*` those of `*/*` and of
   `oci/*`. A repo's webhook, which its maintainer manages, gets none.
3. **The request as the API answers it**, plus `names`: the name at the time of each user among
   the requester, the decider and a binding's principal. Never the deploy token's secret or its
   hash, nor the credential or the sign-in of either side.
4. **The expiry's event** is written by the replica that marks the request expired, in that
   transaction, so it is written once, whatever the replicas.
5. **No reminder.** A receiver has `expiresAt`.
6. **`pendingApprovals` in `GET /api/v1/info`** while `caller.admin` is true: the requests
   pending, not expired, and not the caller's own. Left out for every other caller.
7. **No setting and no migration**; it holds whatever `auth.approval` says.

## Consequences

- An admin hears of a request where the operator routes the events: a chat, a mail gateway, an
  incident tool. The operator makes one webhook of `*/*` with the four types, or adds them to one
  it has. A webhook whose `events` is empty, every type, gets the four from the upgrade on; a
  receiver ignores a type it does not know (webhooks.md §1).
- The receiver sees who asks for what and why: the requester's and the principal's names, the
  role, the mask and the reason. These are the admin's to see, and the operator chooses the
  receiver. No secret reaches it.
- An admin who manages the webhooks can turn the events off before it requests. That delays the
  second admin; it never lets the change through. The webhook's change is a `webhook.update` row.
- A web UI that asks `GET /api/v1/info` with an admin's token gets the count; one count per call,
  from the index `approvals_expires`. While the person does nothing, the UI asks without its
  token (rbac.md §10, Idle) and gets no count.
- No mail is sent by Lajstrom, and it keeps no address.
- During a rolling update, a replica of the release before writes no event for the requests,
  decisions and expiries it makes, answers no count, and refuses the new types in a webhook's
  `events` (`400`).
- ADR 0030's "no webhook event is sent for them" no longer holds.

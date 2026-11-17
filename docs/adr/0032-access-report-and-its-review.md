# 0032. The access report and its review: who has what in one document, a review as an audit row

- Status: proposed; B with A as its client, then C, decided on #1279 (2026-11-16)
- Date: 2026-11-17
- Deciders: gecube

## Context

PCI DSS v4.0.1 7.2.4: "All user accounts and related access privileges … are reviewed … at least
once every six months", "Any inappropriate access is addressed" and "Management acknowledges that
access remains appropriate". 7.2.5.1 asks the same for application and system accounts, at the
frequency of the entity's risk analysis. SOC 2 CC6.2 and CC6.3 ask that credentials no longer
authorized are removed, with least privilege (#1279, [compliance.md](../compliance.md)). What
Lajstrom has, each a list of its own for the admin role ([rbac.md](../spec/rbac.md)):

- users (`?kind=service` for service accounts, with their owners), with `lastSeenAt` and
  `inactive` (§1);
- bindings, of users, of groups (§8), and of `cert:` and `k8s:` principals and patterns (§9);
- every token at once (`GET /api/v1/tokens`, #959), with `lastUsedAt` and `beyondMaxTTL`;
- CI trust policies (§11); the admin mode (§12) and the approvals (§13);
- the audit log of every change.

No list joins them per principal; a user's groups show nowhere but in `whoami`; nothing records
that a review took place.

## Considered options

- **A. `lajstrom admin access-report [--format json|csv]`**, from the index.
- **B. `GET /api/v1/access-report`** for the admin role, the same rows; the command line its client.
- **C. The acknowledgement**: `POST /api/v1/access-reviews` with the report's sha256 and a note,
  stored as an audit row `access.review` by the reviewer.

## Decision

B with A as its client, then C (the maintainer, 2026-11-16, on #1279). rbac.md §14 says what
holds; these are the defaults proposed for what the decision left open:

1. **One document per report**, sections by kind of principal: `users` (people and service
   accounts), `groups`, `workloads` (`cert:`, `k8s:`), `deployTokens`, `ciPolicies` and
   `anonymous`; each entry in the shape the existing API answers (a user, a binding, a token, a
   policy), with `principal` and `flags`. Live tokens only; deleted users are left out.
2. **Flags**: `admin`, `allRepos`, `adminWithoutEffect`, `noExpiry`, `unused`, `beyondMaxTTL`,
   `ownerDisabled` and `inactive`; `unused` after `unusedDays`, 90 by default (PCI DSS 8.2.6).
3. **Scope**: `all`, `people` (7.2.4) or `systems` (7.2.5.1), which may be reviewed at different
   frequencies. No other filter.
4. **A canonical form**: compact JSON, keys in byte order, fixed list orders, times to the second.
   The same rows at the same time are the same bytes, from the API and from the command line.
5. **Not paged**: a review covers one document and one sha256.
6. **The command line reads the database**, with the same code and the same bytes, instead of
   being an HTTP client: under the admin mode no terminal reaches the admin role through the API
   (ADR 0029). It also prints the sha256, and a CSV for a spreadsheet.
7. **The review** takes `sha256`, `generatedAt`, `scope` and `note`, the admin role with any
   credential that reaches it, and answers `201` with its audit row. Lajstrom keeps no report and
   does not compare the hash with one: the report changes with every token's use. Past reviews
   are the audit log's (`?action=access.review`). The command line records none.

## Consequences

- An admin sees in one document what it could read before in several, and the users' current
  groups, which no route answered to an admin before. A report is a read: not audited, in the
  access log.
- The evidence of a review is two things: the row (who, when, which sha256, the note) and the
  document the reviewer keeps; the row alone does not show what was reviewed.
- A recorded review is an admin's statement; management's acknowledgement is in its note or in
  the ticket it names. Lajstrom has no reviewer role.
- A large install answers a large document, built from one read; it narrows only by scope.
- The flags and the shape are a contract of the API: a flag added later is a new value that a
  reader must tolerate.

# 0007. Authentication and RBAC model

- Status: accepted
- Date: 2026-11-06
- Deciders: gecube

## Context

Lajstrom fronts every protocol, so it is the single place where authorization and audit happen
(ADR 0002). Clients are very different: `apt` and `pip` send HTTP Basic, `docker`/`oras` do the
distribution token dance, CI jobs want a credential scoped to one repo, people want SSO. The
backend registry's own permissions are not usable: clients never talk to it.

## Considered options

- Per-protocol credentials and permission tables.
- One principal model with one credential that works everywhere, and one evaluator feeding
  both HTTP Basic checks and OCI bearer scopes.
- Delegating all authorization to the backend registry's IAM.

## Decision

One model (`docs/spec/rbac.md`): principals are local users, personal access tokens (acting as
their user, with optional narrowing and mandatory expiry), deploy tokens bound to one repo,
`anonymous`, and later OIDC users and groups. The resource is a repo (`<format>/<repo>`);
actions are `read`, `write`, `delete`, `manage`; roles `reader`, `writer`, `maintainer`, `admin`;
a binding is `(principal, role, repo_mask)` with whole-segment wildcards (`apt/*`, `*/payments`).
Anonymous read is a per-repo flag.

The same evaluator answers Basic-authenticated frontend requests and produces the scopes of OCI
bearer tokens. The backend registry is reachable only by Lajstrom. Every write and every
control-plane change is audited.

## Consequences

- One `user:token` pair for apt, pip, mvn, oras and docker.
- Fine-grained paths inside a repo, groups and OIDC are additive later; nothing in the model
  changes for them.
- Deploy tokens cannot delete or manage, by construction.
- The evaluator sits on the hot path of every request; it must be a cheap in-memory lookup over
  a small bindings table, reloaded on change.

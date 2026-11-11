---
id: P-09
title: "feat(cli): retention by external means: lajstrom admin retention over the delete API, run by the operator's scheduler"
milestone: none
lane: none
size: M
priority: p2
labels: [area/core, kind/feature, post-mvp]
depends: []
---
**Decision (the lead, 2026-11-11)**

Retention is not a rule engine in the core. The registry owns the bytes and their lifecycle
([ADR 0001](https://github.com/lajstrom/lajstrom/blob/main/docs/adr/0001-oci-registry-as-sole-storage.md));
Lajstrom owns the meaning of a version. So retention is done by external means, in three parts:

1. **Deletion only through Lajstrom's API.** `DELETE /api/v1/repos/<format>/<name>/packages/<package>/versions/<version>`
   exists: referrers first, then manifests, rows and rendered indexes, an audit row per manifest,
   the `maintainer` role ([docs/api.md](https://github.com/lajstrom/lajstrom/blob/main/docs/api.md)).
   A version disappears from the index because someone with the right role deleted it, with an
   audit trail, never because a rule inside the server fired.
2. **`lajstrom admin retention`, a client of that API** that the operator's own scheduler runs (a
   Kubernetes CronJob, a CI schedule, cron). Not `admin prune`: in
   [guarantees.md](https://github.com/lajstrom/lajstrom/blob/main/docs/spec/guarantees.md) "prune"
   already names the removal of index rows whose manifests are gone, the opposite direction.
3. **Read repair with a grace period** for what the registry's own lifecycle rules (ECR lifecycle
   policies, Harbor tag retention) delete under us: the index follows at the next reconcile, never
   inside the grace age, and only after two sightings of `MANIFEST_UNKNOWN`. That part is
   [#446](https://github.com/lajstrom/lajstrom/issues/446), not this issue.

docs/comparison.md §5 already describes retention this way; docs/architecture.md and
docs/roadmap.md are corrected to match.

**Scope of this issue**

- `lajstrom admin retention --repo <format>/<name> [--package <glob>] --keep-last N --max-age D`
  with the server URL and a token (environment or file), so it runs anywhere a CronJob runs.
- Default is a dry run that prints the versions it would delete and why (rule, age, rank), as text
  and JSON; `--apply` deletes them through the API, one version at a time, and reports which were
  deleted and which failed (a delete of several versions is not all-or-nothing, see guarantees.md).
- Never deletes a version that a dist-tag (npm) or an equivalent pointer refers to; never the
  newest version of a package unless asked explicitly.
- Exit codes: 0 nothing to do or done, 2 dry run found versions to delete, 1 error (the same
  pattern as the declarative apply in ADR 0015).
- Docs: docs/operations.md "Retention" with a CronJob example, and the registry-rule route with
  its caveats (dist-tag journal `_j…` tags and cosign `sha256-…` tags live in the same OCI
  repository and must be excluded).

**Acceptance criteria**

- [ ] dry run and `--apply` against the dev stack, with an e2e step;
- [ ] a version pointed to by a dist-tag survives `--keep-last 1`;
- [ ] each deletion leaves an audit row naming the token that ran the job.

**Status**

Post-MVP, size M (was L: no rule engine, no scheduler in the server).

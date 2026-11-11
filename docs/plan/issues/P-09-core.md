---
id: P-09
title: "feat(api,cli): retention by external means: conditional delete, a list of versions, a batch, and lajstrom admin retention"
milestone: M3
lane: core
size: L
priority: p1
labels: [area/core, area/api, kind/feature]
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

**The race, and why a list cannot fix it**

The job lists versions, decides, then deletes, while users keep publishing. No snapshot fixes
that: it is stale the moment it is returned, and a set of versions cannot be deleted atomically on
top of a registry (the distribution API has no transactions). So every delete is conditional, and
the server checks the condition under the lock that publishing takes (compare-and-delete; the
delete already runs under the manifest's tag lock, `internal/ingest/delete.go`):

- the version still has the digest the job saw (`If-Match`; a version deleted and re-published
  under the same name is refused, not deleted);
- nothing points to it (an npm dist-tag moved onto it meanwhile: refused unless forced);
- it was published before the job's cut-off (`If-Unmodified-Since`, the grace age).

New uploads alone are harmless: for "keep the last N" and "older than D" a stale list deletes
fewer versions, never more, provided the list's cursor holds a place and not a position.

**Scope**

1. `GET /api/v1/repos/<format>/<name>/packages` and `…/packages/<package>/versions`: pages whose
   cursor holds a place, not a position (like the audit list); each version with its variants'
   digests, its publish time from the server's clock (recoverable by reindex: from the manifest),
   and what points to it (dist-tags). The web UI needs the same list (#136).
2. The single `DELETE` of a version takes `If-Match` and `If-Unmodified-Since`, and refuses a
   version something points to unless `?force=true`; all of it checked under the lock. `412` for a
   failed precondition, `409` naming the pointers.
3. `POST /api/v1/repos/<format>/<name>/versions:delete`: a list of `{package, version, digest}`,
   the cut-off, `dryRun`. Each item is checked and deleted under its own lock, with the same
   conditions as the single `DELETE`; the answer reports each item (`deleted`, `changed`,
   `protected`, `young`, `gone`, `error`). Not all-or-nothing; a rerun is idempotent. One job id
   in the audit rows ties the batch together.
4. `lajstrom admin retention --repo <format>/<name> [--package <glob>] --keep-last N --max-age D`
   with the server URL and a token (environment or file), so it runs anywhere a CronJob runs.
   Default is a dry run that prints the versions it would delete and why (rule, age, rank), as
   text and JSON; `--apply` sends them as one batch with the digests and the cut-off it decided
   on, and reports what the server did with each. Never deletes a version a dist-tag (npm) or an
   equivalent pointer refers to; never the newest version of a package unless asked explicitly.
   Exit codes: 0 nothing to do or done, 2 dry run found versions to delete, 1 error (the pattern
   of the declarative apply in ADR 0015).
5. Docs: docs/api.md, docs/spec/http-routing.md, a guarantee in guarantees.md ("a conditional
   delete never removes a version that changed after the client read it"), docs/operations.md
   "Retention" with a CronJob example, and the registry-rule route with its caveats (dist-tag
   journal `_j…` tags and cosign `sha256-…` tags live in the same OCI repository and must be
   excluded).

**Acceptance criteria**

- [ ] dry run and `--apply` against the dev stack, with an e2e step;
- [ ] a version re-published or dist-tagged between the list and `--apply` is reported, not
      deleted, proven by a test that races a publish against the delete;
- [ ] a version pointed to by a dist-tag survives `--keep-last 1`;
- [ ] each deletion leaves an audit row naming the token that ran the job.

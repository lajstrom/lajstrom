# 0031. Export and retention of the audit log: a line per row, an ascending feed, a prune

- Status: proposed; A, C and E decided on #1278 (2026-11-16)
- Date: 2026-11-17
- Deciders: gecube

## Context

PCI DSS v4.0.1 10.3.2: "Audit log files are protected to prevent modifications by individuals."
10.3.3: they are "promptly backed up to a secure, central, internal log server(s) or other media
that is difficult to modify". 10.3.4: "existing log data cannot be changed without generating
alerts". 10.5.1: "Retain audit log history for at least 12 months, with at least the most recent
three months immediately available for analysis." SOC 2 CC7.2 asks for monitoring (#1278,
[compliance.md](../compliance.md)). What Lajstrom has ([rbac.md](../spec/rbac.md) §5):

- The `audit` table of the index, a row written in the transaction of its change; no route and no
  command changes or deletes one, but the completion of a registry write's row.
- `GET /api/v1/audit` for the admin role, newest first, a page at a time.
- `lajstrom admin snapshot create --audit 90d|all`, a sealed copy of a window of rows in the
  registry (#76); the backups of the database.
- The access log and the application log on standard error, which the platform collects.

Whoever holds the database's credentials can change or delete rows and nothing notices; no row
reaches a store outside the database but in a backup or a snapshot; the table only grows.

## Considered options

- **A. A log line per audit row**, after the commit, on standard error beside the access log.
- **B. Audit rows as webhook events**, written to the outbox in the row's transaction.
- **C. An ascending feed**, `GET /api/v1/audit?after=<id>`, for a collector that polls.
- **D. Tamper evidence**: a hash chain over the rows, `lajstrom admin audit verify`.
- **E. Retention**: `lajstrom admin audit prune --before`, itself audited.

## Decision

A, C and E (the maintainer, 2026-11-16, on #1278); D later, or left to a write-once store
downstream. rbac.md §5 says what holds; these are the defaults proposed for what the decision
left open:

1. **The line** is the row as `GET /api/v1/audit` answers it, `details` whole, with `msg`
   `audit`, at info whatever `log.level`, in the format of every line (`log.format`). One shape
   serves the log store and the feed, and a collector needs no second parser. No secret is in it
   because no row holds one: the line is built from the committed row, never from the request.
2. **When**: after the commit, by the process that committed, `serve` and the commands of
   `lajstrom admin` alike; a line per state that commits, so a registry write has two. A
   rollback, or a commit whose result is unknown, writes none. A line that cannot be written is
   lost, and the change stands.
3. **A switch**, `audit.log`, `true` by default. It is a key of the configuration and not of
   `log.*`, so that turning the export off is a `config.change` row.
4. **The feed** is the list with `?after=<id>`: by id ascending, the existing filters with it,
   `limit` as any list, no `next`. A collector keeps the ids it holds.
5. **Ids** are unique and never reused, but are taken at the insert and seen at the commit: on
   PostgreSQL and CockroachDB a lower id may show after a higher one. The feed does not hide it;
   a collector resumes from the id it held 10 minutes before and drops the ids it has.
6. **The prune** is a command against the database only, so that no credential of the API can
   erase the trail of its own changes. `--before` takes a time or an age in days.
7. **Its floor**: a cut-off less than 365 days old is refused without `--force`, which goes down
   to 90 days and no further. The rows that reindex reads to repair a referrers index (a
   referrer's `push`) and a row still `pending` are never deleted.
8. **Its row**, `audit.prune`, is written as a registry write's: `pending` before the first batch,
   then `ok` or `failed` with the count deleted, so that a prune that stops shows. It deletes in
   batches of 10,000 rows, each a transaction, up to the last id it counted.

## Consequences

- With the defaults, every audit row is also a log line: the log store grows by one line per
  write and per change, and a `config.change` row's line can reach 64 KiB, which a container
  runtime splits (containerd at 16 KiB) and a collector's CRI parser joins.
- The log store, not the table, is the copy that the database's credentials do not reach: the
  operator makes it difficult to modify (object lock, a SIEM with its own admins). Without D,
  Lajstrom finds no change made in the database itself; a comparison of the feed with the log
  store does.
- A process that stops between its commit and its line loses the line; the feed has the row, so
  the comparison finds it.
- A prune deletes rows for good: only a backup or a snapshot made before has them. A snapshot's
  restore brings back the rows of its window, pruned since or not.
- A collector of the feed must hold the ids of an overlap, which a stateless poller of `since=`
  does not.
- A replica of the release before refuses a configuration with `audit.log`, and writes no line
  while it runs.

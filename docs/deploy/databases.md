# Databases: poolers, operators and Postgres look-alikes

Lajstrom's index on Postgres speaks to it through pgx v5 and `database/sql`. Behind a
connection pooler, under an operator such as CloudNativePG, or on a database that speaks the
Postgres protocol, what matters is what it asks of the server:

- **Prepared statements.** pgx's default exec mode prepares every statement once per connection
  under a name (`stmtcache_…`) and reuses it. The DSN parameter `default_query_exec_mode`
  changes that.
- **Transaction-scoped advisory locks only**, `pg_advisory_xact_lock`: the migration lock, the
  reindex lock, the referrers and tag lock, the npm journal lock. No session lock, no
  `LISTEN`/`NOTIFY`, no session `SET`. The referrers lock (`Store.WithReferrersLock`) keeps its
  transaction open, idle, while Lajstrom talks to the registry: seconds. It lifts
  `idle_in_transaction_session_timeout` for that transaction only (`set_config(…, true)`).
- **Read committed**, the default isolation: the index cache generations (INV-27) and the tag
  claims rely on a statement seeing what committed before it; a version write first locks its
  package row with an `UPDATE` (#584). `clock_timestamp()` reads the database's clock. One
  transaction is **repeatable read**: the update of a repo (`PATCH /api/v1/repos/…`), which
  reads the repo and writes it back whole. When another transaction changed the repo, or
  `auth_version`, after it read them, the database aborts it (`40001`) rather than let it
  undo that change, and Lajstrom runs it again from the start, up to four times in all (#791).
- **Migrations** run DDL and DML in one transaction each, under the migration lock.
- **Errors by SQLSTATE**: `23505` is a conflict, `23503` a missing row, `42501` a user without
  the right to migrate; `57P01`, `57P02`, `57P03`, `53300`, the class `08`, and the timeouts
  that end a session, `25P03`, `57P05` and `25P04`, mean the database did not answer, and a
  deadlock or a serialization failure, `40P01` or `40001`, is a transaction it aborted that may
  pass when sent again (Lajstrom sends the update of a repo again itself, up to four times in
  all, before it answers): for all of them the request gets `503` with `Retry-After: 5`
  ([operations.md](../operations.md#readiness)).

Everything below was measured on 2026-11-11 with Lajstrom at `4ab2f4e` (pgx v5.12.0, Go 1.27.4),
in Docker 29.8.2 on linux/arm64: the store conformance suite and the integration tests of
`internal/index`, `internal/ingest` and `internal/facade` (`go test -race -tags integration`,
with `LAJSTROM_TEST_POSTGRES_DSN` at the target and `LAJSTROM_TEST_REGISTRY` at Zot), and a
`serve` smoke: `lajstrom migrate`, `serve`, the e2e scenarios `raw.sh` and `oras.sh` (raw PUT,
GET, range, DELETE; an OCI push, pull and referrers), and `lajstrom reindex --verify`. Behind
PgBouncer in transaction mode the whole `go test -race -tags integration ./...` passed too, 30
packages; CI runs it there (`integration (zot-pgbouncer)`, #613).

## The matrix

| Target | Result | What to set |
| --- | --- | --- |
| PostgreSQL 17.11 | works | nothing |
| PgBouncer 1.26.1, `pool_mode = session` | works | nothing; one server connection per Lajstrom connection, so no saving |
| PgBouncer 1.26.1, `pool_mode = transaction`, `max_prepared_statements = 200` | works | nothing: 200 is PgBouncer's default since 1.24.0 |
| PgBouncer 1.26.1, `pool_mode = transaction`, `max_prepared_statements = 0` | **breaks**: `ERROR: prepared statement "stmtcache_7f80…" already exists (SQLSTATE 42P05)` and `… does not exist (SQLSTATE 26000)`; `lajstrom migrate` may pass, `serve` exits at start with the first | `default_query_exec_mode=exec` in the DSN (or `cache_describe` or `simple_protocol`; all three pass); not `describe_exec`: `ERROR: unnamed prepared statement does not exist (SQLSTATE 26000)` |
| Odyssey 1.5.2, `pool "transaction"`, `pool_reserve_prepared_statement yes` | works | nothing |
| Odyssey 1.5.2, `pool "transaction"`, `pool_reserve_prepared_statement no` | **breaks** as PgBouncer with 0 (`42P05`, `26000`) | `default_query_exec_mode=exec` or `simple_protocol` in the DSN |
| CloudNativePG | Postgres with failover, the rows above | [below](#cloudnativepg) |
| YugabyteDB 2026.1.3.0 (YSQL, PostgreSQL 15.12) | **experimental**: tested in a non-blocking CI cell, not guaranteed. Works with both conditions; without them **breaks** on a migration that changes rows (`23502`) and with two migrators at once (`42710`, `40001`) | `ysql_yb_ddl_transaction_block_enabled=true`; `lajstrom migrate` alone before `serve`; [below](#yugabytedb) |
| CockroachDB v26.3.5 | **experimental**: measured on one node, not tested in CI, not guaranteed. Works since #617: migrations of its own, applied one statement at a time, and read committed asked for by every transaction | `sql.txn.read_committed_isolation.enabled` left `true`, its default; no `DROP SCHEMA` in the database during a migration; [below](#cockroachdb) |

Behind any pooler in transaction mode, three more rules hold, measured on PgBouncer and Odyssey:

- **No session settings in the DSN.** A startup parameter set for one client stays on the server
  connection it ran on. With `search_path` in the DSN behind PgBouncer in transaction mode,
  statements ran in another client's schema (`relation "repos" does not exist (SQLSTATE
  42P01)`) or in none (`no schema has been selected to create in (SQLSTATE 3F000)`):
  PgBouncer restores only the parameters Postgres reports, and Postgres reports `search_path`
  only from version 18 ([`track_extra_parameters`](https://www.pgbouncer.org/config.html#track_extra_parameters)).
  `options=-c…` is refused by PgBouncer (`unsupported startup parameter in options:
  statement_timeout`) and dropped without a word by Odyssey. Set them on the role instead:
  `ALTER ROLE lajstrom SET search_path = lajstrom`, which every server connection of the role
  gets.
- **Every user Lajstrom logs in as must be known to the pooler**: in PgBouncer's `auth_file`, or
  through `auth_user` and `auth_query`; with a data-only user for `serve`
  ([operations.md](../operations.md#a-database-user-for-serve)) that is two users. An Odyssey
  route with `storage_user` logs every client in as that one user: give each user a route of its
  own.
- **Session mode changes nothing.** It works because each client keeps its server connection;
  it saves no server connections either.

## Settings

### The DSN

On Postgres, and behind a pooler that tracks prepared statements (PgBouncer ≥ 1.24.0 by default,
≥ 1.21.0 with `max_prepared_statements` set; Odyssey with `pool_reserve_prepared_statement yes`),
the plain DSN:

```
postgres://lajstrom:<password>@pgbouncer:6432/lajstrom?sslmode=require&pool_max_conns=10
```

Behind a pooler that does not (PgBouncer before 1.21.0 or with `max_prepared_statements = 0`,
Odyssey without it, a managed pooler you cannot configure), add
`default_query_exec_mode=exec`: each statement goes as an unnamed one in a single round trip,
which a transaction pooler keeps on one server connection.

`pool_max_conns` bounds the connections of one process. Without it the pool is the number of
CPUs, at least 4; in a container with a CPU limit that is usually the host's CPUs, not the
limit (Go's `runtime.NumCPU`), so set it.

### Pool sizes

Each `serve` replica opens up to `pool_max_conns` connections; so does each `lajstrom migrate`,
`reindex` and `admin` while it runs. A referrers lock holds one for as long as its push to the
registry takes.

- **Straight to Postgres**: replicas × `pool_max_conns`, plus one pool for a migration Job or a
  reindex, below `max_connections` minus `superuser_reserved_connections` (3) and what else
  connects. Three replicas with `pool_max_conns=10` take 30.
- **Through PgBouncer in transaction mode**: replicas × `pool_max_conns` are client connections,
  below `max_client_conn` of each PgBouncer; the server connections are `default_pool_size` (20
  by default) per database and user, per PgBouncer instance. A CloudNativePG Pooler with 3
  instances and `default_pool_size: "10"` opens up to 30 server connections for Lajstrom's user.
  Keep `default_pool_size` above the transactions Lajstrom has open at once: its requests'
  short ones and the referrers locks held during pushes.

### PgBouncer

```ini
[pgbouncer]
pool_mode = transaction
max_prepared_statements = 200   ; the default since 1.24.0
idle_transaction_timeout = 0    ; the default, and it must stay 0: see the referrers lock below
server_login_retry = 2          ; default 15: see restarts below
query_wait_timeout = 10         ; default 120: see restarts below
default_pool_size = 20
max_client_conn = 1000
```

### Odyssey

```
database "lajstrom" {
	user "lajstrom" {
		pool "transaction"
		pool_reserve_prepared_statement yes
		pool_discard no
		...
	}
}
```

The Odyssey image on ghcr.io (`ghcr.io/yandex/odyssey:1.5.2`) is linux/amd64 only; it was
measured under emulation.

### Postgres

`idle_in_transaction_session_timeout` may have any value: Lajstrom lifts it for the
transactions of its referrers locks, and its other transactions do not idle. `transaction_timeout`
(Postgres 17 and later) must be 0, the default, for Lajstrom's users; see the next section.

## The referrers lock and idle-in-transaction timeouts

`WithReferrersLock` begins a transaction, takes `pg_advisory_xact_lock`, and keeps the
transaction open and idle while its function writes to the registry. A timeout that ends the
transaction ends the lock with it. Measured before the fix of
[#615](https://github.com/lajstrom/lajstrom/issues/615), which no release contains, with a
function that works 6 seconds and a second writer that asks for the same lock at 0.5 s:

| Timeout | The second writer got the lock | The first one's `WithReferrersLock` |
| --- | --- | --- |
| Postgres `idle_in_transaction_session_timeout = '2s'`, on the database | at 2.0 s, while the first still ran | returned nil at 6 s |
| PgBouncer `idle_transaction_timeout = 3`, transaction mode | at 3.2 s (PgBouncer logged `pooler error: idle transaction timeout`) | returned nil at 6 s |

The lock was lost without an error, and two writers of one subject's referrers index
overlapped: one could write the index without the other's referrer. Since the fix:

- **Postgres's `idle_in_transaction_session_timeout`** no longer ends the lock: the statement
  that takes it sets the timeout to 0 for the rest of its transaction. With a timeout of 1 s on
  the role, the second writer waits until the first commits (`TestReferrersLockLost`).
- **A timeout Lajstrom cannot lift** (PgBouncer's `idle_transaction_timeout`, Postgres 17's
  `transaction_timeout`), a terminated backend or a failover still ends the lock, but no longer
  silently: the transaction ends with a checked `COMMIT`, and `WithReferrersLock` returns
  `index.ErrUnavailable`, so the push answers `503` with `Retry-After` and the client retries.
  The overlap itself still happens, and the referrer of the other writer, which succeeded, can
  be missing from the index without a report (docs/spec/index-schema.md, Coordination).

So keep PgBouncer's `idle_transaction_timeout` and Postgres's `transaction_timeout` at 0, their
defaults, for Lajstrom's users; where the server sets `transaction_timeout` for everyone, lift
it for them:

```sql
ALTER ROLE lajstrom SET transaction_timeout = 0;
ALTER ROLE lajstrom_serve SET transaction_timeout = 0;
```

A function under the lock is bounded by the registry's timeouts (`registries[].timeout`, 30 s
for the headers of each request): if a timeout must stay, make it several minutes. Odyssey's
`pool_idle_in_transaction_timeout` applies to session pooling only.

## Restarts and a forced primary switch

`serve` on Postgres 17.11, three clients each sending an authenticated `GET` of a raw file every
200 ms. A restart of the Postgres container (down about 0.7 s), then `pg_terminate_backend` of
every session of Lajstrom's database, as a primary that is switched away does, then Postgres
stopped for 20 s. `serve` was not restarted in any of them.

| | Restart | `pg_terminate_backend` | Stopped 20 s |
| --- | --- | --- | --- |
| Straight to Postgres | `503`, `Retry-After: 5` for 3 requests, then `200` | `503` for 2 requests (`57P01` on the next use of a pooled connection), then `200` | `503`, `Retry-After: 5` at once and throughout; `200` at the first answer |
| PgBouncer, defaults | `503` for 15 s after Postgres was back: `FATAL: server login has been failing, cached error: the database system is shutting down (server_login_retry) (SQLSTATE 08P01)`; one request waited 15 s | no error: PgBouncer replaces the server connections | no `503`: the requests waited up to 30.5 s in PgBouncer's queue and got `200` after the start |
| PgBouncer, `server_login_retry = 2`, `query_wait_timeout = 10` | `503` for about 2 s after Postgres was back | no error | `503` at once (a request already waiting after 10 s); `200` 12 s after the start (the Compose host name is gone while its container is stopped, and PgBouncer caches the failed lookup for `dns_nxdomain_ttl`, 15 s) |

Lajstrom maps PgBouncer's errors (class `08`) to `503`, as it does Postgres's. PgBouncer's
defaults turn an outage into requests that wait, up to `query_wait_timeout` (120 s), and a
restart into 15 s of errors after the database is back; the two settings above make it behave
like Postgres itself.

## CloudNativePG

[CloudNativePG](https://cloudnative-pg.io/) (1.30.1 is the latest release) runs Postgres with
streaming replicas and failover; to Lajstrom it is Postgres, and the rows above apply. Not
installed for this page: what follows is its documentation and the measurements above.

- **Point `database.dsn` at the `<cluster>-rw` Service**, which follows the primary, or at a
  Pooler of `type: rw`. `-ro` and `-r` may reach a replica, which refuses writes, and Lajstrom
  writes on every path ([Service management](https://github.com/cloudnative-pg/cloudnative-pg/blob/v1.30.1/docs/src/service_management.md)).
- **A failover or a switchover drops the connections**: the old primary is shut down "fast",
  which "does not wait for PostgreSQL clients to disconnect"; active transactions are rolled back
  ([Automated failover](https://github.com/cloudnative-pg/cloudnative-pg/blob/v1.30.1/docs/src/failover.md)).
  A rolling update restarts the primary or switches over (`primaryUpdateMethod`, [Rolling
  updates](https://github.com/cloudnative-pg/cloudnative-pg/blob/v1.30.1/docs/src/rolling_update.md)).
  To Lajstrom this is the restart and `pg_terminate_backend` above: `503` with `Retry-After` for
  the requests in flight, and the replicas serve again when the Service points at the new
  primary, without a restart.
- **Replication is asynchronous unless you set it**: "Synchronous replication is disabled by
  default" ([Replication](https://github.com/cloudnative-pg/cloudnative-pg/blob/v1.30.1/docs/src/replication.md)).
  A failover can then lose the last commits. The derived rows (packages, versions, files,
  `oci_repos`, the npm journal) come back with reconcile or `lajstrom reindex` from the registry
  (INV-3, [operations.md](../operations.md#reindex)); the control plane does not: a user,
  token, binding, key or repo made just before the failover may be gone, and so may audit rows.
  For RPO 0, `spec.postgresql.synchronous` with `method: any`, `number: 1` and
  `dataDurability: required`, which stops writes while no standby confirms them.
- **The Pooler is PgBouncer, in `session` mode unless `spec.pgbouncer.poolMode` says
  `transaction`** (the field's default is `session`). Its `spec.pgbouncer.parameters` accept
  `max_prepared_statements`, `server_login_retry`, `query_wait_timeout`,
  `idle_transaction_timeout`, `default_pool_size` and `max_client_conn`
  ([Connection pooling](https://github.com/cloudnative-pg/cloudnative-pg/blob/v1.30.1/docs/src/connection_pooling.md)):
  set them as in [PgBouncer](#pgbouncer). The image is CloudNativePG's PgBouncer build; at 1.24.0 or
  later `max_prepared_statements` is 200 without setting it, and before 1.21.0 the DSN needs
  `default_query_exec_mode=exec`.
- CloudNativePG does not set `transaction_timeout`; if your `spec.postgresql.parameters` do,
  lift it for Lajstrom's users as above.

## YugabyteDB

**Experimental: tested in a non-blocking CI cell; not guaranteed.** Lajstrom runs on YugabyteDB
under two conditions, both measured below:

1. The YB-TServer flag `ysql_yb_ddl_transaction_block_enabled=true`, so that a migration is
   one transaction.
2. One migrator at a time: `lajstrom migrate` alone (the Job of
   [operations.md](../operations.md#several-replicas)) before any `serve` starts, at install
   and at every upgrade, so that the replicas find nothing to apply.

The cell `integration (zot-yugabyte)` of the YugabyteDB workflow (#624) runs the index's
integration tests that way: nightly, on demand, and on pull requests that change
`internal/index`. A failure there blocks no merge; it is a finding to look at.

YugabyteDB 2026.1.3.0 (YSQL on PostgreSQL 15.12), one node started by `yugabyted` with its
defaults: transactions are read committed (`yugabyted` turns `yb_enable_read_committed_isolation`
on for new universes since 2025.2, [Read Committed](https://docs.yugabyte.com/stable/architecture/transactions/read-committed/)),
and `pg_advisory_xact_lock` blocks (`ysql_yb_enable_advisory_locks`, on by default). What
passes, on a new database and one migration at a time:

- `lajstrom migrate` and the `serve` smoke (raw, oras with a referrer, `reindex --verify`);
- the store conformance suite with `-parallel 1`: 61 tests (60 of them took 994 s, most of it
  the 19 migrations of each test's new schema); `TestDataRole`, `TestTerminatedConnection`,
  `TestReplicasCoexist`;
- the integration tests of ingest, reindex and the facade, on a migrated database;
- concurrent first publishes of one package, since
  [#629](https://github.com/lajstrom/lajstrom/issues/629). YugabyteDB takes the entries of the
  two unique indexes of `packages`, `(repo_id, name)` and `(repo_id, name_oci)`, in no fixed
  order, so writers of one package path deadlocked (`40P01`, 9 of 40 rounds of 16 writers),
  where Postgres lets them share the row. Lajstrom recognises YugabyteDB by the `server_version`
  it reports when a connection starts and then takes `pg_advisory_xact_lock` on the package path
  before it inserts a new package; Postgres runs the same statements as before. A `40P01` or
  `40001` that remains is `503` with `Retry-After`. `PackageNameOCIRace` and `PackageRace` pass
  10 of 10 runs.

What breaks:

- **A terminated backend's advisory lock can outlive it by a minute.** A backend that
  `pg_terminate_backend` ends inside a transaction sends the abort of that transaction as it
  exits, and the abort can lose the race with the shutdown of the backend's connection to the
  YB-TServer ([yugabyte-db#31866](https://github.com/yugabyte/yugabyte-db/issues/31866), open).
  The YB-TServer then aborts the transaction, and so releases its `pg_advisory_xact_lock`, when
  it expires the backend's session: at once when the backend's process is gone 50 ms after
  that connection closed
  ([`CheckSessionShutdown`](https://github.com/yugabyte/yugabyte-db/blob/2026.1.3.0-b100/src/yb/tserver/pg_client_service.cc#L956-L981)),
  else [`pg_client_session_expiration_ms`](https://docs.yugabyte.com/stable/reference/configuration/all-flags-yb-tserver/#pg-client-session-expiration-ms)
  later, 60 s by default. Postgres releases it at once. Measured on 2026.1.3.0, one node: in one
  run all 6 terminations released the lock within 0.25 s, in another all 6 waited for the
  expiry, 60.1 s with the default; `TestReferrersLockLost/terminated` once gave up after
  56 s. A backend whose client closed the connection, with a Terminate message or without,
  aborts its transaction itself: the lock went within 0.1 s, 12 of 12. So a replica that dies,
  whose kernel closes its connections, frees its locks at once; a backend terminated while it
  holds a referrers, tag, journal or package lock blocks the other writers of that subject or
  package for up to a minute. On YugabyteDB `TestReferrersLockLost/terminated` waits for that
  ([#640](https://github.com/lajstrom/lajstrom/issues/640)) and logs how long it waited; the
  cell runs `go test -v`, so the line is in its log when the test passes too
  ([#681](https://github.com/lajstrom/lajstrom/issues/681)).

  What to do: stop or restart a replica rather than terminate its backends. To shorten the
  wait, lower `pg_client_session_expiration_ms` on every YB-TServer (a restart), and keep it at
  least three times
  [`pg_client_heartbeat_interval_ms`](https://docs.yugabyte.com/stable/reference/configuration/all-flags-yb-tserver/#pg-client-heartbeat-interval-ms)
  (10 s by default), the interval at which a live backend renews its session: a session not
  renewed within the expiry is ended, and its transaction aborted. Measured:
  `pg_client_session_expiration_ms=30000` released the lock after 30.1–30.2 s, and `=10000`
  with `pg_client_heartbeat_interval_ms=3000` after 10.1 s; with either, a transaction that
  held the lock idle for 60 s and a connection idle for 60 s kept working.
  `yb_cancel_transaction()` frees the lock at once, but it needs the transaction's id, which
  neither `pg_locks` nor `yb_lock_status()` shows for an advisory lock.

- **DDL inside a transaction is not transactional** unless the YB-TServer flag
  `ysql_yb_ddl_transaction_block_enabled` is on (`yb_ddl_transaction_block_enabled = off` by
  default): each DDL statement of a migration commits on its own. Migration 0008 on a database
  that has users fails: `ERROR: column "uid" of relation "users" contains null values (SQLSTATE
  23502)`. Its `ALTER … SET NOT NULL` does not see the `UPDATE` before it, and the column it
  added stays after the rollback, so the migration cannot run again as it is. A new database has
  no rows, which is why `lajstrom migrate` passes there. With
  `--tserver_flags=ysql_yb_ddl_transaction_block_enabled=true` (a restart) `TestMigration0008`,
  `TestMigration0009` and `TestUpgrade` pass.
- **Two migrators at once.** Two stores migrating one new schema, as two replicas starting
  together on an unmigrated database do: `ERROR: type "schema_migrations" already exists
  (SQLSTATE 42710)`, with the flag or without: the second does not see the table the first
  committed while it waited for the migration lock. Migrations of other schemas at the same
  time (the test suite's parallel tests): `ERROR: could not serialize access due to concurrent
  update (query layer retries aren't supported for multi-statement queries issued via the
  simple query protocol, …) (SQLSTATE 40001)`
  ([yugabyte-db#21833](https://github.com/yugabyte/yugabyte-db/issues/21833)); and once a
  migration's `COMMIT` waited more than six minutes (`RPCWait`, `TransactionCommit`) while the
  other migrators waited for the lock, until the run's timeout.

Not measured: a cluster of several nodes, a node failure, load.

## CockroachDB

**Experimental: measured on one node, not tested in CI, not guaranteed** (#617). The `postgres`
driver tells CockroachDB by the `crdb_version` it reports when a connection starts, and then:

- applies migrations of its own (`internal/index/migrations/cockroach`), one statement at a time,
  each in a transaction of its own, under the migration lock held by a transaction on a second
  connection, recording the version after the last statement
  ([index-schema.md](../spec/index-schema.md#migrations)). Their text columns have no
  `COLLATE "C"`: CockroachDB has no such collation, and compares strings by their bytes already
  (measured: `'B' < 'Z' < '_' < 'a' < 'b' < 'é'`). Their ids are identity columns, which ascend
  from 1 as `bigserial` does on Postgres; CockroachDB's `bigserial` is `unique_rowid()`, values
  above 2^53 that a JSON number in JavaScript does not hold;
- begins every transaction `ISOLATION LEVEL READ COMMITTED`, as the index needs (the index cache
  generations, the tag claims, the package row; see the top of this page), where CockroachDB's
  default is `SERIALIZABLE`. `serve`, `migrate` and the other commands refuse to start when the
  cluster would run such a transaction serializable, which it does, without an error, when the
  cluster setting `sql.txn.read_committed_isolation.enabled` is off;
- answers `503` with `Retry-After` for `40003`, a commit whose result is unknown, besides the
  SQLSTATEs of Postgres, and never sends that transaction again.

The one condition: **`sql.txn.read_committed_isolation.enabled` stays `true`**, its default on
v26.3.5 (measured). Nothing is set in the DSN.

Measured on 2026-11-14 with Lajstrom at the commits of #617 (pgx v5.12.0, Go 1.27.4):
CockroachDB v26.3.5, one node in a container (`start-single-node --insecure --cache=256MiB
--max-sql-memory=256MiB`), `go test -race -tags integration` with `LAJSTROM_TEST_POSTGRES_DSN`
at it ([Reproducing](#reproducing)). What passes:

- `pg_advisory_xact_lock` blocks a second session until the first one's transaction ends, and
  its keys are per database, as on Postgres; the store's transactions run read committed
  (`TestCockroachReadCommitted`, which asks `crdb_internal.cluster_sessions`);
- `lajstrom migrate` on an empty database applies the 21 migrations in about 4 s on a fresh
  node, and in 39 s on the same node after the tests had dropped some 500 databases, whose
  1,440 schema-change GC jobs kept its CPUs busy; two at once apply each once, the second
  finding the schema current; `TestConcurrentMigrate`;
  `TestCockroachRerun` runs every statement of every migration a second time without an error;
  `TestUpgrade` on the cockroach dialect;
- the store conformance suite, every case, the races among them, with `-parallel 4` and
  `LAJSTROM_TEST_POSTGRES_ISOLATION=database` (below): 263 s on a fresh node. The 14 race
  cases then passed 5 runs of 5, 70 in all; `PackageRace` and `PackageNameOCIRace` among them without the package lock
  that YugabyteDB needs ([#629](https://github.com/lajstrom/lajstrom/issues/629));
- `TestDataRole`, its conformance half included (the suite again as a user with data rights
  only, `GRANT … ON ALL SEQUENCES` among them; a later table; a pending migration refused);
  `TestReplicasCoexist`; `TestRepeatableRead` (the update of a repo, serializable here, since
  the cluster setting `sql.txn.repeatable_read_isolation.enabled` is off by default, and run
  again on `40001` as on Postgres);
- `TestReferrersLockLost`: with `idle_in_transaction_session_timeout` of 1 s on the role, the
  referrers lock outlives it (`set_config(…, true)` lifts it); a session that `CANCEL SESSION`
  ends while it holds the lock makes `WithReferrersLock` return `ErrUnavailable` (the lock was
  lost), and the next writer gets the lock at once. `TestTerminatedConnection` the same way.
  CockroachDB has no `pg_terminate_backend`; the tests use `CANCEL SESSION` there.
- the integration tests of reindex, ingest, the facade, the management API and `cmd/lajstrom`,
  against Zot v2.1.23, on one migrated database.

What does not work, or works otherwise:

- **A migration is not atomic.** Its statements commit one by one. One that fails leaves the
  ones before it applied and the version unrecorded; the error names the statement, and the
  next `lajstrom migrate` or `serve` applies the migration again from its first statement,
  which the cockroach files allow. Replicas that serve meanwhile see the statements' effects
  one by one ([index-schema.md](../spec/index-schema.md#migrations)).
- **A migration holds two connections** of its pool, one for the lock's transaction: a pool of
  one (`pool_max_conns=1`) is refused with that message.
- **No `DROP SCHEMA` in Lajstrom's database during a migration.** Measured: CockroachDB's
  `DROP SCHEMA` waits for every open transaction of the database, the migration lock's among
  them, and a `CREATE TABLE` in a schema of the database (a `search_path` other than `public`)
  waits for that `DROP SCHEMA`, so the two wait for each other until one is cancelled. A
  `CREATE SCHEMA`, and a `CREATE TABLE` in `public`, do not wait. The tests in parallel drop
  a schema per test, which is why they take a database each on CockroachDB.
- **The errors of its timeouts differ.** `idle_in_transaction_session_timeout` closes the
  connection (Lajstrom sees an EOF: `503`, as on Postgres); `transaction_timeout` ends a
  statement with `57014`, which Lajstrom does not map, a `500`: keep it 0, as on Postgres; a
  deadlock is `40001`, not `40P01`.
- **More aborted transactions than on Postgres.** A transaction that CockroachDB aborts on a
  deadlock (`40001`) is `503` with `Retry-After`, as on Postgres, but under read committed it
  locks the row of a unique key that `INSERT … ON CONFLICT DO NOTHING` finds, where Postgres
  locks nothing, and writers that then delete or update that row deadlock (measured: 8
  writers taking over one stale fetch claim). A store method's own transaction runs again, four
  times in all; a request's transaction (`Store.Tx`) does not, and answers `503`.
- **DDL in an explicit transaction commits it first** (`autocommit_before_ddl`, on by default:
  `NOTICE: auto-committing transaction before processing DDL`), which is one more reason the
  Postgres migrations, one transaction each, cannot run as they are.

Not measured: a cluster of several nodes, a node failure, load, a secure cluster (passwords and
TLS).

## Reproducing

The stack of this page was one Compose project with Postgres
`postgres:17.11-alpine@sha256:3540c0362c809df084ffac0ae69fe794515f9de92692f02178bcdce0ef0302ac`,
PgBouncer `edoburu/pgbouncer:v1.26.1-p0@sha256:488422e38cff912895d0d65d8963fcd306e6ac589a724398b6eb8e294e94d624`
(PgBouncer 1.26.1), Odyssey `ghcr.io/yandex/odyssey:1.5.2@sha256:dceed1de1a2aed0c58a29e8ccf097f61efd75c172ad0e5069b133d4e3a62d278`,
YugabyteDB `yugabytedb/yugabyte:2026.1.3.0-b100@sha256:c1ddbf9c189f6ef33f03c648efadb1b1e82b5e7c8be472c56b1357c024221528`
and CockroachDB `cockroachdb/cockroach:v26.3.5@sha256:9f9172a9c03771263bd8af89dbb76d490d5cd6d8c6029a23de5b3456f01cb9ff`.
Each PgBouncer had `* = host=postgres port=5432` (every database name to the server),
`auth_type = scram-sha-256`, `auth_user = lajstrom` and the pool mode and
`max_prepared_statements` of its row. The suites, with a fresh database per run:

```sh
LAJSTROM_TEST_POSTGRES_DSN='postgres://lajstrom:lajstrom@localhost:6432/lajstrom?sslmode=disable' \
LAJSTROM_TEST_POSTGRES_ISOLATION=database LAJSTROM_TEST_REGISTRY=localhost:5000 \
  go test -race -count=1 -tags integration ./internal/index/... ./internal/ingest/... ./internal/facade/...
```

On CockroachDB, started with `start-single-node --insecure --cache=256MiB --max-sql-memory=256MiB`,
the same command ran with `LAJSTROM_TEST_POSTGRES_DSN='postgres://root@localhost:26257/lajstrom?sslmode=disable'`
and `LAJSTROM_TEST_POSTGRES_ISOLATION=database`, and with `-parallel 4` for `internal/index/postgres`.

`LAJSTROM_TEST_POSTGRES_ISOLATION=database` (#611) gives each test a database of its own,
since a schema in the DSN's `search_path` does not survive a transaction pooler
([testing.md](../testing.md#store-conformance)). One failure in the pooled runs is not
Lajstrom's: `TestDataRole` behind Odyssey, whose route logged the test's data-only user in as
the owner, so the pending migration was tried instead of refused.

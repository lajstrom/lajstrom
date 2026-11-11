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
  transaction open, idle, while Lajstrom talks to the registry: seconds.
- **Read committed**, the default isolation: the index cache generations (INV-27) and the tag
  claims rely on a statement seeing what committed before it; a version write first locks its
  package row with an `UPDATE` (#584). `clock_timestamp()` reads the database's clock.
- **Migrations** run DDL and DML in one transaction each, under the migration lock.
- **Errors by SQLSTATE**: `23505` is a conflict, `23503` a missing row, `42501` a user without
  the right to migrate; `57P01`, `57P02`, `57P03`, `53300` and the class `08` mean the database
  did not answer, and the request gets `503` with `Retry-After: 5`
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
| CockroachDB v26.3.5 | **breaks**: `lajstrom migrate` fails on the first migration | none; [below](#cockroachdb) |

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
idle_transaction_timeout = 0    ; the default; see the referrers lock below
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

`idle_in_transaction_session_timeout` must be 0 (the default) for Lajstrom's users, or longer
than any referrers lock is held; see the next section.

## The referrers lock and idle-in-transaction timeouts

`WithReferrersLock` begins a transaction, takes `pg_advisory_xact_lock`, and keeps the
transaction open and idle while its function writes to the registry. A timeout that ends idle
transactions ends the lock with it. Measured with a function that works 6 seconds and a second
writer that asks for the same lock at 0.5 s:

| Timeout | The second writer got the lock | The first one's `WithReferrersLock` |
| --- | --- | --- |
| Postgres `idle_in_transaction_session_timeout = '2s'`, on the database | at 2.0 s, while the first still ran | returned nil at 6 s |
| PgBouncer `idle_transaction_timeout = 3`, transaction mode | at 3.2 s (PgBouncer logged `pooler error: idle transaction timeout`) | returned nil at 6 s |

The lock is lost without an error, and two writers of one subject's referrers index overlap:
one may write the index without the other's referrer. Keep both timeouts at 0 (their
defaults) for Lajstrom's users; where the server sets one for everyone, lift it for them:

```sql
ALTER ROLE lajstrom SET idle_in_transaction_session_timeout = 0;
ALTER ROLE lajstrom_serve SET idle_in_transaction_session_timeout = 0;
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
- CloudNativePG does not set `idle_in_transaction_session_timeout`; if your
  `spec.postgresql.parameters` do, lift it for Lajstrom's users as above.

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
- the store conformance suite with `-parallel 1`: 60 tests in 994 s, most of it the 19
  migrations of each test's new schema; `TestDataRole`, `TestTerminatedConnection`,
  `TestReplicasCoexist`;
- the integration tests of ingest, reindex and the facade, on a migrated database.

What breaks:

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

CockroachDB v26.3.5 (single node, `--insecure`) answers the Postgres protocol and has
`pg_advisory_xact_lock` (a second transaction waited for the first), `clock_timestamp()` and
`set_config`. `lajstrom migrate` stops at the first migration:

```
lajstrom: index: migration postgres/0001_init.sql: ERROR: at or near ",": syntax error: invalid locale C: language: tag is not well-formed (SQLSTATE 42601)
```

Every text column is `COLLATE "C"`, so that order and comparison match SQLite, and CockroachDB
has no collation named `C` (its default comparison of strings is bytewise already). With the
`COLLATE "C"` removed from a scratch copy of the migrations, it stops at the eighth, which adds a
column and fills it in one transaction:

```
lajstrom: index: migration postgres/0008_user_uid.sql: ERROR: column "uid" does not exist (SQLSTATE 42703)
```

The `UPDATE` after the `ALTER TABLE … ADD COLUMN` does not see the column in the same
transaction; `autocommit_before_ddl=true` in the DSN gave the same error. Its transactions are
also `SERIALIZABLE` unless the cluster setting `sql.txn.read_committed_isolation.enabled` and
the session ask for read committed, where Lajstrom expects read committed and retries no
`40001`. CockroachDB needs migrations of its own and a dialect, which is more than a setting; it
was not measured further.

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

`LAJSTROM_TEST_POSTGRES_ISOLATION=database` (#611) gives each test a database of its own,
since a schema in the DSN's `search_path` does not survive a transaction pooler
([testing.md](../testing.md#store-conformance)). One failure in the pooled runs is not
Lajstrom's: `TestDataRole` behind Odyssey, whose route logged the test's data-only user in as
the owner, so the pending migration was tried instead of refused.

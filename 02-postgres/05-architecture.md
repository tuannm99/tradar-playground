# 2.5 PostgreSQL: architecture, replication and scaling

> **Level:** architecture · **Prerequisites:** [2.1](01-storage-and-mvcc.md), [2.4](04-transactions-and-locking.md), [1.1 §7](../01-fundamentals/01-storage.md#7-durability-the-write-ahead-log) · **After this chapter you can:** describe the processes and memory of a PostgreSQL server, choose a replication and failover design, and decide when partitioning or a pooler is warranted.

## 1. Process and memory model

PostgreSQL uses **one OS process per client connection** (a *backend*), plus background processes. All share memory:

```
clients ─► postmaster ─► backend (one per connection) ─┐
                                                        ├─ shared_buffers (page cache)
 background: checkpointer · background writer ·         ├─ WAL buffers
 WAL writer · autovacuum launcher/workers ·             ├─ lock tables, proc array (snapshots)
 WAL sender/receiver (replication) · stats collector    └─ per-backend: work_mem, temp buffers
```

Consequences:

- A connection costs memory (a few MB at least) and a process; thousands of idle connections hurt. Use a **connection pooler** (§6).
- Each sort/hash node may use up to `work_mem`, and a complex query can have several, so total memory ≈ connections × nodes × `work_mem`. Keep global `work_mem` modest and raise it per session for analytical queries.
- `shared_buffers` (here 128 MB default; commonly ~25% of RAM) caches pages, and the OS page cache holds more; `effective_cache_size` tells the planner the sum.

## 2. WAL, checkpoints, and crash recovery

Changes are appended to WAL before pages are written ([2.1 §5](01-storage-and-mvcc.md#5-write-ahead-log-in-one-paragraph)). The **checkpointer** periodically flushes dirty pages and records a restart point; recovery replays WAL since that point. Frequency: `checkpoint_timeout` (5 min) or when WAL reaches `max_wal_size` (1 GB). Frequent checkpoints = more full-page writes and I/O spikes; too rare = longer crash recovery and more WAL on disk. `wal_level` (`replica` by default) controls how much information is logged (`logical` for logical replication).

```sql pg-local
show shared_buffers;
show wal_level;
show max_wal_size;
select checkpoints_timed, checkpoints_req, buffers_checkpoint from pg_stat_bgwriter;  -- PostgreSQL 16
```

(PostgreSQL 17 moves checkpoint counters to `pg_stat_checkpointer`.) A high `checkpoints_req` relative to `checkpoints_timed` means WAL fills before the timer: raise `max_wal_size`.

## 3. Replication

### Physical (streaming) replication

A standby connects to the primary's **WAL sender** and replays the primary's WAL byte for byte. It is an exact copy of the whole cluster, read-only (a *hot standby* can serve queries).

| Aspect | Detail |
| --- | --- |
| Consistency | async by default → standby may lag; a failover can lose recent commits |
| `synchronous_commit` | `off` < `local` < `remote_write` < `on` (default; waits for local flush, and for sync standbys if configured) < `remote_apply` (visible on the standby) |
| `synchronous_standby_names` | which standbys must confirm; trading commit latency for durability across nodes |
| Standby queries | can conflict with replay (vacuum removing rows the query needs) → cancellations or `hot_standby_feedback` (which holds back vacuum on the primary) |
| Lag | `pg_stat_replication` (`write_lag`, `flush_lag`, `replay_lag`) |

### Logical replication

Publishes row changes (decoded from WAL, `wal_level = logical`) per table to subscribers, which may run a different major version or schema. Uses: zero-downtime upgrades, selective replication, feeding other systems. Limits: no DDL replication, sequences and large objects not replicated, conflict handling is up to you.

### Replication slots

A slot makes the primary retain WAL until a consumer has received it. **An abandoned slot retains WAL forever and fills the disk** and also holds back vacuum (the horizon in [2.1 §4](01-storage-and-mvcc.md#what-blocks-vacuum-from-removing-dead-tuples)). Monitor `pg_replication_slots`.

## 4. High availability

PostgreSQL itself does not elect leaders. Tools do: **Patroni** (uses etcd/Consul/ZooKeeper for the leader lease), **repmgr**, or managed services. The essential problems any HA setup must solve:

1. **Failure detection** without flapping.
2. **Single leader** (avoid *split brain*): fencing the old primary, leader lease with a consensus store.
3. **Promotion** of the most advanced standby (`pg_promote`), and re-pointing clients (virtual IP, DNS, a proxy like HAProxy/PgBouncer).
4. **Data loss window** (RPO): equals replication lag at failure for async; zero for sync.
5. **Rejoining** the old primary (`pg_rewind`).

## 5. Backups

| Method | Notes |
| --- | --- |
| `pg_dump` | logical, per database; portable; slow to restore at scale |
| `pg_basebackup` | physical base copy |
| **WAL archiving + base backup = PITR** | restore to any moment (e.g. just before a bad `DELETE`) |
| Tools: pgBackRest, Barman, WAL-G | incremental, parallel, retention |

A backup you have not **restored** is a hope. Schedule restore drills.

## 6. Connection pooling

Because a connection is a process, apps with hundreds of workers should connect through **PgBouncer** (or pgcat/Odyssey):

| Mode | Server connection held | Caveats |
| --- | --- | --- |
| session | for the whole client session | little gain |
| **transaction** | only during a transaction | no session state across transactions (`SET`, advisory locks, prepared statements need care) |
| statement | per statement | no multi-statement transactions |

A pool sized to roughly the number of CPU cores to a few times that often **raises** throughput versus thousands of direct connections: less context switching and lock contention. The pool also bounds concurrency (Little's law, [1.6](../01-fundamentals/06-measuring.md)).

## 7. Partitioning

Declarative partitioning splits a table by `RANGE`, `LIST` or `HASH`. The planner **prunes** partitions whose bounds contradict the `WHERE`:

```sql pg-local
create table play_part (id bigserial, ts timestamptz not null, v int) partition by range (ts);
create table play_part_2026_10 partition of play_part for values from ('2026-10-01') to ('2026-11-01');
create table play_part_2026_11 partition of play_part for values from ('2026-11-01') to ('2026-12-01');
explain (costs off) select count(*) from play_part where ts >= '2026-10-05' and ts < '2026-10-06';
```

Measured with three monthly partitions: a date filter scans **only** `play_part_2026_10`; a filter on `v` (not the key) shows an `Append` over **every** partition. Use partitioning for: cheap retention (`drop`/`detach` a partition), keeping indexes small and vacuum fast on huge append-mostly tables, and bulk loading. Not for: speeding queries that don't filter on the key. Primary/unique keys must include the partition key. Hundreds are fine; tens of thousands slow planning.

## 8. Scaling out

1. **Vertical**: bigger machine, faster disks — simplest, covers most systems.
2. **Read replicas**: offload reads that tolerate lag; route in the app or a proxy. Beware read-your-writes ([5.2](../05-architecture/02-replication-and-consistency.md)).
3. **Caching** in front of hot reads ([5.4](../05-architecture/04-caching-and-pooling.md)).
4. **Partitioning** within a node.
5. **Sharding** across nodes: application-level or extensions like **Citus**; adds distributed-transaction and cross-shard query costs ([5.3](../05-architecture/03-partitioning-and-sharding.md)).

## 9. Extensions worth knowing

`pg_stat_statements` (query stats), `pg_trgm` (fuzzy text), `PostGIS`, `pgvector`, `pg_partman` (partition automation), `pgstattuple` (bloat), `auto_explain` (log slow plans), `pg_repack` (online table rewrite), `TimescaleDB`.

## Exercises

1. Draw the data path of one `UPDATE ... COMMIT` on a primary with one synchronous standby (`remote_apply`): which fsyncs/acknowledgements are on the critical path, and what is the latency floor?
2. A standby shows `replay_lag` of 40 s during a nightly report. List three possible causes and how to tell them apart.
3. Design the HA setup for a 2-node cluster plus a witness; explain what happens when the network partitions the primary from the other two.
4. Size `max_connections`, PgBouncer pool size and `work_mem` for 16 cores and 64 GB RAM with 800 application workers.
5. Compare partitioning by month vs by hash(customer_id) for `play_orders` for three queries of your choice.

→ Answers: [appendix/solutions.md](../appendix/solutions.md#25-postgresql-architecture).

Next: [2.6 Tuning playbook](06-tuning-playbook.md).

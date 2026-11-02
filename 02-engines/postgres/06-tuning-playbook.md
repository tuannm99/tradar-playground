# 2a.6 PostgreSQL tuning playbook

> **Level:** practical · **Prerequisites:** [2a.1](01-storage-and-mvcc.md)–[2a.5](05-architecture.md) · A task-oriented reference: *symptom → diagnosis → fix*. Queries run on the seed; settings are starting points, not laws.

## 1. First hour on an unfamiliar database

1. Version and settings that matter:

```sql pg-local
select version();
show shared_buffers; show effective_cache_size; show work_mem; show maintenance_work_mem;
show max_connections; show random_page_cost; show max_wal_size; show autovacuum;
```

2. What is it doing right now?

```sql pg-local
select pid, state, wait_event_type, wait_event, now() - query_start as running, left(query, 80) as query
from pg_stat_activity where state <> 'idle' and pid <> pg_backend_pid() order by running desc;
```

3. What costs the most overall (needs `pg_stat_statements`: add it to `shared_preload_libraries`, restart, `create extension pg_stat_statements`):

```sql pg-local
select calls, round(total_exec_time::numeric, 1) as total_ms, round(mean_exec_time::numeric, 2) as mean_ms,
       rows, left(query, 80) as query
from pg_stat_statements order by total_exec_time desc limit 10;
```

Rank by `total_exec_time` (calls × mean), then look at `mean_exec_time` and `stddev_exec_time` for outliers, and `shared_blks_read` for I/O hogs.

4. Biggest tables and indexes, and bloat suspects:

```sql pg-local
select c.relname, pg_size_pretty(pg_total_relation_size(c.oid)) as total, s.n_live_tup, s.n_dead_tup
from pg_class c join pg_stat_user_tables s on s.relid = c.oid order by pg_total_relation_size(c.oid) desc limit 10;
```

5. Cache hit ratio per table:

```sql pg-local
select relname, heap_blks_hit, heap_blks_read,
       round(100.0 * heap_blks_hit / nullif(heap_blks_hit + heap_blks_read, 0), 2) as hit_pct
from pg_statio_user_tables order by heap_blks_read desc limit 10;
```

## 2. Baseline configuration

Rules of thumb for a dedicated server (measure, then adjust):

| Setting | Starting point | Why |
| --- | --- | --- |
| `shared_buffers` | ~25% of RAM | OS cache covers the rest |
| `effective_cache_size` | 50–75% of RAM | planner hint only |
| `work_mem` | 4–64 MB; raise per session for reports | multiplied by nodes × connections |
| `maintenance_work_mem` | 256 MB–2 GB | faster `CREATE INDEX`, vacuum |
| `random_page_cost` | 1.1–2 on SSD | default 4 assumes spinning disks |
| `effective_io_concurrency` | 100–200 on SSD/NVMe | bitmap-scan prefetch |
| `max_wal_size` | 4–16 GB for write-heavy | fewer forced checkpoints |
| `checkpoint_completion_target` | 0.9 | spread I/O |
| `autovacuum_vacuum_scale_factor` | 0.01–0.05 on big tables (per table) | default 0.2 is far too lazy at 500 M rows |
| `autovacuum_vacuum_cost_limit` | raise (e.g. 1000–2000) on fast storage | default throttles heavily |
| `idle_in_transaction_session_timeout` | 1–10 min | kill stuck snapshots |
| `statement_timeout` | per role (e.g. 30 s for web users) | protect the server |
| `log_min_duration_statement` | 200–1000 ms | slow query log |
| `log_lock_waits` | on | find lock pile-ups |

Per-table autovacuum: `alter table big_events set (autovacuum_vacuum_scale_factor = 0.01, autovacuum_analyze_scale_factor = 0.01);`.

## 3. Symptom playbook

### A query is slow

1. `explain (analyze, buffers)`. Compare estimated and actual rows.
2. Estimates wrong → `analyze`, raise statistics target, `create statistics`.
3. `Seq Scan` removing most rows → index on the filter columns ([2a.2](02-indexes.md)).
4. Sort/hash spilling → raise `work_mem` for the session.
5. Large `loops` on an inner scan → missing index on the inner join key.
6. Fast with literals, slow as prepared → generic plan; `plan_cache_mode = force_custom_plan`.

### Everything got slow at once

Look for: a long-running transaction holding locks, an autovacuum falling behind, cache eviction after a big scan, checkpoint storms, connection saturation. `pg_stat_activity` (blocking), `pg_stat_bgwriter`, `pg_locks`, host `iostat`.

### Lock pile-up

```sql pg-local
select a.pid, a.state, pg_blocking_pids(a.pid) as blocked_by, now() - a.query_start as waiting, left(a.query, 60) as query
from pg_stat_activity a where cardinality(pg_blocking_pids(a.pid)) > 0 order by waiting desc;
```

Find the root blocker (a pid that blocks others but is not blocked itself), see what it is doing (`idle in transaction`?), and end it (`pg_terminate_backend(pid)`) if safe. Prevent with timeouts ([2a.4 §9](04-transactions-and-locking.md#9-operational-settings)).

### Table or index bloat

Signs: size grows while row count doesn't, `n_dead_tup` high, index scans slow down. Check long transactions and slots holding the horizon ([2a.1 §4](01-storage-and-mvcc.md#what-blocks-vacuum-from-removing-dead-tuples)). Fix with a tuned autovacuum; reclaim space online with `pg_repack` or `REINDEX CONCURRENTLY`; `VACUUM FULL` only in a maintenance window (exclusive lock).

### Connection exhaustion

`FATAL: too many connections`. Use a pooler ([2a.5 §6](05-architecture.md#6-connection-pooling)); check for leaks (`idle` for hours); set `idle_session_timeout`.

### Replication lag

`pg_stat_replication` on the primary (`replay_lag`), `pg_last_xact_replay_timestamp()` on the standby. Causes: long-running standby query conflicting with replay, slow standby disk, network, huge single transaction, `hot_standby_feedback` side effects.

### Disk filling up

Largest relations; WAL directory (stalled consumer or abandoned replication slot: `select slot_name, active, pg_size_pretty(pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn)) from pg_replication_slots`); temp files (`pg_stat_database.temp_bytes`); logs.

## 4. Recipes

### Keyset pagination

```sql pg-local
-- Slow at depth: reads and discards 400 000 rows (≈36 ms measured)
select * from play_orders order by id offset 400000 limit 20;
-- Fast and stable: ≈0.1 ms measured
select * from play_orders where id > 400000 order by id limit 20;
```

For non-unique sort columns use a tuple comparison with a tiebreaker: `where (created_at, id) < ($1, $2) order by created_at desc, id desc limit 20` with an index on `(created_at desc, id desc)`.

### Counting

`count(*)` scans (visibility checks). Options: an estimate from `reltuples` (instant, approximate), a maintained counter table updated in the same transaction, `count(*)` on a filtered **index-only scan**, or a materialized view refreshed on a schedule.

```sql pg-local
select reltuples::bigint as estimate from pg_class where relname = 'play_orders';
```

### Bulk loading

Use `COPY` (client `\copy`) rather than row-by-row `INSERT`. For very large loads: load into an unlogged or index-free table, create indexes after, then `analyze`. Batch multi-row inserts (hundreds to thousands per statement) when `COPY` isn't available, and wrap batches in transactions to amortise `fsync`.

### Upsert

`insert ... on conflict (id) do update set ...` is atomic and avoids select-then-insert races (needs a unique index). `on conflict do nothing` for idempotent inserts.

### Batched updates and deletes

A single `delete` of 100 M rows creates 100 M dead tuples, a huge WAL burst and one giant transaction. Batch it:

```sql
delete from events where id in (select id from events where ts < now() - interval '90 days' order by id limit 10000);
-- repeat until 0 rows; vacuum between batches or let autovacuum run
```

Better: partition by time and `drop`/`detach` old partitions.

### Idempotent, safe migrations

Expand → migrate data in batches → contract. Use `lock_timeout`, `CREATE INDEX CONCURRENTLY`, `NOT VALID` constraints then `VALIDATE`. Never run a table rewrite (`alter column type`) on a hot table without a plan.

### JSON

Index the fields you filter on (`create index on t ((data->>'user_id'))` or a GIN index for containment). Promote hot fields to real columns.

### Text search

`LIKE '%x%'` → `pg_trgm` GIN index; real search → `tsvector` + GIN; ranking and fuzzy matching beyond that → a search engine.

### Time series

Partition by time, BRIN on the timestamp ([2a.2 §6](02-indexes.md#6-brin-tiny-indexes-for-huge-ordered-tables)), roll up into summary tables, drop old partitions.

## 5. Anti-pattern gallery

| Anti-pattern | Problem | Instead |
| --- | --- | --- |
| `select *` in application queries | wide rows, no covering, schema coupling | name the columns |
| N+1 queries | round trips dominate | join, or `where id = any($1)` |
| `offset` pagination | cost grows with page number | keyset |
| `order by random()` | full scan + sort | sample with `tablesample`, or pick a random id range |
| `where lower(col) = x` with no expression index | scan | expression index |
| `not in (subquery)` with NULLs | wrong results, bad plans | `not exists` |
| transactions spanning user think-time | held snapshots/locks | short transactions |
| a counter row updated by everyone | row-lock hotspot | sharded counters, append + aggregate |
| `uuid v4` PK on huge tables | random page splits, cache misses | time-ordered ids |
| unbounded `in (...)` lists of thousands | parsing/plan cost | `= any(array)` or a join to `values` |
| missing FK index | slow parent deletes ([2a.2 §7](02-indexes.md#7-operating-indexes)) | index the referencing column |
| giant default `max_connections` | memory, contention | pool |

## 6. The tuning checklist

1. Measure first; pick the query by total time ([1.6](../../01-fundamentals/06-measuring.md)).
2. Compare estimated vs actual rows; fix statistics before adding indexes.
3. Right index (order, partial, covering); verify use.
4. Keep rows narrow and transactions short.
5. Keep autovacuum ahead of your churn; watch long transactions and slots.
6. Pool connections; set timeouts.
7. Configure for the hardware (`random_page_cost`, memory settings).
8. Re-measure; record the case ([template](../../01-fundamentals/06-measuring.md#7-a-reusable-case-file-template)).

## Exercises

1. On the seed, find the 3 most expensive statements after running the exercises of chapters 2a.2–2a.4 using `pg_stat_statements`. What would you fix first and why?
2. Write the batched-delete loop as a shell or Go program; measure WAL generated and dead tuples compared with one big `delete`.
3. Introduce a deliberate lock pile-up (idle transaction + `alter table`), then use the pile-up query above to identify the root blocker.
4. Configure autovacuum on `play_orders` so it triggers after 1% churn; prove it by watching `n_dead_tup` and `last_autovacuum`.

→ Answers: [appendix/solutions.md](../../appendix/solutions.md#2a6-postgresql-tuning-playbook).

Next part: [2b. ClickHouse](../clickhouse/01-storage-mergetree.md).

# `.tdb` cheatsheet: observability queries for the three databases

Paste into a `.tdb` file in Neovim with [tradar](https://github.com/tuannm99/tradar), change the connection alias after the dialect (`pg-local`, `ch-local`, `mongo-local`) and run a block with `rr`. Every query here was run against the seed on PostgreSQL 16, ClickHouse 26.10 and MongoDB 8.3 unless marked *(needs setup)*.

## PostgreSQL

```sql pg-local
-- What is running, and for how long
select pid, state, wait_event_type, wait_event, now() - query_start as running, left(query, 80) as query
from pg_stat_activity where state <> 'idle' and pid <> pg_backend_pid() order by running desc;
```

```sql pg-local
-- Who blocks whom
select pid, pg_blocking_pids(pid) as blocked_by, wait_event, left(query, 60) as query
from pg_stat_activity where cardinality(pg_blocking_pids(pid)) > 0;
```

```sql pg-local
-- Idle-in-transaction sessions (they hold snapshots and locks)
select pid, now() - xact_start as xact_age, left(query, 60) as last_query
from pg_stat_activity where state = 'idle in transaction' order by xact_age desc;
```

```sql pg-local
-- Costliest statements (needs pg_stat_statements in shared_preload_libraries + create extension)
select calls, round(total_exec_time::numeric, 1) as total_ms, round(mean_exec_time::numeric, 2) as mean_ms, rows, left(query, 80) as query
from pg_stat_statements order by total_exec_time desc limit 10;
```

```sql pg-local
-- Table size, dead tuples, vacuum activity
select relname, pg_size_pretty(pg_total_relation_size(relid)) as total, n_live_tup, n_dead_tup, last_autovacuum, last_autoanalyze
from pg_stat_user_tables order by pg_total_relation_size(relid) desc;
```

```sql pg-local
-- Unused indexes (not backing a constraint)
select s.indexrelid::regclass as index, s.idx_scan, pg_size_pretty(pg_relation_size(s.indexrelid)) as size
from pg_stat_user_indexes s join pg_index i on i.indexrelid = s.indexrelid
where not i.indisunique and s.idx_scan = 0 order by pg_relation_size(s.indexrelid) desc;
```

```sql pg-local
-- Cache hit ratio per table
select relname, heap_blks_hit, heap_blks_read,
       round(100.0 * heap_blks_hit / nullif(heap_blks_hit + heap_blks_read, 0), 2) as hit_pct
from pg_statio_user_tables order by heap_blks_read desc limit 10;
```

```sql pg-local
-- Planner statistics for a table
select attname, n_distinct, correlation, most_common_vals from pg_stats where tablename = 'play_orders';
```

```sql pg-local
-- Replication state on a primary
select client_addr, state, sync_state, write_lag, flush_lag, replay_lag from pg_stat_replication;
```

```sql pg-local
-- Replication slots (abandoned ones retain WAL forever)
select slot_name, active, pg_size_pretty(pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn)) as retained_wal from pg_replication_slots;
```

```sql pg-local
-- Transaction-id age (wraparound watch)
select datname, age(datfrozenxid) from pg_database order by 2 desc;
```

## ClickHouse

```sql ch-local
-- Parts per table
select database, table, count() as parts, sum(rows) as rows, formatReadableSize(sum(bytes_on_disk)) as on_disk
from system.parts where active group by database, table order by sum(bytes_on_disk) desc;
```

```sql ch-local
-- Costliest query shapes by data read
system flush logs;
select normalized_query_hash, count() as runs, sum(read_rows) as rows, formatReadableSize(sum(read_bytes)) as bytes,
       round(avg(query_duration_ms)) as avg_ms, any(left(query, 80)) as sample
from system.query_log where type = 'QueryFinish' and event_time > now() - interval 1 day and query_kind = 'Select'
group by normalized_query_hash order by sum(read_bytes) desc limit 10;
```

```sql ch-local
-- Running queries
select query_id, user, elapsed, formatReadableSize(memory_usage) as mem, left(query, 80) as query from system.processes;
```

```sql ch-local
-- Pending mutations
select table, mutation_id, command, create_time, parts_to_do, latest_fail_reason from system.mutations where not is_done;
```

```sql ch-local
-- Merges in flight
select table, elapsed, progress, num_parts, formatReadableSize(total_size_bytes_compressed) as size from system.merges;
```

```sql ch-local
-- Column sizes of a table
select name, type, formatReadableSize(data_compressed_bytes) as compressed, formatReadableSize(data_uncompressed_bytes) as raw
from system.columns where table = 'play_events' order by data_compressed_bytes desc;
```

```sql ch-local
-- Index usage for a query
explain indexes = 1 select count() from play_events where user_id = 42;
```

```sql ch-local
-- Failures in the last day
select exception_code, count() from system.query_log where type like 'Exception%' and event_time > now() - interval 1 day group by 1 order by 2 desc;
```

```sql ch-local
-- Replication health (replicated tables only) (needs setup)
select table, is_readonly, absolute_delay, queue_size, last_queue_update_exception from system.replicas;
```

## MongoDB

```js mongo-local
db.serverStatus()
```

```js mongo-local
db.adminCommand({currentOp: 1, active: true, secs_running: {$gt: 2}})
```

```js mongo-local
db.runCommand({collStats: "play_orders"})
```

```js mongo-local
db.play_orders.getIndexes()
```

```js mongo-local
db.play_orders.aggregate([{$indexStats: {}}, {$project: {name: 1, ops: "$accesses.ops", since: "$accesses.since"}}])
```

```js mongo-local
db.play_orders.find({customerId: 42}).sort({createdAt: -1}).limit(5).explain()
```

```js mongo-local
// Slow operations (needs the profiler: db.setProfilingLevel(1, {slowms: 100}))
db.system.profile.find({}, {op: 1, ns: 1, millis: 1, keysExamined: 1, docsExamined: 1, planSummary: 1}).sort({ts: -1}).limit(10)
```

```js mongo-local
db.adminCommand({replSetGetStatus: 1})
```

```js mongo-local
db.adminCommand({getDefaultRWConcern: 1})
```

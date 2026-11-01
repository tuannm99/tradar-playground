# 3.6 ClickHouse tuning playbook

> **Level:** practical · **Prerequisites:** [3.1](01-storage-mergetree.md)–[3.5](05-architecture.md) · Symptom → diagnosis → fix. Queries ran on ClickHouse 26.10 with the seed.

## 1. First hour on an unfamiliar server

```sql ch-local
select version();
select name, value from system.settings where name in ('max_threads', 'max_memory_usage', 'max_bytes_before_external_group_by', 'join_algorithm', 'async_insert') order by name;
```

Tables by size and part count:

```sql ch-local
select database, table, count() as parts, sum(rows) as rows, formatReadableSize(sum(bytes_on_disk)) as on_disk
from system.parts where active group by database, table order by sum(bytes_on_disk) desc limit 20;
```

Queries by total data read:

```sql ch-local
system flush logs;
select normalized_query_hash, count() as runs, sum(read_rows) as rows, formatReadableSize(sum(read_bytes)) as bytes,
       round(avg(query_duration_ms)) as avg_ms, any(left(query, 80)) as sample
from system.query_log where type = 'QueryFinish' and event_time > now() - interval 1 day and query_kind = 'Select'
group by normalized_query_hash order by sum(read_bytes) desc limit 10;
```

What is running now / failing recently:

```sql ch-local
select query_id, user, elapsed, formatReadableSize(memory_usage) as mem, left(query, 80) as query from system.processes;
select exception_code, count() from system.query_log where type like 'Exception%' and event_time > now() - interval 1 day group by 1 order by 2 desc;
```

## 2. Schema design recipe

1. List the 5–10 key queries and their filters, group-bys and time ranges.
2. Choose `ORDER BY` from the most frequent filters ([3.2 §2](02-indexes-and-pruning.md#2-choosing-the-order-by-key)); a time-leading key is wrong if every query is per-entity.
3. `PARTITION BY` a coarse time bucket (month, or day for very high volume) *only if* you need cheap retention/moves. Otherwise omit it.
4. Pick the smallest types; `LowCardinality(String)` for repeated strings; `Decimal` for money; avoid `Nullable`.
5. Add `TTL` for retention from day one.
6. Add **materialized views** for dashboards that scan billions of rows repeatedly.
7. Load a realistic volume and check `EXPLAIN indexes = 1` and `read_rows` for each key query.

## 3. Ingestion rules

- Insert **≥ 10 000 rows (ideally 100 000–1 000 000) per batch**, ≲ 1 insert/second/table, or use `async_insert`.
- Keep the number of **active parts per partition well under 1 000** (`parts_to_delay_insert`; hard stop at `parts_to_throw_insert = 3000`).
- Prefer the native or HTTP interface with compression; use `INSERT ... FORMAT` bulk formats (`RowBinary`, `Native`, CSV).
- Sort inputs by the table's sort key when you can (cheaper part writes and merges).
- Make writers idempotent ([3.4 §5](04-consistency-and-mutations.md#5-idempotent-ingestion-and-deduplication)).

## 4. Query rules

| Do | Don't |
| --- | --- |
| select only needed columns | `SELECT *` |
| filter on the sort-key prefix and partition key | filter only on non-key columns |
| aggregate before joining; small table on the right | join two billion-row tables |
| use `LIMIT`, `max_result_rows` on user-facing queries | unbounded result sets |
| use approximate functions for dashboards (`uniq`, `quantile`) | `uniqExact` on huge sets |
| pre-aggregate with MVs/projections for repeated rollups | recompute the same rollup per page load |
| use `PREWHERE`-friendly narrow filter columns | rely on `LIKE '%x%'` over wide strings without a text/ngram index |
| check `EXPLAIN indexes = 1` | assume the key is used |

## 5. Symptom playbook

### "Too many parts" / insert delays

`DB::Exception: Too many parts` or inserts slow down. Cause: tiny inserts and/or merges falling behind. Fix: batch/`async_insert`; check `system.merges`, `system.part_log` (`MergeParts` rate), background pool sizes, and disk speed. Temporary relief: increase `parts_to_delay_insert`/`parts_to_throw_insert` while you fix the ingestion.

```sql ch-local
select partition, count() as active_parts from system.parts where table = 'play_events' and active group by partition order by active_parts desc limit 5;
```

### Query uses too much memory

`MEMORY_LIMIT_EXCEEDED`. Identify the high-cardinality `GROUP BY` / big join right side / huge `ORDER BY`. Options: reduce the data first (filter, pre-aggregate), spill (`max_bytes_before_external_group_by`, `max_bytes_before_external_sort`, `join_algorithm = 'grace_hash'`), use approximate aggregates, or add RAM. Set per-user limits so one query can't take the node down.

### Query reads far too much

Compare `read_rows` with the table size. If the primary key isn't pruning: wrong key order, filtering on a non-prefix column, or a function on the key column (`toDate(ts) = ...` can still prune for some monotonic functions; `lower(col)` generally doesn't). Add a skip index/projection or change the design.

### Mutation stuck or slow

```sql ch-local
select table, mutation_id, command, create_time, parts_to_do, latest_fail_reason from system.mutations where not is_done;
```

If failing, read `latest_fail_reason`; `KILL MUTATION WHERE ...` if needed. Prefer partition operations or `ReplacingMergeTree` to avoid mutations.

### Replication lag / read-only table

`system.replicas`: `is_readonly`, `absolute_delay`, `queue_size`, `last_queue_update_exception`. Common causes: Keeper unreachable, disk full, long merges on a slow replica, a stuck queue entry (`SYSTEM RESTART REPLICA`; fix the root cause first).

### Disk fills up

Check `system.parts` sizes, `system.part_log` for churn, old mutations' leftover parts, detached parts (`system.detached_parts`), logs (`system.*_log` tables have their own TTL — set it), and backups.

### Slow first query after restart

Caches (mark cache, OS page cache) are cold. Warm critical tables with representative queries, and size the mark cache for the workload.

## 6. Settings worth knowing

| Setting | Why |
| --- | --- |
| `max_threads` | parallelism per query; lower to protect concurrency |
| `max_memory_usage` | per-query memory cap |
| `max_bytes_before_external_group_by` / `..._sort` | spill to disk instead of failing |
| `max_execution_time`, `timeout_before_checking_execution_speed` | bound runaway queries |
| `max_rows_to_read`, `max_bytes_to_read` | refuse over-wide scans (protects interactive users) |
| `async_insert`, `wait_for_async_insert` | server-side batching |
| `optimize_read_in_order` | use sort key order for `ORDER BY`/`GROUP BY` |
| `join_algorithm` | memory/speed trade-off for joins |
| `use_query_cache` | cache identical `SELECT` results for a short TTL |
| `parts_to_delay_insert`, `parts_to_throw_insert` (MergeTree) | back-pressure on too many parts |
| `merge_with_ttl_timeout`, `max_bytes_to_merge_at_max_space_in_pool` | merge scheduling |

## 7. Anti-pattern gallery

| Anti-pattern | Problem |
| --- | --- |
| treating it like OLTP (single-row writes/updates) | parts explosion, mutation storms |
| `PARTITION BY` a high-cardinality column | millions of tiny parts |
| sort key starting with a high-cardinality, never-filtered column | nothing prunes, poor compression |
| `Nullable` everywhere | slower scans and larger storage |
| `OPTIMIZE FINAL` on a schedule | repeated full rewrites |
| relying on `ReplacingMergeTree` dedup without `FINAL`/aggregation | duplicates in results |
| wide joins of big tables | right side must fit memory |
| no resource limits for dashboards | one query exhausts the node |
| forgetting TTL on log tables | system tables eat the disk |

## 8. Checklist

1. Which query matters? (total `read_bytes` in `query_log`)
2. How much did it read vs the table? (`read_rows`, `EXPLAIN indexes = 1`)
3. Does the sort key match the filters? If not, redesign or add a projection/MV.
4. Right types, no `SELECT *`.
5. Healthy ingestion (batch size, active parts).
6. Limits and quotas set.
7. Re-measure and record ([case-file template](../01-fundamentals/06-measuring.md#7-a-reusable-case-file-template)).

## Exercises

1. Produce a "too many parts" situation with a script, observe the delay/throw behaviour and the merge backlog, then fix it with `async_insert`.
2. Find the three most expensive queries in `query_log` after doing the exercises of chapters 3.2–3.4 and propose a schema change for each.
3. Set `max_rows_to_read = 100000` for a user and explain how the same dashboard query must be rewritten to continue working.
4. Write a monitoring query that alerts when any table has more than 300 active parts in a partition.

→ Answers: [appendix/solutions.md](../appendix/solutions.md#36-clickhouse-tuning-playbook).

Next part: [4. MongoDB](../04-mongodb/01-storage-wiredtiger.md).

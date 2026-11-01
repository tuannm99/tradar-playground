# 3.3 ClickHouse: how queries execute and how to read them

> **Level:** internals + practical · **Prerequisites:** [3.1](01-storage-mergetree.md), [3.2](02-indexes-and-pruning.md), [1.3](../01-fundamentals/03-query-execution.md) · **After this chapter you can:** explain the parallel pipeline, reason about memory for aggregations and joins, and diagnose a query with `system.query_log`. Measured on ClickHouse 26.10.

## 1. The execution model

ClickHouse executes a query as a **pipeline of transforms** processing **blocks** (chunks of thousands of rows in columnar form), in parallel across threads. There is no per-row interpretation: operators work on whole column vectors (vectorised execution), often using SIMD.

```sql ch-local
explain pipeline select count() from play_events where user_id = 42;
```

Measured output (condensed):

```
(Expression)
ExpressionTransform
  (Aggregating)
  Resize 6 → 1
    AggregatingTransform × 6
      (Expression)  ExpressionTransform × 6
        (Filter)    FilterTransform × 6
          (ReadFromMergeTree)
            MergeTreeSelect(pool: ReadPool, algorithm: Thread) × 6
```

Six parallel streams each read granules from a shared pool, filter, and compute a partial aggregate; `Resize 6 → 1` merges the partial states. The parallelism is bounded by `max_threads` (auto = number of cores) and by how many granules survive pruning — a query that reads 21 granules cannot use 16 threads well.

## 2. Query phases

1. **Parse and analyse** (new analyzer by default).
2. **Pruning**: partitions, then primary key granule ranges, then skip indexes ([3.2](02-indexes-and-pruning.md)).
3. **Read** only the needed columns from the surviving granules (`PREWHERE` first).
4. **Transform**: filter, compute expressions, partial aggregation per thread.
5. **Merge** partial results, sort/limit, send to the client (streamed in blocks).

## 3. Aggregation

`GROUP BY` uses hash tables, one per thread, merged at the end. Memory ∝ number of distinct keys. High-cardinality group-by (`GROUP BY user_id` over billions of rows) is the usual cause of memory errors. Controls:

| Setting | Default seen | Effect |
| --- | --- | --- |
| `max_memory_usage` | `0` in the test container (unlimited — set a real limit in production) | per-query cap, error `MEMORY_LIMIT_EXCEEDED` when exceeded |
| `max_bytes_before_external_group_by` | `0` (off) | spill the aggregation to disk beyond this size |
| `max_threads` | `auto(16)` here | parallelism |
| `group_by_two_level_threshold` | | switches to two-level hash tables for large groups |

Approximate aggregates trade accuracy for memory and speed: `uniq` (≈1–2% error) vs `uniqExact`, `quantile` vs `quantileExact`, `topK`.

## 4. Joins

ClickHouse historically prefers denormalised wide tables, and its join is a **hash join with the right-hand table built in memory**. Therefore:

- Put the **smaller table on the right**.
- Filter and aggregate both sides before joining when possible.
- `join_algorithm` (here `direct,parallel_hash,hash,ie_join`) lets the server choose among `hash`, `parallel_hash`, `grace_hash` (spills to disk), `partial_merge`/`full_sorting_merge` (low memory, needs sorted inputs), and `direct` (key-value lookups against a dictionary or EmbeddedRocksDB).
- Prefer a **dictionary** or `IN` subquery for dimension lookups.

```sql ch-local
-- Small side on the right, built once and probed by the large scan.
select count() from play_events e
join (select number as user_id from numbers(1000)) u on e.user_id = u.user_id;
```

Distributed joins add network costs: `GLOBAL JOIN`/`GLOBAL IN` broadcast the right side to every shard ([3.5](05-architecture.md)).

## 5. FINAL, SAMPLE, LIMIT BY

- `FINAL` applies the merge semantics (dedup/collapse) at read time. Correct but costly; newer versions parallelise it, still avoid on huge tables when an `argMax`-style query works ([3.4](04-consistency-and-mutations.md)).
- `SAMPLE` (needs a `SAMPLE BY` key) reads a deterministic fraction.
- `LIMIT n BY col` returns the top n rows per group.
- `LIMIT` with `ORDER BY` on the sort key can stop early (`optimize_read_in_order`).

## 6. Observing queries

`system.query_log` is the main tool. Flush then query it:

```sql ch-local
system flush logs;
select normalized_query_hash, count() as runs, sum(read_rows) as rows,
       formatReadableSize(sum(read_bytes)) as bytes, round(avg(query_duration_ms)) as avg_ms,
       any(left(query, 80)) as sample
from system.query_log
where type = 'QueryFinish' and event_time > now() - interval 1 day and query_kind = 'Select'
group by normalized_query_hash order by sum(read_bytes) desc limit 10;
```

Useful columns: `read_rows`, `read_bytes`, `result_rows`, `memory_usage`, `query_duration_ms`, `ProfileEvents` (a map: `SelectedParts`, `SelectedMarks`, `MarkCacheHits`, `FileOpen`, `OSReadBytes`, …), `exception`. `type = 'ExceptionWhileProcessing'` rows show failures.

```sql ch-local
select type, count() from system.query_log group by type;
```

Other windows into a live server: `system.processes` (running queries), `system.metrics` and `system.events` (counters), `system.asynchronous_metrics`, `system.errors`, `system.replication_queue`.

## 7. Evidence-based reading of a query

Ask in this order:

1. **How many rows/bytes were read vs the table size?** (`read_rows` vs `count()`) — pruning working?
2. **Which columns were read?** `SELECT *` multiplies bytes.
3. **Is the work CPU-bound or I/O-bound?** `ProfileEvents`: `OSReadBytes` vs user CPU time.
4. **Memory**: `memory_usage` near the limit? A high-cardinality `GROUP BY` or a big right-hand join side?
5. **Parts**: many active parts hurt every query ([3.1 §2](01-storage-mergetree.md#2-the-insert-path-and-why-batches-matter)).

## 8. Common slow patterns

| Pattern | Why | Fix |
| --- | --- | --- |
| `SELECT *` on wide tables | reads all column files | list columns |
| filter column not in the sort key, no skip index | reads whole column | align key, or add skip index/projection |
| `ORDER BY` big result without `LIMIT` | sorts everything | add `LIMIT`, or order by the sort key |
| `GROUP BY` high-cardinality key | huge hash table | pre-aggregate, approximate, spill to disk |
| joining two large tables | right side must fit memory | denormalise, filter first, `grace_hash`, dictionaries |
| `FINAL` on large tables | merges at read time | restructure query / schedule merges |
| point lookups at high QPS | one granule read per lookup, no row-level cache | put a key-value store or cache in front |
| many tiny inserts | too many parts | batch or `async_insert` |
| `uniqExact`/`DISTINCT` on huge sets | exact needs all values | `uniq`, `uniqCombined` |

## Exercises

1. Run `explain pipeline` for a query with `group by country` and for one with `group by user_id`. How do the aggregating stages differ?
2. Set `max_memory_usage = 10000000` for a session and run `group by user_id` on `play_events`. Observe the error, then raise `max_bytes_before_external_group_by` so it completes with spill. Compare `query_duration_ms`.
3. Using `ProfileEvents` from `system.query_log`, report `SelectedParts` and `SelectedMarks` for the `user_id = 42` queries on both tables.
4. Rewrite `select * from play_events where user_id = 42 order by ts` to read the minimum data, and show `read_bytes` before/after.
5. Compare `uniqExact(user_id)` and `uniq(user_id)` on the seed: time, memory, and error.

→ Answers: [appendix/solutions.md](../appendix/solutions.md#33-clickhouse-query-execution).

Next: [3.4 Consistency, mutations and "transactions"](04-consistency-and-mutations.md).

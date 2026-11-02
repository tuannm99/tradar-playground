# 2b.2 ClickHouse: sparse primary index, pruning, skip indexes and projections

> **Level:** internals + practical · **Prerequisites:** [2b.1](01-storage-mergetree.md), [1.2](../../01-fundamentals/02-indexing.md) · **After this chapter you can:** choose an `ORDER BY` key that prunes granules, prove it with `EXPLAIN`, and know when to add a skip index or a projection. Measured on ClickHouse 26.10.

## 1. The sparse primary index

ClickHouse does not index every row. The primary index holds the sort-key values of the **first row of each granule** (every 8 192 rows). A 3-million-row table has ~376 entries — it fits in cache trivially.

To evaluate `where user_id = 42` on a table sorted by `(user_id, ts)`:

1. **Binary search** the index to find the granule range that *could* contain 42.
2. Read only those granules (all rows in them, not just the matches), then filter.

So an index lookup reads **at least one granule (8 192 rows)** — ClickHouse is built for reading ranges, not for point lookups at B-tree precision.

```sql ch-local
explain indexes = 1 select count() from play_events where user_id = 42;
```

Measured (`play_events`, 21 parts at that moment): `PrimaryKey ... Granules: 21/376` — only 21 of 376 granules (about one per part) are read, thanks to the sort key.

The same data sorted by `(ts)` only:

```sql ch-local
explain indexes = 1 select count() from play_events_by_ts where user_id = 42;
```

There the primary key cannot prune on `user_id` (`Condition: true`), so nearly everything is read. Verify with actual work done:

```sql ch-local
select count(), sum(amount) from play_events where user_id = 42;
select count(), sum(amount) from play_events_by_ts where user_id = 42;
system flush logs;
select query_duration_ms, read_rows, formatReadableSize(read_bytes) as read_bytes, left(query, 70) as query
from system.query_log
where type = 'QueryFinish' and query like '%play_events%' and query not like '%query_log%'
order by event_time desc limit 4;
```

Measured: sorted by `(user_id, ts)` → **~158 k rows (0.6 MiB)**; sorted by `(ts)` → **~2.2 M rows (9.7 MiB)**. Same answer, ~14× less data. Look at `read_rows`/`read_bytes`, not wall time (cache noise).

## 2. Choosing the ORDER BY key

The sort key is the single most important design decision in a ClickHouse table. It is **immutable** (changing it means a new table + `INSERT ... SELECT`), and it determines both pruning and compression (adjacent equal values compress well).

Guidelines:

1. Put columns you **filter by equality or range in most queries** first.
2. Within those, order from **lower to higher cardinality** as a starting heuristic: a low-cardinality prefix (`country`, `event`) lets the high-cardinality column after it still prune within each prefix group, and compresses better. (Measure — if every query filters on `user_id`, `user_id` first is right, as in the seed.)
3. Only a **prefix** of the key prunes efficiently. A filter on the 2nd column alone prunes poorly (it falls back to "generic exclusion search").
4. Don't include columns you never filter by; every extra column widens the index and lowers compression for earlier ones.
5. The `PRIMARY KEY` may be a *prefix* of `ORDER BY` (smaller in-memory index, same sort order).
6. A timestamp is often last: queries filter "this entity, over this period".

```sql ch-local
explain indexes = 1 select count() from play_events where ts > now() - interval 3 day and user_id = 42;
```

Measured: `PrimaryKey ... Granules: 6/68` after min-max and partition pruning — the three pruning stages compound.

## 3. The pruning pipeline in EXPLAIN

`EXPLAIN indexes = 1` lists the stages in order; each shows `Parts: a/b` and `Granules: a/b`:

1. **Min-Max** (per-part min/max of the partition key columns),
2. **Partition** (partition-key condition),
3. **PrimaryKey** (sparse index search on the sort key),
4. **Skip** (data-skipping indexes), then
5. `PREWHERE` filtering at read time.

A healthy plan has the numerator shrinking quickly. `Granules` close to the denominator after PrimaryKey means the key doesn't help this query.

## 4. Data-skipping indexes

A skip index stores a small summary per block of N granules (`GRANULARITY N`) and lets the engine skip blocks that cannot match.

| Type | Summary | Good for |
| --- | --- | --- |
| `minmax` | min and max | values that cluster (monotonic or time-correlated columns) |
| `set(n)` | up to n distinct values | low-cardinality column, equality |
| `bloom_filter` | probabilistic membership | high-cardinality equality (`user_id IN`, tokens) |
| `tokenbf_v1`, `ngrambf_v1`, text index | token/n-gram Bloom | text search |

```sql ch-local
alter table play_events add index idx_amt amount type minmax granularity 4;
alter table play_events materialize index idx_amt settings mutations_sync = 2;
explain indexes = 1 select count() from play_events where amount > 499.9;
```

Measured: `Skip: idx_amt ... Granules: 130/374` — a **65% reduction** for a rare value, because the skip index can rule out blocks whose max is below the threshold. This works because I picked a threshold near the maximum; for a predicate that matches rows in nearly every block, a skip index prunes nothing and only costs space and insert time. Skip indexes help only when the matching values are *concentrated* in few blocks. With uniformly random data (like the seed), most are useless — which is itself a lesson.

## 5. Projections and materialized views

If a second query pattern needs a different sort order or a pre-aggregation:

- A **projection** is a hidden, automatically maintained second copy of (some of) the table inside each part, with its own sort key or an aggregation. The optimizer picks it when it covers the query.

```sql ch-local
alter table play_events add projection p_country (select country, event, sum(amount) group by country, event);
alter table play_events materialize projection p_country settings mutations_sync = 2;
explain select country, event, sum(amount) from play_events group by country, event;
```

Measured: the plan reads `ReadFromMergeTree (p_country)` instead of the base table. Costs: extra storage and insert work, and a limitation found while measuring: **lightweight `DELETE FROM` is rejected on a table with projections** unless `lightweight_mutation_projection_mode` is changed (error code 344, "SUPPORT_IS_DISABLED") — drop the projection or change the setting.

- A **materialized view** is an insert trigger that writes the result of a `SELECT` over each inserted block into a *target table*. Use it for rollups ([2b.4 §4](04-consistency-and-mutations.md#4-incremental-aggregation-with-materialized-views)).

Rule of thumb: projection = transparent second index/rollup of the *same* table; materialized view = explicit separate table with its own engine and retention.

## 6. PREWHERE and column order of work

`PREWHERE` reads only the filter columns first, evaluates the condition, and reads the remaining columns **only for surviving granules/rows**. ClickHouse moves selective `WHERE` conditions to `PREWHERE` automatically (`Prewhere filter` in `EXPLAIN`). Write queries so that selective conditions are on narrow columns, and avoid `SELECT *` so the second phase has few columns to read.

## 7. Dictionaries and lookups

Joining a large fact table to a small dimension is common. A **dictionary** (`CREATE DICTIONARY`) keeps the dimension in memory, key-value style, and `dictGet(...)` is faster than a join and keeps the query simple. Use when the dimension fits in RAM and changes slowly.

## Exercises

1. Create `play_events_c` ordered by `(country, event, user_id, ts)` from the seed. For each of these queries, predict then measure `Granules` and `read_rows`: `user_id = 42`; `country = 'VN' and event = 'buy'`; `ts > now() - interval 1 day`.
2. Add a `bloom_filter` skip index on `user_id` to `play_events_by_ts`. Does `where user_id = 42` improve? Explain (hint: how are the rows for one user distributed over the sort order?).
3. Create a projection sorted by `(country, ts)` and show which of your queries use it. Then find its storage cost in `system.projection_parts`.
4. Explain why lightweight delete and projections conflict, and name two ways to proceed.
5. A table is sorted by `(tenant_id, ts)` and queries filter `ts` only. Give three redesign options.

→ Answers: [appendix/solutions.md](../../appendix/solutions.md#2b2-clickhouse-indexes-and-pruning).

Next: [2b.3 Query execution](03-query-execution.md).

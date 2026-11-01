# 2.3 PostgreSQL: the planner, statistics and EXPLAIN

> **Level:** practical + internals · **Prerequisites:** [1.3](../01-fundamentals/03-query-execution.md), [2.2](02-indexes.md) · **After this chapter you can:** read `EXPLAIN (ANALYZE, BUFFERS)` output line by line, diagnose an estimate error, and fix it with statistics rather than hints. Measured on PostgreSQL 16.

## 1. How the planner decides

For each query PostgreSQL enumerates access paths and join orders (dynamic programming up to `join_collapse_limit` tables, a genetic optimizer beyond `geqo_threshold`) and keeps the plan with the lowest **estimated cost**. Cost is in abstract units built from planner constants:

| Setting | Default | Meaning |
| --- | --- | --- |
| `seq_page_cost` | 1.0 | one sequential page read |
| `random_page_cost` | 4.0 | one random page read (set ~1.1–2 on SSD) |
| `cpu_tuple_cost` | 0.01 | processing one row |
| `cpu_index_tuple_cost` | 0.005 | processing one index entry |
| `cpu_operator_cost` | 0.0025 | evaluating one operator |
| `effective_cache_size` | 4 GB here | planner's guess of cache available for index scans |

## 2. Statistics

`ANALYZE` samples the table and stores, per column, in `pg_stats`: `null_frac`, `n_distinct`, `most_common_vals` (+ frequencies), `histogram_bounds`, and `correlation` (how well physical order matches column order). From these the planner estimates selectivity of each predicate.

```sql pg-local
select attname, n_distinct, correlation, most_common_vals from pg_stats where tablename = 'play_orders';
```

- `n_distinct` negative means a fraction of the row count: `-1` = every value distinct.
- `correlation` near ±1 → index scans are cheap (rows are adjacent) and BRIN works; near 0 (as for `customer_id` in the seed) → matching rows are scattered, favouring bitmap scans.
- `default_statistics_target` (100) controls the sample/histogram size; raise it per column (`alter table ... alter column ... set statistics 1000`) for skewed distributions.
- Autovacuum triggers `ANALYZE` after enough changes; after a **bulk load or mass update**, run it yourself.

### Multi-column predicates and extended statistics

Per-column statistics assume columns are independent, so `a = 5 and b = 5` is estimated as `P(a) × P(b)`. When the columns are correlated this underestimates by orders of magnitude. Measured on a table where `a = b` always (100 distinct values, 100 000 rows):

| | Estimated rows | Actual rows |
| --- | --- | --- |
| default | **12** | 1 000 |
| after `create statistics ... (dependencies)` + `analyze` | **973** | 1 000 |

```sql pg-local
create table play_corr as select g as id, g % 100 as a, g % 100 as b from generate_series(1, 100000) g;
analyze play_corr;
explain (analyze, timing off) select * from play_corr where a = 5 and b = 5;
create statistics play_corr_st (dependencies) on a, b from play_corr;
analyze play_corr;
explain (analyze, timing off) select * from play_corr where a = 5 and b = 5;
```

Other extended kinds: `ndistinct` (for `GROUP BY a, b`) and `mcv` (multi-column most-common values). A 12-vs-1 000 error is harmless on a single table scan but fatal for a join order decision.

## 3. EXPLAIN options

| Command | Runs the query? | Shows |
| --- | --- | --- |
| `EXPLAIN` | no | estimated plan and cost |
| `EXPLAIN ANALYZE` | **yes** | + actual time, rows, loops |
| `EXPLAIN (ANALYZE, BUFFERS)` | yes | + shared hit / read / dirtied / written per node |
| `EXPLAIN (ANALYZE, VERBOSE)` | yes | + output columns, per-worker detail |
| `EXPLAIN (SETTINGS)` | no | + non-default planner settings |

Wrap data-changing statements in `begin; explain analyze ...; rollback;`.

## 4. Reading a plan: worked example

```sql pg-local
explain (analyze, buffers)
select c.country, sum(o.amount)
from play_orders o join play_customers c on c.id = o.customer_id
group by c.country;
```

Abridged measured output:

```
Finalize GroupAggregate  (actual time=140..145 rows=4 loops=1)
  -> Gather Merge  (Workers Planned: 2, Workers Launched: 2)
     -> Sort (Sort Key: c.country; Sort Method: quicksort Memory: 25kB)
        -> Partial HashAggregate (Group Key: c.country; Batches: 1 Memory Usage: 24kB)
           -> Hash Join  (actual rows=166667 loops=3)
                Hash Cond: (o.customer_id = c.id)
                -> Parallel Seq Scan on play_orders o (rows=166667 loops=3)
                -> Hash  (Buckets: 16384 Batches: 1 Memory Usage: 519kB)
                   -> Seq Scan on play_customers c (rows=10000 loops=3)
```

Reading it bottom-up:

1. `Seq Scan on play_customers` builds a **hash** (519 kB, one batch = fits in `work_mem`).
2. Three processes (1 leader + 2 workers, `loops=3`) each scan a third of `play_orders` (`Parallel Seq Scan`, ~166 667 rows each) and probe the hash.
3. Each process does a **partial** aggregate of its rows; `Gather Merge` combines; `Finalize` produces 4 groups.
4. No index helps — the query needs every order. That is a correct plan, not a missed opportunity.

### Per-node checklist

- `actual time=a..b`: a = time to first row, b = time to last; **multiply by `loops`**.
- `Rows Removed by Filter` ≫ `rows`: the scan reads much more than it keeps (candidate for an index).
- `Buffers: shared hit=H read=R`: R > 0 means disk (or OS cache) reads.
- `Sort Method: external merge Disk: ...` or `Hash Batches > 1`: spilled; raise `work_mem` for the session or reduce the data.
- `Heap Fetches` on an index-only scan: visibility map not set ([2.2](02-indexes.md#4-covering-indexes-and-index-only-scans)).
- `Workers Launched < Workers Planned`: the server ran out of worker slots.

## 5. Node reference

| Node | Meaning | Usually bad when |
| --- | --- | --- |
| `Seq Scan` | whole table | `Rows Removed by Filter` is most of the table |
| `Index Scan` | index then heap, ordered | many rows (random heap access) |
| `Index Only Scan` | answered from index | `Heap Fetches` high |
| `Bitmap Index/Heap Scan` | collect tids, sort by page, fetch | `Heap Blocks: lossy` (bitmap exceeded `work_mem`) |
| `Nested Loop` | inner side per outer row | inner is a `Seq Scan` with big `loops` |
| `Hash Join` | hash the smaller side | `Batches > 1` |
| `Merge Join` | merge two sorted inputs | an explicit `Sort` below is large |
| `Sort` / `Incremental Sort` | order rows | spills to disk |
| `HashAggregate` / `GroupAggregate` | group | many groups, spills |
| `Limit` | stop after n | child is a full sort |
| `CTE Scan`, `Subquery Scan` | materialised result | unexpectedly re-executed |
| `Gather` / `Gather Merge` | parallel workers combined | |

## 6. Prepared statements and plan caching

A prepared statement can reuse a **generic plan** that ignores parameter values. PostgreSQL tries custom plans for the first five executions and switches to the generic plan if its estimated cost isn't worse. For skewed columns this can cause "fast with literals, slow from the app": compare `explain (analyze) ... where customer_id = 42` with `prepare q(int) ...; explain (analyze) execute q(42)`. Control it with `plan_cache_mode = force_custom_plan`.

```sql pg-local
prepare q(int) as select * from play_orders where customer_id = $1;
explain execute q(42);
```

## 7. Parallel query and JIT

Large scans/aggregates use parallel workers (`max_parallel_workers_per_gather`, default 2). Small queries don't, because starting workers costs more than it saves. JIT compilation (`jit = on` by default) can speed CPU-heavy analytical queries but adds a fixed compile cost; it is a frequent surprise on mid-sized queries, so check `JIT:` timing lines in `EXPLAIN ANALYZE` and disable per-session if it dominates.

## 8. Influencing the planner, in order of preference

1. **Fix the data**: `ANALYZE`, raise the statistics target, create extended statistics.
2. **Fix the query**: make predicates sargable, remove needless `ORDER BY`, rewrite `NOT IN` to `NOT EXISTS`, select fewer columns.
3. **Give it the right index** ([2.2](02-indexes.md)).
4. **Tune cost constants** to your hardware (`random_page_cost`, `effective_cache_size`).
5. **Per-session diagnostics**: `set enable_nestloop = off;` etc. *to learn*, never as a permanent fix.
6. Last resort: restructure with a CTE `MATERIALIZED`/`NOT MATERIALIZED` to force an evaluation order. (PostgreSQL has no native hints; the `pg_hint_plan` extension adds them.)

## Exercises

1. Make the planner choose a `Nested Loop` for the join above by shrinking the outer side with a `WHERE` that selects ~20 customers. Explain the plan change.
2. Reproduce the `a = 5 and b = 5` mis-estimate, then build a similar case with a *functional dependency* between `country` and `city`. Show how the join order of a three-table join changes before and after `create statistics`.
3. `explain (analyze, buffers)` the same index query twice. What changes in `Buffers`? Why does your average over 100 runs hide this?
4. Find a plan where `Rows Removed by Index Recheck` is non-zero and explain it.
5. Show a prepared statement whose generic plan is worse than the custom plan.

→ Answers: [appendix/solutions.md](../appendix/solutions.md#23-postgresql-planner-and-explain).

Next: [2.4 Transactions and locking](04-transactions-and-locking.md).

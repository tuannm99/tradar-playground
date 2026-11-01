# 1.3 How a query is executed

> **Level:** fundamentals · **Prerequisites:** [1.1](01-storage.md), [1.2](02-indexing.md) · **After this chapter you can:** read any execution plan, explain why the optimizer chose it, and recognise the three classic causes of a bad plan.

## 1. The pipeline

```
SQL / query document
   │ parse            → syntax tree
   │ analyse / bind   → names, types, permissions resolved
   │ rewrite          → views expanded, subqueries flattened, constants folded
   │ optimise         → enumerate candidate plans, estimate cost, pick the cheapest
   ▼
physical plan (a tree of operators)
   │ execute          → operators pull/push rows; results stream to the client
```

Almost all performance work happens in the **optimise** step's inputs (statistics, indexes, the query's shape) and in reading its output (the plan).

## 2. Declarative queries, cost-based choice

You describe *what* you want; the optimizer decides *how*. A **cost-based optimizer** assigns each candidate plan an estimated cost from:

- **Statistics** about the data: row count, distinct values, most common values, histograms, physical correlation.
- A **cost model**: relative prices of sequential page, random page, row processed, operator evaluated.
- **Cardinality estimates**: how many rows each operator will produce.

Cardinality estimation is the weak link. An estimate off by 10× changes join order and algorithm choices; off by 1 000× produces catastrophic plans. **Most "the optimizer is dumb" problems are really "the statistics are wrong or cannot express this".**

## 3. Scan operators

| Operator | What it does | Good when |
| --- | --- | --- |
| Full / sequential scan | reads every page | most of the table is needed; table is small |
| Index scan | walks the index, fetches each row from the table | few rows; ordered output wanted |
| Index-only scan | answers from the index | all needed columns are in the index |
| Bitmap scan | collects matching row addresses, sorts them by page, then reads pages in order | medium selectivity; combines several indexes |
| Block pruning (columnar) | skips granules/parts whose min/max or sparse key excludes the predicate | column stores, partitioned tables |

## 4. Join algorithms

Joins are where plans win or lose. Three classic algorithms:

| Algorithm | How | Cost shape | Wins when |
| --- | --- | --- | --- |
| **Nested loop** | for each outer row, probe the inner side (ideally an index) | outer rows × probe cost | outer side is small and inner is indexed |
| **Hash join** | build a hash table of the smaller side, stream the larger side through it | one pass over each + memory | equi-join, no useful index, build side fits in memory |
| **Merge (sort-merge) join** | both inputs sorted on the join key, then zip | sort cost (or free if already ordered) + one pass | both sides large, already sorted by index |

Join order matters as much as the algorithm: filter early, keep the intermediate result small, put the **small side as the build side** (ClickHouse: the right table). Document databases avoid joins by embedding; when they do join (`$lookup`) it is a nested-loop-style lookup that needs an index on the foreign field.

## 5. Sort, aggregate, limit

- **Sort**: in memory if it fits the allowed memory (`work_mem`, MongoDB's 100 MB stage limit), otherwise spills to disk ("external merge"). An index in the right order removes the sort entirely, and with `LIMIT` stops after *n* rows (top-N).
- **Aggregation**: hash aggregation (one hash table of groups; memory ∝ number of distinct groups) or sort-then-group. High-cardinality `GROUP BY` is memory-hungry in every engine.
- **Limit/offset**: `OFFSET n` reads and discards *n* rows. Page 1 000 of a result costs 1 000 pages of work. Use **keyset pagination** (`where id > last_seen order by id limit n`).
- **Distinct / count(distinct)**: sort or hash over all values; approximate variants (`uniq`, HyperLogLog) trade accuracy for memory.

## 6. Parallelism and vectorisation

Large scans and aggregates are split across worker threads (PostgreSQL parallel workers, ClickHouse processes data in parallel streams by default). Columnar engines process **blocks of thousands of values** per operator call (vectorised execution) instead of one row at a time, which is much of their speed on scans. Parallelism helps CPU-bound scans, not lock contention or a single slow disk.

## 7. Reading a plan

A plan is a tree. Read it **bottom-up, innermost first**: leaves produce rows, parents transform them. For each node note:

1. **Operator** — scan, join, sort, aggregate.
2. **Estimated vs actual rows** — the most useful diagnostic. A node whose actual rows differ from the estimate by 10× or more is where the planner went wrong.
3. **Loops** — a node under a nested loop runs once per outer row; multiply per-loop time and rows by `loops`.
4. **Buffers / rows examined** — how much data it touched, hits vs reads.
5. **Time** — exclusive versus inclusive: a parent's time includes its children.

The same questions apply in every engine; only the vocabulary differs:

| Concept | PostgreSQL | MongoDB | ClickHouse |
| --- | --- | --- | --- |
| Get the plan | `EXPLAIN (ANALYZE, BUFFERS)` | `.explain("executionStats")` | `EXPLAIN indexes = 1`, `system.query_log` |
| Rows touched | `Rows Removed by Filter`, `Buffers` | `totalDocsExamined`, `totalKeysExamined` | `read_rows`, `Granules: a/b` |
| Rows returned | `actual rows` | `nReturned` | result rows |
| Full scan marker | `Seq Scan` | `COLLSCAN` | `Granules` ≈ total |

## 8. The three classic causes of a bad plan

1. **Stale or missing statistics** — table changed a lot since the last `ANALYZE`; the planner thinks 10 rows, there are 10 million.
2. **Correlated predicates** — `country = 'VN' and city = 'Hanoi'` are not independent, but the planner multiplies selectivities. Fix with extended/multi-column statistics.
3. **Un-estimable expressions** — functions, casts, parameters whose value the planner cannot see (generic plans, `col + 0 = x`). Fix by rewriting the predicate or indexing the expression.

Everything else — missing index, wrong join order forced by a hint, memory settings — is secondary and easier to spot once you compare estimate to actual.

## 9. Anti-patterns that defeat the optimizer

- `SELECT *` (blocks covering, reads and ships unneeded columns).
- Functions on the filtered column.
- `OR` across different columns (can need a union of index scans or give up).
- Implicit casts.
- `NOT IN (subquery)` with possible NULLs; prefer `NOT EXISTS`.
- N+1 queries from application code (round trips, not query cost).
- Giant `IN (...)` lists; prefer a join against a temp/derived table.
- `OFFSET` pagination on large tables.

## Try it

```sql pg-local
-- A join + aggregate: find the hash join, the parallel scan and the estimate vs actual rows.
explain (analyze, buffers)
select c.country, sum(o.amount)
from play_orders o join play_customers c on c.id = o.customer_id
group by c.country;
```

```sql pg-local
-- OFFSET vs keyset: same 20 rows, very different work.
explain (analyze, costs off, timing off) select * from play_orders order by id offset 400000 limit 20;
explain (analyze, costs off, timing off) select * from play_orders where id > 400000 order by id limit 20;
```

Measured: the `OFFSET` plan has the index scan produce `rows=400020` and takes ~36 ms; the keyset plan produces `rows=20` and takes ~0.1 ms.

## Exercises

1. In a plan you see `Nested Loop (actual rows=1 loops=1)` whose inner child is `Seq Scan (loops=50000)`. What is wrong and what are two fixes?
2. Estimated rows 12, actual rows 1 000 for `a = 5 and b = 5`. Name the cause and the PostgreSQL feature that fixes it (hands-on in [2.3](../02-postgres/03-planner-and-explain.md)).
3. Why can a query with `LIMIT 10` be fast on an indexed sort column yet slow when you change the `ORDER BY` to an unindexed one?
4. Give two situations where a hash join is better than a nested loop, and one where it is worse.

→ Answers: [appendix/solutions.md](../appendix/solutions.md#13-query-execution).

Next: [1.4 Transactions](04-transactions.md).

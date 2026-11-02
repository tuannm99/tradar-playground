# 1.2 Indexing fundamentals

> **Level:** fundamentals · **Prerequisites:** [1.1 Storage](01-storage.md) · **After this chapter you can:** choose an index for a given query, order composite index columns correctly, and decide when an index will *not* help.

An index is a second data structure that trades **write cost and space** for **read speed**. It never makes a query *correct*; it only makes some queries cheaper. Every claim below is a trade.

## 1. What an index buys

Without an index, finding rows means scanning everything: cost ∝ table size. With a B-tree index, cost ∝ depth (3–4 page reads) plus the matching rows. The gain is large exactly when the query is **selective**.

**Selectivity** = fraction of rows that match. `customer_id = 42` matches 0.01% of `play_orders`: index wins by orders of magnitude. `status = 'paid'` matches 25%: reading a quarter of the table through an index (random page access) is *slower* than a sequential scan, so a good planner ignores the index. An index on a low-selectivity column is mostly cost.

Rule of thumb: an index starts to pay off when the query returns **a few percent or less** of the table; the exact break-even depends on row width, correlation and storage speed.

## 2. The B-tree and what it can answer

A B-tree keeps keys **sorted**, so it supports:

| Predicate | Supported | Notes |
| --- | --- | --- |
| `=`, `IN (...)` | yes | |
| `<`, `>`, `BETWEEN` | yes | contiguous range of leaves |
| `ORDER BY key [LIMIT n]` | yes | returns rows already ordered; `LIMIT` stops early |
| `LIKE 'abc%'`, `^abc` regex | yes (prefix) | needs C collation or `text_pattern_ops` in PostgreSQL |
| `LIKE '%abc'`, unanchored regex | no | cannot seek; at best scans the whole index |
| `f(col) = x` | no, unless indexed on `f(col)` | wrap-the-column kills the index |
| `col <> x`, `NOT IN` | rarely | usually matches most rows |
| `col IS NULL` | engine-dependent | |

## 3. Composite indexes: order is everything

An index on `(a, b, c)` is sorted by `a`, then `b` within equal `a`, then `c`. Think of a phone book sorted by (last name, first name):

- Find by last name → fast. By last **and** first → fast. By first name only → useless.
- So the index serves predicates on a **prefix** of its columns: `a`, `a,b`, `a,b,c`.

Design heuristic (the "ESR" rule, popular in MongoDB docs, valid in general): **E**quality columns first, then **S**ort columns, then **R**ange columns. Equality columns narrow to a contiguous slice; sort columns then arrive pre-ordered; the range column goes last because after a range the later columns are no longer sorted within the scan.

It is a guideline, not a law — measured counter-example in [2c.2](../02-engines/mongodb/02-indexes.md#esr-is-a-guideline): with a very selective range and no `LIMIT`, skipping the pre-sorted order and sorting a few hundred rows wins.

## 4. Covering (index-only) access

If the index contains every column the query needs, the engine answers from the index alone and never touches the table. Variants: a wide composite index, PostgreSQL `INCLUDE (col)`, MongoDB covered queries (projection of only indexed fields, `_id` excluded). It removes the random table access — often the dominant cost — but makes the index bigger. In MVCC systems the engine may still need a visibility check (PostgreSQL visibility map, see [2a.1](../02-engines/postgres/01-storage-and-mvcc.md)).

## 5. Other index shapes

| Type | Idea | Use when |
| --- | --- | --- |
| Hash | hash of key → bucket | equality only, rarely better than B-tree |
| Partial / filtered | index only rows matching a predicate | queries always include that predicate (`status = 'new'`); tiny and fast |
| Expression | index on `lower(email)` | queries use the same expression |
| Unique | enforces uniqueness | integrity, plus lookup |
| Multikey / inverted (GIN, MongoDB array) | one entry per element/token | arrays, JSON, full-text |
| Bitmap / BRIN / block-range | min/max (or bitmap) per block range | huge, physically ordered tables (time series); index is kilobytes |
| Sparse (ClickHouse primary index) | one entry per *granule* | column stores; prunes blocks instead of locating rows |
| Skip index | per-block summary (min/max, set, Bloom) | prunes blocks when values cluster |
| Spatial / geospatial | R-tree, geohash | location queries |
| Text / vector | inverted index, ANN graph | search |

## 6. Indexes are not free

- **Write cost**: every `INSERT`/`UPDATE`/`DELETE` that touches indexed columns updates every affected index. Five indexes ≈ six writes.
- **Space and cache**: indexes compete with data for RAM. Unused indexes are pure loss.
- **Maintenance**: bloat, rebuilds, and (in PostgreSQL) preventing HOT updates.
- **Planner confusion**: more candidate plans, more chances to pick a bad one.

Policy: add an index for a *measured* slow query, verify with the plan that it is used and effective, and review usage statistics (`pg_stat_user_indexes`, MongoDB `$indexStats`) periodically to drop dead ones.

## 7. When an index cannot help

1. The predicate wraps the column in a function or cast (`date(created_at) = ...`).
2. Type mismatch forcing a conversion (`int` column compared with text).
3. Low selectivity (the planner correctly prefers a scan).
4. Leading column of a composite index is not constrained.
5. The query needs most of the table anyway (analytics).
6. Stale statistics make the planner *think* one of the above.

## Try it

```sql pg-local
-- Before any index: scan the table. After: touch ~50 pages.
explain (analyze, buffers) select * from play_orders where customer_id = 42;
```

```sql pg-local
create index play_orders_customer_idx on play_orders (customer_id);
explain (analyze, buffers) select * from play_orders where customer_id = 42;
```

```sql pg-local
-- Low selectivity: with 4 equally likely statuses the planner keeps scanning.
create index play_orders_status_idx on play_orders (status);
explain select * from play_orders where status = 'paid';
```

## Exercises

1. For `where status = ? and created_at > ? order by amount desc limit 20`, design a composite index. Then design a second one for the case where the date range is only one day. Which is better, and how would you prove it?
2. Which of these can use an index on `(a, b)`: `a = 1`; `b = 2`; `a = 1 and b > 5`; `a > 1 and b = 2`; `a = 1 order by b`? Explain each.
3. You have a 500 GB `events` table where rows are appended in time order and you query by time range only. Compare B-tree, BRIN, and partitioning by day.
4. A table has 12 indexes and slow inserts. Describe a procedure to decide which to drop.

→ Answers: [appendix/solutions.md](../appendix/solutions.md#12-indexing).

Next: [1.3 Query execution](03-query-execution.md).

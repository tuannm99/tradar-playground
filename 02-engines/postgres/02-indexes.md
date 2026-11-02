# 2a.2 PostgreSQL: indexes in depth

> **Level:** practical + internals · **Prerequisites:** [1.2](../../01-fundamentals/02-indexing.md), [2a.1](01-storage-and-mvcc.md) · **After this chapter you can:** pick the right index type, build covering and partial indexes, and verify with a plan that an index is used. Measured on PostgreSQL 16.

## 1. Index types

| Type | Structure | Operators | Use for |
| --- | --- | --- | --- |
| **B-tree** (default) | balanced tree of sorted keys | `< <= = >= >`, `BETWEEN`, `IN`, `IS NULL`, prefix `LIKE`, ordering | almost everything |
| **Hash** | hash table | `=` | rarely better than B-tree |
| **GIN** | inverted index (key → list of rows) | `@>`, `?`, `&&`, `@@` | `jsonb`, arrays, full-text, trigram (`pg_trgm`) |
| **GiST** | generalised search tree | overlap, nearest neighbour | geometry, ranges, exclusion constraints |
| **SP-GiST** | space-partitioned trees | | tries, quadtrees, IP ranges |
| **BRIN** | min/max summary per block range | `= < >` | huge tables physically correlated with the column |

Every index points to rows by `ctid`, so it is maintained on each insert and on each non-HOT update ([2a.1](01-storage-and-mvcc.md#3-hot-updates)).

## 2. B-tree specifics

- Composite order and prefix rules: see [1.2](../../01-fundamentals/02-indexing.md#3-composite-indexes-order-is-everything).
- **Direction and nulls**: `create index on t (a asc, b desc)` matches `order by a, b desc`. A B-tree can be scanned backwards, so a single-column index serves both directions.
- **Multicolumn with equality + range**: put the equality column first.
- **`LIKE 'abc%'`** needs `text_pattern_ops` unless the database uses the C collation.
- A B-tree entry is limited (~1/3 of a page); don't index huge text — index a hash or a prefix.

## 3. Partial indexes

Index only rows that matter. On the seed, only `status = 'new'` rows are hot:

```sql pg-local
create index play_o_new on play_orders (created_at) where status = 'new';
select pg_size_pretty(pg_relation_size('play_o_new')) as partial,
       pg_size_pretty(pg_relation_size('play_orders_pkey')) as primary_key;
```

Measured: the partial index is **2.7 MB** against a 13 MB primary key — about a quarter of the rows, queried constantly. The planner uses it only when the query's `WHERE` **implies** the index predicate:

```sql pg-local
explain (costs off) select * from play_orders where status = 'new' and created_at > now() - interval '1 day';
```

→ `Bitmap Index Scan on play_o_new`. A query on `status = 'paid'` cannot use it. Partial **unique** indexes also enforce "unique among active rows" (`unique (email) where deleted_at is null`).

## 4. Covering indexes and index-only scans

`INCLUDE` stores extra columns in the leaf pages without making them part of the search key:

```sql pg-local
create index play_o_cov on play_orders (customer_id) include (amount);
vacuum analyze play_orders;
explain (analyze, costs off, timing off) select customer_id, amount from play_orders where customer_id = 42;
```

Measured: `Index Only Scan ... Heap Fetches: 0`. The covering index is **15 MB**, against **3.5 MB** for a plain index on `customer_id` — that is the price of carrying `amount`.

### Index-only scans need the visibility map

An index entry doesn't say whether its row is visible to your snapshot. For pages marked **all-visible** in the visibility map, PostgreSQL skips the heap; otherwise it must fetch the tuple (`Heap Fetches`). Right after bulk loading, nothing is marked all-visible:

```sql pg-local
create index if not exists play_orders_customer_idx on play_orders (customer_id);
explain (analyze, costs off, timing off) select customer_id from play_orders where customer_id = 42;
vacuum play_orders;
explain (analyze, costs off, timing off) select customer_id from play_orders where customer_id = 42;
```

Measured: first a `Bitmap Heap Scan` (~48 heap pages), then — after `VACUUM` — `Index Only Scan` with `Heap Fetches: 0`. On a table that is updated constantly, `Heap Fetches` rises and the advantage fades.

## 5. Expression indexes

If queries filter on an expression, index the expression:

```sql pg-local
create index play_c_lower on play_customers (lower(name));
explain (costs off) select * from play_customers where lower(name) = 'user 5';
```

→ `Bitmap Index Scan on play_c_lower`. The query must use **the same expression** (`lower(name)`), or the index is not eligible.

## 6. BRIN: tiny indexes for huge ordered tables

A BRIN index stores the min and max of a column for each block range (128 pages by default). If the column correlates with physical order — an append-only timestamp — it prunes most of the table at a tiny cost. Measured on a 1 000 000-row, time-ordered table:

| Index | Size |
| --- | --- |
| B-tree on `ts` | **21 MB** |
| BRIN on `ts` | **24 kB** |

```sql pg-local
create table play_log as
  select g as id, now() - (g || ' seconds')::interval as ts, md5(g::text) as msg from generate_series(1, 1000000) g;
create index play_log_brin on play_log using brin (ts);
analyze play_log;
explain (analyze, timing off) select count(*) from play_log
where ts between now() - interval '2 hours' and now() - interval '1 hour';
```

Plan: `Bitmap Heap Scan ... Heap Blocks: lossy=128` over a BRIN scan, ~2 ms. It reads whole block ranges and rechecks rows (`Rows Removed by Index Recheck`), so it is **approximate by block** — worth it when the table is far larger than RAM. If data is inserted out of order (low `correlation` in `pg_stats`), BRIN degrades.

## 7. Operating indexes

- **`CREATE INDEX CONCURRENTLY`** builds without blocking writes (two table scans, slower; cannot run in a transaction; if it fails it leaves an `INVALID` index to drop).
- **Rebuild bloated indexes** with `REINDEX INDEX CONCURRENTLY`.
- **Find unused indexes** (and always keep indexes that back constraints):

```sql pg-local
select s.indexrelid::regclass as index, s.idx_scan, pg_size_pretty(pg_relation_size(s.indexrelid)) as size
from pg_stat_user_indexes s join pg_index i on i.indexrelid = s.indexrelid
where not i.indisunique and s.idx_scan = 0 order by pg_relation_size(s.indexrelid) desc;
```

- **Find duplicate indexes** (same columns, same predicate) by comparing `pg_index.indkey`.
- **Foreign keys need an index on the referencing column.** Deleting a parent row checks the child table:

| `play_child(parent_id)` indexed? | `delete from play_parent where id = 2000` |
| --- | --- |
| no | **26 ms** (scans 500 000 child rows) |
| yes | **0.8 ms** |

## 8. Why the planner may ignore your index

1. Predicate not sargable (`date(created_at) = ...`, a cast, an implicit type conversion).
2. Low selectivity — a sequential scan is genuinely cheaper.
3. Stale statistics — run `ANALYZE`.
4. Leading column of a composite not constrained.
5. Planner cost constants don't match your hardware (`random_page_cost` defaults to 4, tuned for spinning disks; 1.1–2 is common on SSD).
6. Table is tiny — scanning one page beats an index.

Check with `set enable_seqscan = off;` *for diagnosis only*: if the index plan is now faster, the cost model is wrong somewhere; if it's slower, the planner was right.

## Exercises

1. Design indexes for: "the 20 latest `paid` orders of customer X". Show the plan, then drop the index and show what changes.
2. Create a partial index for active rows only and prove (with `EXPLAIN`) that a query without the predicate cannot use it.
3. On `play_log`, compare B-tree and BRIN for a 1-hour range: size, plan, and buffers. Then insert 100 000 rows with random timestamps and check `pg_stats.correlation` and the BRIN plan again.
4. Remove the FK index in the `play_child` experiment; reproduce the 26 ms delete; add it back.
5. Write a query that lists indexes with `idx_scan = 0` and explain why you must not blindly drop what it returns.

→ Answers: [appendix/solutions.md](../../appendix/solutions.md#2a2-postgresql-indexes).

Next: [2a.3 Planner and EXPLAIN](03-planner-and-explain.md).

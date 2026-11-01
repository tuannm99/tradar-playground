# 4.2 MongoDB: indexes, `explain` and the ESR guideline

> **Level:** practical + internals · **Prerequisites:** [1.2](../01-fundamentals/02-indexing.md), [4.1](01-storage-wiredtiger.md) · **After this chapter you can:** read `explain("executionStats")`, build compound, partial, multikey and TTL indexes, and apply ESR with the judgment it needs. Measured on MongoDB 8.3, `playground.play_orders` (300 000 documents).

Create indexes in tradar with `db.runCommand({createIndexes: ...})` (tradar has no `createIndex()`); in mongosh you can use `createIndex()`.

## 1. Index types

| Type | Notes |
| --- | --- |
| Single field | B-tree on one field; direction irrelevant for a single field |
| **Compound** | multiple fields, order and direction matter ([§4](#4-compound-indexes-and-esr)) |
| **Multikey** | automatic when an indexed field is an array: one entry per element |
| Text | tokenised text search (`$text`) |
| Hashed | for hash-based sharding and equality |
| Geospatial (`2d`, `2dsphere`) | location queries |
| Wildcard | indexes unknown/dynamic field names |
| **Partial** | only documents matching a filter |
| **TTL** | deletes documents after N seconds |
| **Unique** | enforces uniqueness (per index key, possibly compound) |
| Sparse | skips documents missing the field (partial is more flexible) |
| Hidden | test the effect of dropping an index without dropping it |

Every collection has a unique index on `_id`.

## 2. Reading `explain`

`find(...).explain()` in tradar runs the `executionStats` verbosity.

| Field | Meaning |
| --- | --- |
| `queryPlanner.winningPlan` | the chosen tree: `COLLSCAN`, `IXSCAN`, `FETCH`, `SORT`, `LIMIT`, `SKIP`, `PROJECTION_COVERED`, `EXPRESS_IXSCAN` (a fast path for simple `_id`/equality) |
| `executionStats.nReturned` | documents returned |
| `executionStats.totalKeysExamined` | index entries scanned |
| `executionStats.totalDocsExamined` | documents fetched |
| `executionStats.executionTimeMillis` | time |
| `rejectedPlans` | other candidates considered |

**Efficiency = `nReturned` vs `totalKeysExamined` and `totalDocsExamined`.** Ideal is ~1:1. `COLLSCAN` with `totalDocsExamined` equal to the collection size means "no usable index".

Without an index on `customerId`:

```js mongo-local
db.play_orders.find({customerId: 42}).explain()
```

Measured: `COLLSCAN`, `nReturned 36`, `totalDocsExamined 300000`, ~215 ms.

```js mongo-local
db.runCommand({createIndexes: "play_orders", indexes: [{key: {customerId: 1}, name: "customerId_1"}]})
```

Then the same query: `FETCH > IXSCAN`, `totalKeysExamined 36`, `totalDocsExamined 36`, ~2 ms. A **covered query** (projection contains only indexed fields, `_id` excluded) fetches nothing:

```js mongo-local
db.play_orders.find({customerId: 42}, {_id: 0, customerId: 1}).explain()
```

→ `PROJECTION_COVERED > IXSCAN`, `totalDocsExamined 0`.

The planner chooses by **trial**: it runs competing candidate plans for a short while and picks the winner, caching the plan shape. `.hint(<index name or key pattern>)` forces an index (use for diagnosis and as a last resort).

## 3. Multikey, partial, TTL, regex and negation

### Multikey

An index on `items.sku` (an array field) has one entry per element.

```js mongo-local
db.runCommand({createIndexes: "play_orders", indexes: [{key: {"items.sku": 1}, name: "items_sku_1"}]})
db.play_orders.find({"items.sku": "sku-7"}).explain()
```

Measured: `FETCH > IXSCAN`, 1 221 keys, 1 221 documents. Limits: a compound index may contain **at most one array field**; multikey indexes can't be covering for array fields.

### Partial

```js mongo-local
db.runCommand({createIndexes: "play_orders", indexes: [{key: {createdAt: 1}, name: "recent_new",
  partialFilterExpression: {status: "new"}}]})
```

Measured size **0.87 MB** vs 3.8 MB for the compound customer index; for the query `{status: "new", createdAt: {$gt: …}}` the plan was `FETCH > IXSCAN` with **173 keys examined, 173 documents** (~1 ms). The planner uses a partial index only when the query filter is guaranteed to be contained in the index filter.

### TTL

```js mongo-local
db.runCommand({createIndexes: "play_ttl", indexes: [{key: {at: 1}, name: "at_1", expireAfterSeconds: 60}]})
```

A background task removes expired documents about once a minute (not instantly); the field must be a `Date`.

### Regex and negation

Measured on `play_customers.name`:

| Query | Index | Plan | Keys examined |
| --- | --- | --- | --- |
| `/^user 12/` (anchored) | none | `COLLSCAN` | 0 keys, 10 000 documents |
| `/^user 12/` | `{name: 1}` | `FETCH > IXSCAN` | **112** |
| `/12/` (unanchored) | `{name: 1}` | `FETCH > IXSCAN` | **10 000** (scans every key; cannot seek) |
| `{status: {$ne: "cancelled"}}` | `{status: 1}` | `FETCH > IXSCAN` | 225 036 (matches 75% of the collection) |

Anchored regexes can seek; unanchored regexes and `$ne`/`$nin` usually examine most of the index. Having an index does not make a non-selective query cheap.

## 4. Compound indexes and ESR

### Prefix rule

An index `{a: 1, b: 1, c: 1}` supports queries on `a`, `a,b`, `a,b,c`, and sorts that follow the same prefix; not queries on `b` alone.

### ESR: Equality, Sort, Range

Put **E**quality fields first, then the **S**ort field(s), then **R**ange fields. The sort field then arrives pre-ordered (no in-memory `SORT` stage) for every equality value, and the range is applied after the sort field in the index scan.

### ESR is a guideline

Query: *paid orders from the last 7 days, sorted by `amount` descending*.

```js mongo-local
db.runCommand({createIndexes: "play_orders", indexes: [
  {key: {status: 1, createdAt: 1}, name: "status_createdAt"},
  {key: {status: 1, amount: -1, createdAt: 1}, name: "status_amount_createdAt"}
]})
db.play_orders.find({status: "paid", createdAt: {$gt: ISODate("2026-10-25T00:00:00Z")}}).sort({amount: -1}).hint({status: 1, amount: -1, createdAt: 1}).explain()
```

Measured results (7-day range = 1 436 matching documents; 300-day range with `limit(20)`):

| Case | Index | Winning stages | Keys examined | ms |
| --- | --- | --- | --- | --- |
| 7 days, no limit | E-R `{status, createdAt}` | `SORT > FETCH > IXSCAN` | 1 436 | ~8 |
| 7 days, no limit | **ESR** `{status, amount, createdAt}` | `FETCH > IXSCAN` (no sort) | **39 831** | ~157 |
| 300 days, `limit(20)` | E-R | `SORT > FETCH > IXSCAN` | 61 554 | ~225 |
| 300 days, `limit(20)` | **ESR** | `LIMIT > FETCH > IXSCAN` | **26** | ~0 |

Why it flips: the ESR index scans in `amount` order and must *test the date range while walking*, so with a narrow range it examines many keys it throws away. The E-R index jumps straight to the 1 436 matching entries and sorts them. With a wide range and a small `limit`, ESR stops after 20 hits. **The planner picks correctly without hints in the second case**, so let it choose, and decide index sets by measuring your real query shapes.

### Sorting rules

- `sort` direction must match the index direction pattern (or its exact reverse) across all sort fields.
- An in-memory `SORT` stage fails at 100 MB unless `allowDiskUse` — an index that provides order avoids it.
- **`skip(n)` still walks n keys.** Measured: `sort({_id: 1}).skip(250000).limit(20)` examined **250 020 keys** (~123 ms); the range form `find({_id: {$gt: last}}).sort({_id: 1}).limit(20)` examined **20** (~0 ms). Paginate by range.

```js mongo-local
db.play_orders.find({customerId: 42}).sort({createdAt: -1}).limit(5).explain()
```

With `{customerId: 1, createdAt: -1}` this is `LIMIT > FETCH > IXSCAN` examining exactly 5 keys.

## 5. Inspect and maintain indexes

```js mongo-local
db.play_orders.getIndexes()
```

```js mongo-local
db.play_orders.aggregate([{$indexStats: {}}])
```

`accesses.ops` is the number of times each index has been used since the server (or index) started. Zero after a long time → candidate to drop (or **hide** first: `db.runCommand({collMod: "play_orders", index: {name: "x", hidden: true}})`). Index sizes: `db.runCommand({collStats: "play_orders"}).indexSizes`.

- Builds are online since 4.4 (a brief lock at start and end); they still consume I/O and memory.
- More indexes = slower writes; each insert updates all.
- An index not in memory is slow: indexes must fit with the hot data in RAM ([4.1 §3](01-storage-wiredtiger.md#3-sizes-and-the-working-set)).
- Max 64 indexes per collection; keep far fewer.

## Exercises

1. Find an index that makes "the 20 latest orders of customer X with status `paid`" examine ≤ 21 keys. Prove it with `explain`.
2. Reproduce the ESR table with ranges of 1, 30 and 300 days, with and without `limit`. Find the break-even range.
3. Create a partial index for `status: "new"` and show two queries: one that can use it and one that cannot.
4. On a collection with an array field, build a compound multikey index and prove why a second array field is rejected.
5. Write a script that lists indexes with zero `$indexStats` ops and hide (don't drop) them for a week.

→ Answers: [appendix/solutions.md](../appendix/solutions.md#42-mongodb-indexes).

Next: [4.3 Queries and aggregation](03-queries-and-aggregation.md).

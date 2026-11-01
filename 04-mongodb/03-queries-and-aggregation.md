# 4.3 MongoDB: queries, aggregation pipelines and data-model-driven performance

> **Level:** practical · **Prerequisites:** [4.2](02-indexes.md), [1.3](../01-fundamentals/03-query-execution.md) · **After this chapter you can:** write queries the planner can serve from an index, build efficient aggregation pipelines, use `$lookup` safely, and decide when the model, not the query, is the problem. Measured on MongoDB 8.3 with the seed.

## 1. The query engine at a glance

A `find` is parsed into a **filter**, **projection**, **sort**, `skip`, `limit`. The planner enumerates candidate indexes, **runs them in a trial period** and caches the winner for that *query shape* (filter structure ignoring constants). A plan can be evicted when the collection changes a lot, when an index is added/dropped, or when the cached plan performs much worse than expected; this is MongoDB's version of "replanning".

Key knobs: `.hint()`, `.explain()`, `allowDiskUse`, `maxTimeMS`, `batchSize`, `readPreference`, `collation`.

## 2. Writing sargable queries

| Do | Don't |
| --- | --- |
| equality and range on indexed fields | `$where` (JavaScript, no index) |
| anchored regex `/^abc/` | unanchored regex `/abc/` |
| `$in` with a small list | `$nin`, `$ne` on large collections |
| project only needed fields | return whole documents |
| `$or` with each branch indexed | `$or` where one branch has no index (forces a scan) |
| range-based pagination | `skip` with large offsets |
| `$expr` only when necessary | `$expr` comparisons that block index use |
| compare using the same type as stored (`Int32` vs string) | type-mismatched predicates (they silently match nothing or scan) |

`$text` search needs a text index and is limited; for serious search use Atlas Search or a search engine.

## 3. Aggregation framework

A pipeline is an ordered list of stages; each stage's output feeds the next. The optimizer reorders some stages (pushing `$match`/`$sort` before `$project`, combining `$sort+$limit`), but relies on you to put filters first.

Index-friendly pipeline pattern:

```js mongo-local
db.play_orders.aggregate([
  {$match: {status: "paid", createdAt: {$gt: ISODate("2026-09-01T00:00:00Z")}}},   // uses an index
  {$group: {_id: "$customerId", total: {$sum: "$amount"}, orders: {$sum: 1}}},
  {$sort: {total: -1}},
  {$limit: 10}
])
```

Measured for the simpler `$match: {status: "paid"}` followed by the same `$group`: **74 766 keys and 74 766 documents examined**, ~600 ms — the group needs `customerId` and `amount`, so every matching document is fetched.

### Stage costs

| Stage | Notes |
| --- | --- |
| `$match` | first, always; can use indexes only at the beginning of the pipeline |
| `$sort` | uses an index only if it comes before stages that change fields; otherwise in-memory (100 MB limit/stage unless `allowDiskUse`) |
| `$group` | holds groups in memory (100 MB/stage by default, spills with `allowDiskUse`); memory ∝ distinct keys |
| `$project`/`$addFields` | cheap; project early to shrink documents flowing through |
| `$unwind` | multiplies documents; do it after `$match` |
| `$lookup` | a join; see §4 |
| `$facet` | runs several sub-pipelines on the same input, buffers the input (16 MB result limit per output document) |
| `$out` / `$merge` | write results to a collection (materialised views, incremental ETL) |
| `$setWindowFields` | window functions |

Pipeline stages after the first non-index-friendly stage work on every document that reaches them: filter as early as possible.

### Covering aggregations and counts

`countDocuments(filter)` runs an aggregation and honours the filter; `estimatedDocumentCount()` reads collection metadata (fast, approximate after crashes or shard migrations). `count`-like questions on big collections should use the estimate or a maintained counter.

```js mongo-local
db.play_orders.estimatedDocumentCount()
```

## 4. `$lookup` (joins)

`$lookup` joins another collection by a local field and a foreign field. It is executed as a nested loop; **the foreign field must have an index**, otherwise each input document scans the other collection.

```js mongo-local
db.play_orders.aggregate([
  {$match: {customerId: 42}},
  {$lookup: {from: "play_customers", localField: "customerId", foreignField: "_id", as: "customer"}},
  {$limit: 3}
])
```

Here the foreign field is `_id` (always indexed), so each lookup is one index probe. Use `$lookup` for occasional joins and reports; if you need it on every hot read, the model should probably **embed or denormalise** ([1.5](../01-fundamentals/05-data-modeling.md)). Patterns that scale better: precomputed summary documents, the *subset pattern* (embed the top N children, reference the rest), and the *computed pattern* (store aggregates updated on write).

## 5. Modelling patterns that decide performance

| Pattern | Idea |
| --- | --- |
| **Embedding** | store children inside the parent; one read |
| **Subset** | embed the 10 latest comments; the rest in a separate collection |
| **Bucket** | group many small measurements into one document per time bucket (fewer index entries and documents) |
| **Computed** | maintain totals/counters on write instead of aggregating on read |
| **Outlier** | handle rare huge documents separately so they don't dictate the schema |
| **Schema versioning** | a `schemaVersion` field enables rolling migrations |
| **Extended reference** | copy a few frequently needed fields of the referenced document (name) to avoid a lookup |

The cost of each is **write complexity and consistency of copies**; apply where a query is measurably hot.

## 6. Reads, writes and batching

- `insertMany` with `ordered: false` lets the server continue after an error and parallelise; the seed uses it.
- `bulkWrite` batches mixed operations in one round trip (not available in tradar's parser; use a driver).
- Prefer **one update with an operator** (`$inc`, `$push` with `$slice`, `$addToSet`) over read-modify-write.
- Updating an array element: positional operators (`$`, `$[]`, `$[<id>]`) avoid replacing the whole array.
- `findOneAndUpdate` returns the old/new document atomically (queue claims, counters).
- Cursor results arrive in batches (`batchSize`); the default first batch is 101 documents, later up to 16 MB. Don't `toArray()` a million documents.
- Unbounded array pushes: use `$push` with `$each/$slice` to cap size.

## 7. Diagnosing slow operations

### The profiler

```js
// mongosh: capture all operations in the current database (slowms = 0 only for demos!)
db.setProfilingLevel(1, {slowms: 100})
```

```js mongo-local
db.system.profile.find({}, {op: 1, ns: 1, millis: 1, keysExamined: 1, docsExamined: 1, planSummary: 1}).sort({ts: -1}).limit(10)
```

Measured with `slowms: 0`, two queries produced entries such as `op: query, keysExamined: 32, docsExamined: 32, planSummary: "IXSCAN { customerId: 1, createdAt: -1 }"` and `EXPRESS_IXSCAN { status: 1 }`. Profiling level `2` records everything (high overhead; use briefly). `show profile` in tradar shows recent operations.

### Current operations

```js mongo-local
db.adminCommand({currentOp: 1, active: true, secs_running: {$gt: 5}})
```

Look for `waitingForLock`, `planSummary: COLLSCAN`, large `docsExamined`, and `killOp` (`db.adminCommand({killOp: 1, op: <opid>})`) for runaways. Set `maxTimeMS` on client queries to bound them.

### Server status

`db.serverStatus()`: `opcounters`, `connections`, `globalLock.currentQueue`, `wiredTiger.cache`, `metrics.queryExecutor.scanned`/`scannedObjects` (an application-wide ratio of scanned to returned), `metrics.operation.scanAndOrder` (in-memory sorts).

## Exercises

1. Rewrite the top-10 customers pipeline so that it examines the fewest documents; compare keys examined using `explain` on the aggregation (mongosh: `db.play_orders.explain("executionStats").aggregate([...])`).
2. Add `$lookup` from `play_orders` to `play_customers` without an index on the foreign field (use `customerId` on a collection of customers keyed differently) and compare with the indexed case.
3. Create a *bucket* collection from 1 million single-measurement documents and compare size, index size and query time for a range query.
4. Turn on the profiler with a 50 ms threshold, run your exercise queries, and list the three slowest by `millis` with their `planSummary`.
5. Show how a type mismatch (querying `customerId: "42"` against integer ids) behaves in `explain`.

→ Answers: [appendix/solutions.md](../appendix/solutions.md#43-mongodb-queries-and-aggregation).

Next: [4.4 Transactions and concerns](04-transactions-and-concerns.md).

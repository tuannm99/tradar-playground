# 4.6 MongoDB tuning playbook

> **Level:** practical · **Prerequisites:** [4.1](01-storage-wiredtiger.md)–[4.5](05-architecture.md) · Symptom → diagnosis → fix, with copy-pasteable commands. Measured on MongoDB 8.3 with the seed.

## 1. First hour on an unfamiliar deployment

```js mongo-local
db.version()
```

```js mongo-local
db.getCollectionInfos()
```

Collection sizes, counts and indexes:

```js mongo-local
db.runCommand({collStats: "play_orders"})
```

Server health snapshot:

```js mongo-local
db.serverStatus()
```

Read: `connections` (current/available), `opcounters`, `globalLock.currentQueue` (readers/writers waiting), `wiredTiger.cache` (bytes in cache vs max, pages read), `metrics.queryExecutor.scanned` vs `scannedObjects`, `metrics.operation.scanAndOrder`, `repl` and `opLatencies`.

What is running right now:

```js mongo-local
db.adminCommand({currentOp: 1, active: true, secs_running: {$gt: 2}})
```

The slowest recent operations (enable the profiler first: `db.setProfilingLevel(1, {slowms: 100})` in mongosh):

```js mongo-local
db.system.profile.find({millis: {$gt: 100}}).sort({ts: -1}).limit(10)
```

Unused indexes:

```js mongo-local
db.play_orders.aggregate([{$indexStats: {}}, {$project: {name: 1, ops: "$accesses.ops", since: "$accesses.since"}}])
```

## 2. The method for one slow query

1. `explain("executionStats")` ([4.2 §2](02-indexes.md#2-reading-explain)). Record `nReturned`, `totalKeysExamined`, `totalDocsExamined`, stages.
2. `COLLSCAN` or `docsExamined ≫ nReturned` → index. Choose by equality/sort/range, then **measure** ([ESR is a guideline](02-indexes.md#esr-is-a-guideline)).
3. In-memory `SORT` → index that provides order.
4. Fetching big documents for few fields → projection or covered query.
5. `skip` large → range pagination.
6. Aggregation: `$match` first, project early, check index use of the first stage.
7. Still slow → document model (embedding, bucketing, computed fields).
8. Re-run `explain`; record before/after.

## 3. Symptom playbook

### CPU high, many `COLLSCAN`s

Profiler/`currentOp` → find the scanning queries, add indexes, set `maxTimeMS`, and use the Atlas Performance Advisor or `$indexStats` to confirm. Check `metrics.queryExecutor.scanned / returned` ratio over time.

### Slow after a deployment

A changed query shape (new filter order, a type change that defeats the index), a dropped index, a plan cache flip. Compare `explain` for old and new; `db.collection.getPlanCache()` / `$planCacheStats` to inspect cached plans; clear with `db.runCommand({planCacheClear: "coll"})` for diagnosis.

### Cache pressure

`wiredTiger.cache` bytes in cache ≈ configured max, "pages evicted by application threads" rising (application threads are doing eviction work), page-read rate high. Remedies: more RAM, fewer/smaller indexes, narrower documents, projection, TTL for old data, move cold data to another collection/cluster.

### Replication lag

`rs.printSecondaryReplicationInfo()` / `replSetGetStatus` (optime differences), oplog window, secondary disk and CPU, big transactions, index builds on secondaries, network. Flow control may throttle the primary when majority lag exceeds a threshold.

### Writes slow

Too many indexes, `w: "majority"` waiting for a slow secondary (check `replSetGetStatus`), large documents, journal fsync on slow disk, lock contention in a long transaction, bulk inserts done one at a time (use `insertMany`, `ordered: false`).

### "Too many open connections"

Connection pooling mis-sized (each `MongoClient` has its own pool; one per application instance, not one per request). Check `serverStatus().connections`; set `maxPoolSize` thoughtfully; add `maxIdleTimeMS`.

### Disk full

Largest collections/indexes (`collStats`), oplog (it is preallocated up to `oplogSizeMB`), journal, diagnostic data, logs, and **dropped-but-not-reclaimed** space (compact or resync) — after large deletes, space returns to the file only after `compact` or an initial sync; freed space is reused internally meanwhile.

### Long-running index builds

`currentOp` shows `msg` with progress; they run online but compete for I/O. Schedule off-peak. Build on secondaries first in rolling fashion for very large collections (advanced).

## 4. Recipes

### Range-based pagination

```js mongo-local
// Slow at depth (walks 250 020 keys, ≈123 ms measured):
db.play_orders.find({}).sort({_id: 1}).skip(250000).limit(20)
// Fast (20 keys, ≈0 ms measured): remember the last _id of the previous page
db.play_orders.find({_id: {$gt: ObjectId("<last id of previous page>")}}).sort({_id: 1}).limit(20)
```

### Counting

`estimatedDocumentCount()` for the whole collection; `countDocuments(filter)` for filtered counts (uses indexes but still walks them); maintain a counter document updated with `$inc` when you need a cheap exact count under write load.

### Schema evolution without downtime

Add `schemaVersion` to new documents; readers handle both versions; migrate in batches (`updateMany` with a range of `_id` per batch); then drop the old path.

### Deleting a lot of data

Prefer a **TTL index** or dropping a time-partitioned collection over `deleteMany` of millions (which generates oplog and cache pressure). If you must `deleteMany`, batch by `_id` range with pauses.

### Bulk loading

`insertMany` batches of 1 000–10 000 with `ordered: false` (the seed program does this: 30 batches of 10 000); build secondary indexes **after** loading for big imports; disable the balancer while loading a sharded collection that is pre-split.

### Hot document / counter contention

Many writers updating one document serialise on its write conflicts. Shard the counter across N documents and sum on read, or aggregate in an append-only collection.

### Time series

Use a **time series collection** (`timeseries: {timeField, metaField, granularity}`) or the bucket pattern; add a TTL via `expireAfterSeconds`.

## 5. Configuration reference

| Setting | Default | Guidance |
| --- | --- | --- |
| `storage.wiredTiger.engineConfig.cacheSizeGB` | 50% of (RAM − 1 GB) | rarely change; lower in containers with shared memory |
| `operationProfiling.slowOpThresholdMs` | 100 | log slow operations |
| `replication.oplogSizeMB` | 5% of free disk (bounded) | size for your outage window |
| `net.maxIncomingConnections` | 65 536 | cap with pool sizing instead |
| `setParameter: transactionLifetimeLimitSeconds` | 60 | keep transactions short |
| `maxTimeMS` (per operation) | none | always set on user-facing queries |
| `writeConcern` / `readConcern` defaults | `w: majority` (5.0+) | cluster-wide defaults via `setDefaultRWConcern` |

## 6. Anti-pattern gallery

| Anti-pattern | Problem |
| --- | --- |
| unbounded arrays inside documents | growing documents, slow updates, 16 MB limit |
| one collection per tenant/entity (thousands) | file handles, metadata overhead |
| `$where`, unanchored regex, `$ne` as primary filters | scans |
| deep `skip` pagination | O(n) walking |
| indexes on every field "just in case" | write cost, RAM pressure |
| `ObjectId`-as-string comparisons, mixed types in one field | silent mismatches, broken index use |
| treating it as relational (normalised + `$lookup` on every read) | scatter of reads |
| long multi-document transactions | conflicts, memory |
| `w: 1` for critical data | rollback loss |
| sharding too early, or with a monotonic key | operational burden, hot shard |

## 7. Checklist

1. Find the query by total cost (profiler, `currentOp`).
2. `explain`: keys/docs examined vs returned.
3. Right index; verify the plan; check `$indexStats` later.
4. Right document shape for the access pattern.
5. Right concerns: durability vs latency.
6. Working set fits in RAM.
7. Re-measure and record.

## Exercises

1. Enable the profiler at `slowms: 0`, run each chapter-4.2 query, and build a report table (query, keys, docs, millis, plan).
2. Rewrite a `$lookup`-heavy read path as an *extended reference* + subset pattern; compare latency and write cost.
3. Demonstrate hot-document contention with 50 concurrent `$inc`s on one document, then fix it with sharded counters.
4. Reproduce the "type mismatch" slow query and the fix.

→ Answers: [appendix/solutions.md](../appendix/solutions.md#46-mongodb-tuning-playbook).

Next part: [5. Architecture](../05-architecture/01-oltp-vs-olap.md).

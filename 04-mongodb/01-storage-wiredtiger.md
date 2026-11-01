# 4.1 MongoDB: documents and the WiredTiger storage engine

> **Level:** internals · **Prerequisites:** [1.1](../01-fundamentals/01-storage.md), [1.5](../01-fundamentals/05-data-modeling.md) · **After this chapter you can:** explain how a document is stored and cached, what journal, checkpoint and oplog each protect, and why the 16 MB document limit and working set matter. Measured on MongoDB 8.3 (single-node replica set) with the [seed](../README.md#seed-data) (`playground.play_orders`, 300 000 documents).

The queries in this part are mongosh/tradar-style. tradar's Mongo parser accepts `db.<collection>.<method>(...)`, `explain()` at the end of a `find` chain, and `db.runCommand({...})` for everything else (including `createIndexes`). Transactions and `createIndex()` are not available in tradar's parser, so those examples are marked for `mongosh`.

## 1. The document model and its limits

A document is BSON (binary JSON with extra types: `ObjectId`, `Date`, `Decimal128`, `Int64`, `BinData`…). Documents live in collections without a fixed schema; field order is preserved; the maximum size is **16 MB**; every document has a unique `_id` (default `ObjectId`: 4 bytes time + 5 random + 3 counter ≈ roughly increasing).

```js mongo-local
db.play_orders.findOne()
```

The seed's documents embed their line items (`items`: an array of `{sku, qty}`) — one read returns the order and its lines, and one write updates both atomically. See [embedding vs referencing](../01-fundamentals/05-data-modeling.md#3-embedding-vs-referencing-documents) for when that is right.

Why the model constrains performance:

- **A document is the unit of read and write.** Reading one field reads (and caches) the whole document unless a covered index serves the query. Wide documents waste cache and I/O.
- **Unbounded arrays** (comments appended forever) grow a document until rewrites get expensive and the 16 MB limit is hit.
- **Field names are stored in every document.** A million documents with a 30-character field name carry that string a million times (compression recovers much of it, but short names still help).
- **Update cost ∝ document size** in practice, since changes are written as new versions of the record (see MVCC below).

## 2. WiredTiger in one page

WiredTiger is MongoDB's default storage engine (since 3.2).

| Component | Role |
| --- | --- |
| **B-tree per collection and per index** | the collection is a B-tree keyed by an internal `RecordId`; `_id` and other indexes map keys to `RecordId`s |
| **Cache** | in-memory pages (decompressed); default = the larger of 50% of (RAM − 1 GB) or 256 MB |
| **Compression** | on-disk blocks (collections: `snappy` by default; indexes: prefix compression); `zstd`/`zlib` available |
| **MVCC + snapshots** | readers see a consistent snapshot; writers create new versions; old versions go to the **history store** while some snapshot needs them |
| **Document-level concurrency** | write conflicts are detected per document (`WriteConflict`, retried internally for single-document ops) |
| **Journal** | write-ahead log for crash recovery between checkpoints |
| **Checkpoints** | consistent on-disk snapshots, taken roughly every 60 seconds |

### Durability path

1. A write modifies the in-memory page and appends a journal record.
2. The journal is flushed to disk every ~100 ms (and when the client asks with `j: true`, or on `w: "majority"` in a replica set that needs a durable majority).
3. A **checkpoint** writes dirty pages as a new consistent version of the files; older journal data is then discardable.
4. After a crash, the engine opens the last checkpoint and replays the journal.

### Replication log: the oplog

In a replica set, every write on the primary also appends an idempotent record to `local.oplog.rs`, a **capped collection**. Secondaries tail it. The oplog's size (here ~46 GB maximum on the test node) sets the **oplog window**: how long a secondary can be offline and still catch up without a full resync. See [4.5](05-architecture.md).

## 3. Sizes and the working set

```js mongo-local
db.runCommand({collStats: "play_orders"})
```

Read `size` (uncompressed data, ~51 MB measured), `storageSize` (compressed on disk, ~13 MB), `avgObjSize` (~171 B), `nindexes`, `indexSizes`, `totalIndexSize`.

Measured on the seed after the index exercises of [4.2](02-indexes.md): the `_id` index 7.7 MB, `status_1` 1.6 MB, `{customerId, createdAt}` 3.8 MB, partial `recent_new` 0.9 MB — together ~14 MB: about 27% of the uncompressed data size and **more than the compressed collection on disk (~13 MB)**. **Indexes count against RAM**.

The **working set** is the data and index pages your workload touches regularly. If it fits in the WiredTiger cache + OS cache, reads are memory-speed; if not, throughput collapses into disk waits (`wiredTiger.cache.pages read into cache` rising, high page-fault rate).

```js
// mongosh
const c = db.serverStatus().wiredTiger.cache;
({ maxBytes: c["maximum bytes configured"], inCache: c["bytes currently in the cache"], dirty: c["tracked dirty bytes in the cache"], readInto: c["pages read into cache"] })
```

(`db.serverStatus()` alone also works in tradar; the destructuring above needs mongosh.)

## 4. How operations map to storage

| Operation | Cost |
| --- | --- |
| `insertOne` | write document + one entry per index + journal + oplog |
| `updateOne` `$set` | new version of the record; update each index whose key changed |
| `deleteOne` | remove record and index entries |
| `find` by `_id` | one tree descent + one record read |
| Collection scan | read every page in `RecordId` order |
| Index scan + fetch | tree descent in the index, then one random record read per match |

Document growth no longer relocates documents like the old MMAPv1 engine did, but a large rewrite still costs I/O and cache.

## 5. Identifiers

`ObjectId` values are roughly time-ordered so inserts append near the right edge of the `_id` index (cache friendly). Fully random ids (UUIDv4 stored as strings/binary) scatter inserts across the index — the same problem described in [1.5 §4](../01-fundamentals/05-data-modeling.md#4-keys). Store UUIDs as BSON binary subtype 4, not 36-character strings, to halve key size.

## 6. Time-series and capped collections

- **Capped collections** are fixed-size circular logs (insertion order preserved, oldest overwritten, no deletes). The oplog is one.
- **Time series collections** (`timeseries` option) store measurements bucketed by time and metadata, compressing columnar-style and giving efficient range queries — an alternative to plain collections for metrics/IoT.
- **TTL indexes** (`expireAfterSeconds`) delete documents after a time; deletion runs in a background task about once a minute.

## Exercises

1. Insert 1 000 000 tiny documents `{a: <random int>}` and compare `size` vs `storageSize`. Then repeat with a 30-character field name and with a 3-character one.
2. Compare insertion time and `indexSizes` for an index on a monotonically increasing key vs a random key, on a collection bigger than the WiredTiger cache you configure (start `mongod` with `--wiredTigerCacheSizeGB 0.25` to force it).
3. Find the oplog size and compute the oplog window on a busy test run (`db.getReplicationInfo()` in mongosh).
4. Model an order with 10 000 line items and discuss whether to embed, reference, or *bucket* them.

→ Answers: [appendix/solutions.md](../appendix/solutions.md#41-mongodb-storage).

Next: [4.2 Indexes](02-indexes.md).

# 2c.5 MongoDB: replica sets, sharding and cluster architecture

> **Level:** architecture · **Prerequisites:** [2c.1](01-storage-wiredtiger.md), [2c.4](04-transactions-and-concerns.md), [3.2](../../03-architecture/02-replication-and-consistency.md), [3.3](../../03-architecture/03-partitioning-and-sharding.md) · **After this chapter you can:** explain elections, oplog replication and rollback, design a replica set for availability, and choose a shard key. The test node was a one-member replica set, so cluster behaviour below is explained from the design rather than measured.

## 1. Replica sets

A **replica set** is a group of `mongod` processes holding the same data: one **primary** (accepts writes) and **secondaries** (replicate and optionally serve reads). Minimum for fault tolerance is three voting members (two data-bearing members plus an *arbiter* is possible but weakens durability — avoid arbiters with `w: "majority"`).

```js mongo-local
db.adminCommand({replSetGetStatus: 1})
```

Measured: `ok: 1`, a single member in state `PRIMARY` for the test node.

### Replication

1. The primary applies a write and records an idempotent entry in `local.oplog.rs` (a capped collection).
2. Secondaries **pull** the oplog (tailing cursor) and apply entries, in batches, in parallel by collection/document.
3. The primary learns each member's progress (replication heartbeats) and can advance the **majority commit point** (the latest operation replicated to a majority) — the basis of `readConcern: "majority"` and `w: "majority"`.

Oplog window = how much history the oplog holds. A secondary offline longer than the window cannot catch up and needs an **initial sync** (full copy). Size the oplog for your worst expected outage plus margin, and watch `rs.printReplicationInfo()`.

### Elections and failover

- Members exchange heartbeats (every 2 s). If a secondary cannot reach the primary for `electionTimeoutMillis` (10 s default), it calls an election (Raft-like protocol); a majority vote elects the most up-to-date eligible member.
- During the election (typically seconds) the set has **no primary and writes fail**; drivers with `retryWrites` retry once transparently.
- **Rollback**: a stepped-down primary may hold writes that never reached a majority; on rejoining it rolls them back (saved to rollback files). This is why `w: 1` writes can disappear and `w: "majority"` cannot.
- Tune with member `priority` (preferred primary), `votes`, `hidden` members (reporting, backups, never primary), `delayed` members (time-delayed copy for human error recovery), and `tags` (data-centre-aware write concerns and read preferences).

### Reads on secondaries

Scales reads and gives geographic locality but exposes replication lag. Use causal consistency sessions when you need read-your-writes ([2c.4 §5](04-transactions-and-concerns.md#5-read-preference)). Heavy analytics belong on a hidden/analytics-tagged secondary or in a separate system.

## 2. Sharding

Sharding partitions a collection across **shards** (each shard is a replica set) to scale storage, write throughput and working set beyond one machine.

```
 clients ─► mongos (routers, stateless) ─► config servers (replica set: chunk map, cluster metadata)
                    │ route by shard key
        ┌───────────┼───────────┐
     shard A      shard B      shard C      (each a replica set)
```

- **Shard key**: one or more fields that determine which chunk (range or hash) a document belongs to. The **balancer** moves chunks between shards to keep data even.
- `mongos` routes queries: a query containing the shard key (or its prefix) goes to **one or a few shards** (*targeted*); otherwise it is **scatter-gather** to all shards and merged.

### Choosing a shard key

A good key has:

1. **High cardinality** (many distinct values, so many chunks).
2. **Even distribution of writes and data** (no hot shard).
3. **Query isolation**: your frequent queries include it (targeted queries).
4. **Not monotonic** if range-sharded: a key like `_id`/timestamp sends all inserts to the last chunk (a *hot shard*). Use a **hashed** key to spread inserts at the cost of range queries, or a compound key like `{tenantId, ts}`.

| Strategy | Pros | Cons |
| --- | --- | --- |
| Ranged on `{tenantId: 1, _id: 1}` | tenant queries are targeted; locality | large tenants → jumbo chunks |
| Hashed on `customerId` | even inserts and distribution | range queries become scatter-gather |
| Zoned (tag-aware) ranges | data residency; tiering | manual ops |

Since 5.0 you can **reshard** a collection online (`reshardCollection`); it's heavy but possible, and `refineCollectionShardKey` can append fields. Still, choose carefully. Do not shard before you must: sharding adds operational complexity (config servers, balancer, scatter-gather costs, transactions across shards are slower).

### Sharding and indexes

Every shard keeps its own indexes. Unique indexes must include the shard key (or be enforced differently). An index on the shard key prefix is required.

## 3. Storage and cluster sizing

- **Working set in RAM** is the main sizing rule ([2c.1](01-storage-wiredtiger.md#3-sizes-and-the-working-set)).
- Separate disks/volumes for data and journal on spinning media; fast SSD/NVMe otherwise. XFS is the recommended filesystem on Linux.
- Disable transparent huge pages; set high `ulimit`s; keep clocks in sync (NTP).
- Provision CPU for compression and index builds; IOPS for random document fetches.

## 4. Backups and recovery

| Method | Notes |
| --- | --- |
| `mongodump`/`mongorestore` | logical; slow for large data; use with `--oplog` for point-in-time consistency of the dump |
| File system / cloud snapshots | fast; need journal on the same volume, or `fsyncLock` / backup cursors |
| Continuous backup with oplog replay (Ops Manager, Atlas) | point-in-time recovery |
| Delayed secondary | cheap human-error protection (not a backup) |

Replication is not backup. Practise restores.

## 5. Security and operations checklist

Authentication and role-based access control on; TLS between members and clients; IP allow-listing; separate users per service with least privilege; audit if required; monitor replication lag, oplog window, connections, queue lengths, page faults, cache pressure, slow query log; rolling upgrades one member at a time (secondaries first, then step down the primary).

## 6. Choosing MongoDB

Good fit: document-shaped aggregates read and written together, flexible/evolving schemas, high write throughput with horizontal scale, geo/time-series features. Poor fit: heavy multi-entity relational queries and joins, strict cross-document integrity everywhere, analytical scans over huge data (use a column store, see [3.5](../../03-architecture/05-choosing-a-database.md)).

## Exercises

1. Draw the timeline of a primary failure: heartbeat loss, election, new primary, the old primary rejoining with an unreplicated `w: 1` write (rollback). Where does `w: "majority"` change the story?
2. Choose a shard key for each: (a) a multi-tenant SaaS with a few huge tenants, (b) an IoT firehose queried by device and time, (c) a product catalogue read by id. Justify with the four criteria.
3. Compute the oplog size needed to survive a 12-hour secondary outage at 5 MB/s of oplog generation.
4. A scatter-gather query takes 800 ms on 8 shards; the same query with the shard key takes 40 ms. What does this tell you about adding shards to speed this query?
5. Plan a rolling upgrade of a 3-member replica set with zero write downtime.

→ Answers: [appendix/solutions.md](../../appendix/solutions.md#2c5-mongodb-architecture).

Next: [2c.6 Tuning playbook](06-tuning-playbook.md).

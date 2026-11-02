# 2b.5 ClickHouse: architecture, replication and clusters

> **Level:** architecture · **Prerequisites:** [2b.1](01-storage-mergetree.md), [2b.4](04-consistency-and-mutations.md), [3.2](../../03-architecture/02-replication-and-consistency.md) · **After this chapter you can:** lay out shards and replicas, explain how replication and `Distributed` tables work, and choose a sharding key. This chapter is conceptual: the single-node seed doesn't run a cluster, so no measured numbers appear here.

## 1. Single node internals

A ClickHouse server is **one multi-threaded process** (no process-per-connection): query threads, background pools for merges, mutations, fetches, moves and flushes, plus caches:

| Cache | Holds |
| --- | --- |
| mark cache | column `.mrk` files (maps granule → offset) |
| uncompressed cache | decompressed blocks (off by default; helps repeated small reads) |
| primary index | kept in memory per part |
| OS page cache | compressed column data — the main data cache |

Storage is local disks by default, or **object storage (S3, GCS, Azure)** through storage policies/disks, with local caching and tiered volumes (hot → cold by TTL `TO VOLUME`). ClickHouse Cloud's `SharedMergeTree` puts the data on object storage shared by all replicas, removing replication of data between nodes.

## 2. Replication: ReplicatedMergeTree and Keeper

`ReplicatedMergeTree` tables replicate **parts**, not statements. Coordination metadata (the log of part additions/merges/mutations, block hashes for dedup, leader information) lives in **ClickHouse Keeper** (a Raft-based, ZooKeeper-compatible service; ZooKeeper also works).

```
client ─insert─► replica A ──write part──► local disk
                    │ publish "new part 202610_5_5_0" to Keeper log
                    ▼
            replica B ◄─fetch part directly from A (over HTTP)─ replica C
```

How it behaves:

- Any replica accepts inserts (**multi-master** for writes); parts are fetched peer to peer.
- **Merges are deterministic**: one replica is assigned a merge in the log; others either fetch the merged part or repeat the merge.
- Replication is **asynchronous** by default. `insert_quorum = N` waits for N replicas to confirm; `select_sequential_consistency = 1` makes reads wait to see quorum-committed data.
- Keeper becomes a critical dependency: keep an odd number (3 or 5), on low-latency nodes. A Keeper outage makes replicated tables read-only.
- Monitor `system.replicas` (`is_readonly`, `absolute_delay`, `queue_size`) and `system.replication_queue`.

## 3. Sharding and the Distributed engine

A **shard** holds a subset of the rows; a **replica** is a copy of a shard. A cluster is defined in server configuration (`remote_servers`).

```
                     query ─► Distributed table `events`  (a view over the cluster; stores nothing)
                               │ rewrites and fans out
        ┌──────────────────────┼──────────────────────┐
     shard 1 (replicas A1,A2) shard 2 (B1,B2)        shard 3 (C1,C2)
     events_local             events_local            events_local
```

- Create the physical table `events_local` (ReplicatedMergeTree) on every node, and a `Distributed` table `events` pointing to it with a **sharding key** (`rand()`, `cityHash64(user_id)`, ...).
- **Reads**: the initiator sends the query to one replica per shard, each computes a partial result, the initiator merges. Aggregations are pushed down; only intermediate states cross the network.
- **Writes** to the `Distributed` table are forwarded asynchronously to shards (buffered on disk, then sent). For high throughput, writers often **insert directly into `events_local`** on the right shard to avoid the extra hop, with `internal_replication = true` so the replicated table handles replica copying.
- **Joins and `IN`**: by default the right side is evaluated *on each shard*; use `GLOBAL JOIN`/`GLOBAL IN` to broadcast a right-side result, or co-locate join keys by sharding both tables on the same key.

### Choosing the sharding key

| Key | Result |
| --- | --- |
| `rand()` | even load; any query hits all shards; entity data scattered |
| `cityHash64(user_id)` | all rows for a user on one shard → per-user queries and `GROUP BY user_id` run locally without moving data; risk of skew with heavy users |
| time | hot shard for current data, bad |
| `tenant_id` (multi-tenant) | isolation; unbalanced for large tenants |

**Rule:** shard by the key that you most often `GROUP BY`, `JOIN` or filter on, provided it is high-cardinality and not skewed. Resharding later is expensive (copy data), so decide early.

## 4. Scaling guide

1. **Start with one node.** ClickHouse scales vertically very far (tens of cores, TBs of NVMe, billions of rows per second of scan).
2. **Add replicas** for availability and concurrent query capacity.
3. **Add shards** when the data or the per-query CPU no longer fits one node.
4. Prefer **materialized views and projections** to reduce what each query reads before scaling hardware.
5. Separate **ingest** from **query** load (different replicas, resource limits).

## 5. Ingestion architecture

| Source | Typical path |
| --- | --- |
| App events | batch in the app/agent, `INSERT` over HTTP/native in large blocks; or `async_insert` |
| Kafka | the `Kafka` table engine + materialized view into a MergeTree; or Kafka Connect / ClickPipes |
| Files / object storage | `INSERT ... SELECT FROM s3(...)`, `file()`, `url()` table functions |
| Another database | `postgresql()`/`mysql()` table functions, or CDC (Debezium → Kafka) |
| Backfill | insert into a staging table, then swap partitions |

## 6. Multi-tenancy, limits and safety

- **Users, roles, profiles, quotas** (`max_memory_usage`, `max_execution_time`, `max_rows_to_read`, `readonly`) protect the server from one bad query. Set them for every non-admin user.
- **Row policies** restrict rows per user/role.
- `max_concurrent_queries` and the query queue prevent overload; `max_server_memory_usage` caps the process.
- Always put a **limit on result size** (`max_result_rows`, `LIMIT`) for user-facing endpoints.

## 7. Backup and recovery

- `BACKUP TABLE ... TO Disk/S3(...)` / `RESTORE` natively, incremental via `base_backup`.
- `FREEZE` partitions and copy the hard-linked parts (what `clickhouse-backup` does).
- Replication is **not** backup: a bad `DELETE` replicates too.
- Test restores.

## 8. When ClickHouse is the wrong tool

- High-rate **point lookups and updates** by key (use PostgreSQL, a KV store, or a cache in front).
- **Multi-statement transactions** or foreign-key integrity.
- Frequent updates/deletes of individual rows.
- Small data where PostgreSQL does the job with far less operational complexity.

A common architecture is **PostgreSQL (system of record) + ClickHouse (analytics)**, fed by CDC or periodic exports ([3.5](../../03-architecture/05-choosing-a-database.md)).

## Exercises

1. Draw the message flow for `INSERT` into a 2-shard × 2-replica cluster through (a) the `Distributed` table and (b) directly into `events_local`. Where can data be lost if a node dies mid-flight, and how does `internal_replication` matter?
2. You shard `events` by `rand()` and a dashboard query does `count(distinct user_id)` per country. How much data crosses the network, and how would sharding by `cityHash64(user_id)` change it?
3. Design the Keeper deployment for a 3-AZ region: how many nodes, where, and what happens when one AZ fails.
4. A report joins `events` (sharded) with a 500 MB `users` table. Compare plain `JOIN`, `GLOBAL JOIN`, a replicated dimension table on every node, and a dictionary.

→ Answers: [appendix/solutions.md](../../appendix/solutions.md#2b5-clickhouse-architecture).

Next: [2b.6 Tuning playbook](06-tuning-playbook.md).

# 2d.5 Elasticsearch: clusters, shards, replicas and data lifecycle

> **Level:** architecture · **Prerequisites:** [2d.1](01-storage-lucene.md), [2d.4](04-writes-consistency-concurrency.md), [3.2](../../03-architecture/02-replication-and-consistency.md), [3.3](../../03-architecture/03-partitioning-and-sharding.md) · **After this chapter you can:** size shards, choose replica counts, explain how a search fans out and how a cluster elects a master, and manage time-based data with data streams and ILM. The test node was a single node, so cluster behaviour is explained from the design; the few measurements are labelled.

## 1. Nodes and roles

A cluster is a set of nodes sharing a `cluster.name`. A node has one or more **roles**:

| Role | Job |
| --- | --- |
| **master-eligible** | holds and changes the **cluster state** (index metadata, shard allocation); one elected master at a time |
| **data** (`data_hot`, `data_warm`, `data_cold`, `data_frozen`) | stores shards, executes indexing and search; tiers map to storage cost |
| **ingest** | runs ingest pipelines (parse, enrich) before indexing |
| **coordinating** (any node, or dedicated) | receives a request, fans it out, merges results |
| **ml**, **transform**, **remote_cluster_client** | specialised |

Production minimum: **three master-eligible nodes** so a majority (quorum, 2 of 3) survives the loss of one, preventing split brain ([3.2 §5](../../03-architecture/02-replication-and-consistency.md#5-failure-failover-and-split-brain)). Since 7.x the cluster manages its voting configuration itself; do not set the old `minimum_master_nodes`.

## 2. Primary and replica shards

`number_of_shards` is fixed at creation (changing it means split/shrink/reindex); `number_of_replicas` can change any time.

```
index products: 3 primaries (P), 1 replica each (R)

  node 1: P0  R1          node 2: P1  R2          node 3: P2  R0
```

- Replicas give **availability** (a copy survives a node loss) and **read capacity** (searches go to any copy).
- Replicas **never** sit on the same node as their primary. Measured: with one node, `number_of_replicas: 1` turned the index **yellow** (1 unassigned shard); `green` needs all replicas assigned.
- Each replica repeats the indexing work, so write throughput does not grow with replicas; read throughput does.

### How a search runs

1. Any node is the **coordinator**. It picks one copy of each shard (adaptive replica selection).
2. **Query phase**: each shard searches its segments and returns the top `from + size` (doc ids and scores).
3. **Fetch phase**: the coordinator takes the global top `size` and fetches those `_source`s from the owning shards.

Latency is dominated by the **slowest shard**; the more shards a query touches, the higher the tail. This is why very deep `from` is expensive ([2d.3 §6](03-queries-and-relevance.md#6-pagination)).

### How a write is routed

`shard = hash(_routing or _id) % number_of_primary_shards`. That is why the primary shard count cannot change freely. A custom `_routing` (for example the tenant id) sends one tenant's documents to one shard, so its searches can touch a single shard, at the risk of hot or oversized shards.

## 3. Sizing shards

Shards are not free: each one costs file handles, heap for cluster state and segment metadata, and a thread slot per search.

| Rule of thumb (documentation guidance, not measured here) | Why |
| --- | --- |
| Target shard size roughly **10-50 GB** (logs/time series up to ~50 GB) | smaller = overhead dominates; bigger = slow recovery and merges |
| Keep **fewer than ~20 shards per GB of heap** on a node | cluster-state and per-shard memory |
| Do not create one index per user/day with tiny shards | **oversharding** is the most common cluster failure |

Measured reference: the seed's shards hold 11.2 MB and 39.7 MB, which is absurdly small for production, fine for learning. A 1 GB heap node showed 26 % heap used with both indices loaded; Lucene segment data lives mostly **outside the heap** in the OS page cache, so give Elasticsearch about **half the RAM as heap (max ~30 GB)** and leave the rest to the filesystem cache. This is the same buffer-cache logic as [1.1 §3](../../01-fundamentals/01-storage.md#3-the-buffer-cache).

## 4. Time-based data: data streams, rollover, ILM

Logs, metrics and events are append-only and expire. Do not delete documents ([2d.4 §7](04-writes-consistency-concurrency.md#7-deletes-and-data-lifecycle)); organise by time:

- A **data stream** is an append-only abstraction over hidden backing indices (`.ds-logs-000001`, ...).
- **Rollover** starts a new backing index when the current one reaches an age or size (for example 50 GB or 7 days).
- **ILM** (index lifecycle management) moves indices through phases: *hot* (writing, fast disks) → *warm* (read-only, `forcemerge`, fewer replicas) → *cold* (cheap storage) → *delete* (drop the index).

This gives partition-pruning for free: a query with a `@timestamp` range only touches the indices whose time range overlaps ([3.3](../../03-architecture/03-partitioning-and-sharding.md)). Compare with `PARTITION BY` + `DROP PARTITION` in PostgreSQL and MergeTree TTL in ClickHouse ([2b.1](../clickhouse/01-storage-mergetree.md)).

## 5. Cross-cluster and snapshots

- **Cross-cluster search/replication** queries or replicates between clusters (regions, DR).
- **Snapshots** (to object storage) are the backup mechanism; replicas are *not* backups. Test restores.
- **Searchable snapshots** serve cold/frozen data straight from object storage at lower cost.

## 6. When Elasticsearch is the right piece

Good fit: full-text and relevance search, faceting, log/event search and observability, autocomplete, geo search, "search box over a lot of semi-structured documents". Poor fit: system of record, multi-document transactions, heavy joins, frequent updates to the same documents, strict read-after-write ([2d.4](04-writes-consistency-concurrency.md)). For pure analytical scans ClickHouse is usually cheaper per query ([3.1](../../03-architecture/01-oltp-vs-olap.md)). Typical combination: **PostgreSQL/MongoDB (truth) + Elasticsearch (search) + cache** ([3.5](../../03-architecture/05-choosing-a-database.md)).

## Try it

```esq es-local
GET /_cluster/health
```

```esq es-local
GET /_cat/shards/play_*?v&h=index,shard,prirep,state,docs,store
```

```esq es-local
GET /_nodes/stats/jvm
```

## Exercises

1. A cluster has 3 master-eligible nodes. One is partitioned away from the other two. Which side keeps a master, which side rejects writes, and why?
2. An application creates one index per customer per day, each holding about 5 MB. What fails first as customers grow, and how do you re-model it (two options)?
3. Your logs index must keep 30 days, ingest 100 GB/day, and query mostly the last 24 hours. Sketch the data stream, rollover and ILM policy, and estimate the number of shards.
4. Why does adding replicas not speed up bulk indexing, and when does it speed up search?

→ Answers: [appendix/solutions.md](../../appendix/solutions.md#2d5-elasticsearch-architecture).

Next: [2d.6 Tuning playbook](06-tuning-playbook.md).

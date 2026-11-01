# 5.3 Partitioning and sharding

> **Level:** architecture · **Prerequisites:** [1.5](../01-fundamentals/05-data-modeling.md), [2.5 §7](../02-postgres/05-architecture.md#7-partitioning), [3.1 §4](../03-clickhouse/01-storage-mergetree.md#4-partitions), [4.5 §2](../04-mongodb/05-architecture.md#2-sharding) · **After this chapter you can:** tell partitioning from sharding, pick a partition or shard key, and predict the operational cost of each strategy.

## 1. Definitions

- **Partitioning**: splitting a table into pieces *within one database instance* (or one logical dataset), so queries can skip pieces and maintenance can work per piece.
- **Sharding**: distributing pieces *across machines*, so no single machine holds or serves everything.
- **Replication**: copies of the same data ([5.2](02-replication-and-consistency.md)). Real clusters combine all three: each shard is a replica set.

## 2. Why split

| Goal | Partitioning | Sharding |
| --- | --- | --- |
| Skip data on reads (pruning) | yes | yes, if the key is in the query |
| Cheap deletion/retention | yes (drop a partition) | per shard |
| Smaller indexes, faster maintenance | yes | yes |
| Exceed one machine's storage | no | **yes** |
| Exceed one machine's write throughput | rarely | **yes** |
| Exceed one machine's working-set RAM | no | **yes** |
| Parallel query execution across nodes | limited (parallel workers) | **yes** |

Rule: **partition early when it is cheap and obviously useful (time-ordered data); shard only when a single node can no longer cope after vertical scaling, caching and read replicas.**

## 3. Strategies

| Strategy | Mapping | Good for | Weak at |
| --- | --- | --- | --- |
| **Range** | key ranges → partitions (dates, ids) | time series, range scans, archiving | hot spots when the key is monotonic (all inserts go to the newest partition) |
| **Hash** | `hash(key) mod N` or consistent hashing | even distribution, point lookups | range queries (scatter-gather); resharding moves a lot of data with plain `mod N` |
| **List** | explicit value → partition | regions, tenants with known values | uneven sizes |
| **Composite** | e.g. range by time, then hash by entity | large time-series per entity | complexity |
| **Directory (lookup table)** | a service maps keys to shards | flexible placement, tenant moves | the directory is a dependency and a bottleneck |

**Consistent hashing** (and virtual nodes) remaps only ~1/N of keys when a node joins or leaves; fixed `mod N` remaps almost everything.

## 4. Choosing a key — the questions

1. **What do the dominant queries filter by?** The key should appear in them (pruning/targeting).
2. **Is the value space large and evenly used?** Low cardinality or skew creates hot partitions/shards.
3. **Is the key monotonic?** If yes, range-splitting concentrates writes; hash or add a prefix.
4. **Does it co-locate data that is read together?** (tenant + child rows; users and their orders) — avoids cross-shard joins.
5. **Can it change?** A key that changes means moving rows. Prefer immutable.
6. **What happens at 10× scale?** A key with 50 tenants of wildly different sizes will not scale evenly.

## 5. Per-engine view

| | PostgreSQL | ClickHouse | MongoDB |
| --- | --- | --- | --- |
| Within a node | declarative `RANGE/LIST/HASH` partitions with pruning; BRIN | `PARTITION BY` (coarse, time) + sort key inside parts | none (collections; time-series buckets) |
| Across nodes | Citus, application sharding, FDW | `Distributed` table + sharding key | built-in sharding: ranged/hashed shard key, balancer |
| Pruning | partition pruning by `WHERE` on key | partition + sparse-index + skip index | targeted queries by shard key |
| Retention | `DETACH/DROP PARTITION` | TTL, `DROP PARTITION` | TTL index, drop collection |

Measured in this handbook: PostgreSQL pruned a daily filter to a single monthly partition and scanned all partitions for a non-key filter ([2.5 §7](../02-postgres/05-architecture.md#7-partitioning)); ClickHouse pruning is visible as `Parts: 6/21` in `EXPLAIN` ([3.2 §2](../03-clickhouse/02-indexes-and-pruning.md#2-choosing-the-order-by-key)).

## 6. Costs of sharding

- **Cross-shard queries**: scatter-gather latency is bounded by the slowest shard; tail latency grows with shard count.
- **Cross-shard transactions**: 2PC or sagas ([1.4 §7](../01-fundamentals/04-transactions.md#7-beyond-one-node)); higher latency and failure modes.
- **Cross-shard joins/aggregations**: move data or broadcast one side.
- **Global uniqueness and secondary indexes**: must include the shard key or become distributed.
- **Rebalancing and resharding**: expensive, risky, needs tooling.
- **Operational complexity**: more nodes, topology awareness, backups per shard, schema changes everywhere.

Mitigations: choose keys that keep most operations single-shard; denormalise to avoid cross-shard joins; keep a small global/reference data set replicated to every shard.

## 7. Hot spots and skew

Detect: per-shard/partition size and ops rate; top keys; `p99` per shard. Fix: salt hot keys (`key#0..k-1`) and aggregate on read; split big tenants onto dedicated shards; use hash instead of range; pre-split ranges before bulk load; cache extremely hot reads.

## 8. A migration path

1. Single node, well-indexed.
2. Read replicas + caching.
3. Partition large append-only tables by time.
4. Move cold data to cheaper storage / another system.
5. Shard by tenant or entity key, behind an abstraction in the application (a "shard router") so the topology can change.
6. Keep the ability to **reshard**: stable logical shards (e.g. 1 024 logical shards mapped to N physical nodes) make adding nodes a mapping change.

## Exercises

1. Design partitioning for a 10-billion-row `events(tenant_id, user_id, ts, payload)` table in PostgreSQL, ClickHouse and MongoDB. For each, give the key(s), the retention plan and the three queries that benefit.
2. A hash-sharded cluster uses `hash(user_id) mod 8`. You must go to 12 shards. Estimate the fraction of keys that move with `mod`, and with consistent hashing.
3. A "top 100 customers by spend" query runs across 16 shards sharded by `order_id`. Describe the execution and why its latency scales poorly. How would you restructure?
4. Show a hot-shard scenario with a monotonic key and the effect on write throughput; propose two fixes.

→ Answers: [appendix/solutions.md](../appendix/solutions.md#53-partitioning-and-sharding).

Next: [5.4 Caching and connection pooling](04-caching-and-pooling.md).

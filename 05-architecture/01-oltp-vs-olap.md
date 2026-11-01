# 5.1 OLTP vs OLAP: two workloads, two designs

> **Level:** architecture · **Prerequisites:** [1.1](../01-fundamentals/01-storage.md), [1.3](../01-fundamentals/03-query-execution.md) · **After this chapter you can:** classify a workload, explain why one engine is poor at the other's job, and design the data flow between an operational store and an analytical store.

## 1. The two workloads

| | **OLTP** (online transaction processing) | **OLAP** (online analytical processing) |
| --- | --- | --- |
| Typical operation | read/write a few rows by key; short transaction | scan millions–billions of rows; aggregate |
| Concurrency | thousands of small queries per second | tens of heavy queries |
| Latency target | milliseconds | seconds (interactive) to minutes |
| Data size touched per query | KB–MB | GB–TB |
| Writes | frequent, small, updates and deletes | bulk loads / appends, rare updates |
| Consistency needs | strong (ACID) | often eventual; snapshots |
| Schema | normalised | denormalised, wide, star/snowflake |
| Storage layout | row store, B-tree | column store, compressed, sorted |
| Examples here | PostgreSQL, MongoDB | ClickHouse |

Real systems contain both. Trying to serve both from one engine usually fails at scale: a long analytical scan evicts the OLTP working set from cache, holds snapshots that block vacuum/history cleanup, and competes for I/O and locks.

## 2. Why the layouts differ

- **Point lookups** are cheapest in a row store: one page holds the whole row ([1.1 §4](../01-fundamentals/01-storage.md#4-row-store-vs-column-store)).
- **Aggregations** over one or two columns read a tiny fraction of bytes in a column store, and compress 5–20×.
- **Updates in place** are natural in a B-tree; in an immutable-parts column store they become rewrites ([3.4](../03-clickhouse/04-consistency-and-mutations.md)).
- **High-rate small commits** need a WAL and group commit; **large-batch ingest** needs sorted immutable files.

Seed illustration: `select country, count() from play_events group by country` over 3 M events read only the `country` column (**5.3 MiB**, 18 ms measured); the PostgreSQL equivalent on 500 k orders must read every full page of the heap (`Parallel Seq Scan` over ~3.9 k pages, [2.3 §4](../02-postgres/03-planner-and-explain.md)). In reverse, fetching one order by id in PostgreSQL is a handful of pages, while in ClickHouse it reads at least a full 8 192-row granule of each requested column ([3.2](../03-clickhouse/02-indexes-and-pruning.md)).

## 3. Where documents fit

MongoDB is an operational store whose unit is the aggregate document. It sits on the OLTP side: strong per-document operations, flexible shape, horizontal scaling by shard key. Its aggregation framework handles moderate analytics, but large scans over many documents are better done elsewhere.

## 4. HTAP and the in-between

Some systems blur the line (HTAP: hybrid transactional/analytical): column indexes on a row store, or replicated column copies for analytics. They remove the pipeline but inherit compromises. Evaluate them against your actual scan sizes and write rates.

## 5. Architectures that connect the two

```
 apps ──► OLTP primary (PostgreSQL/MongoDB) ──► ┌ change data capture (WAL / oplog) ┐
                                               │  Debezium → Kafka                 │
                                               └──────────────┬───────────────────┘
                                                              ▼
                                     analytical store (ClickHouse) ──► dashboards, reports
```

| Approach | How | Freshness | Notes |
| --- | --- | --- | --- |
| Batch export | nightly `COPY`/dump → load | hours | simplest; replace partitions atomically ([3.4 §6](../03-clickhouse/04-consistency-and-mutations.md#6-patterns-that-replace-a-transaction)) |
| Logical replication / CDC | stream WAL/oplog changes | seconds | must handle deletes/updates (ReplacingMergeTree, tombstones) |
| Dual write (app writes both) | two inserts | immediate | risks divergence; prefer the **outbox pattern** ([1.4 §7](../01-fundamentals/04-transactions.md#7-beyond-one-node)) |
| Direct federation | ClickHouse `postgresql()` table function, `Foreign data wrappers` | per query | fine for small dimension tables, not big scans |
| Read replica for reports | long queries on a standby | seconds | watch replay conflicts ([2.5](../02-postgres/05-architecture.md)) |

Design rules:

1. **Single source of truth**: the OLTP store owns the data; the analytical store is derived and rebuildable.
2. **Make the pipeline idempotent**: replays must not double-count ([3.4 §5](../03-clickhouse/04-consistency-and-mutations.md#5-idempotent-ingestion-and-deduplication)).
3. **Model the analytical side for queries**: wide denormalised tables ordered by common filters.
4. **Treat deletes explicitly** (GDPR, soft deletes).
5. **Monitor lag** end to end.

## 6. Capacity planning cheat sheet

| Question | OLTP | OLAP |
| --- | --- | --- |
| What limits throughput? | random I/O, locks, connections | scan bandwidth, CPU, memory for aggregation |
| Cache sizing | hot working set | hot columns and the mark/index cache |
| Scale-up path | bigger instance, read replicas, partition, then shard | more cores, then shards; compress better; pre-aggregate |
| Biggest risk | contention, long transactions | uncontrolled queries, too many parts |

## Exercises

1. Classify these workloads and justify: user login, order checkout, monthly revenue report, fraud scoring on a live stream, product search, audit log.
2. You must show "orders per minute for the last 24 hours" on a dashboard refreshed every 5 seconds for 200 users. Compare computing it live on PostgreSQL, from a ClickHouse table fed by CDC, and from a materialized view; discuss freshness, cost and complexity.
3. Draw a CDC pipeline and list five failure modes (broker down, duplicate delivery, schema change, late events, deletes) with a mitigation each.
4. Measure: run a heavy aggregate on the PostgreSQL seed while a point-lookup benchmark runs. How does the lookup p99 change?

→ Answers: [appendix/solutions.md](../appendix/solutions.md#51-oltp-vs-olap).

Next: [5.2 Replication and consistency](02-replication-and-consistency.md).

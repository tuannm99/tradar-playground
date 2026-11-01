# 5.5 Choosing a database and combining them

> **Level:** architecture · **Prerequisites:** all of Parts 1–4 helps · **After this chapter you can:** compare PostgreSQL, ClickHouse and MongoDB on the dimensions that matter, avoid the usual selection mistakes, and sketch a polyglot architecture with clear ownership of data.

## 1. Start from requirements, not products

Write the answers first:

1. **Access patterns**: the 10 queries, their frequency, latency targets, and result sizes.
2. **Data shape and volume**: rows/documents now and in 3 years; growth rate; average record size.
3. **Consistency needs**: which invariants must never break; acceptable staleness.
4. **Write pattern**: rate, batch vs single, updates vs appends, deletes.
5. **Availability and durability**: RPO/RTO, regions.
6. **Team and operations**: skills, managed service availability, cost, compliance.

Then eliminate options that cannot meet a hard requirement. Most choices resolve at this step.

## 2. Comparison

| Dimension | PostgreSQL | ClickHouse | MongoDB |
| --- | --- | --- | --- |
| Model | relational tables, rich types, JSONB | columnar tables | JSON-like documents |
| Sweet spot | OLTP, relational integrity, mixed workloads | analytics over huge append-only data | aggregate-oriented operational data, flexible schema |
| Storage | heap + B-tree indexes, MVCC | immutable sorted column parts, sparse index | WiredTiger B-trees, MVCC |
| Point read by key | excellent (µs–ms) | poor-to-OK (reads a granule) | excellent |
| Large scans/aggregates | OK on moderate data; parallel query | **excellent** (GB/s per node) | OK for moderate; weaker at scale |
| Updates/deletes | normal | expensive (mutations); use append + engines | normal |
| Transactions | full ACID, serializable | none in the OLTP sense | multi-document, snapshot |
| Joins | strong planner, many algorithms | hash joins, right side in memory | `$lookup`, limited |
| Schema | strict, evolves with migrations | strict, sort key immutable | flexible, validation optional |
| Scale-out writes | vertical + replicas; sharding via Citus/app | shards + replicas natively | **native sharding** |
| Scale-out reads | replicas | replicas + shards | replicas + shards |
| HA | external tools (Patroni) or managed | Keeper-based replication | built-in replica sets & elections |
| Weak spots | horizontal write scale, analytic scans at billions | updates, point lookups, transactions | multi-entity integrity, large analytics |
| Index types | B-tree, GIN, GiST, BRIN, hash | sparse primary, skip indexes, projections | B-tree, multikey, text, geo, hashed, TTL |
| Typical tuning lever | indexes, statistics, vacuum, pooling | sort key, batch size, types | indexes, document shape, concerns |

## 3. Decision heuristics

- **Default to PostgreSQL** for a new product's system of record unless a requirement rules it out: it is general, well understood, and its JSONB handles many "document" needs. Optimise last.
- **Add ClickHouse** when analytical queries over large volumes slow down the OLTP system or no longer fit it: event logs, metrics, product analytics, observability, ad-tech, billing aggregates.
- **Choose MongoDB** when the data is naturally aggregate documents with varied shapes, access is mostly by key or a few indexed fields, you want built-in horizontal scaling and replica-set HA, and cross-document integrity is limited.
- **Use none of them** for: graph traversal (graph DB), full-text relevance (search engine), vector similarity at scale (vector index/engine, or `pgvector` for moderate), pure key–value at extreme QPS (Redis/Dynamo-style), blob storage (object store).
- **Prefer fewer systems.** Each additional datastore multiplies operations, monitoring, security, backup and failure modes. Add one only for a measured need.

## 4. Common selection mistakes

| Mistake | Consequence |
| --- | --- |
| Choosing for scale you don't have | operational burden, slower development |
| "Schemaless" as a benefit | the schema still exists — in application code, undocumented |
| Using an OLAP store for OLTP updates (or vice versa) | constant fighting the engine |
| Ignoring the team's skills | production incidents you can't debug |
| One store for everything with no isolation | analytics hurting checkout |
| Premature polyglot persistence | data sync problems before product-market fit |
| Benchmarks on unrealistic data | wrong conclusion |
| Not testing failover/restore | surprise on the worst day |

## 5. Polyglot architectures (when justified)

### A. System of record + analytics

```
App ──► PostgreSQL ──CDC──► Kafka ──► ClickHouse ──► BI / dashboards
```

PostgreSQL owns truth; ClickHouse is rebuildable. See [5.1](01-oltp-vs-olap.md).

### B. Document store for catalog + relational for orders

MongoDB holds flexible product documents served at high read rates; PostgreSQL holds orders, payments and inventory with integrity. Link by ids and keep a small, explicit contract between them.

### C. Cache in front

Redis or in-process caches for hot read paths ([5.4](04-caching-and-pooling.md)).

### D. Event backbone

Events in Kafka feed several derived stores (search index, analytics, cache invalidation). The **outbox pattern** ([1.4 §7](../01-fundamentals/04-transactions.md#7-beyond-one-node)) ensures the event matches the committed state.

### Ownership rules

1. Each fact has **one owner** (system of record). Others hold derived copies.
2. Derived stores are **rebuildable** from the owner or the event log.
3. Data contracts (schemas) are explicit and versioned.
4. Define **freshness SLOs** for every derived copy and monitor lag.
5. Secure and back up every store; the weakest one defines your posture.

## 6. A worked example

*Requirement*: an online marketplace — product catalogue (varied attributes), orders & payments (money), activity events (billions/month), "sales by category last 30 days" dashboards, seller search.

| Data | Store | Reason |
| --- | --- | --- |
| Orders, payments, inventory | PostgreSQL | integrity, transactions, constraints |
| Product catalogue | PostgreSQL (JSONB) first; MongoDB only if attribute variety/read scale demands | fewer systems; graduate when measured |
| Activity events | ClickHouse | append-only, large, scan-heavy |
| Sales dashboards | ClickHouse MV fed by CDC from PostgreSQL | pre-aggregated, fast |
| Search | search engine fed from the catalogue | relevance ranking |
| Sessions, hot product pages | Redis / app cache | latency |

Begin with PostgreSQL alone (plus its JSONB, partitioning, read replicas). Add ClickHouse when the dashboards or event volume justify it, via the pipeline above. Revisit MongoDB if catalogue read throughput or schema variability outgrows PostgreSQL.

## Exercises

1. For each system, pick a store and justify: (a) a chat application with message history, (b) IoT telemetry for 1 M devices at 1 Hz, (c) a bank ledger, (d) a CMS with page documents and revisions, (e) real-time ad auction logs analysed hourly.
2. Take your last project's top 10 queries and score each candidate store 1–5 on read pattern fit, write pattern fit, consistency fit and operations. Which wins and by what margin?
3. Write the data contract and freshness SLO for a ClickHouse table fed from PostgreSQL by CDC. What do you alert on?
4. List what you would measure in a two-week proof of concept to decide between PostgreSQL JSONB and MongoDB for a catalogue.

→ Answers: [appendix/solutions.md](../appendix/solutions.md#55-choosing-a-database).

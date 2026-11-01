# 5.4 Caching, connection pooling and protecting the database

> **Level:** architecture · **Prerequisites:** [1.1 §3](../01-fundamentals/01-storage.md#3-the-buffer-cache), [1.6](../01-fundamentals/06-measuring.md) · **After this chapter you can:** size a connection pool with Little's law, choose a caching strategy and invalidation scheme, and add back-pressure so overload degrades gracefully.

## 1. Where caches live

| Layer | Example | Granularity | Notes |
| --- | --- | --- | --- |
| CPU caches | L1/L2/L3 | cache line | cache-friendly layouts (columns, narrow rows) |
| Database buffer cache | `shared_buffers`, WiredTiger cache, mark cache | page / granule | automatic; size = RAM decision |
| OS page cache | Linux | page | double caching with the DB cache |
| Query result cache | ClickHouse `use_query_cache`, MySQL (removed) | whole result | invalidation by TTL / table change |
| Application cache | Redis, Memcached, in-process | object / query result | **you** own correctness |
| HTTP/CDN cache | CDN, reverse proxy | response | for public/semi-public data |
| Materialised aggregates | MVs, summary tables | rolled-up rows | a cache that is also queryable |

Every cache trades **freshness for latency/cost**. Decide the acceptable staleness first.

## 2. Application caching patterns

| Pattern | Flow | Pros | Cons |
| --- | --- | --- | --- |
| **Cache-aside (lazy)** | read: check cache → miss → DB → fill; write: update DB, invalidate/update cache | simple; only caches what is read | stale window; stampedes |
| **Read-through** | cache library loads on miss | centralised logic | cache coupling |
| **Write-through** | write goes to cache and DB together | cache always warm | write latency; failure atomicity |
| **Write-behind (write-back)** | write to cache, flush to DB asynchronously | fast writes | data loss risk, ordering |
| **Refresh-ahead** | refresh popular keys before expiry | no miss latency | complexity |

### Invalidation

"There are only two hard things…". Practical options:

1. **TTL** only — simplest; accept staleness up to TTL.
2. **Explicit invalidation on write** — delete (not update) the key after the DB commit. Race: a reader can repopulate with old data between the write and the delete → use short TTLs or versioned keys.
3. **Versioned keys**: embed a version/updated-at in the key; bump on write.
4. **Event-driven** invalidation from CDC.

### Cache stampede (thundering herd)

When a hot key expires, thousands of requests hit the database at once. Mitigations: **request coalescing / single-flight** (one loader, others wait), **jittered TTLs**, **stale-while-revalidate**, a lock around regeneration, pre-warming.

### Negative caching

Cache "not found" for a short time to stop repeated lookups of missing keys (and as a guard against enumeration attacks).

## 3. Hit ratio arithmetic

Average latency = hit_ratio × L_cache + (1 − hit_ratio) × L_db. With L_cache = 1 ms, L_db = 20 ms: 90% hits → 2.9 ms; 99% → 1.2 ms. The database sees `(1 − hit) × traffic`, so going from 90% to 99% cuts DB load **10×**. Cache the hot, *reused* data; caching data read once adds cost for nothing.

## 4. Connection pooling

Why: connections cost memory and (in PostgreSQL) a process each; too many active connections cause contention. A smaller number of connections working steadily is often **faster**.

### Sizing with Little's law

Connections busy on average = arrival rate × time each is held. At 400 requests/s and 25 ms hold time: 400 × 0.025 = **10 busy connections** on average. If latency degrades to 100 ms, demand becomes 40, and a pool capped at 20 starts queueing — latency rises further. This is the overload spiral from [1.6 §3](../01-fundamentals/06-measuring.md).

Guidelines:

- Pool size ≈ (a small multiple of) the database's useful parallelism (CPU cores, I/O queue depth) — often 10–50 per database server, **not** thousands.
- Size the pool **per application instance** knowing `instances × pool_size ≤ DB limit`.
- Hold connections **briefly**: acquire late, release early; never across user think-time or external calls.
- Use an **external pooler** (PgBouncer, pgcat, ProxySQL, mongos) when many app instances exist; use transaction pooling where session state isn't needed.
- Set **timeouts**: acquire timeout (fail fast), idle timeout, max lifetime (to rebalance after failover).
- MongoDB drivers: one `MongoClient` per process; the pool is per server. ClickHouse: HTTP keep-alive; limit concurrent queries per user.

## 5. Back-pressure and load shedding

An overloaded database must be able to say no:

1. **Timeouts everywhere** (client, pool, `statement_timeout`, `maxTimeMS`, `max_execution_time`).
2. **Bounded queues** in front of the pool; reject when full (HTTP 503) instead of queueing unboundedly.
3. **Rate limits per tenant/user**; **resource profiles** (ClickHouse quotas, PostgreSQL roles with timeouts).
4. **Circuit breakers**: stop calling a failing dependency for a while.
5. **Retries with exponential backoff and jitter**, and a retry budget — naive retries multiply load during an incident.
6. **Priority**: keep write paths and checkout alive; degrade reports first.
7. **Bulkheads**: separate pools for OLTP and reports so a slow report can't starve checkout.

## 6. Prepared statements and plan reuse

Prepared statements save parse/plan time for hot queries, but generic plans can be worse for skewed data ([2.3 §6](../02-postgres/03-planner-and-explain.md#6-prepared-statements-and-plan-caching)). Behind transaction-mode poolers, protocol-level prepared statements need pooler support.

## 7. Don't cache what you can fix

Before adding a cache, check: missing index? N+1? a query returning too many columns? A cache hides a slow query, adds a consistency problem, and creates a new failure mode (cache outage → database stampede). Fix the query; then cache what is still hot.

## Exercises

1. A product page triggers 12 queries; p95 is 300 ms. Without caching, propose three optimisations; then add a cache-aside layer and list the invalidation rules for price changes.
2. Compute the pool size needed for 1 200 req/s at 15 ms hold time with a 70% headroom target, and the effect of a latency spike to 60 ms.
3. Reproduce a stampede in a script: 200 concurrent requests for a key whose cache just expired. Then add single-flight and compare database query counts.
4. Design retry policy and circuit breaker settings for a service that talks to PostgreSQL and a read-only ClickHouse.

→ Answers: [appendix/solutions.md](../appendix/solutions.md#54-caching-and-pooling).

Next: [5.5 Choosing a database](05-choosing-a-database.md).

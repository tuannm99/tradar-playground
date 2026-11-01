# Exercise solutions and hints

Short answers: enough to check your reasoning, not to replace running the experiment. Where an exercise says "measure", the expected *shape* of the result is given; exact numbers depend on your machine.

## 1.1 Storage

1. 10 M × 200 B = 2 GB ≈ 250 000 pages of 8 KB. Sequential scan at 1 GB/s ≈ **2 s**. 10 M random page reads at 100 µs = **1 000 s** (≈ 500× slower) — which is why scans beat index lookups when you need a large fraction of the table.
2. `avg(amount)` in a column store reads only the `amount` column (compressed, contiguous). A row-store scan reads every column's bytes to reach `amount`. A single-row `select *` in ClickHouse reads at least one granule (8 192 rows) of **every** requested column from separate files — the row store reads one page.
3. Group commit (batch concurrent commits per fsync), larger transactions (batch several inserts per commit), faster storage / battery-backed write cache, or relax durability (`synchronous_commit = off`, `w: 1`) and accept losing the last moments on a crash. Each trades durability or latency for throughput.
4. Random keys insert into random leaf pages: when the index is larger than memory, most inserts need a disk read to bring the page in, page splits happen everywhere, and the cache fills with cold pages. Use a time-ordered id (UUIDv7, ULID, Snowflake, ObjectId) or a sequence plus a separate random public id.

## 1.2 Indexing

1. `(status, createdAt)` serves equality + range and a small sort afterwards; `(status, amount desc, createdAt)` avoids the sort. With a one-day range (few matching rows) the first is better (sort a few rows, scan few keys); with a wide range and `limit 20` the second wins. Prove with `EXPLAIN` keys examined ([4.2](../04-mongodb/02-indexes.md#esr-is-a-guideline)).
2. `a = 1` yes; `b = 2` no (not a prefix); `a = 1 and b > 5` yes (both used); `a > 1 and b = 2` only `a` narrows the scan, `b` is filtered inside the range; `a = 1 order by b` yes (rows come out ordered by `b`).
3. B-tree works but is large (tens of GB) and costly to maintain on appends. BRIN is kilobytes and effective because physical order matches time. Partitioning by day lets you drop old days and prunes without any index. Best: partition by day (+ BRIN or small B-tree within).
4. Capture usage statistics for a representative period (`pg_stat_user_indexes.idx_scan`), exclude indexes backing constraints, check duplicates/prefix-redundant indexes, "hide"/disable before dropping where supported, drop one at a time while monitoring p95, keep DDL scripts to recreate.

## 1.3 Query execution

1. A nested loop probing a `Seq Scan` 50 000 times: each probe scans the whole inner table. Fixes: index the inner join key; or let the planner use a hash join (fix statistics, increase `work_mem`, `analyze`).
2. Correlated columns (planner multiplies selectivities). Fix with extended statistics `create statistics ... (dependencies)`.
3. With an index in the sort order, the engine reads the first 10 entries and stops. Without, it must read and sort all candidate rows (top-N heap sort still touches everything).
4. Hash join better: large unsorted inputs, equi-join, no useful index; also when the outer side is large. Nested loop better: tiny outer side with an indexed inner. Hash is worse when the build side doesn't fit memory (spills) or the join is non-equi.

## 1.4 Transactions

1. T1 reads 100, T2 reads 100, T1 writes 101, T2 writes 101 → one increment lost. Fixes: atomic `set x = x + 1` (no extra cost); `select ... for update` (second waits; serialises hot rows); `REPEATABLE READ`/`SERIALIZABLE` + retry (aborts under contention); optimistic version column (retry on 0 rows updated).
2. Write skew/phantom: two buyers each see 99. A **unique constraint on seat number** is the best (structural). A guard row with `FOR UPDATE` serialises purchases. `SERIALIZABLE` + retry also works. A plain lock on counted rows doesn't help (the rows that would be inserted don't exist yet).
3. Retrying a single statement inside a transaction that has already aborted does nothing (the transaction is dead after a serialization error). Retry boundary = the **whole transaction**, including re-reading data.
4. Failure modes: payment succeeds but commit fails (charged, no order); commit succeeds but payment fails; timeouts leave unknown state; the transaction holds locks during the HTTP call. Outbox: in one local transaction write the order and a `payment_requested` event; a relay publishes it; the payment service acts idempotently with the order id as key; results come back as events that update the order.

## 1.5 Data modelling

1. Blog: show post with comments → documents (embed or subset); user's latest 20 posts → both fine with an index; top 10 tags this week → relational/aggregation, documents need a precomputed counter; delete a user → relational (cascades, integrity) is simpler.
2. Random UUIDv4 keys spread inserts across the whole B-tree: with the index ≫ RAM, each insert is a cache miss and page splits increase write amplification. Fixes: time-ordered ids (UUIDv7), bigserial + public random id, or partitioning by time with local indexes.
3. Binary floating point cannot represent most decimals exactly; sums drift. Store `numeric(12,2)` / integer cents.
4. (a) Query live — slow at scale; (b) a summary table or materialized view updated incrementally — fast, adds write cost and staleness handling; (c) ClickHouse fed by CDC — fastest ad-hoc, extra system. Pick by freshness and volume.

## 1.6 Measuring

1. 400 × 0.025 = **10** busy connections. At 100 ms: 40 needed > 20 pool → the queue grows without bound; latency rises; more connections are held — an overload spiral. Fix: shed load, bound queues, speed up queries.
2. Cold vs warm cache? Same data size and distribution as production? Did the plan really change (is the index used), and what is the write-side cost? Also p95/p99, not one run.
3. Use the template in [1.6 §7](../01-fundamentals/06-measuring.md#7-a-reusable-case-file-template).

## 2.1 PostgreSQL storage and MVCC

1. After rollback `ctid` and `xmin` are unchanged: the new tuple version is never visible (its `xmin` aborted), the old one's `xmax` is ignored.
2. With `fillfactor = 70`, `n_tup_hot_upd` is a large fraction (≈42% in the test); with 100 it is ≈0, and the heap grows faster. After `VACUUM`, sizes don't shrink in either case.
3. Open a `REPEATABLE READ` transaction, run a query, then update and `vacuum verbose` from another session; find the blocker with `backend_xmin` in `pg_stat_activity` ([2.1 §4](../02-postgres/01-storage-and-mvcc.md#what-blocks-vacuum-from-removing-dead-tuples)).
4. MVCC visibility must be checked per row, so a count needs every tuple (or an index-only scan with a fresh visibility map). Alternatives: estimate from `reltuples`, a maintained counter table, a materialized view.
5. Move the frequently updated narrow column to its own table (or keep the blob in a separate table), so updates rewrite small tuples and stay HOT.

## 2.2 PostgreSQL indexes

1. `create index on play_orders (customer_id, created_at desc) where status = 'paid'` (or include status in the key): equality column first, then the sort column; the plan shows no `Sort`, with `Limit` on top of an index scan. Dropping it reverts to a bitmap scan + sort.
2. A query without `status = 'new'` has no guarantee that its rows are in the partial index; the planner cannot use it (`explain` shows a different index or a seq scan).
3. BRIN is KBs vs tens of MBs; the plan is a bitmap heap scan reading block ranges. After inserting random timestamps, `correlation` drops and BRIN becomes much less selective.
4. Drop the index on `play_child(parent_id)`: a parent delete scans all children (≈26 ms vs 0.8 ms in the test).
5. `idx_scan = 0` is per-statistics-reset and per-node: indexes used only by monthly jobs, by replicas, or enforcing uniqueness/foreign keys would show zero. Check reset time (`stats_reset`), replicas, and constraints first.

## 2.3 PostgreSQL planner and EXPLAIN

1. Restrict customers to ~20 ids (`where c.id < 21`): few outer rows → `Nested Loop` with an index on `play_orders(customer_id)`; the hash join is no longer worthwhile.
2. Define `country`/`city` columns where city determines country; without statistics the planner multiplies selectivities and picks a wrong join order; `create statistics (dependencies)` fixes the estimate.
3. First run: `shared read` > 0 (disk/OS cache); later runs: `shared hit` only. Averages over 100 runs hide the cold run that the first user pays.
4. Bitmap heap scans with lossy bitmaps or BRIN: the index returns whole pages/ranges, then the executor rechecks each row; non-zero `Rows Removed by Index Recheck` is the extra work.
5. A skewed column (`status` with a rare value) with a prepared statement: after five executions PostgreSQL may keep a generic plan assuming an average selectivity; compare against `force_custom_plan`.

## 2.4 PostgreSQL transactions and locking

1. With `for update`, the second session shows `wait_event = transactionid` / `Lock` in `pg_stat_activity`, then proceeds after commit with the new value.
2. Retry only on SQLSTATE `40001`, `40P01` (and optionally `55P03` lock timeout); loop the entire transaction; cap at ~5 attempts; sleep `random(0, 2^n × 10 ms)`; propagate other errors.
3. `FOR UPDATE SKIP LOCKED` lets each worker take a different job; without it, workers serialise on the first row, or both read the same job if you don't lock.
4. At `READ COMMITTED` the second count sees the inserted row. At `REPEATABLE READ` PostgreSQL uses one snapshot for the whole transaction, so no phantom — stronger than the SQL standard requires.
5. `alter table` waits for `ACCESS EXCLUSIVE`; all later queries queue behind it. Use the lock-wait query in [2.6](../02-postgres/06-tuning-playbook.md#lock-pile-up); the root blocker is the old transaction.

## 2.5 PostgreSQL architecture

1. Critical path: WAL record written + fsynced on the primary → shipped to the standby → written, fsynced and **replayed** on the standby (`remote_apply`) → acknowledgement back. Floor ≈ local fsync + one network round trip + remote fsync + apply.
2. Long standby query conflicting with replay (`max_standby_streaming_delay`), slow standby disk/CPU, network, a huge transaction. Tell apart by `write_lag`/`flush_lag`/`replay_lag` and standby logs.
3. Two data nodes + witness across three failure domains with a consensus store (etcd) holding the leader lease. If the primary is partitioned from the others it cannot renew the lease, steps down (fencing), and the majority side promotes the standby.
4. Pool of ~2–4 × cores ≈ 32–64 server connections; `max_connections` ≈ pool + admin; `work_mem` ≈ 64 GB × 25% / (pool × nodes per query ≈ 4) ≈ 64–128 MB at most; start lower. 800 app workers connect to PgBouncer, not to PostgreSQL.
5. By month: good for time-range queries and retention; hash(customer_id): good for customer-level queries and even size, poor for time ranges and retention. Pick by dominant queries.

## 2.6 PostgreSQL tuning playbook

1. Use `pg_stat_statements` ranked by `total_exec_time`; fix the one with the most total time first (it is usually a frequent medium-cost query, not the single slowest).
2. Batched delete (10 000 rows, repeat): WAL and dead tuples spread over time, autovacuum keeps pace; one giant delete creates a single huge WAL burst and bloat.
3. Idle transaction holds a lock; `alter table` waits; everything else queues. The root blocker is the idle transaction's pid in `pg_blocking_pids`.
4. `alter table play_orders set (autovacuum_vacuum_scale_factor = 0.01)`; update ≥1% rows, watch `n_dead_tup` fall and `last_autovacuum` update.

## 3.1 ClickHouse storage

1. 1 000 single-row inserts → ~1 000 parts (until merges reduce them); one `INSERT ... SELECT` → one part. `system.part_log` shows `NewPart` events and merge activity.
2. `Delta` + `ZSTD` on sorted timestamps typically gives several times better ratio than plain LZ4; measure with `data_compressed_bytes`.
3. `PARTITION BY user_id` creates a part per user per insert: huge `parts` counts, slow inserts (many directories), slow queries, merge backlog.
4. A TTL expression must depend on a column (`ts + interval 30 day`). Tiering: `TTL ts + interval 30 day TO VOLUME 'cold'` with a storage policy that defines the volume.
5. Count `MergeParts` events in `system.part_log`; the largest `rows` of a merged part is the biggest merge.

## 3.2 ClickHouse indexes and pruning

1. `(country, event, user_id, ts)`: `user_id = 42` prunes poorly (third key column); `country='VN' and event='buy'` prunes extremely well; `ts > ...` prunes only via partition/min-max, not the primary key.
2. A Bloom filter on `user_id` for a table sorted by `ts` won't help much: one user's rows are spread across all blocks, so every block "may contain" the value.
3. A projection sorted by `(country, ts)` serves country-filtered queries; its cost shows in `system.projection_parts` (extra bytes) and insert time.
4. Lightweight delete writes a mask into the parts; projections hold their own copies that would not see the mask. Proceed by dropping the projection, or setting `lightweight_mutation_projection_mode` (e.g. `rebuild`/`drop` per docs), or using a mutation.
5. Add a projection or a second table sorted by `ts` (with a materialized view feeding it), make `ts` part of the key prefix, or partition by time and rely on partition pruning.

## 3.3 ClickHouse query execution

1. `group by country`: tiny hash tables per stream; `group by user_id`: large per-thread hash tables, two-level aggregation, much more memory and merge work.
2. With a low `max_memory_usage` the high-cardinality group-by fails with `MEMORY_LIMIT_EXCEEDED`; with `max_bytes_before_external_group_by` set it completes using disk, slower.
3. `SelectedParts`/`SelectedMarks` in `ProfileEvents` are far lower for the table sorted by `user_id`.
4. Select only needed columns and add `limit`, order by the sort key; compare `read_bytes`.
5. `uniq` uses a small sketch (less memory, ≈1–2% error); `uniqExact` keeps all values (exact, high memory).

## 3.4 ClickHouse consistency and mutations

1. Plain `MergeTree` ingests both copies (2 000 rows). A table with a deduplication window ignores the exact duplicate block (1 000 rows).
2. `FINAL` is simplest; `argMax` often scans less per group; `limit 1 by id` after `order by version desc` sorts everything. Compare `read_rows` and time.
3. After `KILL MUTATION`, the table has some parts rewritten and some not — queries see a mix of old/new values (mutations are not transactional).
4. After deleting source rows, the rollup still contains their contribution (the view fired only on insert). Correct by recomputing the affected partitions (`REPLACE PARTITION` from a recomputed staging table) or using signed rows.
5. `create table staging as target`; load into staging; `alter table target replace partition '202610' from staging`.

## 3.5 ClickHouse architecture

1. (a) Through `Distributed`: client → initiator → async buffered send → shard; loss possible if the initiator dies before forwarding (data on its local disk queue). (b) Direct to `events_local`: client chooses the shard; with `internal_replication = true` the replicated table copies to replicas through Keeper. `internal_replication` prevents double writes.
2. With `rand()` sharding, every shard sends distinct-id state/sets to the initiator (a lot of data); with `cityHash64(user_id)` each shard owns distinct users, so partial counts add up without overlap.
3. Three Keeper nodes, one per AZ; one AZ failing leaves a majority (2 of 3) and Keeper stays available; two AZ failures lose quorum → replicated tables go read-only.
4. Plain `JOIN` ships the 500 MB table to each shard's evaluation (wrongly if not broadcast); `GLOBAL JOIN` broadcasts it per query (cost per query); a replicated dimension table on every node and a **dictionary** are cheapest per query.

## 3.6 ClickHouse tuning playbook

1. Insert in a loop of tiny inserts; observe `Too many parts`/delays at the configured thresholds; switch to `async_insert` or larger batches and watch active parts fall.
2. Look at top `read_bytes` queries; usual fixes: key alignment, projections/MVs, narrower selects.
3. Rewrite the dashboard to filter by sort key / partition so it stays under the limit, or use a pre-aggregated table.
4. `select partition, count() from system.parts where active group by partition having count() > 300`.

## 4.1 MongoDB storage

1. Field-name length changes `size` (uncompressed) a lot and `storageSize` less (compression recovers some). Shorter names help at huge scale.
2. A monotonic key appends to the right edge (cache-friendly); a random key causes random page access and larger index growth once the index exceeds the cache.
3. Oplog window = oplog size ÷ oplog generation rate; e.g. 46 GB at 5 MB/s ≈ 2.5 hours.
4. 10 000 line items: don't embed (16 MB and array growth risk). Reference in a separate collection or *bucket* in groups (e.g. 500 per document) keyed by order id and bucket number.

## 4.2 MongoDB indexes

1. `{customerId: 1, status: 1, createdAt: -1}`: equality on customer and status, then the sort/limit; `explain` shows `LIMIT > FETCH > IXSCAN` with ~21 keys.
2. For narrow ranges the E-R index wins; for wide ranges with a small `limit`, the ESR index wins; the break-even depends on selectivity of the range — find it by measuring.
3. A partial index on `status:"new"` is used by queries that include `status:"new"` in the filter; a query without that condition cannot use it.
4. Two array fields in one compound index fail with "cannot index parallel arrays".
5. Run `$indexStats`, filter `accesses.ops == 0`, then `collMod` to hide those indexes for a week before dropping.

## 4.3 MongoDB queries and aggregation

1. `$match` with the most selective indexed condition first; use `$sort`+`$limit` together; project early; check `explain` of the aggregation (`explain("executionStats").aggregate(...)`).
2. Without an index on the foreign field each lookup scans the other collection (orders of magnitude slower); with an index it is one probe per document.
3. Bucketing reduces document and index-entry counts (fewer, larger documents), usually improving range queries and storage.
4. Use `db.setProfilingLevel(1, {slowms: 50})` and query `system.profile` sorted by `millis`.
5. `customerId: "42"` (string) against integer ids matches nothing and the index is used for an empty range — a silent logic bug, not a slow query; mixed types in one field are the real fix.

## 4.4 MongoDB transactions and concerns

1. On `TransientTransactionError`, abort and re-run the whole callback with backoff and a cap; on `UnknownTransactionCommitResult`, retry `commitTransaction` only. `withTransaction` implements this.
2. `findOneAndUpdate({status: "ready"}, {$set: {status: "running"}}, {sort: {_id: 1}})` is atomic per document; an index on `{status: 1, _id: 1}` makes it efficient.
3. With `w: 1`, a write acknowledged by the old primary can be rolled back after failover; with `w: "majority"` it cannot.
4. Read from a secondary right after a primary write and see the old value; use a causally consistent session (with `majority` read concern) to fix.
5. (a) `updateOne({_id, balance: {$gte: amt}}, {$inc: {balance: -amt}})` — one atomic conditional update, cheap. (b) A transaction can do the same plus other documents' updates, at higher cost.

## 4.5 MongoDB architecture

1. Heartbeats time out (10 s) → secondary elections → new primary; the old primary rejoins, finds unreplicated `w: 1` writes and rolls them back; `w: "majority"` writes were already on a majority and survive.
2. (a) `{tenantId: 1, _id: 1}` or hashed `{tenantId, _id}` (large tenants need zoning/hashing); (b) `{deviceId: 1, ts: 1}` hashed on device or compound with time bucketing; (c) hashed `_id` or `productId`.
3. 12 h × 3 600 s × 5 MB/s = **216 GB** of oplog plus margin (e.g. 250–300 GB).
4. Scatter-gather latency is dominated by the slowest shard and coordination, so adding shards doesn't speed it up — use the shard key, or restructure the query/model.
5. Upgrade secondaries one at a time (stop, upgrade, restart, wait for catch-up), then `rs.stepDown()` the primary, upgrade it last; clients with `retryWrites` ride through the election.

## 4.6 MongoDB tuning playbook

1. Collect the profiler documents in a collection, then project `command`, `keysExamined`, `docsExamined`, `millis`, `planSummary`.
2. Embed the few fields used on read (extended reference) and keep the rest referenced; reads avoid the lookup, writes must update the copies.
3. 50 concurrent `$inc` on one document serialise on write conflicts; N counter documents (shard by `hash % N`) reduce contention; sum on read.
4. A numeric field queried with a string value returns no rows; normalise types at write time and validate with a JSON schema validator.

## 5.1 OLTP vs OLAP

1. Login: OLTP key lookup. Checkout: OLTP transaction. Monthly revenue: OLAP. Fraud scoring: stream + OLTP lookups, often with a feature store. Product search: search engine. Audit log: append-only, write once (OLAP-style storage or object store).
2. Live on PostgreSQL works at small scale but loads the primary every 5 s × 200 users; from ClickHouse fed by CDC it's cheap but adds a pipeline; a materialized view/summary table gives the best cost with bounded staleness.
3. Failure modes: broker down (buffer, retry, replay from offsets), duplicates (idempotent sink), schema change (versioned contracts), late events (watermarks/reprocess partitions), deletes (tombstones/ReplacingMergeTree).
4. Expect lookup p99 to rise as the scan competes for cache, I/O and CPU; isolate analytics on a replica.

## 5.2 Replication and consistency

1. Read the profile from the primary for a short time after a write (or always for the writer); pass a replication position and wait; use sticky sessions; or accept eventual consistency in the UI by updating it locally.
2. With a single sync standby and `synchronous_commit = on`, commits stall while the standby is down; `ANY 1 (s1, s2)` keeps committing as long as one is up.
3. `w: 1`: RPO = replication lag (usually ms–s), RTO = election time (~10–30 s). `w: "majority"`: RPO = 0 for acknowledged writes, same RTO.
4. In a quorum system, with `R + W > N` every read quorum overlaps every write quorum, so a read sees the latest acknowledged write; failures: sloppy quorums/hinted handoff and concurrent writes needing conflict resolution.

## 5.3 Partitioning and sharding

1. PostgreSQL: partition by `ts` (month) with indexes `(tenant_id, user_id, ts)`; ClickHouse: `PARTITION BY toYYYYMM(ts)` `ORDER BY (tenant_id, user_id, ts)`; MongoDB: shard key `{tenant_id: 1, user_id: 1}` or hashed, TTL index for retention.
2. With `mod`, going 8 → 12 moves ≈ 2/3 of keys. With consistent hashing, ≈ 1 − 8/12 = 1/3 of keys move.
3. Scatter-gather partial top-100 from each of the 16 shards, merge, sort; latency = slowest shard + merge. Restructure: shard by `customer_id`, or maintain a pre-aggregated spend table.
4. A monotonic key (timestamp) sends all inserts to one shard (hot) so throughput = one shard; fix with hashing or a compound key with a hash prefix.

## 5.4 Caching and pooling

1. Collapse N+1 queries into joins, add missing indexes, return fewer columns; then cache-aside the product document with TTL and delete the key after price changes (versioned keys to avoid stale repopulation).
2. 1 200 × 0.015 = 18 busy; with 70% headroom ≈ 26 connections. At 60 ms: 72 needed → a pool of 26 will queue; shed load.
3. Without single-flight, ~200 queries hit the DB; with it, 1.
4. Retry only idempotent operations, 2–3 attempts with exponential backoff + jitter, a retry budget (e.g. ≤10% of traffic), circuit breaker opening on error rate / timeouts for 5–30 s.

## 5.5 Choosing a database

1. (a) PostgreSQL (or Cassandra-class if very large); (b) ClickHouse or a time-series store; (c) PostgreSQL; (d) PostgreSQL JSONB or MongoDB; (e) ClickHouse.
2. Score honestly by access pattern; the answer usually favours the store you already operate unless a hard requirement fails.
3. Contract: schema, keys, update semantics (insert/update/delete handling); SLO: e.g. 99% of changes visible within 30 s; alert on consumer lag, row-count drift versus source, and failed batches.
4. Measure p50/p99 latency of key queries, storage size, write throughput, index sizes, operational effort, behaviour under failover.

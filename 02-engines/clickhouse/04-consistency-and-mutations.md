# 2b.4 ClickHouse: consistency, mutations and what replaces transactions

> **Level:** internals · **Prerequisites:** [2b.1](01-storage-mergetree.md), [1.4](../../01-fundamentals/04-transactions.md) · **After this chapter you can:** state exactly what ClickHouse guarantees, model updates and deletes without in-place writes, and design idempotent ingestion. Measured on ClickHouse 26.10.

## 1. What ClickHouse guarantees

ClickHouse is not an ACID OLTP database. The real guarantees:

| Property | In ClickHouse |
| --- | --- |
| Atomic insert | One `INSERT` block becomes one part (or none). Large inserts are split into multiple blocks (`max_insert_block_size`), so a big `INSERT` is **not** atomic as a whole unless it fits one block |
| Snapshot reads | A query reads a fixed set of active parts chosen at start; concurrent merges don't change what it sees |
| Isolation across statements | none — no `BEGIN/COMMIT` for ordinary use (an experimental transaction feature exists; don't build on it) |
| Row locks / deadlocks | none |
| Uniqueness | none — the primary key is a sort key, not a constraint |
| Durability | after the part is written (and fsynced, depending on settings); replicated tables add replication |
| Consistency across replicas | eventual; configurable quorum for inserts (`insert_quorum`) and reads (`select_sequential_consistency`) |

Design for it: **append-only facts, idempotent ingestion, and queries that tolerate duplicates** — or use engines that resolve them.

## 2. Mutations (ALTER UPDATE and DELETE)

An `ALTER TABLE ... UPDATE|DELETE` is a **mutation**: a background job that rewrites every part that might contain affected rows. It is heavy, asynchronous and not transactional.

```sql ch-local
alter table play_events update amount = 0 where user_id = 42 settings mutations_sync = 0;
select mutation_id, command, is_done, parts_to_do from system.mutations where table = 'play_events';
```

Measured: right after submitting, `is_done = 0` and `parts_to_do = 7` — the mutation must visit **all 7 parts**, including parts with no `user_id = 42` rows (it has to check each). `mutations_sync` controls waiting: `0` = return immediately, `1` = wait on this server, `2` = wait on all replicas.

Properties: you cannot cancel a half-applied rewrite by "rollback" (use `KILL MUTATION`), a pending mutation affects only parts that exist when it is applied plus newly merged ones, and a slow or failing mutation blocks later mutations on the same table (`system.mutations.latest_fail_reason`).

### Lightweight delete

```sql ch-local
-- The projection from chapter 2b.2 blocks lightweight deletes (see below), so drop it first.
alter table play_events drop projection if exists p_country settings mutations_sync = 2;
delete from play_events where user_id = 77;
```

`DELETE FROM` marks rows as deleted via a hidden mask column and takes effect immediately for queries; physical removal happens at later merges. Measured: `count()` for `user_id = 77` went 28 → 0 at once. Limits found while testing: it is **rejected on tables that have projections** (error 344 unless `lightweight_mutation_projection_mode` is set), and it still adds work at merge. Use it for occasional deletes (GDPR requests), not as a primary update mechanism.

**Rule: if you update or delete many rows regularly, change the design** ([§3](#3-updates-by-appending-engines-that-resolve-versions) or partition-level drops).

### Partition operations are the cheap bulk tools

`ALTER TABLE ... DROP PARTITION`, `DETACH PARTITION`, `ATTACH PARTITION FROM`, `REPLACE PARTITION FROM` and `MOVE PARTITION` are metadata-level operations on whole parts: near-instant. They are the correct way to expire, replace or reload a slice of data.

## 3. Updates by appending: engines that resolve versions

Instead of changing a row, write a **new version** and let merges (or the query) pick the winner.

```sql ch-local
create table play_users (id UInt32, name String, version UInt32)
engine = ReplacingMergeTree(version) order by id;
insert into play_users values (1, 'a', 1);
insert into play_users values (1, 'b', 2);
select count() from play_users;          -- 2: two parts not merged yet
select count() from play_users final;    -- 1: FINAL resolves at read time
select * from play_users final;          -- (1, 'b', 2)
optimize table play_users final;
select count() from play_users;          -- 1
```

Because deduplication only happens at merge time (unpredictable), **every query that needs the current state must either `FINAL` or aggregate**: `select id, argMax(name, version) from play_users group by id`. `argMax` reads all versions but scales without `FINAL`'s merge overhead; `FINAL` is simpler and has been made much faster in recent releases — measure both.

Related engines: `CollapsingMergeTree(sign)` / `VersionedCollapsingMergeTree` cancel `+1`/`-1` row pairs (you must emit the cancelling row, so you need the old values); `SummingMergeTree` sums columns for equal keys.

## 4. Incremental aggregation with materialized views

A **materialized view** in ClickHouse is an *insert trigger*: for each block inserted into the source table it runs the `SELECT` and writes the result to a target table. Pair with `SummingMergeTree` or `AggregatingMergeTree` for rollups that stay current.

```sql ch-local
create table play_daily (day Date, country LowCardinality(String), cnt UInt64, amount Float64)
engine = SummingMergeTree order by (day, country);
create materialized view play_daily_mv to play_daily as
  select toDate(ts) as day, country, count() as cnt, sum(amount) as amount from play_events group by day, country;

insert into play_events select now(), 1, 'VN', 'buy', 10.5, 'p' from numbers(5);
select day, country, sum(cnt), sum(amount) from play_daily group by day, country;
```

Measured: after inserting 5 rows the target held `cnt = 5, amount = 52.5`. Notes:

- The view only sees **new inserts** after creation; backfill separately.
- It fires per inserted block; **deletes/updates on the source are not propagated**.
- Always re-aggregate when reading a `SummingMergeTree` (`sum(cnt) ... group by`), because unmerged parts hold partial rows.
- A failing view `SELECT` can fail the source insert; test it.

## 5. Idempotent ingestion and deduplication

Clients retry; you must make retries harmless.

- **Replicated tables** deduplicate identical *blocks* by default using a block hash (window `replicated_deduplication_window`, default 1000 blocks): re-sending the exact same batch is ignored. This works only if the retry has identical content and identical block boundaries.
- Non-replicated tables don't deduplicate unless you set `non_replicated_deduplication_window`.
- For stronger guarantees: include a **natural unique key + version** and use `ReplacingMergeTree`, or assign `insert_deduplication_token` per batch.
- **Exactly-once from a queue** is typically "at-least-once delivery + idempotent write": track offsets, make each batch deterministic (same offsets → same rows → same block hash), and commit the offset after the insert.

## 6. Patterns that replace a transaction

| Need | Pattern |
| --- | --- |
| Load a day atomically | insert into a staging table, then `ATTACH PARTITION ... FROM staging` or `REPLACE PARTITION` (atomic swap of whole parts) |
| Reload a corrected slice | `REPLACE PARTITION` |
| "Latest state" per key | `ReplacingMergeTree` + `argMax`/`FINAL` |
| Running totals | `SummingMergeTree`/`AggregatingMergeTree` via materialized view |
| Delete old data | TTL or `DROP PARTITION` |
| Read-your-writes on replicas | `select_sequential_consistency = 1` or read from the same replica |
| Cross-table atomic change | not available; make facts self-contained, or apply the staging-swap above |

## Exercises

1. Insert the same 1 000-row batch twice into a fresh `MergeTree` table and into a `ReplicatedMergeTree`-style table (or set `non_replicated_deduplication_window = 100`). Compare counts and explain.
2. Implement "current state" for `play_users` three ways (`FINAL`, `argMax`, `limit 1 by id`) with 10 versions per id across 100 000 ids; compare `read_rows` and time.
3. Submit a heavy `ALTER ... UPDATE`, watch `system.mutations` and `system.merges`, then `KILL MUTATION`. What state is the table in?
4. Build a daily rollup with a materialized view, then delete some source rows. Show the rollup is now wrong, and design a correction procedure.
5. Do an atomic reload of one monthly partition using a staging table and `REPLACE PARTITION`.

→ Answers: [appendix/solutions.md](../../appendix/solutions.md#2b4-clickhouse-consistency-and-mutations).

Next: [2b.5 Architecture](05-architecture.md).

# 3.1 ClickHouse: storage and the MergeTree family

> **Level:** internals · **Prerequisites:** [1.1](../01-fundamentals/01-storage.md) · **After this chapter you can:** explain what an `INSERT` physically creates, why small inserts are harmful, and what merges, partitions and TTLs do. Measured on ClickHouse 26.10 with the [seed](../README.md#seed-data) (`play_events`, 3 M rows).

ClickHouse is a **column-oriented OLAP** database. Its design goal is scanning and aggregating billions of rows quickly, writing in large batches, and compressing hard. It is *not* a transactional store ([3.4](04-consistency-and-mutations.md)).

## 1. Anatomy of a MergeTree table

```
table play_events
 ├─ partition 202610/
 │   ├─ part 202610_5_5_0/         ← immutable directory per insert block
 │   │    ├─ ts.bin, ts.mrk3       ← one data file + one marks file PER COLUMN
 │   │    ├─ user_id.bin, user_id.mrk3 ...
 │   │    └─ primary.idx           ← sparse primary index (1 entry per granule)
 │   └─ part 202610_13_13_0/
 └─ partition 202611/ ...
```

- A **part** is an immutable directory holding a *sorted* chunk of the table. Rows inside are sorted by the table's `ORDER BY` key.
- Each **column** is its own file (compressed in blocks). Reading 2 of 50 columns reads 2 files.
- Rows are grouped into **granules** (default 8 192 rows). The **marks** file maps a granule number to its offset in each column file, so the engine can read just the granules it needs.
- The **primary index** stores the sort-key value of the first row of every granule (sparse; [3.2](02-indexes-and-pruning.md)).
- Part names encode `partition_minblock_maxblock_level` — the level counts how many merges produced it.

```sql ch-local
select partition, name, rows, marks, formatReadableSize(bytes_on_disk) as on_disk
from system.parts where table = 'play_events' and active order by partition, name;
```

`marks` ≈ `rows / 8192`. Right after seeding you will see many small parts per partition.

## 2. The insert path and why batches matter

Each `INSERT` block becomes **a new part** (written fully sorted, committed atomically by a directory rename). Background **merges** combine parts of one partition into larger ones. This is what "MergeTree" means, and it makes ClickHouse an LSM-flavoured design: writes are sequential and cheap, reads must consult every active part until merges catch up.

Measured: three single-row inserts created **three parts** (`parts = 3, rows = 3`). Consequences:

- Thousands of tiny inserts → thousands of parts → slow queries, merge backlog, and eventually `Too many parts` errors (insert delayed at `parts_to_delay_insert = 1000` active parts per partition, rejected at `parts_to_throw_insert = 3000`).
- **Rule: insert in batches of at least tens of thousands of rows, a few times per second at most** — or let the server batch for you with `async_insert`.

```sql ch-local
create table play_tiny (id UInt32, v String) engine = MergeTree order by id;
insert into play_tiny select number, 'x' from numbers(1);
insert into play_tiny select number, 'x' from numbers(1);
insert into play_tiny select number, 'x' from numbers(1);
select count() as parts, sum(rows) as rows from system.parts where table = 'play_tiny' and active;
```

`async_insert = 1` buffers small inserts on the server and flushes them as one part (set `wait_for_async_insert = 1` to get an acknowledgement only after the flush; with `0` you trade durability visibility for latency):

```sql ch-local
insert into play_tiny settings async_insert = 1, wait_for_async_insert = 1 values (9, 'y');
```

## 3. Merges

A merge reads several parts of one partition and writes one bigger sorted part, then removes the sources. Merges are throttled by size and background thread pools. You can force one (`OPTIMIZE TABLE ... FINAL`), which is useful for demos and one-off maintenance but heavy on large tables and *not* something to schedule routinely.

```sql ch-local
optimize table play_events final;
select partition, count() as parts from system.parts where table = 'play_events' and active group by partition order by partition;
```

Measured: **21 parts → 7** (one per monthly partition). Merging reduces file count and also lets *engine-specific* logic run: `ReplacingMergeTree` removes duplicates, `SummingMergeTree` sums, `TTL` expires data — all **only at merge time**, which is not predictable ([3.4](04-consistency-and-mutations.md)).

## 4. Partitions

`PARTITION BY toYYYYMM(ts)` puts rows of each month in separate parts that never merge with each other. Use partitions to:

- **drop or move old data cheaply** (`ALTER TABLE ... DROP PARTITION '202601'`), and TTL by partition;
- **skip whole partitions** when the query filters on the partition expression (`Partition ... Parts: 6/21` in `EXPLAIN`).

Do not use partitioning as a query accelerator: the sort key does that. Aim for **tens to a few hundred** partitions in total; partitioning by a high-cardinality column (`user_id`) is a classic mistake that creates millions of tiny parts.

## 5. Compression and data types

Each column is compressed independently (default LZ4; `ZSTD` for higher ratio; per-column codecs like `Delta`, `DoubleDelta`, `Gorilla`, `T64` improve timestamps and counters).

```sql ch-local
select name, type, formatReadableSize(data_compressed_bytes) as compressed,
       formatReadableSize(data_uncompressed_bytes) as raw
from system.columns where table = 'play_events' order by data_compressed_bytes desc;
```

Measured on the seed: `payload` (100 repeated characters) **308 MiB → 11 MiB**; `LowCardinality(String)` columns (`country`, `event`) ~1.6 MiB each; `amount` (random floats) barely compresses (23 → 8 MiB).

Type guidance:

| Use | For |
| --- | --- |
| `LowCardinality(String)` | strings with up to ~10⁴–10⁵ distinct values (dictionary-encoded) |
| `UInt8/16/32/64`, `Int*` | smallest fitting integer; narrower = less to read |
| `Date`, `DateTime`, `DateTime64(n)` | time, not strings |
| `Enum8/16` | fixed small sets |
| `Decimal(P, S)` | money (avoid `Float` for exact sums) |
| `Nullable(T)` | only when needed: it adds a hidden bitmap column and slows scans; prefer defaults like `0`/empty string |
| `Array`, `Map`, `Nested`, `JSON` | semi-structured; promote hot keys to columns |

## 6. The MergeTree family

| Engine | Behaviour on merge | Use |
| --- | --- | --- |
| `MergeTree` | none | general fact tables |
| `ReplacingMergeTree(version)` | keeps the latest row per sort key | upsert-like "current state" |
| `SummingMergeTree` | sums numeric columns per sort key | pre-aggregated counters |
| `AggregatingMergeTree` | merges aggregate *states* | incremental rollups with materialized views |
| `CollapsingMergeTree` / `VersionedCollapsingMergeTree` | cancels +1/−1 row pairs | updates by sign |
| `Replicated*MergeTree` | same, plus replication via Keeper | production clusters ([3.5](05-architecture.md)) |

Other engines: `Memory`, `Log` family (tiny tables), `Distributed`, `Kafka`, `S3`, `Dictionary`, `Merge`, `Null`.

## 7. TTL: data lifecycle

```sql ch-local
alter table play_events modify ttl ts + interval 400 day;
```

TTL expressions must **depend on a column** (`now() + interval 1 day` alone is rejected: "does not depend on any of the columns of the table"). TTL can delete rows, move parts to other disks/volumes (tiering), or aggregate (`GROUP BY` TTL). It applies during merges (`merge_with_ttl_timeout`), not instantly.

## 8. Where to look

| Question | Table |
| --- | --- |
| Parts, sizes, rows | `system.parts` |
| Per-column sizes | `system.columns`, `system.parts_columns` |
| Part lifecycle (new, merge, remove, mutate) | `system.part_log` |
| Running merges / mutations | `system.merges`, `system.mutations` |
| Disk usage and volumes | `system.disks`, `system.storage_policies` |

## Exercises

1. Insert 1 000 rows as 1 000 single-row inserts into a fresh `MergeTree` table, count parts, then do the same with one `INSERT ... SELECT`. Observe `system.part_log` for the difference.
2. Predict the compression ratio of a column of sorted timestamps with `Delta + ZSTD` versus default LZ4, then measure it.
3. Create a table `PARTITION BY user_id` on a sample of the seed and watch `system.parts` explode. Explain why this hurts both inserts and queries.
4. What must be true for a TTL expression? Write one that moves data older than 30 days to a cheaper disk (reading the documentation for storage policies).
5. Find out how many merges ran during the seed (`system.part_log`, `event_type = 'MergeParts'`) and what the largest merged part was.

→ Answers: [appendix/solutions.md](../appendix/solutions.md#31-clickhouse-storage).

Next: [3.2 Indexes and pruning](02-indexes-and-pruning.md).

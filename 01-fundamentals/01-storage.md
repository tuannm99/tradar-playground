# 1.1 Storage fundamentals

> **Level:** fundamentals · **Prerequisites:** none · **After this chapter you can:** explain why a query is slow in terms of pages read, predict which storage layout suits a workload, and reason about durability and write cost.

Every database performance question eventually reduces to one: *how many bytes must move, from how far away, and how many times?* This chapter builds the vocabulary for that question.

## 1. The cost hierarchy

Orders of magnitude (they vary by hardware; the ratios are what matter):

| Layer | Typical latency | Relative |
| --- | --- | --- |
| CPU cache | ~1–10 ns | 1× |
| RAM | ~100 ns | 10–100× |
| NVMe SSD (random read) | ~50–150 µs | ~1 000× RAM |
| SATA SSD | ~100–500 µs | |
| Network round trip, same datacenter | ~0.2–1 ms | |
| Spinning disk (seek) | ~5–10 ms | ~100 000× RAM |
| Cross-region round trip | ~50–150 ms | |

Consequences that recur through this handbook:

- **A cache hit is ~1 000× cheaper than a disk read.** Most tuning is "make the working set fit in memory" or "touch fewer pages".
- **Sequential beats random.** Even on SSDs, reading 1 GB sequentially is far cheaper than reading 1 GB as 8 KB random pages, because of read-ahead, fewer I/O requests and less per-request overhead.
- **A network round trip costs as much as thousands of in-memory operations.** An "N+1" query pattern (one query per row) is slow because of round trips, not because each query is slow.

## 2. Pages: the unit of I/O

Databases do not read rows; they read **pages** (blocks): fixed-size chunks of the file.

| System | Unit | Size |
| --- | --- | --- |
| PostgreSQL | page | 8 KB (compile-time constant) |
| MySQL InnoDB | page | 16 KB |
| MongoDB WiredTiger | B-tree page | configurable; leaf pages are tens of KB, compressed on disk |
| ClickHouse | granule | 8 192 rows per column (default `index_granularity`) |

A page holds many rows. To read one 100-byte row the database reads the whole page, so **locality matters**: rows read together should live together. This is why a column that correlates with physical order (a monotonically increasing timestamp, say) makes range scans cheap, and why a random UUID primary key scatters inserts across the whole index.

## 3. The buffer cache

The database keeps recently used pages in RAM (the **buffer pool / shared buffers / WiredTiger cache**). A request first looks there; only a miss goes to disk.

- **Hit ratio** = hits / (hits + reads). A healthy OLTP system sits above ~99% on its hot path. A query that "reads 3 933 buffers" is cheap if they are hits and expensive if they are reads — always ask which.
- Most engines also benefit from the **OS page cache**. This means a page can be cached twice (database cache + OS cache) and that "cold" numbers differ from "warm" ones. Benchmark both and say which one you measured.
- **Working set** = the data and index pages touched by your regular workload. If it exceeds the cache, throughput falls off a cliff because every query becomes I/O-bound. This is the single most common cause of "it was fast last month".
- Dirty pages (modified but not yet written) are flushed in the background; see §6.

## 4. Row store vs column store

```
Row store (heap):                     Column store:
page 1: [id1 name1 amt1 ts1]          file id:   [id1 id2 id3 ...]
        [id2 name2 amt2 ts2]          file name: [name1 name2 name3 ...]
        [id3 name3 amt3 ts3]          file amt:  [amt1 amt2 amt3 ...]
```

| | Row store | Column store |
| --- | --- | --- |
| Read one whole row | **1 page** | one read per column |
| Aggregate one column over millions of rows | reads every column's bytes | **reads only that column** |
| Insert one row | **one append** | one append per column file |
| Compression | modest (mixed types per page) | **excellent** (same type, similar values, often sorted) |
| Typical engines | PostgreSQL, MySQL, MongoDB (document = row) | ClickHouse, Parquet-based systems |
| Workload | OLTP: many small reads and writes | OLAP: few huge scans and aggregates |

The choice is not a quality ranking; it is a trade of point-access speed against scan speed. See [5.1 OLTP vs OLAP](../05-architecture/01-oltp-vs-olap.md).

## 5. How rows are organised

- **Heap table** (PostgreSQL): rows are stored in insertion order wherever there is room; every index is separate and points to the row's physical address. Many indexes → many pointers to maintain.
- **Clustered / index-organised table** (InnoDB, SQL Server; MongoDB's record store is keyed by an internal id): the table *is* a B-tree on the primary key. Lookups by key are one tree walk; secondary indexes store the primary key, costing a second lookup.
- **Sorted parts** (ClickHouse MergeTree): data is sorted by the `ORDER BY` key inside immutable parts; the "index" is a sparse sample of that order.

## 6. Tree structures and the write path

### B+tree

Keys are kept sorted in a shallow, wide tree; leaves are linked for range scans. With 8 KB pages and short keys, one page holds hundreds of keys, so **a billion-row index is only ~4 levels deep**: 4 page reads to find any key (the top levels are always cached). Lookups, range scans and ordered output are all efficient; writes modify pages **in place**, which means random I/O and page splits.

### LSM tree

Writes go to an in-memory table and are flushed as immutable sorted files; background **compaction** merges files. Writes are sequential and fast, reads may check several files (mitigated by Bloom filters), and compaction costs extra write I/O later. MergeTree in ClickHouse is LSM-flavoured: immutable sorted parts merged in the background. PostgreSQL and WiredTiger's default B-tree use in-place pages.

### Amplification

Three numbers describe any storage design, and you can only optimise two at once:

- **Read amplification** — pages touched per logical read.
- **Write amplification** — bytes physically written per byte logically written (WAL + page + index + compaction).
- **Space amplification** — bytes on disk per logical byte (dead tuples, free space, indexes).

B-trees favour reads; LSM trees favour writes; compression trades CPU for space and I/O.

## 7. Durability: the write-ahead log

Writing every changed data page to disk on commit would be slow and not atomic. Instead:

1. Append a record describing the change to a **write-ahead log (WAL)** — sequential, small.
2. `fsync` the log on commit. **This is the moment the transaction becomes durable.**
3. Modify the page in memory; flush the page later (a **checkpoint** bounds how much log must be replayed).
4. After a crash, **replay the log** from the last checkpoint ("redo").

Rules of thumb:

- Commit latency ≈ one `fsync`. Many tiny transactions are slow because each pays for it; **group commit** lets concurrent commits share one fsync.
- Relaxing durability (`synchronous_commit = off` in PostgreSQL, `w:1` / no journal in MongoDB) can lose the last moments of data on a crash but **does not corrupt** the database.
- Replicas are fed from the same log, which is why replication lag is measured in log positions (see [5.2](../05-architecture/02-replication-and-consistency.md)).

| Engine | Log | Notes |
| --- | --- | --- |
| PostgreSQL | WAL | page images + deltas; checkpoints every `checkpoint_timeout` or `max_wal_size` |
| MongoDB (WiredTiger) | journal + oplog | journal for local crash recovery; oplog (a collection) for replication |
| ClickHouse | none for the data path in the classic sense | an insert writes a complete new immutable part and commits it atomically; durability then depends on fsync settings and replication |

## 8. Compression and encoding

Columnar systems exploit that one column holds similar values: dictionary encoding (`LowCardinality`), delta encoding for timestamps, general-purpose LZ4/ZSTD on top. Row stores compress too (TOAST and page compression in PostgreSQL, snappy/zstd in WiredTiger) but gain less. Compression saves I/O and cache space at some CPU cost, and on I/O-bound workloads it usually **speeds queries up**.

## Try it

The seed (see the [README](../README.md#seed-data)) gives you real tables to look at.

```sql pg-local
-- Where rows physically live: (page, slot), plus the transaction ids that created/removed them.
select ctid, xmin, xmax, id from play_orders limit 3;
```

```sql pg-local
-- Pages and size per relation: 8 KB pages, so relpages * 8 KB ≈ size.
select relname, relpages, reltuples::bigint, pg_size_pretty(pg_relation_size(oid)) as heap
from pg_class where relname like 'play_%' and relkind = 'r';
```

```sql ch-local
-- Column files compress very differently depending on content.
select name, type, formatReadableSize(data_compressed_bytes) as compressed,
       formatReadableSize(data_uncompressed_bytes) as raw
from system.columns where table = 'play_events' order by data_compressed_bytes desc;
```

Observe: the `payload` column (100 repeated characters) compresses ~30×, `LowCardinality` columns are tiny, and `amount` (floats) barely compresses.

## Exercises

1. A table has 10 million rows of 200 bytes each. About how many 8 KB pages is that? How long does a full scan take at 1 GB/s sequential, and how long would 10 million random single-page reads take at 100 µs each?
2. Explain why `select avg(amount) from play_events` is cheaper in ClickHouse than the equivalent in a row store, and why `select * from play_events where ...` for one row is not.
3. A service commits 5 000 tiny transactions per second and the disk does 1 000 fsyncs per second. What are your options, and what does each risk?
4. Why does a random UUID primary key hurt a B-tree on a table larger than memory, and what would you use instead if you still need unpredictable ids?

→ Answers: [appendix/solutions.md](../appendix/solutions.md#11-storage).

Next: [1.2 Indexing](02-indexing.md).

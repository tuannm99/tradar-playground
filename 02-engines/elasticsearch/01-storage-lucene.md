# 2d.1 Elasticsearch: shards, Lucene segments and the write path

> **Level:** internals · **Prerequisites:** [1.1](../../01-fundamentals/01-storage.md) · **After this chapter you can:** explain what an index, a shard and a segment are, why a document is not searchable the instant it is written, what refresh, flush, translog and merge each do, and where the bytes of an index actually go. Measured on Elasticsearch 8.15.3 (single node, 1 GB heap) with the [seed](../../README.md#seed-data) (`play_products`, 100 000 documents; `play_logs`, 500 000).

## 1. Index, shard, segment

Elasticsearch is a distributed layer over **Apache Lucene**. The nesting matters for every performance question:

| Level | What it is | Lives on |
| --- | --- | --- |
| **Index** | a named collection of documents with one mapping | the cluster |
| **Shard** | one self-contained Lucene index; the unit of distribution, replication and parallel search | one node (primary) + copies (replicas) |
| **Segment** | an **immutable** set of files inside a shard holding a slice of its documents | one node's disk |
| **Document** | a JSON object (`_source`) plus the structures built from it | one segment |

A search runs on **every shard** of the index (or a pruned subset), and inside a shard on **every segment**, then merges the partial results. More shards and more segments mean more parallel work *and* more fixed overhead per query.

```esq es-local
GET /_cat/segments/play_*?v&h=index,segment,docs.count,size
```

Measured: each seed index is **one shard with one segment** (`play_products`: 100 000 docs, 11.2 MB; `play_logs`: 500 000 docs, 39.7 MB). The seeder disables refresh while loading and refreshes once at the end, so the bulk data lands in a single segment. A live index would have many.

## 2. Immutability and its consequences

A segment is never modified. This is the same trade as an LSM tree ([1.1 §6](../../01-fundamentals/01-storage.md#6-tree-structures-and-the-write-path)): cheap sequential writes and lock-free readers, paid for with background merging.

- **Insert**: goes to an in-memory buffer, later written as a new segment.
- **Delete**: the document is only marked deleted in a per-segment bitmap; space returns when its segment is merged.
- **Update**: a delete of the old version **plus** an insert of the new one. There is no in-place update.

Measured on a scratch index: 20 documents updated 3 times each left `docs.count = 31` and **`docs.deleted = 61`** until a merge ran. Heavy update workloads produce dead documents that still cost disk and, until merged, search time.

## 3. The write path: buffer, translog, refresh, flush, merge

```
bulk request ──► primary shard ──► in-memory indexing buffer ─┐
                       │                                      │  refresh (default 1 s)
                       └──► translog (fsync) ◄── durability   ▼
                                                     new searchable segment (in OS cache)
                                                              │  flush (when translog is large)
                                                              ▼
                                            Lucene commit: segments fsynced, translog trimmed
                                                              │  merge (background)
                                                              ▼
                                                  fewer, larger segments
```

| Step | What it does | Default | Cost of changing it |
| --- | --- | --- | --- |
| **Refresh** | makes buffered documents searchable by writing a new small segment | every `1s` if the index was searched recently | shorter = fresher but more tiny segments; `-1` = off during bulk loads |
| **Translog** | append-only log of operations so a crash loses nothing acknowledged | `durability: request` (fsync before ack), `sync_interval: 5s` | `async` is faster, may lose up to `sync_interval` of acked writes |
| **Flush** | Lucene commit: fsync segments, start a new translog generation | automatic | not something you normally tune |
| **Merge** | combine small segments into larger ones, dropping deleted docs | automatic, throttled | `forcemerge` only on indexes that will never be written again |

This is the same idea as the PostgreSQL WAL ([2a.1 §7](../postgres/01-storage-and-mvcc.md)): the log gives durability, the segment files are written lazily.

### Near-real-time search, measured

On an index with `refresh_interval: 30s`:

| Step | Result |
| --- | --- |
| `PUT /play_scratch/_doc/1` | `result: created`, `_seq_no: 0` |
| `GET /play_scratch/_doc/1` | `found: true` (a get by `_id` reads the translog, so it is real-time) |
| `POST /play_scratch/_search` (match_all) | **0 hits** |
| `POST /play_scratch/_refresh`, search again | **1 hit** |

*Get by id is real-time; search is near-real-time.* Code that writes then immediately searches for the document will fail intermittently. Options: `?refresh=wait_for` on the write (waits for the next refresh), `?refresh=true` (forces one, costly if repeated), or design so reads do not need to see the write at once ([2d.4](04-writes-consistency-concurrency.md)).

### Small segments merge themselves

30 single-document writes each with `refresh=true` created 30 tiny segments; the background merger had already reduced them to four (`20`, `9`, `1`, `1` documents) when inspected, and a `_forcemerge?max_num_segments=1` produced one segment of 31 docs. Many small segments make searches slower (each segment is searched separately); never "fix" that by forcing merges on an index that is still being written, because new large segments then stop being merged efficiently.

## 4. Where the bytes go

```esq es-local
POST /play_products/_disk_usage?run_expensive_tasks=true
```

Measured for `play_products` (total 11.7 MB):

| Part | Bytes | Share | Used for |
| --- | --- | --- | --- |
| `_source` (stored JSON) | 6.88 MB | 59 % | returning the document; reindex, update |
| `_id` | 1.50 MB | 13 % | get by id, uniqueness |
| `created_at` (doc values + BKD points) | 0.96 MB | 8 % | sorting/aggregating + range search |
| `description` (inverted index) | 0.69 MB | 6 % | full-text search |
| `price` (doc values + points) | 0.60 MB | 5 % | sort/aggregate + range |
| `rating`, `_seq_no`, `name`, `brand`, `category`, others | the rest | ~9 % | |

Three Lucene structures coexist, each good at one question:

- **Inverted index**: term → list of documents. Answers "which documents contain this word?" ([2d.2](02-indexing-analysis.md)).
- **Doc values**: column-oriented, on-disk, per field. Answers "what is the value of this field for these documents?" for sorting and aggregations. This is the same idea as a column store ([1.1 §4](../../01-fundamentals/01-storage.md#4-row-store-vs-column-store)).
- **Stored fields** (`_source`): row-oriented JSON, for fetching hits.

So `_source` dominating is normal; indexing everything "just in case" adds more structures per field, and every extra structure costs write time and disk.

## 5. Compare with the other engines

| Question | PostgreSQL | MongoDB | ClickHouse | Elasticsearch |
| --- | --- | --- | --- | --- |
| Storage unit | heap page | WiredTiger page | part (column files) | Lucene segment |
| Update | new row version, vacuum | in-place + journal | async mutation | delete + insert, merge |
| Search after write | immediately (MVCC) | immediately | after insert commit | after **refresh** |
| Durability | WAL | journal + checkpoint | part on disk | translog + commit |

## Try it

```esq es-local
GET /_cat/indices/play_*?v&h=index,pri,rep,docs.count,docs.deleted,store.size
```

```esq es-local
GET /play_products/_stats/segments,store,translog
```

## Exercises

1. Why can a `search` right after a successful `PUT` miss the document, while `GET /index/_doc/<id>` finds it? Give two ways to make the search see it and the cost of each.
2. A job updates every document of a 50 GB index once a day. What happens to disk usage and search speed in between merges, and what would you change in the data flow?
3. From the disk-usage table, which single change to `play_products` would shrink it most, and what would you lose?
4. Explain why `forcemerge` on a time-based index is safe after it stops receiving writes but harmful before.

→ Answers: [appendix/solutions.md](../../appendix/solutions.md#2d1-elasticsearch-storage).

Next: [2d.2 Indexing and analysis](02-indexing-analysis.md).

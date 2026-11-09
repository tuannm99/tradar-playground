# 2d.7 Elasticsearch versions: what changed from 5.x to 8.x/9.x

> **Level:** practical + internals · **Prerequisites:** [2d.1](01-storage-lucene.md)–[2d.4](04-writes-consistency-concurrency.md) · **After this chapter you can:** recognise which major version a cluster, mapping or client snippet belongs to, explain the internal change behind each breaking change, and plan an upgrade path. Behaviour marked *measured* ran on **5.6.16**, **6.8.23**, **7.16.1** and **8.15.3** single nodes side by side (official images, security off), with the same seed where the API allowed it. 9.x was not run; its row comes from the release notes.

## 1. The short version

| Major | Lucene | Year | The change that matters most |
| --- | --- | --- | --- |
| **5.x** | 6 | 2016 | `string` split into `text`/`keyword`; BM25 by default; numbers in BKD trees; Painless; ingest nodes |
| **6.x** | 7 | 2017 | **one mapping type per index**; sequence numbers (`_seq_no`, `_primary_term`); `_all` gone; ILM, index sorting, cross-cluster search |
| **7.x** | 8 | 2019 | **types removed** from the API (`_doc`); **1 primary shard by default**; new cluster coordination (no `minimum_master_nodes`); `hits.total` is an object capped at 10 000; data streams, data tiers, searchable snapshots |
| **8.x** | 9 | 2022 | **security on by default**; type APIs gone; approximate kNN (HNSW) vectors; synthetic `_source`; ES\|QL; TSDS and logsdb index modes |
| **9.x** | 10 | 2025 | vector quantization (BBQ) by default for dense vectors, logsdb defaults, removal of 7.x compatibility; check the release notes for your minor |

Upgrades go **one major at a time** through the last minor of the previous major (5.6 → 6.8 → 7.17 → 8.x). An index can only be read by the next major after the one that created it, so a 5.x index must be **reindexed** before a cluster can reach 7.x.

## 2. Mapping types: from many, to one, to none

Up to 5.x, an index could hold several **mapping types** (`/shop/product`, `/shop/order`), sold as "index = database, type = table". It was never true: all types of an index share one Lucene index, so a field `name` had to have the same mapping in every type, and sparse fields across unrelated types wasted space and hurt compression.

Measured on 5.6: documents of type `product` and `order` went into the same index `shop`; after `product` had a numeric `price`, indexing `{"price": "cheap"}` into **`order`** failed with `failed to parse [price]`: the two "tables" shared one field definition.

| Request | 5.6.16 | 6.8.23 | 7.16.1 | 8.15.3 |
| --- | --- | --- | --- | --- |
| Index into a second type in the same index | works | **rejected**: `final mapping would have more than 1 type: [other, _doc]` | n/a | n/a |
| `PUT /v_typed/mytype/1` (custom type) | works | works | **201 + deprecation `Warning` header** (`[types removal] ... use the typeless endpoints`) | **no handler found** (endpoint gone) |
| `PUT /v_doc/_doc/1` | **rejected**: `type name can't start with '_'` | works (`_type: "_doc"`) | works (`_type: "_doc"`) | works, response has **no `_type`** |
| Typeless mapping body `{"mappings": {"properties": ...}}` | rejected | **rejected** (`Root mapping definition has unsupported parameters`) unless `?include_type_name=false` | default | only form |
| `?include_type_name=true` | n/a | default behaviour | works with deprecation warning | **400 unrecognized parameter** |
| `_bulk` with `{"index":{}}` and no `_type` | `type is missing` | **`type is missing`** | works | works |

The handbook's own seeder (typeless bulk, typeless mapping) runs unchanged on 7.16 and 8.15 and **fails on 6.8 and 5.6** at the first request. That is exactly the migration pain old applications hit.

How to recognise the era of a snippet: `"_type"` in documents or a type name in the URL → ≤ 6; `include_type_name` → 6.x/7.x migration code; no type anywhere → ≥ 7.

## 3. Text: `string` → `text` + `keyword`

Before 5.0, one `string` type was either analyzed or `not_analyzed`. 5.0 split it into **`text`** (analyzed, for search) and **`keyword`** (exact, doc values, for filters/aggregations), and turned off fielddata on text ([2d.2 §3](02-indexing-analysis.md#3-text-vs-keyword-the-classic-mistake)). Dynamic mapping of a JSON string has created the pair ever since. Measured on 5.6 and 6.8: a dynamic string field became `{"type": "text", "fields": {"keyword": {"type": "keyword", "ignore_above": 256}}}`, the same as in 8.x. On 5.6, a legacy mapping `{"type": "string", "index": "not_analyzed"}` was still accepted, with a `Warning: The [string] field is deprecated` header, and silently stored as `keyword`.

`_all` (a hidden field concatenating every value, on by default up to 5.x) was disabled in 6.0. Measured on 5.6: `GET /shop/_search?q=keyboard` with no field name found the document through `_all`. Measured on 6.8: enabling it is rejected with *"Enabling [_all] is disabled in 6.0. As a replacement, you can use [copy_to]"*. Old mappings that rely on `_all` need an explicit `copy_to` field.

## 4. Scoring and numbers (5.0, Lucene 6)

- **BM25** replaced classic TF-IDF as the default similarity. Term frequency saturates and field length normalises differently ([2d.3 §2](03-queries-and-relevance.md#2-how-relevance-is-computed-bm25)). Ranking changed on upgrade: relevance tests from 2.x do not carry over.
- **Points (BKD trees)** replaced numbers indexed as prefix-encoded terms. Range queries on numbers, dates and IPs became faster and smaller, and `geo_point` moved to the same structure.
- **Painless** became the default scripting language; Groovy (with its sandbox problems) went away.

## 5. Replication internals: sequence numbers (6.0)

6.0 gave every operation a per-shard **sequence number** and a **primary term**. Measured on 5.6: `GET` returns only `_version`, and `?if_seq_no=...` is refused as *unrecognized parameters: [if_primary_term], [if_seq_no]*; the only optimistic check is `?version=N` (`current version [1] is different than the one provided [7]`). Measured on 6.8: a new document comes back with `_seq_no: 0, _primary_term: 1`, and `PUT ...?if_seq_no=5&if_primary_term=1` is refused with a version conflict. The same calls work in 7.x and 8.x ([2d.4 §2](04-writes-consistency-concurrency.md#2-concurrency-control-_seq_no-and-_primary_term)).

Why it matters beyond the API:

- **Operation-based recovery**: a replica that was briefly offline replays the operations it missed from its last known sequence number instead of copying whole segment files. Before 6.0, a restarted node often re-copied gigabytes.
- **Correct optimistic concurrency** across failover: the older `?version=N` check could not tell apart two primaries that had both accepted writes; `_primary_term` can. Version-based OCC on internal versions is deprecated in favour of `if_seq_no`/`if_primary_term`.

## 6. Defaults that changed under your feet (7.0)

| Default | ≤ 6.x | ≥ 7.0 | Measured |
| --- | --- | --- | --- |
| Primary shards for a new index | **5** | **1** | 5.6 and 6.8: `number_of_shards: "5"`; 7.16 and 8.15: `"1"` |
| `hits.total` | integer, exact | object `{value, relation}`, counts up to **10 000** | 5.6: `{"total": 1}`, 6.8: `{"total": 0}` (integers); 7.16 and 8.15: `{"value": 10000, "relation": "gte"}` on `play_products` |
| Integer `hits.total` | default | `?rest_total_hits_as_int=true` | both 7.16 and 8.15 return `{"total": 100000}` with the flag, and then count exactly |

The **five-shard default** is the root of many oversharded clusters still running today: thousands of daily indices, each with 5 tiny shards ([2d.5 §3](05-architecture-clusters.md#3-sizing-shards)). If you inherit one, the fix is `_shrink` or reindexing into fewer, bigger shards, not more nodes.

The **10 000 cap** exists because Lucene 8 added *block-max WAND*: the top-k hits can be found while skipping blocks of documents that cannot make the top k. Counting every match would defeat the skipping, so 7.0 stopped doing it by default ([2d.3 §3](03-queries-and-relevance.md#3-counting-and-the-track_total_hits-cap)). Client code that reads `hits.total` as a number breaks on 7.x.

## 7. Cluster coordination (7.0)

Up to 6.x, master election (Zen discovery) needed **`discovery.zen.minimum_master_nodes`** set by hand to a majority of master-eligible nodes. If it was wrong (forgotten after adding nodes), a network partition could elect **two masters: split brain**, with divergent data. 7.0 replaced it with a new coordination layer that keeps its own **voting configuration**, so quorums are computed automatically. A new cluster is bootstrapped once with `cluster.initial_master_nodes`; `minimum_master_nodes` is ignored. Setting `discovery.type: single-node` (as every container in this handbook does) skips bootstrapping entirely.

## 8. Data lifecycle features (6.x late → 7.x)

| Feature | Since | Replaces |
| --- | --- | --- |
| ILM (index lifecycle management) | 6.6 | Curator cron jobs |
| Frozen indices (`_freeze`) | 6.6, removed in 8.0 | searchable snapshots, frozen tier |
| Data streams | 7.9 | hand-built rollover aliases |
| Data tiers (hot/warm/cold/frozen node roles) | 7.10 | custom node attributes + allocation filtering |
| Searchable snapshots | 7.10–7.12 | keeping old indices on expensive disks |
| Runtime fields | 7.11 | scripted fields in Kibana, reindexing to add a field |

Measured: `POST /v_doc/_freeze` is acknowledged on 7.16 and returns *no handler found* on 8.15. Automation that still freezes old indices stops working on 8.x.

## 9. 8.x: security, vectors, ES|QL

- **Security is on by default**: the official image generates a TLS certificate and an `elastic` password at first start; plain `http://` and anonymous access stop working. This handbook disables it explicitly (`xpack.security.enabled: false`) because it is a local playground. Never do that on a reachable machine.
- **Approximate kNN** on `dense_vector` (HNSW graphs per segment). Measured: a top-level `knn` search section is parsed on 8.15 and rejected by 7.16 (`Unknown key for a START_OBJECT in [knn]`); on 7.x vectors are only scored by brute force through `script_score`. Each segment has its own HNSW graph, so merges and many small segments matter even more for vector search ([2d.1 §3](01-storage-lucene.md#3-the-write-path-buffer-translog-refresh-flush-merge)).
- **ES|QL**, a piped query language next to the JSON DSL. Measured on 8.15:

```esq es-local
POST /_query
{"query": "FROM play_logs | STATS c = COUNT(*) BY level | SORT c DESC"}
```

returned `INFO 332 683, ERROR 83 935, WARN 83 382`. On 7.16 the same endpoint answers *405 Incorrect HTTP method*.

- **Index modes**: `time_series` (TSDS, 8.7) sorts and routes metrics by dimensions and stores them compactly; `logsdb` (8.17) sorts by host and timestamp and uses synthetic `_source` for much smaller log indices. Both are the same idea as ClickHouse's `ORDER BY`: sort the data so it compresses ([2b.1](../clickhouse/01-storage-mergetree.md)).

## 10. Did storage size change?

Same seed (100 000 products, 500 000 log events), one shard, force-merged to one segment:

| Index | 7.16.1 (Lucene 8.10) | 8.15.3 (Lucene 9.11) |
| --- | --- | --- |
| `play_products` | 11.4 MB | 11.5 MB |
| `play_logs` | 40.3 MB | 40.7 MB |

For this data the on-disk format is essentially unchanged across Lucene 8 → 9. Savings in newer versions come from **opting in** to an index mode (`logsdb`, `time_series`), synthetic `_source` or `best_compression`, not from the upgrade alone.

## 11. OpenSearch and the license split

In 2021 (7.11) Elastic moved from Apache 2.0 to the Elastic License / SSPL. AWS forked the last Apache release, **7.10.2**, as **OpenSearch**. Since then the two diverge: OpenSearch 1.x/2.x is API-compatible with Elasticsearch 7.10 for most search and indexing calls, but not with 8.x features (ES|QL, security defaults, newer vector and index-mode work), and clients check the server's product header. In 2024 Elastic added AGPL as a third license option. Treat "Elasticsearch-compatible" as *7.10-compatible* unless stated otherwise.

## 12. Upgrade checklist

1. Find the oldest index: `GET /_cat/indices?v&h=index,creation.date.string` and `GET /<index>/_settings?filter_path=**.version.created`. Indices created two majors back must be reindexed first.
2. Run the **upgrade assistant** / `GET /_migration/deprecations` on the last minor of your current major, and read the `Warning` headers your applications receive (7.x sends one for every typed request, as measured above).
3. Remove types from mappings, URLs and bulk metadata; replace `_all` with `copy_to`; replace `string` with `text`/`keyword`.
4. Check client code for `hits.total` as an integer, `minimum_master_nodes`, `_freeze`, Curator jobs.
5. Re-test relevance (BM25 and analyzer changes alter ranking) and the slowest queries with `profile`.
6. Take a **snapshot** before each step; snapshots, not replicas, are the rollback path.

## Try it

```esq es-local
GET /
```

The `version.number` and `version.lucene_version` fields tell you which column of the tables above applies.

```esq es-local
GET /_migration/deprecations
```

## Exercises

1. A legacy service writes `PUT /logs-2019.05.01/event/<id>`, reads `hits.total` as an integer and its mapping uses `"type": "string"`. List what breaks on 6.x, 7.x and 8.x, and the change needed for each.
2. Why did Elasticsearch choose to drop multiple types instead of fixing them? Use what you know about Lucene segments ([2d.1](01-storage-lucene.md)) in the answer.
3. A 6.x cluster with 3 master-eligible nodes had `minimum_master_nodes: 1`. Describe the failure during a partition, and what 7.x does instead.
4. Your 7.x cluster has 4 000 indices of 5 shards each, most under 1 GB. What do you do before upgrading to 8.x, and why does the upgrade not fix it by itself?

→ Answers: [appendix/solutions.md](../../appendix/solutions.md#2d7-elasticsearch-versions).

Back to the part index: [2d.1](01-storage-lucene.md) · Next part: [3. Architecture](../../03-architecture/01-oltp-vs-olap.md).

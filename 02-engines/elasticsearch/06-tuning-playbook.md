# 2d.6 Elasticsearch tuning playbook

> **Level:** practical · **Prerequisites:** [2d.1](01-storage-lucene.md)-[2d.5](05-architecture-clusters.md) · Symptom → diagnosis → fix, with copy-pasteable requests. Measured on Elasticsearch 8.15.3 with the seed.

## 1. First hour on an unfamiliar cluster

```esq es-local
GET /_cluster/health
```
`status`: `green` (all shards assigned), `yellow` (replicas missing), `red` (a primary missing: data unavailable). Check `unassigned_shards`, `number_of_pending_tasks`.

```esq es-local
GET /_cat/indices?v&s=store.size:desc&h=index,health,pri,rep,docs.count,docs.deleted,store.size
```
Biggest indices first; many indices of a few MB each = oversharding ([2d.5 §3](05-architecture-clusters.md#3-sizing-shards)); high `docs.deleted` = update-heavy ([2d.1 §2](01-storage-lucene.md#2-immutability-and-its-consequences)).

```esq es-local
GET /_cat/shards?v&s=store:desc
```

```esq es-local
GET /_nodes/stats/jvm,indices,thread_pool
```
Watch heap (`jvm.mem.heap_used_percent`; sustained > 85 % is trouble), GC time, `thread_pool.search.rejected` and `write.rejected`, `indices.fielddata`, `indices.query_cache`.

```esq es-local
GET /_cluster/allocation/explain
```
Why a shard is unassigned.

```esq es-local
GET /_nodes/hot_threads
```
What the CPUs are doing right now.

## 2. A slow search

1. Is it slow on **every** shard or one? Add `"profile": true` ([2d.3 §5](03-queries-and-relevance.md#5-reading-a-plan-the-profile-api)) and find the slowest child query.
2. Turn on the **search slow log** for the index, with thresholds that fit your SLO:

```esq es-local
PUT /play_products/_settings
{"index.search.slowlog.threshold.query.warn": "500ms", "index.search.slowlog.threshold.query.info": "200ms"}
```

3. Typical causes and fixes:

| Cause | Evidence | Fix |
| --- | --- | --- |
| Conditions in `must` that should be `filter` | wrong ranking; no cache hits | move to `filter` ([2d.3 §1](03-queries-and-relevance.md#1-two-questions-does-it-match-how-well)) |
| Deep `from`/`size` | latency rises with page (17 ms at 9 000 vs 4 ms for `search_after`) | `search_after` + PIT |
| Exact `total` needed on large results | `track_total_hits: true` slower | keep the 10 000 cap if "10 000+" is enough |
| Aggregation computed over everything | `date_histogram` + `percentiles` over 500 k docs ≈ 150 ms | filter first, narrower buckets, `sampler`, pre-aggregate with rollups/transforms |
| Leading wildcard, regex, script | profile shows large `build_scorer` / scan | n-gram field, precomputed field |
| Too many small segments | `_cat/segments` shows hundreds | let merging catch up; reduce tiny refreshes |
| Too many shards per query | each query touches 100s of shards | fewer, larger shards; time-based pruning; `routing` |
| Heap pressure / GC | long GC in node stats | fewer fields, no fielddata on text, smaller aggregations, more heap or nodes |

## 3. A slow or failing indexing job

| Symptom | Cause | Fix |
| --- | --- | --- |
| 429 `es_rejected_execution_exception` | write queue full | smaller concurrency, backoff and retry |
| Throughput low | refresh every 1 s, replicas on, tiny bulks | for the **initial load**: `refresh_interval: -1`, `number_of_replicas: 0`, bulks of 5-15 MB, then restore (the seed does exactly this: 600 000 docs in ~22 s) |
| Mapping errors halfway | `dynamic` surprises | explicit mapping, `dynamic: strict`; check bulk `errors` ([2d.4 §3](04-writes-consistency-concurrency.md#3-writing-in-bulk)) |
| CPU high on merges | many updates, large documents | fewer updates (rebuild instead), `_source` excludes, `index: false` on unused fields |
| Cluster state huge, master slow | mapping explosion, too many indices/fields | cap fields (`index.mapping.total_fields.limit`), flatten dynamic keys ([2d.2 §4](02-indexing-analysis.md#4-mappings)) |

Restore after a bulk load:

```esq es-local
PUT /play_products/_settings
{"index": {"refresh_interval": "1s", "number_of_replicas": 1}}
```

## 4. Disk and memory

- Disk: leave headroom (default watermarks: low 85 %, high 90 %, flood stage 95 % makes indices read-only). Check `_cat/allocation?v`.
- Reduce size: remove unused indexing (`index: false`, `doc_values: false`), compress `_source` (`index.codec: best_compression`), drop old indices through ILM.
- Memory: heap ≤ half of RAM and ≤ ~30 GB; the rest is page cache. Never enable `fielddata` on `text` fields; use a `keyword` sub-field ([2d.2 §3](02-indexing-analysis.md#3-text-vs-keyword-the-classic-mistake)). Disable swapping.

## 5. Mappings and settings checklist

- Explicit mapping; `dynamic: strict` or `false` on anything public.
- `keyword` for ids/filters/aggregations, `text` for searched prose, multi-field when both.
- Number of shards chosen from expected size (10-50 GB), not from node count.
- `refresh_interval` set for your freshness need, not left at 1 s for write-heavy logs (try 5-30 s).
- Aliases in front of every index the application uses.
- Snapshot policy configured and a restore tested.

## 6. A short case file

Use the template in [1.6 §7](../../01-fundamentals/06-measuring.md#7-a-reusable-case-file-template). Example entry:

| Field | Value |
| --- | --- |
| Symptom | product list at page 5 000 takes seconds, then errors |
| Evidence | `from=10000` → `Result window is too large ... [10010]`; `from=9000` 17 ms |
| Hypothesis | deep offset paging fans out `from + size` per shard |
| Change | `search_after` with `[price, _doc]` sort values |
| Result | 4 ms for the next page; flat in depth |

## Exercises

1. Run the §1 requests on the seed and write down, from the output alone: number of shards, largest index, heap used, and whether anything is unassigned.
2. Set the slow log threshold to `0ms` on `play_products`, run three different searches, and find them in the log. Which one cost the most?
3. Using only `_cat/indices`, how would you tell an update-heavy index from an append-only one?
4. Load `play_logs` again with `refresh_interval: 1s` and replicas on, then with the seed's settings. Compare the wall time and explain the difference.

→ Answers: [appendix/solutions.md](../../appendix/solutions.md#2d6-elasticsearch-tuning-playbook).

Next: [2d.7 Versions: 5.x to 9.x](07-versions-5-to-9.md).

# 2d.3 Elasticsearch: queries, relevance, aggregations and pagination

> **Level:** practical · **Prerequisites:** [2d.2](02-indexing-analysis.md), [1.3](../../01-fundamentals/03-query-execution.md) · **After this chapter you can:** build `bool` queries that separate matching from scoring, read a BM25 explanation, profile a query, aggregate on doc values, and paginate without hitting the 10 000 wall. Measured on Elasticsearch 8.15.3 with the seed (`play_products` 100 k, `play_logs` 500 k, one shard each).

## 1. Two questions: does it match? how well?

Every clause in a `bool` query lives in one of two contexts:

| Clause | Context | Effect on `_score` | Cacheable |
| --- | --- | --- | --- |
| `must`, `should` | query | contributes | no |
| `filter`, `must_not` | filter | none (constant) | yes, per segment, when reused |

```esq es-local
POST /play_products/_search
{
  "size": 10,
  "query": {"bool": {
    "must":   [{"match": {"description": "wireless keyboard"}}],
    "filter": [{"term": {"category": "input"}}, {"range": {"price": {"lte": 100}}}]
  }}
}
```

Put text that should rank in `must`; put every yes/no condition (category, status, range, tenant) in `filter`.

### Measured: the effect is on scores first, speed second

The same query written with the two conditions in `filter` vs in `must` (30 runs each, request cache off):

| Variant | median `took` | `max_score` |
| --- | --- | --- |
| conditions in `filter` | 6-7 ms | **4.33** |
| conditions in `must` | 5-7 ms | **7.12** |

On 100 000 documents the latency difference is within noise. What changed is the **score**: the `must` version adds the `category` and `price` clauses to the relevance, which pollutes ranking. The cache benefit of `filter` shows when the same filter is reused often on large segments; Elasticsearch only caches filters that appear repeatedly and live on segments above a minimum size, and `_stats/query_cache` showed 0 hits here after a handful of runs. Do not expect a speed-up from `filter` on small data; use it because it is correct for ranking.

## 2. How relevance is computed (BM25)

```esq es-local
GET /play_products/_explain/<id>
{"query": {"match": {"name": "wireless keyboard"}}}
```

For one hit the explanation (measured) is a sum over terms of `idf × tf`, with `k1 = 1.2` and `b = 0.75`:

- **idf** = `log(1 + (N - n + 0.5) / (n + 0.5))`. For `wireless`, `n = 10 006` of `N = 100 000` documents gave **2.30**; for the rarer `keyboard`, `n = 8 326` gave **2.49**. A rarer term weighs more.
- **tf** saturates: `freq / (freq + k1 * (1 - b + b * len/avglen))`. A one-term hit in an average-length field gave **0.4545**; repeating a word does not keep raising the score, and a long field dilutes it.

Two consequences: scores are only comparable *within one query*, and with several shards each computes `idf` from its own statistics (`search_type=dfs_query_then_fetch` fixes that at an extra round trip, rarely worth it).

## 3. Counting and the `track_total_hits` cap

By default Elasticsearch counts hits only up to **10 000** and says so:

```
"total": {"value": 10000, "relation": "gte"}      ← seen for term brand = "acme"
```

Exact counts need `"track_total_hits": true`; measured on `match "wireless keyboard"` on `description` it cost 9-12 ms versus 7-11 ms capped on this data, and the gap grows with the number of matching documents, because capping lets the engine stop collecting early. If the UI only needs "10 000+", leave the default.

## 4. Aggregations

Aggregations run on **doc values** ([2d.1 §4](01-storage-lucene.md#4-where-the-bytes-go)), so fields must be `keyword`, numeric or date.

```esq es-local
POST /play_logs/_search
{"size": 0, "aggs": {"per_hour": {
   "date_histogram": {"field": "@timestamp", "fixed_interval": "1h"},
   "aggs": {"p95": {"percentiles": {"field": "latency_ms", "percents": [95]}}}}}}
```

| Aggregation on `play_logs` (500 000 events) | median `took` |
| --- | --- |
| `terms(path)`, whole index | 5 ms |
| `terms(path)` where `level = ERROR` | 5-8 ms |
| `terms(level)` over the last hour (range filter first) | 6-7 ms |
| `date_histogram` (1 h buckets, 168 buckets) + `percentiles` | **150 ms** (145-357) |

Filtering narrows the work before aggregating; a metric computed for every document in every bucket costs real time. Notes:

- `terms` returns **approximate** top-N counts when the index has several shards; raise `shard_size` for accuracy.
- High-cardinality `terms` (user ids, URLs) is memory-hungry; use `composite` to page through buckets.
- **Request cache** (`size: 0` searches only): second identical request is served from memory. Seen in `_stats`: `request_cache` 1 hit, 1 miss.

## 5. Reading a plan: the `profile` API

```esq es-local
POST /play_products/_search
{"profile": true, "size": 0, "query": {"bool": {"must": [{"match": {"description": "wireless keyboard"}}], "filter": [{"term": {"category": "input"}}]}}}
```

The profile shows each Lucene query, its type and nanoseconds per phase. In the measured run, with `size: 0` (no scores needed), the whole `bool` was rewritten to a `ConstantScoreQuery` of ~15 ms. This is the Elasticsearch equivalent of `EXPLAIN ANALYZE` ([1.3 §7](../../01-fundamentals/03-query-execution.md)): find the slowest child before changing anything.

## 6. Pagination

| Method | Use | Limit / cost |
| --- | --- | --- |
| `from` + `size` | UI pages | `from + size <= 10 000` (`index.max_result_window`), and every shard must produce `from + size` hits |
| `search_after` + sort | infinite scroll, deep pages, exports | needs a stable total order (add a tiebreaker such as `_doc` or `_id`); no jumping to page N |
| Point in time (PIT) + `search_after` | consistent view across pages | PIT holds segments open; close it |
| scroll | legacy bulk export | not for user-facing pagination |

Measured: `from: 10000, size: 10` fails with `Result window is too large, from + size must be less than or equal to: [10000] but was [10010]`. A page at `from: 9000` took **17 ms**; the next page by `search_after` took **4 ms**. The cost of `from` grows with depth, `search_after` stays flat. Raising `max_result_window` hides the problem and moves it to memory.

## 7. Query habits that hurt

- **Leading wildcards** (`*abc`) and regex cannot use the term dictionary order; cost scales with dictionary size. On the 8-brand `brand` field it was cheap (3 ms), but on millions of distinct terms it scans them all. Use n-grams instead.
- **`script` queries and scripted sorts** run per document in Painless.
- **`_source` for everything**: fetching 100 hits with or without `_source` was equal here (2 ms) because documents are tiny; with large documents, `_source` filtering and `docvalue_fields` matter.
- **One giant `bool` with hundreds of clauses**: prefer `terms` or `terms_set` with a list.

## Try it

```esq es-local
POST /play_products/_search
{"size": 0, "aggs": {"by_brand": {"terms": {"field": "brand"}, "aggs": {"avg_price": {"avg": {"field": "price"}}}}}}
```

```esq es-local
POST /play_products/_search
{"size": 3, "query": {"match": {"description": "wireless keyboard"}}, "sort": [{"price": "asc"}, {"_doc": "asc"}], "_source": ["name", "price"]}
```

## Exercises

1. Your search ranks "cheap wireless keyboard" results oddly: items in the "accessories" category outrank "input". Which clause is responsible, and how do you change the query so category stays a pure condition?
2. A product page uses `from`/`size` and a bot requests page 5 000. What happens, and which three mechanisms would you use instead for deep access?
3. Explain why the BM25 score for the same term is lower in a long description than in a short title.
4. A dashboard runs the same `terms` aggregation every second for 200 users. Which two features reduce load, and what does each require?

→ Answers: [appendix/solutions.md](../../appendix/solutions.md#2d3-elasticsearch-queries-and-relevance).

Next: [2d.4 Writes, consistency and concurrency](04-writes-consistency-concurrency.md).

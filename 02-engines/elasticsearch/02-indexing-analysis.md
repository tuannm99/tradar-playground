# 2d.2 Elasticsearch: inverted index, analysis and mappings

> **Level:** practical + internals · **Prerequisites:** [2d.1](01-storage-lucene.md), [1.2](../../01-fundamentals/02-indexing.md) · **After this chapter you can:** explain how text becomes searchable terms, choose between `text` and `keyword`, predict why a query returns nothing, and design a mapping that does not waste disk or memory. Measured on Elasticsearch 8.15.3 with `play_products`.

## 1. The inverted index

A B-tree maps *key → row*. An **inverted index** maps *term → documents containing it*, the way a book's index maps a word to page numbers:

```
wireless  → [3, 17, 21, 84, ...]
keyboard  → [3, 9, 84, 120, ...]       "wireless keyboard" = intersect/union of the lists
```

Lists are sorted and compressed, so intersecting them is fast, and each entry can carry frequency and positions for scoring and phrase search. It answers "which documents contain this word?" extremely well; it is poor at "what is the value of field X in document N?" (that is doc values, [2d.1 §4](01-storage-lucene.md#4-where-the-bytes-go)).

## 2. Analysis: text in, terms out

At index time and at query time, a `text` field passes through an **analyzer**: character filters → **tokenizer** → token filters (lowercase, stop words, stemming...). You can run it directly:

```esq es-local
POST /_analyze
{"analyzer": "standard", "text": "Wireless Mechanical Keyboard, 75% layout!"}
```

Measured result of the **standard** analyzer: `wireless`, `mechanical`, `keyboard`, `75`, `layout`: lowercased, punctuation dropped. With the **english** analyzer on `"Wireless keyboards are running"`: `wireless`, `keyboard`, `run`: stop word `are` removed, plural and `-ing` stemmed. With the **keyword** tokenizer the whole string `Wireless Keyboard` stays one term, case preserved.

Rule: **the query must be analyzed the same way as the field**, or terms never meet.

## 3. `text` vs `keyword`: the classic mistake

| | `text` | `keyword` |
| --- | --- | --- |
| Indexed as | analyzed tokens | the exact string, one term |
| Good for | full-text search, relevance | filters, sorting, aggregations, exact match, ids |
| Sort / aggregate | no (fielddata disabled) | yes (doc values) |

Measured on `play_products` (`name` is `text`, `brand` is `keyword`):

| Query | Hits | Why |
| --- | --- | --- |
| `term` on `name` = `"Wireless Keyboard"` | **0** | the index holds `wireless` and `keyboard`, never the capitalised phrase |
| `term` on `brand` = `"acme"` | 10 000+ | exact term exists |
| `match` on `name` = `"keyboards"` | **0** | the standard analyzer does not stem; only `keyboard` is indexed |
| `match` on `name` = `"keybord"` with `fuzziness: AUTO` | 8 326 | edit distance 1 reaches `keyboard` |
| `match` `"wireless keyboard"` (default OR) | 17 519 | documents with either word |
| same with `operator: and` | 813 | documents with both words |
| `terms` aggregation on `name` | **error** | `Fielddata is disabled on [name]` |

The error on aggregating a `text` field is a feature: it prevents un-inverting the whole index into heap. The standard fix is a **multi-field**, indexing the same value both ways:

```esq es-local
PUT /play_demo_multi
{"mappings": {"properties": {"name": {"type": "text", "fields": {"raw": {"type": "keyword"}}}}}}
```

Search on `name`, aggregate or sort on `name.raw`.

## 4. Mappings

The mapping declares each field's type and indexing options. Choosing is a design decision, not a detail:

- **Prefer explicit mappings** over `dynamic: true`. Dynamic mapping guesses (`"123"` becomes `long`, a string becomes `text` plus a `keyword` sub-field), and every new key creates a new field. Unbounded keys (user ids as field names) cause **mapping explosion**: huge cluster state and memory. The seed uses `"dynamic": "strict"` so unknown fields are rejected.
- A field's type cannot be changed in place; you create a new index and **reindex**.
- Turn off what you never use: `"index": false` (not searchable, still in `_source`), `"doc_values": false` (no sort/agg), `"enabled": false` for an object you only store. `"norms": false` on text fields where length normalisation is unwanted saves memory.
- **Numbers**: use `scaled_float` for prices (seed: `scaling_factor: 100`) and the smallest integer type that fits. Numerics and dates use BKD trees for range search; `price` costs 0.40 MB for points plus 0.20 MB for doc values.
- **`keyword` for identifiers** even if they look numeric, unless you do range queries on them.

### Cost of text analysis, measured

`name` (2-word values) costs 0.17 MB and `description` (7-word values) 0.69 MB on 100 000 documents, roughly proportional to the number of tokens. `brand`, with 8 distinct values, costs 0.12 MB: a keyword's cost is tied to **cardinality** and document count, not to string length.

## 5. Related structures

| Need | Use |
| --- | --- |
| Prefix search / autocomplete | `edge_ngram` analyzer, `search_as_you_type`, or `completion` suggester |
| Typo tolerance | `fuzziness` (costly on large dictionaries), or n-grams |
| Exact phrase | `match_phrase` (needs stored positions) |
| Several languages | a language analyzer per field/subfield |
| Nested objects with correct per-object matching | `nested` type (each object becomes a hidden document; costly) |
| Parent-child | `join` field (slow; prefer denormalising, [2d.4](04-writes-consistency-concurrency.md)) |

## Try it

```esq es-local
GET /play_products/_mapping
```

```esq es-local
POST /play_products/_search
{"size": 0, "track_total_hits": true, "query": {"match": {"name": {"query": "wireless keyboard", "operator": "and"}}}}
```

## Exercises

1. A user searches `"Keyboards"` and your `name` is `text` with the standard analyzer; they get nothing although the catalogue is full of keyboards. Give two different fixes.
2. Why does `term: {name: "wireless"}` find documents while `term: {name: "Wireless"}` does not, even though a user typed it with a capital?
3. You need to filter by `brand`, search text in `name` and show a count per brand. Write the mapping.
4. A service stores each customer's custom attributes as top-level fields (`attr_<key>`). What will go wrong at 100 000 customers, and what is the safer model?

→ Answers: [appendix/solutions.md](../../appendix/solutions.md#2d2-elasticsearch-indexing-and-analysis).

Next: [2d.3 Queries, relevance and pagination](03-queries-and-relevance.md).

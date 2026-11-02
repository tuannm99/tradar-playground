# 2d.4 Elasticsearch: writes, consistency and what replaces transactions

> **Level:** internals + practical · **Prerequisites:** [2d.1](01-storage-lucene.md), [1.4](../../01-fundamentals/04-transactions.md) · **After this chapter you can:** state exactly what Elasticsearch guarantees, use optimistic concurrency with `_seq_no`, design a safe pipeline from a source-of-truth database, and avoid the usual data-loss and duplication traps. Measured on Elasticsearch 8.15.3, single node; replica behaviour is described from the design.

## 1. What is (and is not) guaranteed

| Property | Elasticsearch |
| --- | --- |
| Atomicity | per **document** (single-document write is all-or-nothing). Not across documents; a `_bulk` is a batch of independent operations |
| Isolation | none across documents. Search sees a point-in-time snapshot of segments |
| Durability | translog fsync before acknowledging (`durability: request`, default) |
| Consistency of reads | **near-real-time**: visible after refresh ([2d.1 §3](01-storage-lucene.md#3-the-write-path-buffer-translog-refresh-flush-merge)). Get by `_id` is real-time |
| Transactions | **none** |

Elasticsearch is a **derived, search-optimised store**, not a system of record. The usual architecture keeps truth in PostgreSQL or MongoDB and feeds Elasticsearch ([3.5](../../03-architecture/05-choosing-a-database.md)).

## 2. Concurrency control: `_seq_no` and `_primary_term`

Every write returns a monotonically increasing `_seq_no` (per shard) and the shard's `_primary_term` (incremented on primary failover). Pass them back for **optimistic concurrency**:

| Step | Request | Result (measured) |
| --- | --- | --- |
| 1 | `GET /play_scratch/_doc/1` | `_seq_no: 0`, `_primary_term: 1` |
| 2 | `PUT .../_doc/1?if_seq_no=0&if_primary_term=1` | `updated`, now `_seq_no: 1` |
| 3 | same request again (stale values) | **409** `version_conflict_engine_exception: required seqNo [0], primary term [1]. current document has seqNo [1]` |

This is the same compare-and-set pattern as a conditional update in SQL (`where version = :v`) and the *optimistic* approach of [1.4 §4](../../01-fundamentals/04-transactions.md#optimistic-concurrency-control-occ). On conflict, re-read and retry the whole read-modify-write. `_update` with a `script` is atomic on the shard and also needs `retry_on_conflict`.

Alternatively use **external versioning** (`version_type=external`, your own monotonically increasing number, e.g. the source row's update timestamp or LSN): a write with an older version is rejected, which makes out-of-order or replayed events harmless.

## 3. Writing in bulk

- Use `_bulk` (the seed sends 5 000 documents per request). Measured: loading 100 000 products and 500 000 log events, including index creation, took about **22 s** on a laptop with refresh disabled during the load and one refresh at the end.
- Check the **per-item** `errors` flag. A bulk returns HTTP 200 even when some items failed (mapping errors, conflicts, rejected). The seeder fails the run on `errors: true`.
- Rejections (HTTP 429 `es_rejected_execution_exception`) mean the write queue is full: back off and retry, do not add more concurrency.
- Use deterministic `_id`s when the source has a key. Then retries and replays **overwrite** instead of duplicating (idempotent). Auto-generated ids are faster on append-only logs but duplicate on retry.

## 4. Replication and acknowledgement

Each write goes to the **primary shard**, is applied, then forwarded to all **in-sync replicas** before the client is acknowledged. A write is acknowledged only after the primary and the in-sync replicas have it. `wait_for_active_shards` (default 1: just the primary) controls how many copies must be available to *start* a write. Failover promotes an in-sync replica (new `_primary_term`) and no acknowledged write is lost as long as one in-sync copy survives. With `number_of_replicas: 0` (as in this seed) one disk failure loses the shard.

Measured: on a single node, raising `number_of_replicas` to 1 turned cluster status from `green` to **`yellow` with 1 unassigned shard**: a replica is never placed on the same node as its primary.

## 5. Keeping Elasticsearch in sync with the source of truth

| Pattern | How | Watch out |
| --- | --- | --- |
| Dual write | application writes to both | partial failure leaves them different; no ordering guarantee |
| **Outbox + consumer** | write the row and an outbox event in one DB transaction; a worker indexes with external versioning | ([1.4 §7](../../01-fundamentals/04-transactions.md#7-beyond-one-node)) idempotent consumer needed |
| CDC (Debezium, logical decoding) | stream row changes into a queue, then Elasticsearch | schema changes, backfill |
| Periodic reindex | rebuild from the source | staleness window |

Every sync design should have a **rebuild** path: the index can always be recreated from the source. Use **aliases** so you can build `products_v2` in the background and flip the alias `products` atomically.

## 6. Denormalise: there are no joins

Elasticsearch has no cross-index joins at query time. Choose one:

- **Denormalise** into the document: copy `brand_name` into each product. Fast; updating a brand renames many documents (`update_by_query`).
- **`nested`**: arrays of objects whose fields must match together (each is a hidden document; updating one rewrites the whole parent).
- **`join` (parent-child)**: separate documents with a relation; query cost and memory grow, avoid unless the child count is huge and changes often.
- **Join in the application**: two queries.

Same trade as in MongoDB ([2c.3 §5](../mongodb/03-queries-and-aggregation.md)) and in [1.5](../../01-fundamentals/05-data-modeling.md): model for the query.

## 7. Deletes and data lifecycle

A delete is a tombstone until the segment merges ([2d.1 §2](01-storage-lucene.md#2-immutability-and-its-consequences)). For time-based data do not delete documents: use **time-based indices or data streams** and drop whole indices (instant, frees disk), driven by ILM ([2d.5](05-architecture-clusters.md)).

## Try it

```esq es-local
PUT /play_scratch/_doc/1?refresh=wait_for
{"t": "hello"}
```

```esq es-local
GET /play_scratch/_doc/1
```

Run two of the conditional `PUT`s from §2 with the `_seq_no` returned by the `GET` to reproduce the 409.

## Exercises

1. Two workers read a product, each adds a tag, and each indexes the full document. What happens at the end, and how do you fix it with `_seq_no` and with a script update?
2. An indexer reads change events out of order and sometimes replays old ones. Design the write so the index never goes back in time.
3. Why does a `_bulk` of 5 000 items with HTTP 200 still require inspecting the response?
4. You must show a "just ordered" item in search immediately after checkout. List three approaches and their cost.

→ Answers: [appendix/solutions.md](../../appendix/solutions.md#2d4-elasticsearch-writes-and-consistency).

Next: [2d.5 Architecture: clusters, shards and lifecycle](05-architecture-clusters.md).

# 1.7 Query paradigms: SQL, aggregation pipelines and search DSLs

> **Level:** fundamentals · **Prerequisites:** [1.0](00-sql-primer.md), [1.3](03-query-execution.md) · **After this chapter you can:** translate one analytical question between SQL and a MongoDB pipeline, and explain why forcing SQL habits onto a non-relational engine hurts.

The same question, "number of active products and average price per category, only categories with more than 10, most expensive first", is phrased differently in each engine because each executes differently.

## 1. Declarative: SQL

```sql
select category_id, count(*) as total, avg(price) as avg_price
from products
where status = 'active'
group by category_id
having count(*) > 10
order by avg_price desc;
```

You describe the result; the optimizer picks scans, indexes and join order from statistics. Logical evaluation order: [1.0 §3](00-sql-primer.md#3-queries-and-logical-order).

## 2. Pipeline: MongoDB aggregation

You write the *stages*; documents flow through them in order, and the planner can only reorder a few of them.

```js
db.products.aggregate([
  { $match: { status: 'active' } },                                                        // WHERE
  { $group: { _id: '$category_id', total: { $sum: 1 }, avg_price: { $avg: '$price' } } },  // GROUP BY
  { $match: { total: { $gt: 10 } } },                                                      // HAVING
  { $sort: { avg_price: -1 } }                                                             // ORDER BY
])
```

Rule: put `$match` (and `$sort` if it can use an index) **first**; only the leading stages can use indexes, and early filtering shrinks everything after. Details and `$lookup` costs: [2c.3](../02-engines/mongodb/03-queries-and-aggregation.md).

## 3. Scored search: Elasticsearch-style query DSL

Elasticsearch ([part 2d](../02-engines/elasticsearch/03-queries-and-relevance.md)) separates two questions:

```json
{ "query": { "bool": {
    "filter": [ { "term": { "status": "active" } }, { "range": { "price": { "gte": 100 } } } ],
    "must":   [ { "match": { "title": "wireless keyboard" } } ] } } }
```

- **Filter context** (`filter`, `must_not`): yes/no, no relevance score, and eligible for Elasticsearch's own filter cache.
- **Query context** (`must`, `should`): "how well does it match", scored with BM25.

Put exact constraints (status, ranges, categories) in `filter` and keep `must` for text that needs scoring.

## 4. Translating, not copying

| Habit from SQL | Where it hurts | Do instead |
| --- | --- | --- |
| Normalise and `JOIN` everywhere | MongoDB `$lookup` is costly; Elasticsearch has no joins | model for the access pattern ([1.5](05-data-modeling.md)) |
| `SELECT *` over wide tables | column stores read every requested column | name the columns ([1.1 §4](01-storage.md#4-row-store-vs-column-store)) |
| Filter late, in a later stage | a pipeline stage after `$group` cannot use indexes | filter in the first stage |

## Exercises

1. Write the pipeline for "total `amount` per `status` for orders of the last 30 days" against `playground.play_orders`, then the equivalent SQL for `play_orders`.
2. When can a `$match` placed after `$group` be moved before it without changing the result, and when not?

→ Answers: [appendix/solutions.md](../appendix/solutions.md#17-query-paradigms).

Next: [2a.1 PostgreSQL storage and MVCC](../02-engines/postgres/01-storage-and-mvcc.md) (or pick your engine: [2b ClickHouse](../02-engines/clickhouse/01-storage-mergetree.md), [2c MongoDB](../02-engines/mongodb/01-storage-wiredtiger.md), [2d Elasticsearch](../02-engines/elasticsearch/01-storage-lucene.md)).

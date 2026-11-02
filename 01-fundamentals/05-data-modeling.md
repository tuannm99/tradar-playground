# 1.5 Data modelling for performance

> **Level:** fundamentals · **Prerequisites:** [1.1](01-storage.md)–[1.4](04-transactions.md) · **After this chapter you can:** pick keys and types that keep indexes small, decide between normalising and denormalising, and model for an access pattern instead of for "the data".

Schema decisions are made once and paid for forever. A missing index can be added in minutes; a wrong primary key or document shape means a migration.

## 1. Model for the access pattern

The right question is not "what are the entities?" but **"what are the 5–10 queries that must be fast, and how often does each run?"** Write them down first. A relational model derived from entities alone is correct but may be slow; a document model derived from queries is fast but may duplicate data.

## 2. Normalisation and denormalisation

**Normalised** (3NF): each fact stored once; joins reassemble it. Benefits: no update anomalies, small rows, flexible queries. Cost: joins at read time.

**Denormalised**: copy data to avoid joins or to precompute aggregates. Benefits: fewer reads, simple queries. Cost: **you** keep copies consistent; writes touch several places; storage grows.

Guideline: normalise by default in OLTP; denormalise *deliberately*, for a measured hot read path, and record how the copy is kept in sync (trigger, application code, materialized view, change stream).

| Technique | Where it shines | Watch out |
| --- | --- | --- |
| Materialized view / summary table | dashboards, rollups | refresh lag, maintenance cost |
| Counter column | "N comments" | contention on one hot row; drift |
| Embedded copy (e.g. customer name in an order) | historical record ("name at purchase time") | stale if you meant "current name" |
| Wide denormalised fact table | column stores (ClickHouse) | much cheaper there because unread columns cost nothing |

## 3. Embedding vs referencing (documents)

Embed when the child is **owned by and read with** the parent and its size is bounded (order → line items). Reference when the child is shared, changes independently, or is unbounded (user → millions of events). Embedding gives one read and an atomic single-document write; referencing gives smaller documents and flexibility at the price of a second query or `$lookup`. See [2c.1](../02-engines/mongodb/01-storage-wiredtiger.md#1-the-document-model-and-its-limits).

## 4. Keys

The primary key determines physical layout in clustered engines and is copied into every secondary index there, so its size multiplies.

| Key | Pros | Cons |
| --- | --- | --- |
| Sequential integer / `bigserial` | small, ordered inserts append at the right edge of the B-tree, cache-friendly | guessable; hot right edge under extreme insert rates; sharding needs care |
| Random UUIDv4 | unguessable, generated anywhere | 16 bytes, random inserts split pages across the whole index → cache misses and write amplification on large tables |
| Time-ordered ids (UUIDv7, ULID, Snowflake, MongoDB ObjectId) | roughly sorted → append-friendly **and** generated anywhere | leak creation time; clock skew |
| Natural key | meaningful | changes, is wide, hard to rename |

Default: a compact surrogate key, time-ordered if generated outside the database. In ClickHouse the primary key is not unique — it is a **sort key** chosen for pruning ([2b.2](../02-engines/clickhouse/02-indexes-and-pruning.md)).

## 5. Data types

- **Smallest type that is always enough.** `int` vs `bigint`, `smallint` for enums, `date` vs `timestamp`. Narrow rows mean more rows per page and per cache line.
- **Store time as a real time type in UTC**, never as text.
- **Money: `numeric`/`decimal` or integer minor units**, never floating point.
- **Text**: avoid `text` for low-cardinality enums where an enum/`LowCardinality`/small int works; keep large blobs out of hot rows (PostgreSQL TOASTs them; MongoDB caps documents at 16 MB).
- **JSON columns**: flexible, but queries on inside fields need expression/GIN indexes, and the planner has weaker statistics for them. Promote frequently filtered fields to real columns.
- **NULL** has costs (three-valued logic, index semantics); use it for "unknown", not as a flag.

## 6. Partitioning and clustering data

Splitting a big table by a key (usually time) gives: cheap bulk deletion (`DROP PARTITION`), **partition pruning** for queries that filter on the key, and smaller per-partition indexes. It is **not** a general speed-up: queries without the key touch all partitions, and too many partitions hurt planning and metadata. Details per engine in [2a.5](../02-engines/postgres/05-architecture.md), [2b.2](../02-engines/clickhouse/02-indexes-and-pruning.md), [3.3](../03-architecture/03-partitioning-and-sharding.md).

## 7. Constraints are performance features too

`NOT NULL`, `UNIQUE`, foreign keys, `CHECK` cost a little on write and buy a lot: correctness the optimizer can exploit (a unique index proves "at most one row"), and bugs prevented. Foreign key columns usually need their **own index** on the referencing side, or deletes on the parent scan the child table.

## 8. Time series and event data

- Append-mostly, query by time range and by an entity: partition (or order) by time, then by entity.
- Keep raw events and **roll up** into coarser aggregates (hourly/daily) with materialized views or scheduled jobs; set retention (TTL / partition drop) from day one.
- Choose the right engine: ClickHouse excels; PostgreSQL works to moderate scale with partitioning + BRIN; MongoDB offers time-series collections.

## Try it

```sql pg-local
-- Width matters: compare how many rows fit per page for a narrow vs wide table.
select relname, reltuples::bigint as rows, relpages,
       round(reltuples / nullif(relpages, 0)) as rows_per_page
from pg_class where relname in ('play_orders', 'play_customers');
```

```sql ch-local
-- Same logical data, different sort keys: the sort key decides how many granules a filter reads.
explain indexes = 1 select count() from play_events where user_id = 42;
explain indexes = 1 select count() from play_events_by_ts where user_id = 42;
```

## Exercises

1. Model a blog (users, posts, comments, tags, "likes") first as normalised SQL and then as MongoDB documents. For each of these queries state which model is cheaper: show post with its comments; list a user's latest 20 posts; top 10 tags this week; delete a user.
2. Your table's primary key is `uuid4()` and inserts have become slow at 200 M rows while the table fits in 8× RAM. Explain why and propose two fixes.
3. Why might `price float` produce a wrong invoice total, and what would you store instead?
4. A dashboard sums `amount` by `country` over 3 B events on every page load. List three modelling options and their trade-offs.

→ Answers: [appendix/solutions.md](../appendix/solutions.md#15-data-modelling).

Next: [1.6 Measuring](06-measuring.md).

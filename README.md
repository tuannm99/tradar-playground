# The Database Performance Handbook

A hands-on handbook on **database performance, query optimisation, transactions and architecture**, with **PostgreSQL**, **ClickHouse**, **MongoDB** and **Elasticsearch** as the worked examples. It goes from fundamentals, through practical tuning, down to storage internals, and up to system architecture. Every chapter has runnable examples against a shared dataset and exercises with solutions.

It is designed to be read **inside [tradar](https://github.com/tuannm99/tradar)** (a Neovim database client): the code fences are `.tdb` blocks you run with `rr`. You can equally paste them into `psql`, `clickhouse-client` or `mongosh`.

## How the book is organised

| Part | Contents | Level |
| --- | --- | --- |
| **1. Fundamentals** | SQL primer, storage and I/O, indexing theory, query execution, transactions and concurrency, data modelling, measuring, query paradigms (SQL vs pipelines vs search DSLs) | engine-neutral |
| **2a. Engines · PostgreSQL** | storage and MVCC, indexes, planner and `EXPLAIN`, transactions and locking, architecture and replication, tuning playbook | practical → internals → architecture |
| **2b. Engines · ClickHouse** | MergeTree storage, sparse indexes and pruning, query execution, consistency and mutations, clusters, tuning playbook | |
| **2c. Engines · MongoDB** | documents and WiredTiger, indexes and `explain`, queries and aggregation, transactions and concerns, replica sets and sharding, tuning playbook | |
| **2d. Engines · Elasticsearch** | shards and Lucene segments, inverted index and analysis, queries and relevance, writes and consistency, clusters and lifecycle, tuning playbook | |
| **3. Architecture** | OLTP vs OLAP, replication and consistency, partitioning and sharding, caching and pooling, choosing a database | cross-engine design |
| **Appendix** | [glossary](appendix/glossary.md), [`.tdb` observability cheatsheet](appendix/tdb-cheatsheet.md), [exercise solutions](appendix/solutions.md) | reference |

Each chapter follows the same shape: **header** (level, prerequisites, what you will be able to do) → **explanation** → **"Try it"** runnable blocks with the measured result → **exercises** (solutions in the appendix) → **link to the next chapter**.

### Chapter index

**1 · Fundamentals**
[1.0 SQL primer](01-fundamentals/00-sql-primer.md) · [1.1 Storage](01-fundamentals/01-storage.md) · [1.2 Indexing](01-fundamentals/02-indexing.md) · [1.3 Query execution](01-fundamentals/03-query-execution.md) · [1.4 Transactions](01-fundamentals/04-transactions.md) · [1.5 Data modelling](01-fundamentals/05-data-modeling.md) · [1.6 Measuring](01-fundamentals/06-measuring.md) · [1.7 Query paradigms](01-fundamentals/07-query-paradigms.md)

**2a · Engines: PostgreSQL**
[2a.1 Storage and MVCC](02-engines/postgres/01-storage-and-mvcc.md) · [2a.2 Indexes](02-engines/postgres/02-indexes.md) · [2a.3 Planner and EXPLAIN](02-engines/postgres/03-planner-and-explain.md) · [2a.4 Transactions and locking](02-engines/postgres/04-transactions-and-locking.md) · [2a.5 Architecture](02-engines/postgres/05-architecture.md) · [2a.6 Tuning playbook](02-engines/postgres/06-tuning-playbook.md)

**2b · Engines: ClickHouse**
[2b.1 Storage: MergeTree](02-engines/clickhouse/01-storage-mergetree.md) · [2b.2 Indexes and pruning](02-engines/clickhouse/02-indexes-and-pruning.md) · [2b.3 Query execution](02-engines/clickhouse/03-query-execution.md) · [2b.4 Consistency and mutations](02-engines/clickhouse/04-consistency-and-mutations.md) · [2b.5 Architecture](02-engines/clickhouse/05-architecture.md) · [2b.6 Tuning playbook](02-engines/clickhouse/06-tuning-playbook.md)

**2c · Engines: MongoDB**
[2c.1 Storage: WiredTiger](02-engines/mongodb/01-storage-wiredtiger.md) · [2c.2 Indexes](02-engines/mongodb/02-indexes.md) · [2c.3 Queries and aggregation](02-engines/mongodb/03-queries-and-aggregation.md) · [2c.4 Transactions and concerns](02-engines/mongodb/04-transactions-and-concerns.md) · [2c.5 Architecture](02-engines/mongodb/05-architecture.md) · [2c.6 Tuning playbook](02-engines/mongodb/06-tuning-playbook.md)

**2d · Engines: Elasticsearch**
[2d.1 Storage: Lucene](02-engines/elasticsearch/01-storage-lucene.md) · [2d.2 Indexing and analysis](02-engines/elasticsearch/02-indexing-analysis.md) · [2d.3 Queries and relevance](02-engines/elasticsearch/03-queries-and-relevance.md) · [2d.4 Writes and consistency](02-engines/elasticsearch/04-writes-consistency-concurrency.md) · [2d.5 Clusters and lifecycle](02-engines/elasticsearch/05-architecture-clusters.md) · [2d.6 Tuning playbook](02-engines/elasticsearch/06-tuning-playbook.md)

**3 · Architecture**
[3.1 OLTP vs OLAP](03-architecture/01-oltp-vs-olap.md) · [3.2 Replication and consistency](03-architecture/02-replication-and-consistency.md) · [3.3 Partitioning and sharding](03-architecture/03-partitioning-and-sharding.md) · [3.4 Caching and pooling](03-architecture/04-caching-and-pooling.md) · [3.5 Choosing a database](03-architecture/05-choosing-a-database.md)

## Reading paths

- **I just need a slow query fixed:** [1.6 Measuring](01-fundamentals/06-measuring.md) → the engine's *Tuning playbook* (2a.6 / 2b.6 / 2c.6 / 2d.6) → *Indexes* chapter of that engine.
- **Back-end developer, want solid foundations:** Part 1 in order (start at 1.0 if SQL is new), then 2a.3 and 2a.4.
- **Preparing for a database interview or on-call:** 1.3, 1.4, 2a.1, 2a.4, 3.2, then each engine's internals chapter.
- **Architect / tech lead:** 1.4, 3.1–3.5, the architecture chapter of each engine (2a.5, 2b.5, 2c.5, 2d.5).
- **Learning ClickHouse or MongoDB coming from SQL:** 1.1, 1.5, then part 2b, 2c or 2d (Elasticsearch for search and logs).

## Seed data

All examples run against one small, reproducible dataset created by a Go program.

| Database | Objects | Size |
| --- | --- | --- |
| PostgreSQL | `play_customers` (10 k), `play_orders` (500 k, only a primary key) | ~31 MB heap |
| ClickHouse | `play_events` (3 M, sorted by `(user_id, ts)`), `play_events_by_ts` (same data sorted by `ts`) | ~300 MB raw |
| MongoDB | database `playground`: `play_customers` (10 k), `play_orders` (300 k with embedded `items`) | ~51 MB |
| Elasticsearch | `play_products` (100 k, analysed text + keywords), `play_logs` (500 k events over 7 days); one shard each, no replicas | ~51 MB |

### Start the databases (Docker Compose)

```
docker compose up -d --wait        # PostgreSQL 16, ClickHouse (latest), MongoDB 8, Elasticsearch 8.15 — waits until all are healthy
docker compose run --rm seed       # loads the data (runs the Go program in a golang container)
docker compose down                # stop, keep data;  `down -v` also deletes it
```

What the [compose file](docker-compose.yml) sets up for you:

- **PostgreSQL** (`localhost:5432`, user `postgres`, password `x`) with `pg_stat_statements` preloaded and created, `log_lock_waits` on and `deadlock_timeout = 1s` (used by chapters 2a.4 and 2a.6).
- **ClickHouse** (HTTP `8123`, native `9000`; passwordless `default` user, which recent images only allow from outside the container with `CLICKHOUSE_SKIP_USER_SETUP=1`).
- **MongoDB** (`localhost:27017`) as a **one-node replica set**, initiated automatically by its healthcheck, so the transaction experiments of chapter 2c.4 work.
- **Elasticsearch** (`localhost:9200`, security off, 1 GB heap, single node, no replicas). If the port is taken (another Elasticsearch running), start it on another one: `ES_PORT=9201 docker compose up -d --wait elasticsearch`, and seed with `-es http://localhost:9201`.
- A one-shot **`seed`** service (profile `seed`) so you do not need Go installed. Pass targets after the service name: `docker compose run --rm seed go run . postgres mongo`.

### Run the seed from your own Go toolchain

Requires Go ≥ 1.26 and the databases above (or your own).

```
cd seed
go run .                       # all four
go run . postgres mongo        # a subset (also: clickhouse, elasticsearch)
go run . -pg 'postgres://u:p@host:5432/db' postgres
```

Addresses default to `postgres://postgres:x@localhost:5432/postgres`, `clickhouse://default@localhost:9000/default` (the **native** port 9000; tradar itself uses the HTTP port 8123) and `mongodb://localhost:27017` and `http://localhost:9200`; override with `-pg`, `-ch`, `-mongo`, `-es` or `PLAY_PG`, `PLAY_CH`, `PLAY_MONGO`, `PLAY_ES`. Each run drops and recreates the `play_*` objects, **including the ones created by chapter exercises**, so it is also the "reset" button.

## Using the examples with tradar

Every runnable fence names its connection after the dialect (`sql pg-local`, `sql ch-local`, `js mongo-local`, `esq es-local`). Add the four entries from [`connections.example.toml`](connections.example.toml) to your tradar `connections.toml` once (`:TradarConnections`, paste, save) — they match [`docker-compose.yml`](docker-compose.yml). tradar has no per-project connections file and no inline connection string for SQL/Mongo/ClickHouse fences, so this one-time step cannot live inside the query files. Then open a chapter in Neovim, put the cursor inside a block and press `rr`. `rK` opens a database's built-in cheatsheet. Notes:

- tradar's MongoDB parser is deliberately small: `db.<coll>.<method>(...)`, `explain()` at the end of a `find` chain, `db.runCommand({...})`. There is no `createIndex()` (use `runCommand({createIndexes: ...})`), no sessions or transactions, no JavaScript loops — those examples are shown as plain `js` fences for `mongosh`.
- Elasticsearch blocks are Kibana-style (`GET /index/_search` then a JSON body), one request per block. Where a `<id>` is shown, copy a real `_id` from a previous result.
- Experiments that need **two concurrent sessions** (isolation, locking, deadlocks, MongoDB transactions) are written as step tables for two `psql`/`mongosh` windows.
- Blocks that need `\gset` or other client features say so.

## How the numbers were produced

Examples were run on **PostgreSQL 16**, **ClickHouse 26.10**, **MongoDB 8.3** and **Elasticsearch 8.15.3** on a laptop, against the seed above, and the figures quoted in the text are from those runs (timings vary between machines and runs; the *shape* of the result — which plan, which ratio — is what to compare). Where a statement comes from documentation or design knowledge and was **not** reproduced (for example multi-node replication behaviour, because the test deployments were single nodes), the chapter says so.

## Conventions

- *Level* tags: **fundamentals** (engine-neutral), **practical** (do this), **internals** (how it works), **architecture** (systems around the database).
- Object names in the dataset are prefixed `play_` so they never collide with your own tables.
- "Measured" always refers to a run on the seed.

## Contributing / extending

Add a chapter by copying an existing one's shape. Prefer a reproduced experiment over an assertion, and record version numbers. Planned extensions: Redis and Kafka chapters; MySQL/InnoDB for comparison; a "schema design clinic" with worked redesigns.

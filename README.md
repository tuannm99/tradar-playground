# The Database Performance Handbook

A hands-on handbook on **database performance, query optimisation, transactions and architecture**, with **PostgreSQL**, **ClickHouse** and **MongoDB** as the worked examples. It goes from fundamentals, through practical tuning, down to storage internals, and up to system architecture. Every chapter has runnable examples against a shared dataset and exercises with solutions.

It is designed to be read **inside [tradar](https://github.com/tuannm99/tradar)** (a Neovim database client): the code fences are `.tdb` blocks you run with `rr`. You can equally paste them into `psql`, `clickhouse-client` or `mongosh`.

## How the book is organised

| Part | Contents | Level |
| --- | --- | --- |
| **1. Fundamentals** | storage and I/O, indexing theory, query execution, transactions and concurrency, data modelling, measuring | engine-neutral |
| **2. PostgreSQL** | storage and MVCC, indexes, planner and `EXPLAIN`, transactions and locking, architecture and replication, tuning playbook | practical → internals → architecture |
| **3. ClickHouse** | MergeTree storage, sparse indexes and pruning, query execution, consistency and mutations, clusters, tuning playbook | |
| **4. MongoDB** | documents and WiredTiger, indexes and `explain`, queries and aggregation, transactions and concerns, replica sets and sharding, tuning playbook | |
| **5. Architecture** | OLTP vs OLAP, replication and consistency, partitioning and sharding, caching and pooling, choosing a database | cross-engine design |
| **Appendix** | [glossary](appendix/glossary.md), [`.tdb` observability cheatsheet](appendix/tdb-cheatsheet.md), [exercise solutions](appendix/solutions.md) | reference |

Each chapter follows the same shape: **header** (level, prerequisites, what you will be able to do) → **explanation** → **"Try it"** runnable blocks with the measured result → **exercises** (solutions in the appendix) → **link to the next chapter**.

### Chapter index

**1 · Fundamentals**
[1.1 Storage](01-fundamentals/01-storage.md) · [1.2 Indexing](01-fundamentals/02-indexing.md) · [1.3 Query execution](01-fundamentals/03-query-execution.md) · [1.4 Transactions](01-fundamentals/04-transactions.md) · [1.5 Data modelling](01-fundamentals/05-data-modeling.md) · [1.6 Measuring](01-fundamentals/06-measuring.md)

**2 · PostgreSQL**
[2.1 Storage and MVCC](02-postgres/01-storage-and-mvcc.md) · [2.2 Indexes](02-postgres/02-indexes.md) · [2.3 Planner and EXPLAIN](02-postgres/03-planner-and-explain.md) · [2.4 Transactions and locking](02-postgres/04-transactions-and-locking.md) · [2.5 Architecture](02-postgres/05-architecture.md) · [2.6 Tuning playbook](02-postgres/06-tuning-playbook.md)

**3 · ClickHouse**
[3.1 Storage: MergeTree](03-clickhouse/01-storage-mergetree.md) · [3.2 Indexes and pruning](03-clickhouse/02-indexes-and-pruning.md) · [3.3 Query execution](03-clickhouse/03-query-execution.md) · [3.4 Consistency and mutations](03-clickhouse/04-consistency-and-mutations.md) · [3.5 Architecture](03-clickhouse/05-architecture.md) · [3.6 Tuning playbook](03-clickhouse/06-tuning-playbook.md)

**4 · MongoDB**
[4.1 Storage: WiredTiger](04-mongodb/01-storage-wiredtiger.md) · [4.2 Indexes](04-mongodb/02-indexes.md) · [4.3 Queries and aggregation](04-mongodb/03-queries-and-aggregation.md) · [4.4 Transactions and concerns](04-mongodb/04-transactions-and-concerns.md) · [4.5 Architecture](04-mongodb/05-architecture.md) · [4.6 Tuning playbook](04-mongodb/06-tuning-playbook.md)

**5 · Architecture**
[5.1 OLTP vs OLAP](05-architecture/01-oltp-vs-olap.md) · [5.2 Replication and consistency](05-architecture/02-replication-and-consistency.md) · [5.3 Partitioning and sharding](05-architecture/03-partitioning-and-sharding.md) · [5.4 Caching and pooling](05-architecture/04-caching-and-pooling.md) · [5.5 Choosing a database](05-architecture/05-choosing-a-database.md)

## Reading paths

- **I just need a slow query fixed:** [1.6 Measuring](01-fundamentals/06-measuring.md) → the engine's *Tuning playbook* (2.6 / 3.6 / 4.6) → *Indexes* chapter of that engine.
- **Back-end developer, want solid foundations:** Part 1 in order, then 2.3 and 2.4.
- **Preparing for a database interview or on-call:** 1.3, 1.4, 2.1, 2.4, 5.2, then each engine's internals chapter.
- **Architect / tech lead:** 1.4, 5.1–5.5, the architecture chapter of each engine (2.5, 3.5, 4.5).
- **Learning ClickHouse or MongoDB coming from SQL:** 1.1, 1.5, then Part 3 or 4.

## Seed data

All examples run against one small, reproducible dataset created by a Go program.

| Database | Objects | Size |
| --- | --- | --- |
| PostgreSQL | `play_customers` (10 k), `play_orders` (500 k, only a primary key) | ~31 MB heap |
| ClickHouse | `play_events` (3 M, sorted by `(user_id, ts)`), `play_events_by_ts` (same data sorted by `ts`) | ~300 MB raw |
| MongoDB | database `playground`: `play_customers` (10 k), `play_orders` (300 k with embedded `items`) | ~51 MB |

### Start the databases (Docker Compose)

```
docker compose up -d --wait        # PostgreSQL 16, ClickHouse (latest), MongoDB 8 — waits until all are healthy
docker compose run --rm seed       # loads the data (runs the Go program in a golang container)
docker compose down                # stop, keep data;  `down -v` also deletes it
```

What the [compose file](docker-compose.yml) sets up for you:

- **PostgreSQL** (`localhost:5432`, user `postgres`, password `x`) with `pg_stat_statements` preloaded and created, `log_lock_waits` on and `deadlock_timeout = 1s` (used by chapters 2.4 and 2.6).
- **ClickHouse** (HTTP `8123`, native `9000`; passwordless `default` user, which recent images only allow from outside the container with `CLICKHOUSE_SKIP_USER_SETUP=1`).
- **MongoDB** (`localhost:27017`) as a **one-node replica set**, initiated automatically by its healthcheck, so the transaction experiments of chapter 4.4 work.
- A one-shot **`seed`** service (profile `seed`) so you do not need Go installed. Pass targets after the service name: `docker compose run --rm seed go run . postgres mongo`.

### Run the seed from your own Go toolchain

Requires Go ≥ 1.26 and the databases above (or your own).

```
cd seed
go run .                       # all three
go run . postgres mongo        # a subset
go run . -pg 'postgres://u:p@host:5432/db' postgres
```

Addresses default to `postgres://postgres:x@localhost:5432/postgres`, `clickhouse://default@localhost:9000/default` (the **native** port 9000; tradar itself uses the HTTP port 8123) and `mongodb://localhost:27017`; override with `-pg`, `-ch`, `-mongo` or `PLAY_PG`, `PLAY_CH`, `PLAY_MONGO`. Each run drops and recreates the `play_*` objects, **including the ones created by chapter exercises**, so it is also the "reset" button.

## Using the examples with tradar

Every runnable fence names its connection after the dialect (`sql pg-local`, `sql`→`ch-local`, `js mongo-local`). Add the three entries from [`connections.example.toml`](connections.example.toml) to your tradar `connections.toml` once (`:TradarConnections`, paste, save) — they match [`docker-compose.yml`](docker-compose.yml). tradar has no per-project connections file and no inline connection string for SQL/Mongo/ClickHouse fences, so this one-time step cannot live inside the query files. Then open a chapter in Neovim, put the cursor inside a block and press `rr`. `rK` opens a database's built-in cheatsheet. Notes:

- tradar's MongoDB parser is deliberately small: `db.<coll>.<method>(...)`, `explain()` at the end of a `find` chain, `db.runCommand({...})`. There is no `createIndex()` (use `runCommand({createIndexes: ...})`), no sessions or transactions, no JavaScript loops — those examples are shown as plain `js` fences for `mongosh`.
- Experiments that need **two concurrent sessions** (isolation, locking, deadlocks, MongoDB transactions) are written as step tables for two `psql`/`mongosh` windows.
- Blocks that need `\gset` or other client features say so.

## How the numbers were produced

Examples were run on **PostgreSQL 16**, **ClickHouse 26.10** and **MongoDB 8.3** on a laptop, against the seed above, and the figures quoted in the text are from those runs (timings vary between machines and runs; the *shape* of the result — which plan, which ratio — is what to compare). Where a statement comes from documentation or design knowledge and was **not** reproduced (for example multi-node replication behaviour, because the test deployments were single nodes), the chapter says so.

## Conventions

- *Level* tags: **fundamentals** (engine-neutral), **practical** (do this), **internals** (how it works), **architecture** (systems around the database).
- Object names in the dataset are prefixed `play_` so they never collide with your own tables.
- "Measured" always refers to a run on the seed.

## Contributing / extending

Add a chapter by copying an existing one's shape. Prefer a reproduced experiment over an assertion, and record version numbers. Planned extensions: Redis, Elasticsearch and Kafka chapters; MySQL/InnoDB for comparison; a "schema design clinic" with worked redesigns.

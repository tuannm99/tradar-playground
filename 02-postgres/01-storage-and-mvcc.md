# 2.1 PostgreSQL: storage, MVCC and vacuum

> **Level:** internals · **Prerequisites:** [1.1](../01-fundamentals/01-storage.md), [1.4](../01-fundamentals/04-transactions.md) · **After this chapter you can:** explain what an `UPDATE` physically does, why tables bloat, what vacuum does and what stops it. Examples ran on PostgreSQL 16; load the [seed](../README.md#seed-data) first.

## 1. Where rows live

A table (a **heap**) is a file split into 8 KB **pages**. A page has a header, an array of **line pointers** growing from the front, free space in the middle, and the **tuples** growing from the back. A row's physical address is its **`ctid` = (page number, line pointer)**. Indexes store `ctid`s; that is why they must be updated when a row moves.

```
page: [header][lp1 lp2 lp3 ... →         free space         ← ... tuple3 tuple2 tuple1]
```

Each tuple carries a 23-byte header with, among other fields, **`xmin`** (id of the transaction that created it) and **`xmax`** (id of the transaction that deleted or locked it; `0` if none). Values over ~2 KB are compressed and/or moved out of line to a **TOAST** table, so wide columns do not bloat the main heap but cost an extra lookup when read.

```sql pg-local
select ctid, xmin, xmax, id from play_orders limit 3;
```

```sql pg-local
select relname, relpages, reltuples::bigint, pg_size_pretty(pg_relation_size(oid)) as heap,
       pg_size_pretty(pg_total_relation_size(oid)) as with_indexes_and_toast
from pg_class where relname like 'play_%' and relkind = 'r';
```

## 2. MVCC: what `UPDATE` and `DELETE` do

PostgreSQL never overwrites a tuple in place.

- `INSERT` writes a tuple with `xmin = current txid`.
- `DELETE` sets `xmax = current txid` on the existing tuple.
- `UPDATE` = delete the old tuple (set `xmax`) **and** insert a new tuple (new `xmin`), usually with a new `ctid`.

A snapshot is a set of "which transaction ids are visible". A tuple is visible to a snapshot if its `xmin` committed before the snapshot and its `xmax` is unset, aborted or not yet visible. Readers never take locks on rows, so **readers do not block writers**, and an old version stays around for any snapshot that can still see it.

```sql pg-local
select ctid, xmin, xmax, id, amount from play_orders where id = 1;
update play_orders set amount = amount where id = 1;   -- no value change, still a new tuple
select ctid, xmin, xmax, id, amount from play_orders where id = 1;
```

The `ctid` and `xmin` change. The old tuple is still in the page, marked dead once no snapshot needs it. **An `UPDATE` costs a full row write regardless of how many columns you change.**

### Consequences

- **Dead tuples accumulate** until vacuum reclaims them (bloat).
- **Wide rows are expensive to update** (the whole tuple is rewritten). Split frequently updated narrow columns from rarely updated wide ones.
- **Rollback is nearly free**: the new tuple is simply never visible.
- **`SELECT count(*)` must check visibility** of every row; it cannot read a stored counter.

## 3. HOT updates

If an `UPDATE` changes **no indexed column** and the new tuple fits in **the same page**, PostgreSQL creates a **heap-only tuple (HOT)**: no new index entries are needed and old versions are pruned cheaply inside the page. This is a large saving when it applies.

Measured with 50 000 rows, one `UPDATE` touching every row, no indexed column changed:

| Table | `fillfactor` | HOT updates |
| --- | --- | --- |
| `play_hot` | 70 (30% of each page left free) | **20 982 of 50 000** |
| `play_nohot` | 100 (default, pages full) | **0 of 50 000** |

On the seed's `play_orders` (full pages), a 100 000-row update achieved only 65 HOT updates. Takeaways: HOT needs **free space in the page** (`fillfactor < 100` on update-heavy tables) and **no index on the updated column**; every extra index on a column you update frequently disables it.

```sql pg-local
select relname, n_tup_upd, n_tup_hot_upd from pg_stat_user_tables where relname like 'play_%';
```

## 4. Vacuum

**VACUUM** scans a table and:

1. marks space of dead tuples (no longer visible to any snapshot) as reusable — **within the same file**; it does *not* normally return disk to the OS (`VACUUM FULL` rewrites the table under an exclusive lock and does);
2. removes the corresponding index entries;
3. updates the **visibility map** (pages where all tuples are visible to everyone — enabling index-only scans and letting later vacuums skip them);
4. **freezes** old transaction ids so they never wrap around;
5. updates the **free space map**.

**Autovacuum** runs this in the background when `dead tuples > threshold + scale_factor × rows` (defaults: 50 + 0.2 × rows). On a 500 M-row table the default 20% means 100 M dead tuples before vacuum triggers: lower the scale factor on big tables.

Observed after updating 100 000 rows of `play_orders`:

| Step | Heap size |
| --- | --- |
| before | 31 MB |
| after the update (dead tuples + new versions) | 37 MB |
| after `VACUUM` | 37 MB (space is reusable, not returned) |

```sql pg-local
select relname, n_live_tup, n_dead_tup, last_autovacuum, last_autoanalyze
from pg_stat_user_tables where relname like 'play_%';
```

### What blocks vacuum from removing dead tuples

A dead tuple can be removed only when **no existing snapshot can see it**. The oldest snapshot sets the "xmin horizon". Measured with a read-only transaction left open while another session updated and vacuumed `play_hot`:

| Open transaction | `VACUUM VERBOSE` result |
| --- | --- |
| idle at `READ COMMITTED` | `50000 removed ... 0 are dead but not yet removable` |
| idle at `REPEATABLE READ` | `0 removed ... **50000 are dead but not yet removable**` |

At `READ COMMITTED` each statement takes a fresh snapshot, so an idle transaction holds nothing back (unless it has already written, which assigns it a transaction id). At `REPEATABLE READ` the snapshot lives for the whole transaction. Other horizon holders: long-running queries, **abandoned replication slots**, prepared transactions, and standbys with `hot_standby_feedback`.

```sql pg-local
-- Who holds the horizon back?
select pid, state, now() - xact_start as xact_age, backend_xmin, left(query, 50) as query
from pg_stat_activity where backend_xmin is not null order by xact_age desc;
```

### Transaction id wraparound

Transaction ids are 32-bit and compared modulo 2³²; a tuple older than ~2 billion transactions would appear to be in the future. Freezing prevents this. If autovacuum cannot keep up, PostgreSQL eventually refuses writes to protect data. Monitor `age(datfrozenxid)` and `age(relfrozenxid)`.

## 5. Write-ahead log in one paragraph

Before a changed page may be flushed, its change is appended to the **WAL**. Commit = WAL record flushed (`fsync`). Crash recovery replays WAL from the last checkpoint. WAL volume is a direct measure of write cost: 1 000 single-row updates of `play_orders` produced ~237 KB of WAL in the test (about 237 bytes per updated row, including whatever full-page images the first touch of each page required). `synchronous_commit = off` skips the wait for the flush: you can lose the last few hundred milliseconds of commits on a crash but never corrupt data. See [2.5](05-architecture.md).

## Try it

```sql pg-local
-- Measure WAL generated by a statement.
select pg_current_wal_lsn() as before \gset
update play_orders set amount = amount where id <= 1000;
select pg_wal_lsn_diff(pg_current_wal_lsn(), :'before') as wal_bytes;
```

(`\gset` is a `psql` feature; in a `.tdb` block run the two `select`s separately and subtract.)

## Exercises

1. Run the `ctid`/`xmin` experiment inside a transaction that you roll back. What are `ctid` and `xmin` afterwards, and why?
2. Create a table with `fillfactor = 70` and one with 100, update every row, and compare `n_tup_hot_upd` and `pg_relation_size` after another update round followed by `VACUUM`.
3. Reproduce the "dead but not yet removable" result. Then find the blocker using `pg_stat_activity.backend_xmin`.
4. Why does `select count(*) from big_table` take seconds even with an index, and what are three ways to get a fast approximate or maintained count?
5. A table receives 10 000 updates/s to one narrow column and holds a 4 KB JSON blob per row. Propose a schema change that reduces write amplification.

→ Answers: [appendix/solutions.md](../appendix/solutions.md#21-postgresql-storage-and-mvcc).

Next: [2.2 Indexes](02-indexes.md).

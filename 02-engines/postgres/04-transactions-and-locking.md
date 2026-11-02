# 2a.4 PostgreSQL: transactions, isolation and locking

> **Level:** practical + internals · **Prerequisites:** [1.4](../../01-fundamentals/04-transactions.md), [2a.1](01-storage-and-mvcc.md) · **After this chapter you can:** reproduce every classic anomaly, pick the minimal fix for each, and diagnose lock waits and deadlocks from the catalog views. All outputs below were produced on PostgreSQL 16.

**How to run the experiments.** They need two sessions holding transactions open at the same time. Use two `psql` windows (labelled S1 and S2 below). The numbered steps are the order of execution across both windows.

```
docker exec -it play-pg psql -U postgres       # S1
docker exec -it play-pg psql -U postgres       # S2
```

## 1. Isolation levels in PostgreSQL

| Level | Snapshot taken | Forbids | Notes |
| --- | --- | --- | --- |
| `READ COMMITTED` (default) | at the start of **each statement** | dirty reads | a statement can see rows committed mid-transaction by others |
| `REPEATABLE READ` | at the **first statement** of the transaction | + non-repeatable reads, phantoms | this is *snapshot isolation*; still allows write skew; a conflicting update aborts with `40001` |
| `SERIALIZABLE` | as `REPEATABLE READ` + dependency tracking (SSI) | everything | may abort any transaction with `40001`; **you must retry** |

`READ UNCOMMITTED` behaves as `READ COMMITTED`. Set per transaction: `begin isolation level repeatable read;` or per session with `set default_transaction_isolation`.

## 2. Experiment A — non-repeatable read

| Step | S1 | S2 |
| --- | --- | --- |
| 1 | `begin isolation level read committed;` `select amount from play_orders where id = 1;` | |
| 2 | | `update play_orders set amount = amount + 1000 where id = 1;` (autocommit) |
| 3 | `select amount from play_orders where id = 1;` `commit;` | |

Measured:

| Isolation level of S1 | Step 1 | Step 3 |
| --- | --- | --- |
| `READ COMMITTED` | 177.60 | **1177.60** (sees S2's commit) |
| `REPEATABLE READ` | 177.60 | **177.60** (snapshot unchanged) |

## 3. Experiment B — lost update

Two clients each read `amount`, compute `+1` in application code, and write it back (a read-modify-write).

| Step | S1 / S2 (both) |
| --- | --- |
| 1 | `begin;` `select amount from play_orders where id = 3;` → 100 |
| 2 | (both pause) |
| 3 | `update play_orders set amount = 101 where id = 3;` `commit;` |

Measured result after two concurrent increments from 100:

| Strategy | Final value | Correct? |
| --- | --- | --- |
| read in app, write `amount + 1` computed client-side | **101** | no — one increment lost |
| `update ... set amount = amount + 1` | **102** | yes |

Why the atomic form is safe at `READ COMMITTED`: the second `UPDATE` blocks on the row lock until the first commits, then **re-reads the newest version** of the row and re-evaluates `amount + 1`. Fixes for read-modify-write code, cheapest first:

1. Make it a single atomic statement (`set x = x + n`, or `where` clause as the condition).
2. `select ... for update` before reading, so the second client waits.
3. Run at `REPEATABLE READ` or `SERIALIZABLE` and retry on `40001`.
4. Add an optimistic version column: `update ... set v = v + 1, ... where id = ? and v = ?` and check the affected-row count.

## 4. Experiment C — update conflict at REPEATABLE READ

| Step | S1 | S2 |
| --- | --- | --- |
| 1 | `begin isolation level repeatable read;` `select amount from play_orders where id = 2;` | |
| 2 | | `update play_orders set amount = 2 where id = 2;` (commits) |
| 3 | `update play_orders set amount = 1 where id = 2;` | |

Measured: S1's update fails with

```
ERROR:  could not serialize access due to concurrent update
```

Under snapshot isolation a transaction may not overwrite a row changed after its snapshot. It must roll back and retry from the start.

## 5. Experiment D — write skew

Invariant: at least one doctor stays on call.

```sql
create table play_oncall (doctor text primary key, on_call bool not null);
insert into play_oncall values ('alice', true), ('bob', true);
```

Each session runs: `begin isolation level <L>;` `select count(*) from play_oncall where on_call;` (sees 2) → `update play_oncall set on_call = false where doctor = '<me>';` `commit;`. They touch *different* rows.

| Isolation | Outcome |
| --- | --- |
| `REPEATABLE READ` | both commit → **0 doctors on call** (invariant broken) |
| `SERIALIZABLE` | one commits, the other fails: `ERROR: could not serialize access due to read/write dependencies among transactions` `DETAIL: Reason code: Canceled on identification as a pivot, during write.` `HINT: The transaction might succeed if retried.` → **1 on call** |

Snapshot isolation cannot see that each transaction read a row the other wrote. SSI tracks those read–write dependencies and cancels the "pivot" transaction. Alternative fixes without `SERIALIZABLE`: lock a shared guard row (`select ... from oncall_guard for update`), or enforce the rule with a constraint trigger/exclusion constraint.

## 6. Row locks

| Clause | Lock | Blocks |
| --- | --- | --- |
| `FOR UPDATE` | exclusive on selected rows | other `FOR UPDATE`, `UPDATE`, `DELETE`, `FOR SHARE` |
| `FOR NO KEY UPDATE` | like update but allows FK checks | |
| `FOR SHARE` | shared | writers |
| `FOR KEY SHARE` | weakest (what FK checks take) | only key changes/deletes |
| `... SKIP LOCKED` | skip rows another transaction holds | never waits |
| `... NOWAIT` | error instead of waiting | |

### Experiment E — blocking and diagnosing

| Step | S1 | S2 |
| --- | --- | --- |
| 1 | `begin; select id from play_orders where id = 5 for update;` | |
| 2 | | `update play_orders set status = 'paid' where id = 5;` → hangs |
| 3 | (third session) inspect | |

```sql pg-local
select pid, state, wait_event_type, wait_event, pg_blocking_pids(pid) as blocked_by, left(query, 60) as query
from pg_stat_activity
where datname = current_database() and pid <> pg_backend_pid() and wait_event_type = 'Lock';
```

Measured: the waiting backend shows `wait_event_type = Lock`, `wait_event = transactionid`, and one blocker in `pg_blocking_pids`. In `pg_locks` the waiter has a row with `granted = false`, `locktype = transactionid`, `mode = ShareLock`: **a waiting writer is waiting for the other transaction to finish**, not for the row.

```sql pg-local
select locktype, mode, granted, pid from pg_locks where not granted;
```

Set `log_lock_waits = on` (logs waits longer than `deadlock_timeout`) and `lock_timeout` so pile-ups fail loudly instead of silently queuing.

### Experiment F — work queue with `SKIP LOCKED`

S1 holds `select ... where id = 6 for update` in an open transaction. In S2:

```sql
select id from play_orders where id in (6, 7) for update skip locked;   -- returns only 7
select id from play_orders where id = 6 for update nowait;
-- ERROR:  could not obtain lock on row in relation "play_orders"
```

This is the standard queue pattern: `select ... from jobs where status = 'ready' order by id limit 1 for update skip locked`.

## 7. Experiment G — deadlock

| Step | S1 | S2 |
| --- | --- | --- |
| 1 | `begin; update play_orders set amount = amount where id = 8;` | `begin; update play_orders set amount = amount where id = 9;` |
| 2 | `update ... where id = 9;` (waits) | `update ... where id = 8;` (waits → cycle) |

After `deadlock_timeout` (1 s) one transaction is aborted:

```
ERROR:  deadlock detected
DETAIL:  Process 250 waits for ShareLock on transaction 766; blocked by process 251.
         Process 251 waits for ShareLock on transaction 765; blocked by process 250.
HINT:  See server log for query details.
CONTEXT:  while updating tuple (0,9) in relation "play_orders"
```

Prevention: acquire locks in a **global order** (`select ... where id in (...) order by id for update` then update), keep transactions short, and treat `40P01` as retryable.

## 8. Table-level locks and migrations

DDL takes table locks. `ALTER TABLE ... ADD COLUMN` with a constant default is instant in modern PostgreSQL, but many forms take `ACCESS EXCLUSIVE`, which waits behind **every** running transaction on the table — and every new query then queues behind the waiting `ALTER`. Always set `lock_timeout` before DDL and retry:

```sql
set lock_timeout = '2s';
alter table play_orders add column note text;   -- fails fast if blocked, instead of freezing the table
```

Use `CREATE INDEX CONCURRENTLY`, `ADD CONSTRAINT ... NOT VALID` followed by `VALIDATE CONSTRAINT`, and expand/contract migrations for zero-downtime changes.

## 9. Operational settings

| Setting | Why |
| --- | --- |
| `idle_in_transaction_session_timeout` | kills sessions that hold snapshots and locks while idle |
| `lock_timeout` | bounded wait for locks (especially DDL) |
| `statement_timeout` | caps runaway queries |
| `deadlock_timeout` (1 s) | how long to wait before checking for a deadlock |
| `log_lock_waits` | log lock waits longer than `deadlock_timeout` |
| `max_connections` | each connection = a backend process; use a pooler ([2a.5](05-architecture.md)) |

```sql pg-local
select pid, state, now() - xact_start as xact_age, left(query, 60) as last_query
from pg_stat_activity where state = 'idle in transaction' order by xact_age desc;
```

## Exercises

1. Reproduce experiment B with `select ... for update`; show the second session waiting in `pg_stat_activity`.
2. At `SERIALIZABLE`, write a Python/Go/shell retry loop for experiment D that retries only on SQLSTATE `40001`/`40P01`, with jittered backoff and a max attempts.
3. Build a minimal job queue (`jobs(id, status, payload)`) and run two workers claiming with `skip locked`. Verify no job is claimed twice and show what happens without `skip locked`.
4. Create a phantom: at `READ COMMITTED`, count rows with `status = 'new'` twice around a concurrent insert; repeat at `REPEATABLE READ`. Explain why PostgreSQL's `REPEATABLE READ` shows no phantom.
5. Take a lock-wait with `alter table` queued behind a long query and show the queue of blocked backends behind it.

→ Answers: [appendix/solutions.md](../../appendix/solutions.md#2a4-postgresql-transactions-and-locking).

Next: [2a.5 Architecture](05-architecture.md).

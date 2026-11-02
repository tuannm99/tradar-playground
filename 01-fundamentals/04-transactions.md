# 1.4 Transactions, isolation and concurrency

> **Level:** fundamentals · **Prerequisites:** [1.1](01-storage.md) · **After this chapter you can:** name the anomaly a given isolation level permits, choose between locking, MVCC and optimistic approaches, and write retry logic that is actually correct.

## 1. ACID, precisely

| Letter | Guarantee | Mechanism (typical) |
| --- | --- | --- |
| **A**tomicity | all of a transaction's changes happen, or none | undo/rollback, MVCC visibility, write-ahead log |
| **C**onsistency | the database moves between states that satisfy declared constraints | constraints, triggers — *your* invariants, enforced by the application plus the database |
| **I**solation | concurrent transactions behave as if run in some order, to a degree set by the isolation level | locks, MVCC snapshots, validation |
| **D**urability | committed changes survive a crash | WAL + fsync, replication |

"Consistency" here is not the same word as in CAP ([3.2](../03-architecture/02-replication-and-consistency.md)). Isolation is the subtle one, because it is a *spectrum*.

## 2. Anomalies

Anomalies are the visible symptoms of insufficient isolation. Learn them as concrete interleavings.

| Anomaly | What happens | Example |
| --- | --- | --- |
| **Dirty read** | read another transaction's *uncommitted* change | T1 updates balance, T2 reads it, T1 rolls back |
| **Non-repeatable read** | same row read twice gives different values | T1 reads `amount`, T2 commits an update, T1 reads again |
| **Phantom read** | same predicate returns a different *set of rows* | T1 counts `status='new'`, T2 inserts one, T1 counts again |
| **Lost update** | two read-modify-write cycles overwrite each other | both read 100, both write 101; one increment lost |
| **Write skew** | two transactions read overlapping data, write *different* rows, and jointly break an invariant | two doctors each see "2 on call", each goes off call → 0 on call |
| **Read skew** | reading related rows at different moments sees an inconsistent pair | reading `a` before and `b` after a transfer |

## 3. Isolation levels

The SQL standard defines levels by which anomalies they forbid; real engines implement them differently.

| Level | Dirty | Non-repeatable | Phantom | Write skew |
| --- | --- | --- | --- | --- |
| Read uncommitted | possible* | possible | possible | possible |
| **Read committed** | no | possible | possible | possible |
| **Repeatable read / snapshot** | no | no | engine-dependent (PostgreSQL: no) | **possible** |
| **Serializable** | no | no | no | **no** |

\* PostgreSQL treats read uncommitted as read committed; it never shows dirty data.

**Serializable** means the outcome equals *some* serial order of the transactions — the only level where you can reason about transactions one at a time. It is also the only level that stops write skew.

The levels' vendor-specific meaning matters. For example, "repeatable read" in PostgreSQL is snapshot isolation (stronger than the standard requires, but still allows write skew); in MongoDB, the equivalent is a transaction with `readConcern: snapshot`.

## 4. How engines implement isolation

### Two-phase locking (2PL)

Readers take shared locks, writers exclusive locks, all held until commit. Simple and strict, but **readers block writers and vice versa**, and deadlocks are possible. Used in classical engines and for explicit row locks everywhere.

### MVCC (multi-version concurrency control)

Writers create a **new version** of a row; each reader sees a **snapshot** — the versions committed as of a moment. Readers never block writers. Costs: old versions must be kept and cleaned up (PostgreSQL vacuum; WiredTiger history store), and write–write conflicts still need handling. PostgreSQL, MongoDB/WiredTiger, MySQL InnoDB and Oracle are MVCC-based.

### Optimistic concurrency control (OCC)

Do the work assuming no conflict; at commit, validate and abort if someone else interfered. Cheap when conflicts are rare, wasteful when frequent. MongoDB transactions fail fast with `WriteConflict`; PostgreSQL's `SERIALIZABLE` (SSI) is a form of OCC layered over snapshots. **OCC means the application must retry.**

### Serializable snapshot isolation (SSI)

Snapshot isolation plus tracking of read–write dependencies; when a dangerous pattern (a "pivot") is found, one transaction is aborted with a serialization failure. It prevents write skew without locking everything.

## 5. Locks and deadlocks

- **Row locks** (`SELECT ... FOR UPDATE`, `FOR SHARE`) make a read-then-write sequence safe by serialising access to that row. `SKIP LOCKED` lets workers pick *different* rows (a job queue); `NOWAIT` fails instead of waiting.
- **Table / metadata locks** are taken by DDL; a long transaction can make `ALTER TABLE` wait, and everything queued behind the `ALTER` then waits too.
- **Deadlock**: T1 holds A and waits for B, T2 holds B and waits for A. The engine detects the cycle and aborts one. Prevention: **always acquire locks in a consistent order** (e.g. by primary key), keep transactions short.
- **Lock timeouts** (`lock_timeout`, `statement_timeout`) turn silent pile-ups into clean errors.

## 6. Retries, idempotency and long transactions

- A transaction aborted by deadlock, serialization failure or `WriteConflict` is *expected* behaviour, not a bug. Wrap the **whole** transaction (not a single statement) in a retry loop with a bounded number of attempts and backoff with jitter.
- The transaction body must be **safe to re-run** (no external side effects inside, or make them idempotent with a key).
- Long transactions hold snapshots (blocking cleanup), hold locks, and increase conflict probability. Keep them as short as the invariant allows; do slow work (HTTP calls, user think-time) *outside*.
- Prefer **atomic single-statement updates** (`set n = n + 1`) over read-modify-write. If a single document/row can hold the invariant, no multi-row transaction is needed.

## 7. Beyond one node

- **Two-phase commit (2PC)** coordinates a commit across several resource managers: prepare (all promise), then commit (all do). It is correct but blocks if the coordinator dies after prepare.
- **Sagas** replace one distributed transaction by a sequence of local transactions with compensating actions. You trade isolation for availability and must design for visible intermediate states.
- **Distributed SQL** systems (Spanner, CockroachDB) provide serializable distributed transactions using consensus and synchronized clocks, at a latency cost per commit.
- **Outbox pattern**: write the business change and the "event to publish" in one local transaction, publish asynchronously. Avoids dual-write inconsistency.

## 8. Choosing

| Need | Use |
| --- | --- |
| Counters, balances | atomic update (`set x = x + n`) |
| "Check then act" on one row | `SELECT ... FOR UPDATE`, or conditional update (`where status = 'new'`) |
| Invariant over several rows (on-call, inventory limits) | `SERIALIZABLE` + retry, or lock a parent/guard row |
| Work queue | `FOR UPDATE SKIP LOCKED` |
| Analytics reading a stable view | `REPEATABLE READ` / snapshot, read-only |
| Cross-service workflow | saga/outbox, not a distributed transaction |

## Try it (PostgreSQL)

The chapter's anomalies are reproduced step by step, with measured output, in [2a.4 Transactions and locking](../02-engines/postgres/04-transactions-and-locking.md). The MongoDB equivalent (snapshot transactions, `WriteConflict`, concerns) is [2c.4](../02-engines/mongodb/04-transactions-and-concerns.md); ClickHouse's very different model is [2b.4](../02-engines/clickhouse/04-consistency-and-mutations.md).

## Exercises

1. Write the exact interleaving of two transactions that loses an update at `READ COMMITTED`, then give three different fixes and the cost of each.
2. A booking system must never sell more than 100 tickets. Each purchase does `select count(*)`, then `insert` if `< 100`. Which anomaly breaks it, and which of {row lock on a guard row, `SERIALIZABLE`, unique constraint on seat number} fix it?
3. Explain why a retry loop around a single `UPDATE` inside a larger transaction is wrong, and what the correct retry boundary is.
4. Your service calls a payment API inside a database transaction. List the failure modes and redesign it with the outbox pattern.

→ Answers: [appendix/solutions.md](../appendix/solutions.md#14-transactions).

Next: [1.5 Data modelling](05-data-modeling.md).

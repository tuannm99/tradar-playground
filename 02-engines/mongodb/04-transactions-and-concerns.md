# 2c.4 MongoDB: atomicity, transactions, read and write concerns

> **Level:** practical + internals · **Prerequisites:** [1.4](../../01-fundamentals/04-transactions.md), [2c.1](01-storage-wiredtiger.md) · **After this chapter you can:** decide when a single-document atomic update is enough, run a correct multi-document transaction with retries, and choose read/write concern and read preference for a durability and freshness target. Measured on MongoDB 8.3, single-node replica set.

Transactions need a **replica set** (or sharded cluster). The experiments below need `mongosh`; tradar's parser does not support sessions or transactions.

## 1. Atomicity levels

| Scope | Guarantee |
| --- | --- |
| Single document (any number of fields and nested arrays) | **always atomic and isolated**; readers see before or after, never half |
| Multiple documents, single statement (`updateMany`) | **not** atomic as a group; each document is atomic, others may be visible mid-way, failures can leave a partial result |
| Multi-document transaction (replica set, 4.0+; sharded, 4.2+) | atomic and isolated across documents/collections/databases |

Design first for single-document atomicity: an embedded document + operators such as `$inc`, `$set`, `$push`, `$addToSet`, `$setOnInsert` can express most invariants without a transaction. Measured: `updateOne(..., {$inc: {amount: 1}})` took `amount` from 1 to 2 in a single atomic step, with no read-modify-write in the client.

Conditional updates give optimistic checks in one atomic operation:

```js mongo-local
db.play_orders.updateOne({_id: ObjectId("<id>"), status: "new"}, {$set: {status: "paid"}})
```

If `status` was no longer `new`, `matchedCount` is 0 — the "compare-and-set" idiom for state machines and claims. `findOneAndUpdate` returns the document too, which is the usual way to claim a job.

## 2. Multi-document transactions

Semantics:

- **Snapshot isolation**: reads in a transaction see a snapshot taken at the first operation.
- Writes become visible to others only at commit; aborted transactions leave no trace.
- Commit durability depends on the transaction's `writeConcern`.

```js
// mongosh
const session = db.getMongo().startSession();
const orders = session.getDatabase("playground").play_orders;
session.startTransaction({readConcern: {level: "snapshot"}, writeConcern: {w: "majority"}});
orders.updateOne({_id: someId}, {$set: {amount: 1}});
// another client reading _id: sees the OLD value (measured)
session.abortTransaction();      // or session.commitTransaction()
```

Measured: while the transaction was open and uncommitted, a read outside it returned the old value.

### Conflicts fail fast

Two transactions updating the same document: the second one **errors immediately** (no waiting like row locks):

```js
const s1 = db.getMongo().startSession(), s2 = db.getMongo().startSession();
s1.startTransaction(); s2.startTransaction();
s1.getDatabase("playground").play_orders.updateOne({_id: id}, {$set: {amount: 1}});
s2.getDatabase("playground").play_orders.updateOne({_id: id}, {$set: {amount: 2}});   // throws
```

Measured: `WriteConflict` with `errorLabels: ["TransientTransactionError"]`.

### Retrying correctly

Two error labels guide you:

- `TransientTransactionError`: **retry the whole transaction**.
- `UnknownTransactionCommitResult`: **retry the commit only**.

Drivers provide `withTransaction(callback)` which implements this loop (with a time limit). Use it. Make the callback idempotent and side-effect free. Measured: `session.withTransaction(() => { ... })` returned normally in the test.

### Limits and costs

| Limit | Detail |
| --- | --- |
| Lifetime | 60 s by default (`transactionLifetimeLimitSeconds`) |
| Size | oplog entry limits (since 4.2 large transactions split across entries; keep them small) |
| Locks and snapshot | long transactions hold resources and raise `WriteConflict` probability |
| Collections | cannot create collections inside a transaction in old versions; restrictions relaxed in recent ones |
| Sharded | commit needs a two-phase protocol between shards; higher latency |
| Performance | slower than single-document operations; use sparingly |

**If most of your operations need transactions, revisit the model**: you may want a relational database, or to embed more.

## 3. Write concern

How many replica-set members must acknowledge a write before the call returns.

| `w` | Meaning | Risk |
| --- | --- | --- |
| `0` | no acknowledgement | silent loss |
| `1` | primary acknowledged | write may be **rolled back** if the primary fails before replication |
| `"majority"` | a majority of voting members have it | survives failover; default since 5.0 (`getDefaultRWConcern` showed `w: "majority"`) |
| `n` | n members | |

Add `j: true` to require the journal flush on the acknowledging nodes, and `wtimeout` (ms) to bound waiting. A `w: "majority"` write that times out may still eventually succeed — "timed out" does not mean "did not happen".

## 4. Read concern

| Level | You read | Notes |
| --- | --- | --- |
| `local` | the node's latest data (may later be rolled back) | default for primary reads |
| `available` | like local, but on a shard may include orphaned documents | lowest latency |
| `majority` | data acknowledged by a majority — cannot be rolled back | may be slightly stale |
| `linearizable` | the most recent majority-committed state as of the read start | primary only, slow; for "read what I just wrote" across clients |
| `snapshot` | a consistent snapshot (transactions, or majority-committed point in time) | |

## 5. Read preference

`primary` (default), `primaryPreferred`, `secondary`, `secondaryPreferred`, `nearest`. Reading from secondaries scales reads but exposes **replication lag**: you can miss your own recent write. **Causal consistency** (sessions with `causalConsistency: true`, which requires `majority` read concern) guarantees that reads in a session observe their preceding writes even on secondaries.

## 6. Choosing

| Requirement | Setting |
| --- | --- |
| Never lose an acknowledged write | `w: "majority"`, `j: true` |
| Cheap, loss-tolerant telemetry | `w: 1` or even `0` |
| Read data that cannot roll back | `readConcern: "majority"` |
| Read-your-writes on secondaries | causal consistency session |
| Strict single-document invariants | atomic operators; unique index; conditional update |
| Multi-document invariant | transaction, with `snapshot` read concern and majority write concern |

## Exercises

1. Run the two-session conflict (§2) in mongosh and write the retry wrapper without using `withTransaction`; then replace it with `withTransaction`.
2. Implement a job queue with `findOneAndUpdate({status: "ready"}, {$set: {status: "running", worker: me}}, {sort: {_id: 1}})`. Show two workers never claim the same job; what index makes it efficient?
3. In a three-node replica set (`docker compose`), kill the primary right after a `w: 1` write and show the possible rollback; repeat with `w: "majority"`.
4. Demonstrate a stale read from a secondary, then fix it with a causally consistent session.
5. Express "a user's balance can never go negative" with (a) a conditional update (b) a transaction. Compare cost.

→ Answers: [appendix/solutions.md](../../appendix/solutions.md#2c4-mongodb-transactions-and-concerns).

Next: [2c.5 Architecture](05-architecture.md).

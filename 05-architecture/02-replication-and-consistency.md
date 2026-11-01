# 5.2 Replication, consistency and failure

> **Level:** architecture · **Prerequisites:** [1.4](../01-fundamentals/04-transactions.md) · **After this chapter you can:** reason about what a client can observe after a write with replicas, choose between synchronous and asynchronous replication, and explain CAP and PACELC without slogans.

## 1. Why replicate

1. **Availability**: survive node failures.
2. **Durability**: another copy exists elsewhere.
3. **Read scaling**: serve reads from followers.
4. **Locality**: data near users.
5. **Operational flexibility**: backups, analytics, upgrades.

Replication never gives you something for free: it trades write latency, consistency or complexity for those properties.

## 2. Replication topologies

| Topology | Description | Examples |
| --- | --- | --- |
| **Single-leader (primary/follower)** | one node accepts writes; followers apply its log | PostgreSQL streaming, MongoDB replica set, ClickHouse per shard (any replica may take writes in ReplicatedMergeTree, but parts converge via the log) |
| **Multi-leader** | several nodes accept writes; conflicts must be resolved | multi-region active-active, ClickHouse replicated inserts to any replica (conflict-free because parts are append-only) |
| **Leaderless / quorum** | any node accepts writes; reads and writes use quorums | Dynamo-style systems (Cassandra) |
| **Consensus-based** | writes commit via Raft/Paxos majority | Keeper/ZooKeeper, etcd, Spanner, CockroachDB |

What is replicated: **physical** (bytes of WAL — exact copy), **logical** (row changes — selective, cross-version), or **statement-based** (rare today because of nondeterminism).

## 3. Synchronous vs asynchronous

| | Async | Sync (majority or all) |
| --- | --- | --- |
| Commit latency | local only | + round trip to replicas |
| Data loss on primary failure (RPO) | up to replication lag | zero (for acknowledged commits) |
| Availability under follower failure | unaffected | writes may stall if too few replicas remain |
| Examples | PostgreSQL default; MongoDB `w: 1` | PostgreSQL `synchronous_standby_names`; MongoDB `w: "majority"`; ClickHouse `insert_quorum` |

Semi-synchronous setups (wait for one of several) are the usual compromise. A latency budget that includes a cross-region round trip (~50–150 ms) makes multi-region synchronous commits expensive: decide per workload.

## 4. What readers can observe

With asynchronous followers, a read may come from a replica that has not applied recent writes. Named guarantees, from weakest to strongest:

| Guarantee | Means | How to get it |
| --- | --- | --- |
| **Eventual consistency** | replicas converge if writes stop | default for followers |
| **Monotonic reads** | you never see time go backwards across reads | stick a client to one replica |
| **Read-your-writes** | you see your own earlier writes | read from the primary after a write; or wait for the replica to reach your write's position (LSN/optime); MongoDB causal sessions |
| **Consistent prefix** | you see writes in an order that could have happened | single log order |
| **Linearizability** | operations appear to take effect atomically at some instant between start and end, in real-time order | consensus or single leader with `linearizable`/`majority` + leader check; cost: latency |
| **Serializability** | transactions behave as in some serial order | [1.4](../01-fundamentals/04-transactions.md) — different axis (isolation) |

Don't confuse the two axes: **isolation** (concurrent transactions) vs **consistency of replicas / real-time order** (this chapter). A *strict serializable* system gives both.

### Replication lag in practice

Lag has causes: slow follower disk, long-running queries conflicting with replay, a single large transaction, network, follower CPU. Measure it (`pg_stat_replication`, `replSetGetStatus`, ClickHouse `system.replicas.absolute_delay`) and **alert on it**; design reads that tolerate a bounded lag, and fall back to the primary for the "just wrote" case.

## 5. Failure, failover and split brain

Failures are normal; design the response.

1. **Detection**: heartbeats and timeouts. Too sensitive → flapping failovers; too lax → long outages.
2. **Election/promotion**: pick the most up-to-date follower; use a **majority vote** so two sides of a partition can't both elect leaders.
3. **Fencing**: prevent the old leader from continuing to accept writes (STONITH, lease expiry, epoch/term numbers checked by storage).
4. **Client rerouting**: DNS, virtual IP, proxy, driver topology discovery with `retryWrites`.
5. **Reconciliation**: the old leader may hold unreplicated writes. Async replication loses them (MongoDB rolls back to rollback files; PostgreSQL uses `pg_rewind`). **Decide whether to alert, replay or discard.**

**Split brain** = two nodes believing they are leaders. A majority-quorum design prevents it by making the minority side unavailable for writes.

## 6. CAP and PACELC (carefully)

- **CAP**: in the presence of a network **P**artition, a system must choose between **C**onsistency (linearizable behaviour) and **A**vailability (every non-failing node answers). Partitions will happen, so the real choice is what to do *during* one. It says nothing about normal operation.
- **PACELC**: *if* Partition, trade A vs C; **E**lse (normal operation), trade **L**atency vs **C**onsistency. Synchronous replication = consistency at the cost of latency, in normal times.

| System | During a partition | Normal operation |
| --- | --- | --- |
| PostgreSQL single primary + sync standby | primary refuses writes without a sync standby (C over A) | pays standby latency |
| PostgreSQL async | primary keeps accepting writes, may lose them on failover (A over C) | low latency |
| MongoDB `w: "majority"`, majority read concern | minority side loses writes (C over A) | majority round trip |
| ClickHouse replicated | inserts to any reachable replica succeed; Keeper loss → read-only | eventual consistency across replicas unless quorum set |
| Cassandra-style quorum | tunable by `R + W > N` | tunable |

## 7. Practical decisions

1. State the **RPO** (how much data can you lose) and **RTO** (how long can you be down). They determine sync vs async and automation level.
2. Decide per operation: money movements → majority/sync; page view counters → async.
3. Keep **client behaviour** consistent: retries must be idempotent; timeouts mean "unknown".
4. **Test failover** regularly (game days); the first real failover should not be the first test.
5. Prefer fewer moving parts: a managed HA service often beats a hand-built one.

## Exercises

1. A web app writes to the primary and reads the profile page from a follower. A user changes their name and sees the old name after redirect. Give three fixes with trade-offs.
2. Configure PostgreSQL with one synchronous standby. Kill the standby; what happens to commits? Change to `ANY 1 (s1, s2)` with two standbys and repeat.
3. For a three-node MongoDB replica set across three data centres, write the exact RPO/RTO for `w: 1` and `w: "majority"` when the primary's data centre fails.
4. Explain why `R + W > N` gives read-your-writes in a quorum system and where it still fails (sloppy quorums, concurrent writes).

→ Answers: [appendix/solutions.md](../appendix/solutions.md#52-replication-and-consistency).

Next: [5.3 Partitioning and sharding](03-partitioning-and-sharding.md).

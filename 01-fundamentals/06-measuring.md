# 1.6 Measuring: how to know you improved something

> **Level:** fundamentals · **Prerequisites:** none · **After this chapter you can:** find the queries that actually matter, measure latency honestly, and avoid the common benchmarking traps.

Optimisation without measurement is superstition. This chapter is the method; the tool names per engine are in the playbooks ([2a.6](../02-engines/postgres/06-tuning-playbook.md), [2b.6](../02-engines/clickhouse/06-tuning-playbook.md), [2c.6](../02-engines/mongodb/06-tuning-playbook.md), [2d.6](../02-engines/elasticsearch/06-tuning-playbook.md)) and the [`.tdb` cheatsheet](../appendix/tdb-cheatsheet.md).

## 1. The method

1. **Define the goal as a number**: "p95 of `GET /orders` under 200 ms at 300 req/s", not "make it faster".
2. **Find where time goes** top-down: application → network → database; then the slowest queries; then the slowest operator in the plan.
3. **Form one hypothesis** (missing index, stale stats, too many round trips).
4. **Change one thing.**
5. **Re-measure under the same conditions.** Keep the before/after numbers.
6. **Check side effects** (write speed, memory, other queries).

## 2. Which query to fix first

A query that takes 5 s once a day matters less than one that takes 20 ms and runs 100 times a second. Rank by **total time** = calls × mean time (`pg_stat_statements.total_exec_time`, ClickHouse `system.query_log`, MongoDB profiler / Atlas performance advisor, Elasticsearch search slow log). Then look at worst-case latency separately.

## 3. Latency is a distribution

- **Use percentiles** (p50, p95, p99), not the average. A mean of 50 ms can hide a p99 of 2 s.
- Users experience the tail: a page that makes 20 backend calls hits the p99 of *some* call on most loads ("tail at scale").
- **Little's law**: concurrent requests in flight = arrival rate × latency. If latency doubles at constant arrival rate, you now hold twice as many connections and locks — which makes latency worse. This is how overload spirals.
- Watch **saturation** (queue depth, CPU run queue, connections in use), not just utilisation. Latency explodes near 100% utilisation.

## 4. What to read in a plan

Always compare *estimated* to *actual*; check rows examined versus returned; look for sorts and hashes spilling to disk; for nested loops multiply by loops. [1.3 §7](03-query-execution.md#7-reading-a-plan) has the table.

## 5. Benchmarking traps

| Trap | Effect | Remedy |
| --- | --- | --- |
| Cold vs warm cache | first run reads disk, later runs hit cache | run several times; report both or the warm one explicitly |
| Tiny test data | the planner picks scans because tables fit in a page or two | use realistic row counts and distributions |
| Unrealistic distribution | uniform random hides skew, hot keys, long tails | generate data like production; test the skewed key |
| One client | hides contention | use concurrency matching production |
| Measuring the client | JSON parsing, rendering dominate | measure server-side time (`EXPLAIN ANALYZE`, `query_log`) too |
| Different statistics | plan differs between environments | `ANALYZE` first; compare plans, not only times |
| Ignoring variance | one sample proves nothing | repeat, report spread |
| Optimising the wrong layer | database is fine; the app makes 400 queries | trace the request end to end |

`EXPLAIN ANALYZE` actually executes the statement. For `INSERT`/`UPDATE`/`DELETE`, wrap in `BEGIN; ... ROLLBACK;`. The instrumentation itself adds overhead on some systems; timing-free variants (`timing off`) are useful.

## 6. Observability checklist (any database)

- **Slow query log / profiler** with a threshold (and sampling in production).
- **Per-statement aggregates** (normalised queries, call count, total/mean time, rows).
- **Wait events / lock waits** — is time spent working or waiting?
- **Cache hit ratio** and I/O volume.
- **Replication lag**, connection counts, queue lengths.
- **Resource metrics**: CPU, memory, disk latency/IOPS, network.
- **Growth**: table and index sizes over time; dead-tuple/bloat; part counts.

## 7. A reusable case-file template

When you work a slow query, write down:

```
Query:          <exact text + parameters>
Symptom:        <p95, rows, frequency, who notices>
Environment:    <data size, version, settings, warm/cold>
Baseline:       <plan + time + buffers/rows examined>
Hypothesis:     <one sentence>
Change:         <one change>
After:          <plan + time + buffers/rows examined>
Side effects:   <write cost, size, other queries>
Decision:       <keep / revert / follow up>
```

The habit pays for itself the second time the same symptom appears.

## Try it

```sql pg-local
-- Warm-vs-cold: run twice and compare "shared read" with "shared hit".
explain (analyze, buffers) select count(*) from play_orders where amount > 400;
```

```sql ch-local
-- ClickHouse: read_rows / read_bytes are the honest numbers, not wall-clock time.
select count(), sum(amount) from play_events where user_id = 42;
system flush logs;
select query_duration_ms, read_rows, formatReadableSize(read_bytes) as read_bytes, left(query, 60) as query
from system.query_log where type = 'QueryFinish' and query like '%play_events%' and query not like '%query_log%'
order by event_time desc limit 3;
```

## Exercises

1. Your service handles 400 requests/s, each holding one database connection for 25 ms. How many connections are busy on average (Little's law)? What happens to that number if latency rises to 100 ms and a pool caps at 20?
2. A colleague reports "query went from 800 ms to 40 ms after adding an index". What three questions do you ask before believing it?
3. Take any query from this handbook and fill in the case-file template.

→ Answers: [appendix/solutions.md](../appendix/solutions.md#16-measuring).

Next: [1.7 Query paradigms](07-query-paradigms.md).

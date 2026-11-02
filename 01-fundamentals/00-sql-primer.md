# 1.0 SQL primer

> **Level:** fundamentals · **Prerequisites:** none · **After this chapter you can:** read and write the SQL used throughout the handbook (DDL, DML, queries, joins, aggregation, CTEs) and know which parts are portable between PostgreSQL and ClickHouse.

SQL is *declarative*: you state **what** you want and the planner decides **how** ([1.3](03-query-execution.md)). The examples use the seed tables `play_customers` and `play_orders` ([README](../README.md#seed-data)). Syntax is PostgreSQL's; where ClickHouse differs, a note says so. The standard is only a common core: `SERIAL`, `NOW()`, `ILIKE`, `LIMIT ... OFFSET` and many types are vendor extensions.

## 1. DDL: defining objects

```sql
create table demo_users (
    id         bigserial primary key,          -- PostgreSQL; ClickHouse has no serial/autoincrement
    email      text not null unique,
    status     text not null default 'active',
    created_at timestamptz not null default now()
);

alter table demo_users add column last_login timestamptz;

create index idx_demo_users_status on demo_users (status);   -- B-tree, see 1.2
```

ClickHouse has no `UNIQUE`, foreign keys or `CREATE INDEX` in this sense: a table declares `ENGINE = MergeTree ORDER BY (...)` instead ([2b.2](../02-engines/clickhouse/02-indexes-and-pruning.md)).

## 2. DML: changing data

```sql
insert into demo_users (email, status)
values ('alice@example.com', 'active'), ('bob@example.com', 'inactive');

update demo_users set status = 'active', last_login = now()
where email = 'bob@example.com';           -- always have a WHERE

delete from demo_users where status = 'banned';
```

In PostgreSQL an `UPDATE` writes a new row version ([2a.1](../02-engines/postgres/01-storage-and-mvcc.md)); in ClickHouse `UPDATE`/`DELETE` are asynchronous mutations ([2b.4](../02-engines/clickhouse/04-consistency-and-mutations.md)).

## 3. Queries and logical order

```sql
select id, customer_id, amount
from play_orders
where status = 'paid' and created_at >= now() - interval '30 days'
order by created_at desc
limit 10;
```

The clauses are *evaluated* in this order, which is why a `SELECT` alias cannot be used in `WHERE`:

`FROM / JOIN → WHERE → GROUP BY → HAVING → SELECT → ORDER BY → LIMIT`

`OFFSET` pagination reads and discards every skipped row; prefer keyset pagination (`where id < :last_id order by id desc limit 20`).

## 4. Joins

```sql
-- INNER: only customers that have matching orders
select c.name, o.amount
from play_customers c
join play_orders o on o.customer_id = c.id;

-- LEFT: all customers, with NULLs where there is no order
select c.name, o.amount
from play_customers c
left join play_orders o on o.customer_id = c.id;
```

How the engine executes a join (nested loop, hash, merge) is in [1.3](03-query-execution.md).

## 5. Aggregation

```sql
select customer_id, count(*) as orders, sum(amount) as revenue
from play_orders
where status = 'paid'
group by customer_id
having sum(amount) > 1000;       -- HAVING filters groups; WHERE filters rows
```

## 6. CTEs

```sql
with big_spenders as (
    select customer_id, sum(amount) as revenue
    from play_orders
    group by customer_id
    having sum(amount) > 5000
)
select c.name, b.revenue
from play_customers c
join big_spenders b on b.customer_id = c.id;
```

A CTE is for readability. In PostgreSQL 12+ a non-recursive, single-use CTE is inlined; add `MATERIALIZED` to force it to be computed once.

## Exercises

1. Why does `select amount * 2 as x from play_orders where x > 10` fail, and how do you fix it two ways?
2. Rewrite the `LEFT JOIN` above to list only customers with **no** orders.
3. What is the difference between `WHERE` and `HAVING`? Give a query that needs both.

Next: [1.1 Storage](01-storage.md).

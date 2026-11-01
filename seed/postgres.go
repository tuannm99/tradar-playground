package main

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// Tables: play_customers (10k) and play_orders (500k, primary key only; the
// chapters add the other indexes themselves).
var postgresSteps = []string{
	// Objects created by chapter exercises, so a re-seed returns to a clean state.
	`drop table if exists play_log, play_corr, play_part, play_child, play_parent, play_oncall, play_hot, play_nohot cascade`,
	`drop table if exists play_orders`,
	`drop table if exists play_customers`,
	`create table play_customers (
		id      int primary key,
		name    text not null,
		country text not null
	)`,
	`insert into play_customers
	 select g, 'user ' || g, (array['VN','US','JP','DE'])[1 + g % 4]
	 from generate_series(1, 10000) g`,
	`create table play_orders (
		id          bigserial primary key,
		customer_id int not null references play_customers(id),
		status      text not null,
		amount      numeric(10,2) not null,
		created_at  timestamptz not null
	)`,
	// customer_id and status are uniformly distributed; created_at spans 365 days.
	`insert into play_orders (customer_id, status, amount, created_at)
	 select 1 + floor(random() * 10000)::int,
	        (array['new','paid','shipped','cancelled'])[1 + floor(random() * 4)::int],
	        round((random() * 500)::numeric, 2),
	        now() - random() * interval '365 days'
	 from generate_series(1, 500000)`,
	`analyze play_customers`,
	`analyze play_orders`,
}

func seedPostgres(ctx context.Context, dsn string) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	for _, q := range postgresSteps {
		if _, err := conn.Exec(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

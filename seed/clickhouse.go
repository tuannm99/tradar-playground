package main

import (
	"context"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// play_events is sorted by (user_id, ts); play_events_by_ts holds the same data
// sorted by (ts), to compare the effect of the ORDER BY key (chapter 3.2).
var clickhouseSteps = []string{
	// Objects created by chapter exercises, so a re-seed returns to a clean state.
	`drop view if exists play_daily_mv`,
	`drop table if exists play_daily`,
	`drop table if exists play_tiny`,
	`drop table if exists play_events`,
	`drop table if exists play_events_by_ts`,
	`drop table if exists play_users`,
	`create table play_events (
		ts      DateTime,
		user_id UInt32,
		country LowCardinality(String),
		event   LowCardinality(String),
		amount  Float64,
		payload String
	) engine = MergeTree
	partition by toYYYYMM(ts)
	order by (user_id, ts)`,
	// 3 million events, 100k users, spread over the last ~180 days.
	`insert into play_events
	 select
	   now() - toIntervalSecond(rand() % (180 * 86400)),
	   rand() % 100000,
	   ['VN','US','JP','DE'][1 + rand() % 4],
	   ['view','click','buy','refund'][1 + rand() % 4],
	   round((rand() % 50000) / 100, 2),
	   repeat(char(97 + rand() % 26), 100)
	 from numbers(3000000)`,
	`create table play_events_by_ts engine = MergeTree
	 partition by toYYYYMM(ts)
	 order by (ts)
	 as select * from play_events`,
}

func seedClickHouse(ctx context.Context, dsn string) error {
	opts, err := clickhouse.ParseDSN(dsn)
	if err != nil {
		return err
	}
	conn, err := clickhouse.Open(opts)
	if err != nil {
		return err
	}
	defer conn.Close()
	for _, q := range clickhouseSteps {
		if err := conn.Exec(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

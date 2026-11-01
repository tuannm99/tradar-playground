// Seed dữ liệu mẫu cho playground/*.md: Postgres, ClickHouse, MongoDB.
//
//	go run . [postgres] [clickhouse] [mongo]     (không đối số = cả ba)
//
// Mỗi lần chạy xoá rồi tạo lại bảng/collection `play_*` nên chạy lại được.
// Địa chỉ lấy từ cờ hoặc biến môi trường (xem mặc định bên dưới); các container
// thử nhanh nằm ở README.md cùng thư mục.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	pg := flag.String("pg", env("PLAY_PG", "postgres://postgres:x@localhost:5432/postgres"), "DSN Postgres ($PLAY_PG)")
	ch := flag.String("ch", env("PLAY_CH", "clickhouse://default@localhost:9000/default"), "DSN ClickHouse, cổng native ($PLAY_CH)")
	mg := flag.String("mongo", env("PLAY_MONGO", "mongodb://localhost:27017"), "URI MongoDB ($PLAY_MONGO)")
	flag.Parse()

	seeders := map[string]func(context.Context, string) error{
		"postgres":   seedPostgres,
		"clickhouse": seedClickHouse,
		"mongo":      seedMongo,
	}
	addrs := map[string]string{"postgres": *pg, "clickhouse": *ch, "mongo": *mg}

	targets := flag.Args()
	if len(targets) == 0 {
		targets = []string{"postgres", "clickhouse", "mongo"}
	}
	ctx := context.Background()
	failed := false
	for _, t := range targets {
		fn, ok := seeders[t]
		if !ok {
			fmt.Fprintf(os.Stderr, "không biết đích %q (postgres | clickhouse | mongo)\n", t)
			os.Exit(2)
		}
		start := time.Now()
		fmt.Printf("%-10s ... ", t)
		if err := fn(ctx, addrs[t]); err != nil {
			fmt.Printf("LỖI: %v\n", err)
			failed = true
			continue
		}
		fmt.Printf("xong (%s)\n", time.Since(start).Round(time.Millisecond))
	}
	if failed {
		os.Exit(1)
	}
}

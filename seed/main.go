// Seed sample data for the handbook chapters: PostgreSQL, ClickHouse, MongoDB.
//
//	go run . [postgres] [clickhouse] [mongo]     (no arguments = all three)
//
// Every run drops and recreates the `play_*` tables/collections, so it is
// repeatable. Addresses come from flags or environment variables (defaults below);
// see ../README.md for quick Docker containers.
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
	pg := flag.String("pg", env("PLAY_PG", "postgres://postgres:x@localhost:5432/postgres"), "PostgreSQL DSN ($PLAY_PG)")
	ch := flag.String("ch", env("PLAY_CH", "clickhouse://default@localhost:9000/default"), "ClickHouse DSN, native port ($PLAY_CH)")
	mg := flag.String("mongo", env("PLAY_MONGO", "mongodb://localhost:27017"), "MongoDB URI ($PLAY_MONGO)")
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
			fmt.Fprintf(os.Stderr, "unknown target %q (postgres | clickhouse | mongo)\n", t)
			os.Exit(2)
		}
		start := time.Now()
		fmt.Printf("%-10s ... ", t)
		if err := fn(ctx, addrs[t]); err != nil {
			fmt.Printf("ERROR: %v\n", err)
			failed = true
			continue
		}
		fmt.Printf("done (%s)\n", time.Since(start).Round(time.Millisecond))
	}
	if failed {
		os.Exit(1)
	}
}

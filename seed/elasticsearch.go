package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"
)

// Elasticsearch objects (plain HTTP, no client library):
//
//	play_products - 100k documents with analysed text (name, description) and keyword fields
//	play_logs     - 500k log events spread over 7 days, created through a time-ordered index
const (
	esProducts = 100_000
	esLogs     = 500_000
	esBatch    = 5_000
)

var (
	esAdjectives = []string{"wireless", "mechanical", "ergonomic", "compact", "portable", "gaming", "silent", "premium", "budget", "waterproof"}
	esNouns      = []string{"keyboard", "mouse", "headphones", "monitor", "webcam", "speaker", "charger", "cable", "stand", "router", "microphone", "laptop"}
	esBrands     = []string{"acme", "globex", "initech", "umbrella", "hooli", "stark", "wayne", "wonka"}
	esCategories = []string{"input", "audio", "display", "power", "network", "accessories"}
	esServices   = []string{"api", "auth", "billing", "search", "worker"}
	esLevels     = []string{"INFO", "INFO", "INFO", "INFO", "WARN", "ERROR"}
	esPaths      = []string{"/v1/orders", "/v1/users", "/v1/search", "/v1/login", "/v1/pay", "/healthz"}
)

type esClient struct{ base string }

func (c esClient) do(ctx context.Context, method, path, ctype string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 && !(method == "DELETE" && resp.StatusCode == 404) {
		return out, fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func (c esClient) bulk(ctx context.Context, index string, docs func(i int) any, from, to int) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for i := from; i < to; i++ {
		buf.WriteString(`{"index":{}}` + "\n")
		if err := enc.Encode(docs(i)); err != nil {
			return err
		}
	}
	out, err := c.do(ctx, "POST", "/"+index+"/_bulk", "application/x-ndjson", buf.Bytes())
	if err != nil {
		return err
	}
	var r struct{ Errors bool }
	if json.Unmarshal(out, &r) == nil && r.Errors {
		return fmt.Errorf("bulk into %s reported item errors: %.300s", index, out)
	}
	return nil
}

func seedElasticsearch(ctx context.Context, url string) error {
	c := esClient{base: strings.TrimRight(url, "/")}
	for _, idx := range []string{"play_products", "play_logs"} {
		if _, err := c.do(ctx, "DELETE", "/"+idx, "", nil); err != nil {
			return err
		}
	}
	const productMapping = `{
	  "settings": {"number_of_shards": 1, "number_of_replicas": 0, "refresh_interval": "-1"},
	  "mappings": {"dynamic": "strict", "properties": {
	    "name":        {"type": "text"},
	    "description": {"type": "text"},
	    "brand":       {"type": "keyword"},
	    "category":    {"type": "keyword"},
	    "price":       {"type": "scaled_float", "scaling_factor": 100},
	    "rating":      {"type": "half_float"},
	    "in_stock":    {"type": "boolean"},
	    "created_at":  {"type": "date"}
	  }}}`
	const logMapping = `{
	  "settings": {"number_of_shards": 1, "number_of_replicas": 0, "refresh_interval": "-1"},
	  "mappings": {"dynamic": "strict", "properties": {
	    "@timestamp": {"type": "date"},
	    "service":    {"type": "keyword"},
	    "level":      {"type": "keyword"},
	    "path":       {"type": "keyword"},
	    "status":     {"type": "short"},
	    "latency_ms": {"type": "integer"},
	    "message":    {"type": "text"}
	  }}}`
	if _, err := c.do(ctx, "PUT", "/play_products", "application/json", []byte(productMapping)); err != nil {
		return err
	}
	if _, err := c.do(ctx, "PUT", "/play_logs", "application/json", []byte(logMapping)); err != nil {
		return err
	}

	now := time.Now().UTC()
	pick := func(s []string) string { return s[rand.IntN(len(s))] }
	product := func(int) any {
		name := pick(esAdjectives) + " " + pick(esNouns)
		desc := fmt.Sprintf("%s %s %s with %s and %s, by %s", pick(esAdjectives), pick(esNouns), pick(esNouns), pick(esAdjectives), pick(esAdjectives), pick(esBrands))
		return map[string]any{
			"name": name, "description": desc, "brand": pick(esBrands), "category": pick(esCategories),
			"price":      math.Round(rand.Float64()*50000) / 100,
			"rating":     math.Round(rand.Float64()*40+10) / 10,
			"in_stock":   rand.IntN(10) > 1,
			"created_at": now.Add(-time.Duration(rand.Float64() * float64(365*24*time.Hour))).Format(time.RFC3339),
		}
	}
	logEvent := func(int) any {
		level := pick(esLevels)
		status := 200
		if level == "ERROR" {
			status = 500
		} else if level == "WARN" {
			status = 429
		}
		p := pick(esPaths)
		return map[string]any{
			"@timestamp": now.Add(-time.Duration(rand.Float64() * float64(7*24*time.Hour))).Format(time.RFC3339Nano),
			"service":    pick(esServices), "level": level, "path": p, "status": status,
			"latency_ms": int(math.Exp(rand.NormFloat64()*0.8 + 3.5)),
			"message":    fmt.Sprintf("%s %s completed with status %d", level, p, status),
		}
	}
	for _, job := range []struct {
		index string
		n     int
		gen   func(int) any
	}{{"play_products", esProducts, product}, {"play_logs", esLogs, logEvent}} {
		for from := 0; from < job.n; from += esBatch {
			if err := c.bulk(ctx, job.index, job.gen, from, min(from+esBatch, job.n)); err != nil {
				return err
			}
		}
		// Make the data searchable and restore a normal refresh interval.
		if _, err := c.do(ctx, "PUT", "/"+job.index+"/_settings", "application/json", []byte(`{"index":{"refresh_interval":"1s"}}`)); err != nil {
			return err
		}
		if _, err := c.do(ctx, "POST", "/"+job.index+"/_refresh", "", nil); err != nil {
			return err
		}
	}
	return nil
}

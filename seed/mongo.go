package main

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const (
	mongoDB        = "playground"
	mongoCustomers = 10_000
	mongoOrders    = 300_000
	mongoBatch     = 10_000
)

var (
	countries = []string{"VN", "US", "JP", "DE"}
	statuses  = []string{"new", "paid", "shipped", "cancelled"}
)

// Database `playground`: play_customers (10k) và play_orders (300k, mỗi đơn có
// mảng `items` nhúng). Chỉ có index `_id` — mongo.md tự thêm phần còn lại.
func seedMongo(ctx context.Context, uri string) error {
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		return err
	}
	defer client.Disconnect(ctx)

	db := client.Database(mongoDB)
	for _, c := range []string{"play_orders", "play_customers"} {
		if err := db.Collection(c).Drop(ctx); err != nil {
			return err
		}
	}

	customers := make([]any, 0, mongoCustomers)
	for i := 1; i <= mongoCustomers; i++ {
		customers = append(customers, bson.D{
			{Key: "_id", Value: i},
			{Key: "name", Value: fmt.Sprintf("user %d", i)},
			{Key: "country", Value: countries[i%4]},
		})
	}
	if _, err := db.Collection("play_customers").InsertMany(ctx, customers); err != nil {
		return err
	}

	now := time.Now()
	unordered := options.InsertMany().SetOrdered(false)
	orders := db.Collection("play_orders")
	for done := 0; done < mongoOrders; done += mongoBatch {
		batch := make([]any, 0, mongoBatch)
		for range mongoBatch {
			items := make(bson.A, 1+rand.IntN(3))
			for i := range items {
				items[i] = bson.D{
					{Key: "sku", Value: fmt.Sprintf("sku-%d", rand.IntN(500))},
					{Key: "qty", Value: 1 + rand.IntN(5)},
				}
			}
			age := time.Duration(rand.Float64() * float64(365*24*time.Hour))
			batch = append(batch, bson.D{
				{Key: "customerId", Value: 1 + rand.IntN(mongoCustomers)},
				{Key: "status", Value: statuses[rand.IntN(4)]},
				{Key: "amount", Value: math.Round(rand.Float64()*50000) / 100},
				{Key: "createdAt", Value: bson.NewDateTimeFromTime(now.Add(-age))},
				{Key: "items", Value: items},
			})
		}
		if _, err := orders.InsertMany(ctx, batch, unordered); err != nil {
			return err
		}
	}
	return nil
}

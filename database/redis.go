package database

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"
)

func ConnectRedis(redisURL string) *redis.Client {
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		opts = &redis.Options{Addr: "localhost:6379"}
	}

	client := redis.NewClient(opts)

	if err := client.Ping(context.Background()).Err(); err != nil {
		fmt.Printf("Warning: Redis connection failed: %v\n", err)
	}

	return client
}

package cache_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/DeviosLang/shirakami/internal/cache"
)

func startRedis(t *testing.T) *redis.Client {
	t.Helper()
	ctx := context.Background()
	container, err := tcredis.Run(ctx, "redis:7-alpine")
	if err != nil {
		t.Fatalf("start redis container: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	addr, err := container.Endpoint(ctx, "")
	if err != nil {
		t.Fatalf("redis endpoint: %v", err)
	}
	return redis.NewClient(&redis.Options{Addr: addr})
}

func TestCacheKey_Deterministic(t *testing.T) {
	key1 := cache.CacheKey("same input", []string{})
	key2 := cache.CacheKey("same input", []string{})
	if key1 != key2 {
		t.Errorf("expected same key for same input, got %s vs %s", key1, key2)
	}
}

func TestCacheKey_DifferentInput(t *testing.T) {
	key1 := cache.CacheKey("input A", []string{})
	key2 := cache.CacheKey("input B", []string{})
	if key1 == key2 {
		t.Errorf("expected different keys for different input")
	}
}

func TestCache_SetGet(t *testing.T) {
	rdb := startRedis(t)
	c := cache.New(rdb)
	ctx := context.Background()

	result := &cache.AnalysisResult{
		TaskID:        "task-1",
		CallChain:     json.RawMessage(`["a","b"]`),
		TestScenarios: "scenario 1",
		EntryPoints:   json.RawMessage(`["ep1"]`),
		TokenUsage:    1234,
		StepCount:     5,
		CreatedAt:     time.Now().UTC().Truncate(time.Second),
	}

	key := cache.CacheKey("test input", []string{})
	if err := c.Set(ctx, key, result, 0); err != nil {
		t.Fatalf("Set: %v", err)
	}

	got, ok := c.Get(ctx, key)
	if !ok {
		t.Fatal("expected cache hit, got miss")
	}
	if got.TaskID != result.TaskID {
		t.Errorf("TaskID mismatch: %s vs %s", got.TaskID, result.TaskID)
	}
	if got.TokenUsage != result.TokenUsage {
		t.Errorf("TokenUsage mismatch: %d vs %d", got.TokenUsage, result.TokenUsage)
	}
}

func TestCache_Miss(t *testing.T) {
	rdb := startRedis(t)
	c := cache.New(rdb)
	ctx := context.Background()

	_, ok := c.Get(ctx, "nonexistent-key")
	if ok {
		t.Error("expected cache miss, got hit")
	}
}

func TestCache_TTLExpiry(t *testing.T) {
	rdb := startRedis(t)
	c := cache.New(rdb)
	ctx := context.Background()

	result := &cache.AnalysisResult{TaskID: "task-ttl", CreatedAt: time.Now()}
	key := cache.CacheKey("ttl test", []string{})

	if err := c.Set(ctx, key, result, 100*time.Millisecond); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, ok := c.Get(ctx, key); !ok {
		t.Fatal("expected hit before TTL expiry")
	}

	time.Sleep(200 * time.Millisecond)

	if _, ok := c.Get(ctx, key); ok {
		t.Error("expected miss after TTL expiry, got hit")
	}
}

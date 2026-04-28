package integration

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/DeviosLang/shirakami/internal/cache"
)

func TestCacheKey_SameInputSameKey(t *testing.T) {
	key1 := cache.CacheKey("identical input", []string{})
	key2 := cache.CacheKey("identical input", []string{})
	if key1 != key2 {
		t.Errorf("CacheKey: same input produced different keys: %s vs %s", key1, key2)
	}
}

func TestCacheKey_DifferentInputDifferentKey(t *testing.T) {
	key1 := cache.CacheKey("input A", []string{})
	key2 := cache.CacheKey("input B", []string{})
	if key1 == key2 {
		t.Error("CacheKey: different inputs produced the same key")
	}
}

func TestCacheKey_DifferentGitHashDifferentKey(t *testing.T) {
	// Without real git repos, different path strings cause different keys.
	key1 := cache.CacheKey("same input", []string{"/repo/commit-aaa"})
	key2 := cache.CacheKey("same input", []string{"/repo/commit-bbb"})
	if key1 == key2 {
		t.Error("CacheKey: different repo paths produced the same key")
	}
}

func TestCache_SetGetRoundTrip(t *testing.T) {
	rdb := startRedisClient(t)
	c := cache.New(rdb)
	ctx := context.Background()

	result := &cache.AnalysisResult{
		TaskID:        "task-round-trip",
		CallChain:     json.RawMessage(`["handler","service","repo"]`),
		TestScenarios: "scenario A",
		EntryPoints:   json.RawMessage(`["POST /api/v1/foo"]`),
		TokenUsage:    999,
		StepCount:     7,
		CreatedAt:     time.Now().UTC().Truncate(time.Second),
	}

	key := cache.CacheKey("round trip input", []string{})
	if err := c.Set(ctx, key, result, 0); err != nil {
		t.Fatalf("Set: %v", err)
	}

	got, ok := c.Get(ctx, key)
	if !ok {
		t.Fatal("expected cache hit, got miss")
	}
	if got.TaskID != result.TaskID {
		t.Errorf("TaskID: got %s, want %s", got.TaskID, result.TaskID)
	}
	if got.TokenUsage != result.TokenUsage {
		t.Errorf("TokenUsage: got %d, want %d", got.TokenUsage, result.TokenUsage)
	}
	if got.StepCount != result.StepCount {
		t.Errorf("StepCount: got %d, want %d", got.StepCount, result.StepCount)
	}
}

func TestCache_MissOnUnknownKey(t *testing.T) {
	rdb := startRedisClient(t)
	c := cache.New(rdb)
	ctx := context.Background()

	_, ok := c.Get(ctx, "this-key-does-not-exist")
	if ok {
		t.Error("expected cache miss for unknown key, got hit")
	}
}

func TestCache_TTLExpiry(t *testing.T) {
	rdb := startRedisClient(t)
	c := cache.New(rdb)
	ctx := context.Background()

	result := &cache.AnalysisResult{
		TaskID:    "task-ttl",
		CreatedAt: time.Now().UTC(),
	}
	key := cache.CacheKey("ttl expiry test", []string{})

	if err := c.Set(ctx, key, result, 100*time.Millisecond); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, ok := c.Get(ctx, key); !ok {
		t.Fatal("expected cache hit before TTL expiry")
	}

	time.Sleep(200 * time.Millisecond)

	if _, ok := c.Get(ctx, key); ok {
		t.Error("expected cache miss after TTL expiry, got hit")
	}
}

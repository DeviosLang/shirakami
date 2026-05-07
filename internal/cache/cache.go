package cache

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const defaultTTL = 72 * time.Hour

// AnalysisResult is the cached result of an analysis task.
type AnalysisResult struct {
	TaskID           string          `json:"task_id"`
	CallChain        json.RawMessage `json:"call_chain,omitempty"`
	TestScenarios    string          `json:"test_scenarios,omitempty"`
	EntryPoints      json.RawMessage `json:"entry_points,omitempty"`
	FunctionAnalyses json.RawMessage `json:"function_analyses,omitempty"`
	ImpactSummary    string          `json:"impact_summary,omitempty"`
	TokenUsage       int             `json:"token_usage"`
	StepCount        int             `json:"step_count"`
	CreatedAt        time.Time       `json:"created_at"`
}

// Cache wraps a Redis client for analysis result caching.
type Cache struct {
	rdb *redis.Client
	ttl time.Duration
}

// New creates a new Cache backed by a Redis client.
func New(rdb *redis.Client) *Cache {
	return &Cache{rdb: rdb, ttl: defaultTTL}
}

// CacheKey produces a deterministic cache key from input content and the HEAD
// commit hashes of the supplied repo paths. If git is unavailable for a repo
// the path itself is used as a fallback so the key still changes when the
// input changes.
func CacheKey(inputContent string, repoPaths []string) string {
	h := sha256.New()
	h.Write([]byte(inputContent))
	for _, p := range repoPaths {
		hash := headHash(p)
		h.Write([]byte(p))
		h.Write([]byte(hash))
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// headHash returns the HEAD commit hash for the git repo at path.
// Returns an empty string on error.
func headHash(repoPath string) string {
	out, err := exec.Command("git", "-C", repoPath, "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Get retrieves a cached AnalysisResult by key.
// Returns (nil, false) on miss or error.
func (c *Cache) Get(ctx context.Context, key string) (*AnalysisResult, bool) {
	data, err := c.rdb.Get(ctx, cacheKeyPrefix(key)).Bytes()
	if err != nil {
		return nil, false
	}
	var result AnalysisResult
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, false
	}
	return &result, true
}

// Set stores an AnalysisResult under the given key with the default TTL.
func (c *Cache) Set(ctx context.Context, key string, result *AnalysisResult, ttl time.Duration) error {
	if ttl == 0 {
		ttl = c.ttl
	}
	data, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("marshal result: %w", err)
	}
	return c.rdb.Set(ctx, cacheKeyPrefix(key), data, ttl).Err()
}

// Delete removes the cached entry for the given raw cache key (as stored in
// the tasks table). Returns nil if the key did not exist.
func (c *Cache) Delete(ctx context.Context, key string) error {
	return c.rdb.Del(ctx, cacheKeyPrefix(key)).Err()
}

// DeleteAll removes every shirakami:cache:* entry from Redis.
// Returns the number of deleted keys and any error.
func (c *Cache) DeleteAll(ctx context.Context) (int64, error) {
	const pattern = "shirakami:cache:*"
	var cursor uint64
	var total int64
	for {
		keys, next, err := c.rdb.Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			return total, fmt.Errorf("scan cache keys: %w", err)
		}
		if len(keys) > 0 {
			n, err := c.rdb.Del(ctx, keys...).Result()
			if err != nil {
				return total, fmt.Errorf("del cache keys: %w", err)
			}
			total += n
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return total, nil
}

func cacheKeyPrefix(key string) string {
	return "shirakami:cache:" + key
}

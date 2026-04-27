package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/DeviosLang/shirakami/internal/checkpoint"
	"github.com/redis/go-redis/v9"
)

// Progress holds the high-level task state stored in Redis.
type Progress struct {
	TaskID        string    `json:"task_id"`
	CurrentStep   int       `json:"current_step"`
	AnalyzedNodes []string  `json:"analyzed_nodes"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Layer2 wraps checkpoint.Checkpointer and exposes higher-level task progress
// operations. It uses Redis as the backing store via the Checkpointer.
type Layer2 struct {
	cp  *checkpoint.Checkpointer
	rdb *redis.Client
}

// NewLayer2 creates a Layer2 instance backed by a Redis client.
func NewLayer2(rdb *redis.Client) *Layer2 {
	return &Layer2{
		cp:  checkpoint.New(rdb),
		rdb: rdb,
	}
}

// progressKey returns the Redis key used for storing Progress records.
func progressKey(taskID string) string {
	return "shirakami:progress:" + taskID
}

const progressTTL = 24 * time.Hour

// UpdateProgress persists the current analysis step and the set of already-
// analysed node identifiers for the given task.
func (l *Layer2) UpdateProgress(ctx context.Context, taskID string, currentStep int, analyzedNodes []string) error {
	if analyzedNodes == nil {
		analyzedNodes = []string{}
	}
	p := Progress{
		TaskID:        taskID,
		CurrentStep:   currentStep,
		AnalyzedNodes: analyzedNodes,
		UpdatedAt:     time.Now().UTC(),
	}

	data, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("layer2 marshal progress: %w", err)
	}
	return l.rdb.Set(ctx, progressKey(taskID), data, progressTTL).Err()
}

// GetProgress retrieves the current task progress from Redis.
// Returns (nil, nil) when no progress record exists for the task.
func (l *Layer2) GetProgress(ctx context.Context, taskID string) (*Progress, error) {
	data, err := l.rdb.Get(ctx, progressKey(taskID)).Bytes()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("layer2 get progress: %w", err)
	}
	var p Progress
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("layer2 unmarshal progress: %w", err)
	}
	return &p, nil
}

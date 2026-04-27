package checkpoint_test

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/DeviosLang/shirakami/internal/checkpoint"
	"github.com/DeviosLang/shirakami/internal/llm"
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

func TestCheckpointer_SaveLoadDelete(t *testing.T) {
	rdb := startRedis(t)
	cp := checkpoint.New(rdb)
	ctx := context.Background()

	taskID := "task-abc"
	messages := []llm.Message{
		llm.UserMessage{Content: "analyze this diff"},
		llm.AssistantMessage{Content: "I'll look at the call chain"},
		llm.ToolResultMessage{ToolCallID: "tc1", Content: "tool output"},
	}
	stepCount := 3

	if err := cp.Save(ctx, taskID, messages, stepCount); err != nil {
		t.Fatalf("Save: %v", err)
	}

	state, restored, err := cp.Load(ctx, taskID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if state == nil {
		t.Fatal("expected non-nil state")
	}
	if state.TaskID != taskID {
		t.Errorf("TaskID: want %s got %s", taskID, state.TaskID)
	}
	if state.StepCount != stepCount {
		t.Errorf("StepCount: want %d got %d", stepCount, state.StepCount)
	}
	if len(restored) != len(messages) {
		t.Fatalf("messages count: want %d got %d", len(messages), len(restored))
	}

	// Verify message types and content round-trip
	if um, ok := restored[0].(llm.UserMessage); !ok || um.Content != "analyze this diff" {
		t.Errorf("restored[0] mismatch: %+v", restored[0])
	}
	if am, ok := restored[1].(llm.AssistantMessage); !ok || am.Content != "I'll look at the call chain" {
		t.Errorf("restored[1] mismatch: %+v", restored[1])
	}
	if tr, ok := restored[2].(llm.ToolResultMessage); !ok || tr.ToolCallID != "tc1" {
		t.Errorf("restored[2] mismatch: %+v", restored[2])
	}

	// Delete and verify miss
	if err := cp.Delete(ctx, taskID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	state2, msgs2, err := cp.Load(ctx, taskID)
	if err != nil {
		t.Fatalf("Load after Delete: %v", err)
	}
	if state2 != nil || msgs2 != nil {
		t.Error("expected nil state after Delete")
	}
}

func TestCheckpointer_MissOnUnknownKey(t *testing.T) {
	rdb := startRedis(t)
	cp := checkpoint.New(rdb)
	ctx := context.Background()

	state, msgs, err := cp.Load(ctx, "no-such-task")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if state != nil || msgs != nil {
		t.Error("expected nil on miss")
	}
}

func TestCheckpointer_TTLExpiry(t *testing.T) {
	rdb := startRedis(t)
	// Create checkpointer with very short TTL via direct Redis call
	cp := checkpoint.New(rdb)
	ctx := context.Background()

	taskID := "task-ttl"
	msgs := []llm.Message{llm.UserMessage{Content: "hello"}}

	if err := cp.Save(ctx, taskID, msgs, 1); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Override TTL to 100ms directly
	rdb.Expire(ctx, "shirakami:checkpoint:"+taskID, 100*time.Millisecond)

	time.Sleep(200 * time.Millisecond)

	state, _, err := cp.Load(ctx, taskID)
	if err != nil {
		t.Fatalf("Load after expiry: %v", err)
	}
	if state != nil {
		t.Error("expected nil state after TTL expiry")
	}
}

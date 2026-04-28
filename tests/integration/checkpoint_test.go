package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/DeviosLang/shirakami/internal/checkpoint"
	"github.com/DeviosLang/shirakami/internal/llm"
)

func TestCheckpointer_SaveLoadRestoresFullState(t *testing.T) {
	rdb := startRedis(t)
	cp := checkpoint.New(rdb)
	ctx := context.Background()

	taskID := "cp-task-001"
	messages := []llm.Message{
		llm.UserMessage{Content: "analyze this diff"},
		llm.AssistantMessage{Content: "I will trace the call chain"},
		llm.ToolResultMessage{ToolCallID: "tc-1", Content: "tool output here"},
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
		t.Fatal("expected non-nil state after Save")
	}
	if state.TaskID != taskID {
		t.Errorf("TaskID: got %s, want %s", state.TaskID, taskID)
	}
	if state.StepCount != stepCount {
		t.Errorf("StepCount: got %d, want %d", state.StepCount, stepCount)
	}
	if len(restored) != len(messages) {
		t.Fatalf("message count: got %d, want %d", len(restored), len(messages))
	}

	// Verify individual message round-trips.
	if um, ok := restored[0].(llm.UserMessage); !ok || um.Content != "analyze this diff" {
		t.Errorf("restored[0]: got %+v, want UserMessage{analyze this diff}", restored[0])
	}
	if am, ok := restored[1].(llm.AssistantMessage); !ok || am.Content != "I will trace the call chain" {
		t.Errorf("restored[1]: got %+v, want AssistantMessage{I will trace...}", restored[1])
	}
	if tr, ok := restored[2].(llm.ToolResultMessage); !ok || tr.ToolCallID != "tc-1" {
		t.Errorf("restored[2]: got %+v, want ToolResultMessage{tc-1}", restored[2])
	}
}

func TestCheckpointer_MissOnUnknownKey(t *testing.T) {
	rdb := startRedis(t)
	cp := checkpoint.New(rdb)
	ctx := context.Background()

	state, msgs, err := cp.Load(ctx, "no-such-task-id")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if state != nil || msgs != nil {
		t.Error("expected nil state and messages for unknown task, got non-nil")
	}
}

func TestCheckpointer_DeleteRemovesKey(t *testing.T) {
	rdb := startRedis(t)
	cp := checkpoint.New(rdb)
	ctx := context.Background()

	taskID := "cp-delete-task"
	msgs := []llm.Message{llm.UserMessage{Content: "hello"}}

	if err := cp.Save(ctx, taskID, msgs, 1); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := cp.Delete(ctx, taskID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	state, _, err := cp.Load(ctx, taskID)
	if err != nil {
		t.Fatalf("Load after Delete: %v", err)
	}
	if state != nil {
		t.Error("expected nil state after Delete, got non-nil")
	}
}

func TestCheckpointer_TTLExpiry(t *testing.T) {
	rdb := startRedis(t)
	cp := checkpoint.New(rdb)
	ctx := context.Background()

	taskID := "cp-ttl-task"
	msgs := []llm.Message{llm.UserMessage{Content: "ttl test"}}

	if err := cp.Save(ctx, taskID, msgs, 1); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Override TTL to something very short directly via Redis.
	if err := rdb.Expire(ctx, "shirakami:checkpoint:"+taskID, 100*time.Millisecond).Err(); err != nil {
		t.Fatalf("Expire: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	state, _, err := cp.Load(ctx, taskID)
	if err != nil {
		t.Fatalf("Load after expiry: %v", err)
	}
	if state != nil {
		t.Error("expected nil state after TTL expiry, got non-nil")
	}
}

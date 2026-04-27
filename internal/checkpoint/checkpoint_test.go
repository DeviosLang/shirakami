package checkpoint

import (
	"encoding/json"
	"testing"

	"github.com/DeviosLang/shirakami/internal/llm"
)

func toolCall(id, name string) llm.ToolCall {
	return llm.ToolCall{ID: id, Name: name, Arguments: json.RawMessage(`{}`)}
}

func TestFileCheckpointer_SaveAndLoad(t *testing.T) {
	dir := t.TempDir()
	cp, err := NewFileCheckpointer(dir)
	if err != nil {
		t.Fatalf("NewFileCheckpointer: %v", err)
	}

	messages := []llm.Message{
		llm.UserMessage{Content: "hello"},
		llm.AssistantMessage{
			Content:   "thinking...",
			ToolCalls: []llm.ToolCall{toolCall("tc1", "search")},
		},
		llm.ToolResultMessage{ToolCallID: "tc1", Content: "result"},
	}

	if err := cp.Save("task1", 2, messages); err != nil {
		t.Fatalf("Save: %v", err)
	}

	state, err := cp.Load("task1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if state == nil {
		t.Fatal("Load returned nil for existing checkpoint")
	}
	if state.StepCount != 2 {
		t.Errorf("StepCount = %d, want 2", state.StepCount)
	}
	if len(state.Messages) != len(messages) {
		t.Errorf("message count = %d, want %d", len(state.Messages), len(messages))
	}

	restored, err := RestoreMessages(state.Messages)
	if err != nil {
		t.Fatalf("RestoreMessages: %v", err)
	}
	if len(restored) != len(messages) {
		t.Fatalf("restored count = %d, want %d", len(restored), len(messages))
	}

	// Verify round-trip for each message type.
	if um, ok := restored[0].(llm.UserMessage); !ok || um.Content != "hello" {
		t.Errorf("restored[0]: got %T %v, want UserMessage{Content:hello}", restored[0], restored[0])
	}
	if am, ok := restored[1].(llm.AssistantMessage); !ok || am.Content != "thinking..." {
		t.Errorf("restored[1]: got %T, want AssistantMessage", restored[1])
	} else if len(am.ToolCalls) != 1 || am.ToolCalls[0].ID != "tc1" {
		t.Errorf("restored[1] tool calls = %v, want [{tc1 search}]", am.ToolCalls)
	}
	if tr, ok := restored[2].(llm.ToolResultMessage); !ok || tr.ToolCallID != "tc1" {
		t.Errorf("restored[2]: got %T, want ToolResultMessage", restored[2])
	}
}

func TestFileCheckpointer_LoadMissingReturnsNil(t *testing.T) {
	dir := t.TempDir()
	cp, _ := NewFileCheckpointer(dir)

	state, err := cp.Load("nonexistent")
	if err != nil {
		t.Fatalf("Load of missing checkpoint should not error: %v", err)
	}
	if state != nil {
		t.Errorf("expected nil state for missing checkpoint, got %+v", state)
	}
}

func TestFileCheckpointer_Delete(t *testing.T) {
	dir := t.TempDir()
	cp, _ := NewFileCheckpointer(dir)

	if err := cp.Save("del-task", 1, []llm.Message{llm.UserMessage{Content: "x"}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := cp.Delete("del-task"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	state, err := cp.Load("del-task")
	if err != nil {
		t.Fatalf("Load after delete: %v", err)
	}
	if state != nil {
		t.Error("expected nil after delete")
	}

	// Deleting again should be a no-op.
	if err := cp.Delete("del-task"); err != nil {
		t.Errorf("second Delete should not error: %v", err)
	}
}

func TestFileCheckpointer_Overwrite(t *testing.T) {
	dir := t.TempDir()
	cp, _ := NewFileCheckpointer(dir)

	cp.Save("t", 1, []llm.Message{llm.UserMessage{Content: "first"}})
	cp.Save("t", 5, []llm.Message{llm.UserMessage{Content: "second"}})

	state, err := cp.Load("t")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if state.StepCount != 5 {
		t.Errorf("StepCount = %d, want 5 (overwritten)", state.StepCount)
	}
}

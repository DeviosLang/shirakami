package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/DeviosLang/shirakami/internal/checkpoint"
	"github.com/DeviosLang/shirakami/internal/llm"
)

// mockLLMClient is a deterministic LLM client for testing.
type mockLLMClient struct {
	responses []*llm.Response
	calls     int
}

func (m *mockLLMClient) Complete(_ context.Context, _ []llm.Message, _ []llm.ToolDefinition) (*llm.Response, error) {
	if m.calls >= len(m.responses) {
		return nil, fmt.Errorf("mockLLMClient: no more responses (call %d)", m.calls)
	}
	resp := m.responses[m.calls]
	m.calls++
	return resp, nil
}

// mockTool is a no-op tool for testing.
type mockTool struct {
	name   string
	result string
}

func (t *mockTool) Definition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        t.name,
		Description: "mock tool " + t.name,
		Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
	}
}

func (t *mockTool) Execute(_ context.Context, _ []byte) (string, error) {
	return t.result, nil
}

func makeToolCall(id, name string) llm.ToolCall {
	return llm.ToolCall{ID: id, Name: name, Arguments: json.RawMessage(`{}`)}
}

// TestAgentLoop_ThreeToolUsesThenEndTurn checks the canonical happy-path:
// 3 tool_use responses followed by 1 end_turn.
func TestAgentLoop_ThreeToolUsesThenEndTurn(t *testing.T) {
	echo := &mockTool{name: "echo", result: "pong"}

	client := &mockLLMClient{
		responses: []*llm.Response{
			{StopReason: llm.StopReasonToolUse, ToolCalls: []llm.ToolCall{makeToolCall("tc1", "echo")}},
			{StopReason: llm.StopReasonToolUse, ToolCalls: []llm.ToolCall{makeToolCall("tc2", "echo")}},
			{StopReason: llm.StopReasonToolUse, ToolCalls: []llm.ToolCall{makeToolCall("tc3", "echo")}},
			{StopReason: llm.StopReasonEndTurn, Content: "analysis complete"},
		},
	}

	loop := NewAgentLoop(client, []Tool{echo}, 0, nil, "")
	result, err := loop.Run(context.Background(), "test-task", "analyse this")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Content != "analysis complete" {
		t.Errorf("content = %q, want %q", result.Content, "analysis complete")
	}
	if result.StepCount != 4 {
		t.Errorf("stepCount = %d, want 4", result.StepCount)
	}
	if result.Truncated {
		t.Error("Truncated should be false")
	}
	if client.calls != 4 {
		t.Errorf("llm called %d times, want 4", client.calls)
	}
}

// TestAgentLoop_MaxStepsForceTermination ensures the loop stops at the budget limit.
func TestAgentLoop_MaxStepsForceTermination(t *testing.T) {
	echo := &mockTool{name: "echo", result: "pong"}

	responses := make([]*llm.Response, 10)
	for i := range responses {
		responses[i] = &llm.Response{
			StopReason: llm.StopReasonToolUse,
			ToolCalls:  []llm.ToolCall{makeToolCall(fmt.Sprintf("tc%d", i), "echo")},
		}
	}

	client := &mockLLMClient{responses: responses}
	loop := NewAgentLoop(client, []Tool{echo}, 3, nil, "")
	result, err := loop.Run(context.Background(), "truncation-test", "task")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !result.Truncated {
		t.Error("expected Truncated=true when budget exceeded")
	}
	if result.StepCount > 3 {
		t.Errorf("stepCount = %d, expected <= 3", result.StepCount)
	}
}

// TestAgentLoop_CheckpointResume verifies that a loop resumes from step 3.
func TestAgentLoop_CheckpointResume(t *testing.T) {
	dir := t.TempDir()

	cp, err := checkpoint.NewFileCheckpointer(dir)
	if err != nil {
		t.Fatalf("create checkpointer: %v", err)
	}

	// Pre-seed the checkpoint at stepCount=3.
	priorMessages := []llm.Message{
		llm.UserMessage{Content: "task"},
		llm.AssistantMessage{Content: "", ToolCalls: []llm.ToolCall{makeToolCall("tc1", "echo")}},
		llm.ToolResultMessage{ToolCallID: "tc1", Content: "pong"},
		llm.AssistantMessage{Content: "", ToolCalls: []llm.ToolCall{makeToolCall("tc2", "echo")}},
		llm.ToolResultMessage{ToolCallID: "tc2", Content: "pong"},
		llm.AssistantMessage{Content: "", ToolCalls: []llm.ToolCall{makeToolCall("tc3", "echo")}},
		llm.ToolResultMessage{ToolCallID: "tc3", Content: "pong"},
	}
	if err := cp.Save("resume-task", 3, priorMessages); err != nil {
		t.Fatalf("save checkpoint: %v", err)
	}

	client := &mockLLMClient{
		responses: []*llm.Response{
			{StopReason: llm.StopReasonEndTurn, Content: "resumed result"},
		},
	}
	echo := &mockTool{name: "echo", result: "pong"}
	loop := NewAgentLoop(client, []Tool{echo}, 0, cp, "")

	result, err := loop.Run(context.Background(), "resume-task", "task")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Content != "resumed result" {
		t.Errorf("content = %q, want %q", result.Content, "resumed result")
	}
	// 3 (from checkpoint) + 1 (new end_turn step) = 4
	if result.StepCount != 4 {
		t.Errorf("stepCount = %d, want 4", result.StepCount)
	}
}

// TestAgentLoop_MessageChainCorrectness verifies that ToolResultMessages are
// included in subsequent LLM calls.
func TestAgentLoop_MessageChainCorrectness(t *testing.T) {
	echo := &mockTool{name: "echo", result: "pong"}

	rc := &recordingLLMClient{
		responses: []*llm.Response{
			{StopReason: llm.StopReasonToolUse, ToolCalls: []llm.ToolCall{makeToolCall("t1", "echo")}},
			{StopReason: llm.StopReasonEndTurn, Content: "done"},
		},
	}

	loop := NewAgentLoop(rc, []Tool{echo}, 0, nil, "system-prompt")
	if _, err := loop.Run(context.Background(), "chain-task", "user-task"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(rc.calls) != 2 {
		t.Fatalf("expected 2 LLM calls, got %d", len(rc.calls))
	}

	// Second call must include a ToolResultMessage for "t1".
	foundToolResult := false
	for _, m := range rc.calls[1] {
		if tr, ok := m.(llm.ToolResultMessage); ok && tr.ToolCallID == "t1" {
			foundToolResult = true
			break
		}
	}
	if !foundToolResult {
		t.Error("second LLM call did not include ToolResultMessage for t1")
	}
}

// recordingLLMClient records all message slices passed to Complete.
type recordingLLMClient struct {
	responses []*llm.Response
	calls     [][]llm.Message
	idx       int
}

func (r *recordingLLMClient) Complete(_ context.Context, messages []llm.Message, _ []llm.ToolDefinition) (*llm.Response, error) {
	snapshot := make([]llm.Message, len(messages))
	copy(snapshot, messages)
	r.calls = append(r.calls, snapshot)

	if r.idx >= len(r.responses) {
		return nil, fmt.Errorf("recordingLLMClient: no more responses")
	}
	resp := r.responses[r.idx]
	r.idx++
	return resp, nil
}

// ── not_found dedup tests ────────────────────────────────────────────────────

// notFoundTool returns a JSON not_found response for every Execute call,
// simulating what file_reader returns when the file does not exist.
type notFoundTool struct {
	name string
}

func (t *notFoundTool) Definition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        t.name,
		Description: "always returns not_found",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"file_path":{"type":"string"}}}`),
	}
}

func (t *notFoundTool) Execute(_ context.Context, _ []byte) (string, error) {
	return `{"error":"not_found","hint":"use ripgrep to search for the correct file path before retrying"}`, nil
}

// makeToolCallWithArgs builds a ToolCall carrying a JSON file_path argument.
func makeToolCallWithArgs(id, name, filePath string) llm.ToolCall {
	args := fmt.Sprintf(`{"file_path":%q}`, filePath)
	return llm.ToolCall{ID: id, Name: name, Arguments: json.RawMessage(args)}
}

// TestAgentLoop_NotFoundDedup_FirstFailNoInject verifies that the first
// not_found response for a path does NOT inject a blocking UserMessage —
// the LLM is allowed one retry before the hard stop is applied.
func TestAgentLoop_NotFoundDedup_FirstFailNoInject(t *testing.T) {
	tool := &notFoundTool{name: "file_reader"}

	rc := &recordingLLMClient{
		responses: []*llm.Response{
			{
				StopReason: llm.StopReasonToolUse,
				ToolCalls:  []llm.ToolCall{makeToolCallWithArgs("tc1", "file_reader", "/repo/foo.go")},
			},
			{StopReason: llm.StopReasonEndTurn, Content: "done"},
		},
	}

	loop := NewAgentLoop(rc, []Tool{tool}, 0, nil, "")
	if _, err := loop.Run(context.Background(), "dedup-test-1", "task"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// After the first not_found, the second LLM call must NOT contain
	// a UserMessage with the blocking text.
	if len(rc.calls) < 2 {
		t.Fatalf("expected 2 LLM calls, got %d", len(rc.calls))
	}
	for _, m := range rc.calls[1] {
		if um, ok := m.(llm.UserMessage); ok {
			if strings.Contains(um.Content, "禁止再次尝试") {
				t.Errorf("blocking UserMessage injected after first failure (should only trigger on second+): %q", um.Content)
			}
		}
	}
	// Counter must be 1.
	if loop.notFoundPaths["/repo/foo.go"] != 1 {
		t.Errorf("notFoundPaths[/repo/foo.go] = %d, want 1", loop.notFoundPaths["/repo/foo.go"])
	}
}

// TestAgentLoop_NotFoundDedup_SecondFailInjectsBlock verifies that the second
// consecutive not_found for the same path injects a hard-stop UserMessage into
// the conversation before the next LLM call.
func TestAgentLoop_NotFoundDedup_SecondFailInjectsBlock(t *testing.T) {
	tool := &notFoundTool{name: "file_reader"}
	const badPath = "/repo/missing.go"

	rc := &recordingLLMClient{
		responses: []*llm.Response{
			// Step 1: LLM tries the bad path.
			{
				StopReason: llm.StopReasonToolUse,
				ToolCalls:  []llm.ToolCall{makeToolCallWithArgs("tc1", "file_reader", badPath)},
			},
			// Step 2: LLM retries the same bad path (would waste a step).
			{
				StopReason: llm.StopReasonToolUse,
				ToolCalls:  []llm.ToolCall{makeToolCallWithArgs("tc2", "file_reader", badPath)},
			},
			// Step 3: LLM has seen the block message, gives up and ends.
			{StopReason: llm.StopReasonEndTurn, Content: "gave up"},
		},
	}

	loop := NewAgentLoop(rc, []Tool{tool}, 0, nil, "")
	result, err := loop.Run(context.Background(), "dedup-test-2", "task")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Content != "gave up" {
		t.Errorf("content = %q, want %q", result.Content, "gave up")
	}

	// The third LLM call (index 2) must contain the blocking UserMessage.
	if len(rc.calls) < 3 {
		t.Fatalf("expected 3 LLM calls, got %d", len(rc.calls))
	}
	foundBlock := false
	for _, m := range rc.calls[2] {
		if um, ok := m.(llm.UserMessage); ok {
			if strings.Contains(um.Content, "禁止再次尝试") && strings.Contains(um.Content, badPath) {
				foundBlock = true
			}
		}
	}
	if !foundBlock {
		t.Errorf("expected blocking UserMessage containing %q in third LLM call, not found", badPath)
	}
	// Counter must be 2.
	if loop.notFoundPaths[badPath] != 2 {
		t.Errorf("notFoundPaths[%s] = %d, want 2", badPath, loop.notFoundPaths[badPath])
	}
}

// TestAgentLoop_NotFoundDedup_DifferentPathsNoFalsePositive ensures that two
// different paths each failing once do NOT trigger the blocking injection —
// only repeated failure of the *same* path does.
func TestAgentLoop_NotFoundDedup_DifferentPathsNoFalsePositive(t *testing.T) {
	tool := &notFoundTool{name: "file_reader"}

	rc := &recordingLLMClient{
		responses: []*llm.Response{
			{
				StopReason: llm.StopReasonToolUse,
				ToolCalls: []llm.ToolCall{
					makeToolCallWithArgs("tc1", "file_reader", "/repo/a.go"),
					makeToolCallWithArgs("tc2", "file_reader", "/repo/b.go"),
				},
			},
			{StopReason: llm.StopReasonEndTurn, Content: "done"},
		},
	}

	loop := NewAgentLoop(rc, []Tool{tool}, 0, nil, "")
	if _, err := loop.Run(context.Background(), "dedup-test-3", "task"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(rc.calls) < 2 {
		t.Fatalf("expected 2 LLM calls, got %d", len(rc.calls))
	}
	// Second LLM call must NOT contain a blocking message.
	for _, m := range rc.calls[1] {
		if um, ok := m.(llm.UserMessage); ok {
			if strings.Contains(um.Content, "禁止再次尝试") {
				t.Errorf("false-positive blocking message injected for different paths: %q", um.Content)
			}
		}
	}
	// Each path counted once.
	if loop.notFoundPaths["/repo/a.go"] != 1 {
		t.Errorf("notFoundPaths[/repo/a.go] = %d, want 1", loop.notFoundPaths["/repo/a.go"])
	}
	if loop.notFoundPaths["/repo/b.go"] != 1 {
		t.Errorf("notFoundPaths[/repo/b.go] = %d, want 1", loop.notFoundPaths["/repo/b.go"])
	}
}

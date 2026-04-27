package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	openai "github.com/sashabaranov/go-openai"
)

// mockServer creates a test HTTP server that returns the given response body.
func mockServer(t *testing.T, statusCode int, body interface{}) (*httptest.Server, *Client) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))

	client := NewClient(Config{
		BaseURL:   srv.URL + "/v1",
		APIKey:    "test-key",
		Model:     "gpt-4o",
		MaxTokens: 1024,
	})
	return srv, client
}

// buildEndTurnResponse returns an OpenAI chat completion response with stop reason "stop".
func buildEndTurnResponse(content string) openai.ChatCompletionResponse {
	return openai.ChatCompletionResponse{
		ID:    "chatcmpl-test1",
		Model: "gpt-4o",
		Choices: []openai.ChatCompletionChoice{
			{
				Index: 0,
				Message: openai.ChatCompletionMessage{
					Role:    openai.ChatMessageRoleAssistant,
					Content: content,
				},
				FinishReason: openai.FinishReasonStop,
			},
		},
	}
}

// buildToolUseResponse returns an OpenAI chat completion response with finish_reason "tool_calls".
func buildToolUseResponse(toolID, toolName, toolArgs string) openai.ChatCompletionResponse {
	return openai.ChatCompletionResponse{
		ID:    "chatcmpl-test2",
		Model: "gpt-4o",
		Choices: []openai.ChatCompletionChoice{
			{
				Index: 0,
				Message: openai.ChatCompletionMessage{
					Role: openai.ChatMessageRoleAssistant,
					ToolCalls: []openai.ToolCall{
						{
							ID:   toolID,
							Type: openai.ToolTypeFunction,
							Function: openai.FunctionCall{
								Name:      toolName,
								Arguments: toolArgs,
							},
						},
					},
				},
				FinishReason: openai.FinishReasonToolCalls,
			},
		},
	}
}

func TestComplete_EndTurn(t *testing.T) {
	wantContent := "Hello, world!"
	resp := buildEndTurnResponse(wantContent)
	srv, client := mockServer(t, http.StatusOK, resp)
	defer srv.Close()

	messages := []Message{
		UserMessage{Content: "Say hello"},
	}

	result, err := client.Complete(context.Background(), messages, nil)
	if err != nil {
		t.Fatalf("Complete returned error: %v", err)
	}
	if result.StopReason != StopReasonEndTurn {
		t.Errorf("StopReason = %q, want %q", result.StopReason, StopReasonEndTurn)
	}
	if result.Content != wantContent {
		t.Errorf("Content = %q, want %q", result.Content, wantContent)
	}
	if len(result.ToolCalls) != 0 {
		t.Errorf("expected no ToolCalls, got %d", len(result.ToolCalls))
	}
}

func TestComplete_ToolUse(t *testing.T) {
	toolID := "call_abc123"
	toolName := "get_weather"
	toolArgs := `{"location":"Tokyo"}`

	resp := buildToolUseResponse(toolID, toolName, toolArgs)
	srv, client := mockServer(t, http.StatusOK, resp)
	defer srv.Close()

	tools := []ToolDefinition{
		{
			Name:        "get_weather",
			Description: "Get current weather for a location",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"location":{"type":"string"}},"required":["location"]}`),
		},
	}
	messages := []Message{
		UserMessage{Content: "What's the weather in Tokyo?"},
	}

	result, err := client.Complete(context.Background(), messages, tools)
	if err != nil {
		t.Fatalf("Complete returned error: %v", err)
	}
	if result.StopReason != StopReasonToolUse {
		t.Errorf("StopReason = %q, want %q", result.StopReason, StopReasonToolUse)
	}
	if len(result.ToolCalls) != 1 {
		t.Fatalf("expected 1 ToolCall, got %d", len(result.ToolCalls))
	}
	tc := result.ToolCalls[0]
	if tc.ID != toolID {
		t.Errorf("ToolCall.ID = %q, want %q", tc.ID, toolID)
	}
	if tc.Name != toolName {
		t.Errorf("ToolCall.Name = %q, want %q", tc.Name, toolName)
	}
	var args map[string]string
	if err := json.Unmarshal(tc.Arguments, &args); err != nil {
		t.Fatalf("unmarshal arguments: %v", err)
	}
	if args["location"] != "Tokyo" {
		t.Errorf("arguments.location = %q, want %q", args["location"], "Tokyo")
	}
}

func TestTokenCount(t *testing.T) {
	messages := []Message{
		UserMessage{Content: "Hello"},
	}

	count := TokenCount(messages)
	if count <= 0 {
		t.Errorf("TokenCount = %d, want > 0", count)
	}

	// "Hello" encodes to 1 token with cl100k_base, plus role/formatting overhead (~4) + priming (3)
	// Plus "user: Hello" prefix = "user: " (2 tokens) + "Hello" (1 token) = 3 + 4 overhead + 3 priming = 10
	// We just check it's in a reasonable range
	if count > 50 {
		t.Errorf("TokenCount = %d, suspiciously high for a single 'Hello' message", count)
	}
}

func TestTokenCount_KnownMessage(t *testing.T) {
	// A more precise test: multiple known messages
	messages := []Message{
		UserMessage{Content: "What is 2+2?"},
		AssistantMessage{Content: "4"},
	}
	count := TokenCount(messages)
	if count <= 0 {
		t.Errorf("TokenCount = %d, want > 0", count)
	}
	// Sanity: should be less than 100 tokens for these short messages
	if count > 100 {
		t.Errorf("TokenCount = %d, unexpectedly large", count)
	}
}

func TestTokenRatio(t *testing.T) {
	messages := []Message{
		UserMessage{Content: "Hello"},
	}

	ratio := TokenRatio(messages, 1000)
	if ratio <= 0 || ratio >= 1 {
		t.Errorf("TokenRatio = %f, want between 0 and 1 for a short message with maxTokens=1000", ratio)
	}
}

func TestTokenRatio_ZeroMax(t *testing.T) {
	messages := []Message{
		UserMessage{Content: "Hello"},
	}
	ratio := TokenRatio(messages, 0)
	if ratio != 0 {
		t.Errorf("TokenRatio with maxTokens=0 = %f, want 0", ratio)
	}
}

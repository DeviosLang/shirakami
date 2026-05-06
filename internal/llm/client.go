package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	openai "github.com/sashabaranov/go-openai"
)

// StopReason indicates why the LLM stopped generating.
type StopReason string

const (
	StopReasonEndTurn   StopReason = "end_turn"
	StopReasonToolUse   StopReason = "tool_use"
	StopReasonMaxTokens StopReason = "max_tokens"
)

// Message is the base interface for all message types.
type Message interface {
	messageRole() string
}

// SystemMessage is a system-level instruction message.
type SystemMessage struct {
	Content string
}

func (m SystemMessage) messageRole() string { return "system" }

// UserMessage is a message from the user.
type UserMessage struct {
	Content string
}

func (m UserMessage) messageRole() string { return "user" }

// AssistantMessage is a message from the assistant.
type AssistantMessage struct {
	Content    string
	ToolCalls  []ToolCall
}

func (m AssistantMessage) messageRole() string { return "assistant" }

// ToolResultMessage is a message containing the result of a tool call.
type ToolResultMessage struct {
	ToolCallID string
	Content    string
}

func (m ToolResultMessage) messageRole() string { return "tool" }

// ToolDefinition defines a tool that can be called by the LLM.
type ToolDefinition struct {
	Name        string
	Description string
	Parameters  json.RawMessage // JSON Schema object
}

// ToolCall represents a tool invocation requested by the LLM.
type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

// Response is the result of a Complete call.
type Response struct {
	StopReason StopReason
	Content    string
	ToolCalls  []ToolCall
	Usage      Usage
}

// Usage reports token usage for a single LLM response.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

// Client wraps an OpenAI-compatible API client.
type Client struct {
	openai    *openai.Client
	model     string
	maxTokens int
}

// Config holds configuration for creating a Client.
type Config struct {
	BaseURL   string
	APIKey    string
	Model     string
	MaxTokens int
}

// NewClient creates a new LLM client using the OpenAI-compatible API.
func NewClient(cfg Config) *Client {
	ocfg := openai.DefaultConfig(cfg.APIKey)
	if cfg.BaseURL != "" {
		ocfg.BaseURL = cfg.BaseURL
	}
	maxTokens := cfg.MaxTokens
	if maxTokens == 0 {
		maxTokens = 4096
	}
	return &Client{
		openai:    openai.NewClientWithConfig(ocfg),
		model:     cfg.Model,
		maxTokens: maxTokens,
	}
}

// Complete sends messages to the LLM and returns the response.
// It automatically retries on transient errors (429 rate limit, 503 unavailable)
// using exponential backoff: 2s, 4s, 8s (3 attempts total).
func (c *Client) Complete(ctx context.Context, messages []Message, tools []ToolDefinition) (*Response, error) {
	req, err := c.buildRequest(messages, tools, false)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	const maxAttempts = 3
	backoff := 2 * time.Second

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		resp, err := c.openai.CreateChatCompletion(ctx, req)
		if err == nil {
			return parseResponse(resp)
		}

		// Check whether the error is retryable.
		if !isRetryableError(err) {
			return nil, fmt.Errorf("chat completion: %w", err)
		}

		// Last attempt — don't sleep, just return the error.
		if attempt == maxAttempts {
			return nil, fmt.Errorf("chat completion: %w (after %d attempts)", err, maxAttempts)
		}

		// Wait before retrying, respecting context cancellation.
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("chat completion: context cancelled during retry: %w", ctx.Err())
		case <-time.After(backoff):
		}

		backoff *= 2 // exponential: 2s → 4s → 8s
	}

	// Unreachable, but satisfies the compiler.
	return nil, fmt.Errorf("chat completion: exhausted retries")
}

// isRetryableError returns true for transient HTTP errors that should be retried:
//   - 429 Too Many Requests (rate limit)
//   - 500 Internal Server Error
//   - 502 Bad Gateway
//   - 503 Service Unavailable
//   - 504 Gateway Timeout
func isRetryableError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	retryablePhrases := []string{
		"429",
		"500",
		"502",
		"503",
		"504",
		"rate limit",
		"rate exceeded",
		"too many requests",
		"service unavailable",
		"bad gateway",
		"gateway timeout",
		"connection reset",
		"connection refused",
		"EOF",
	}
	lower := strings.ToLower(msg)
	for _, phrase := range retryablePhrases {
		if strings.Contains(lower, strings.ToLower(phrase)) {
			return true
		}
	}
	return false
}

func (c *Client) buildRequest(messages []Message, tools []ToolDefinition, stream bool) (openai.ChatCompletionRequest, error) {
	oaiMessages, err := convertMessages(messages)
	if err != nil {
		return openai.ChatCompletionRequest{}, err
	}

	req := openai.ChatCompletionRequest{
		Model:     c.model,
		Messages:  oaiMessages,
		MaxTokens: c.maxTokens,
		Stream:    stream,
	}

	if len(tools) > 0 {
		oaiTools := make([]openai.Tool, 0, len(tools))
		for _, t := range tools {
			params := t.Parameters
			if params == nil {
				params = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			oaiTools = append(oaiTools, openai.Tool{
				Type: openai.ToolTypeFunction,
				Function: &openai.FunctionDefinition{
					Name:        t.Name,
					Description: t.Description,
					Parameters:  params,
				},
			})
		}
		req.Tools = oaiTools
	}

	return req, nil
}

func convertMessages(messages []Message) ([]openai.ChatCompletionMessage, error) {
	out := make([]openai.ChatCompletionMessage, 0, len(messages))
	for _, m := range messages {
		switch msg := m.(type) {
		case SystemMessage:
			out = append(out, openai.ChatCompletionMessage{
				Role:    openai.ChatMessageRoleSystem,
				Content: msg.Content,
			})
		case UserMessage:
			out = append(out, openai.ChatCompletionMessage{
				Role:    openai.ChatMessageRoleUser,
				Content: msg.Content,
			})
		case AssistantMessage:
			oaiMsg := openai.ChatCompletionMessage{
				Role:    openai.ChatMessageRoleAssistant,
				Content: msg.Content,
			}
			if len(msg.ToolCalls) > 0 {
				oaiToolCalls := make([]openai.ToolCall, 0, len(msg.ToolCalls))
				for _, tc := range msg.ToolCalls {
					args := string(tc.Arguments)
					if args == "" {
						args = "{}"
					}
					oaiToolCalls = append(oaiToolCalls, openai.ToolCall{
						ID:   tc.ID,
						Type: openai.ToolTypeFunction,
						Function: openai.FunctionCall{
							Name:      tc.Name,
							Arguments: args,
						},
					})
				}
				oaiMsg.ToolCalls = oaiToolCalls
			}
			out = append(out, oaiMsg)
		case ToolResultMessage:
			out = append(out, openai.ChatCompletionMessage{
				Role:       openai.ChatMessageRoleTool,
				Content:    msg.Content,
				ToolCallID: msg.ToolCallID,
			})
		default:
			return nil, fmt.Errorf("unknown message type: %T", m)
		}
	}
	return out, nil
}

func parseResponse(resp openai.ChatCompletionResponse) (*Response, error) {
	if len(resp.Choices) == 0 {
		return &Response{StopReason: StopReasonEndTurn}, nil
	}

	choice := resp.Choices[0]
	result := &Response{
		Usage: Usage{
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			TotalTokens:      resp.Usage.TotalTokens,
		},
	}

	// Map finish reason
	switch choice.FinishReason {
	case openai.FinishReasonToolCalls, openai.FinishReasonFunctionCall:
		result.StopReason = StopReasonToolUse
	case openai.FinishReasonLength:
		result.StopReason = StopReasonMaxTokens
	default:
		result.StopReason = StopReasonEndTurn
	}

	result.Content = choice.Message.Content

	if len(choice.Message.ToolCalls) > 0 {
		result.StopReason = StopReasonToolUse
		result.ToolCalls = make([]ToolCall, 0, len(choice.Message.ToolCalls))
		for _, tc := range choice.Message.ToolCalls {
			result.ToolCalls = append(result.ToolCalls, ToolCall{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: json.RawMessage(tc.Function.Arguments),
			})
		}
	}

	return result, nil
}

// CompleteText is a convenience wrapper for single-turn text conversations.
// It sends an optional system prompt and a user prompt, returning the assistant's
// text response. Satisfies the benchmark.LLMClient interface without importing
// the benchmark package.
func (c *Client) CompleteText(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	var msgs []Message
	if systemPrompt != "" {
		msgs = append(msgs, SystemMessage{Content: systemPrompt})
	}
	msgs = append(msgs, UserMessage{Content: userPrompt})
	resp, err := c.Complete(ctx, msgs, nil)
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}

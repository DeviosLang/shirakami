package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	openai "github.com/sashabaranov/go-openai"
)

// StreamComplete sends messages to the LLM and streams the response via SSE.
// It accumulates all deltas and returns the final Response when the stream ends.
func (c *Client) StreamComplete(ctx context.Context, messages []Message, tools []ToolDefinition) (*Response, error) {
	req, err := c.buildRequest(messages, tools, true)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	stream, err := c.openai.CreateChatCompletionStream(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("create stream: %w", err)
	}
	defer stream.Close()

	return accumulateStream(stream)
}

// accumulator collects streaming deltas.
type accumulator struct {
	content      string
	finishReason openai.FinishReason
	// tool call accumulation: index -> partialToolCall
	toolCalls map[int]*partialToolCall
}

type partialToolCall struct {
	id        string
	name      string
	arguments string
}

func accumulateStream(stream *openai.ChatCompletionStream) (*Response, error) {
	acc := &accumulator{
		toolCalls: make(map[int]*partialToolCall),
	}

	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("stream recv: %w", err)
		}

		if len(chunk.Choices) == 0 {
			continue
		}

		choice := chunk.Choices[0]

		if choice.FinishReason != "" {
			acc.finishReason = choice.FinishReason
		}

		delta := choice.Delta
		if delta.Content != "" {
			acc.content += delta.Content
		}

		for _, tc := range delta.ToolCalls {
			idx := tc.Index
			if idx == nil {
				continue
			}
			i := *idx
			if _, ok := acc.toolCalls[i]; !ok {
				acc.toolCalls[i] = &partialToolCall{}
			}
			ptc := acc.toolCalls[i]
			if tc.ID != "" {
				ptc.id = tc.ID
			}
			if tc.Function.Name != "" {
				ptc.name = tc.Function.Name
			}
			ptc.arguments += tc.Function.Arguments
		}
	}

	result := &Response{
		Content: acc.content,
	}

	// Map finish reason
	switch acc.finishReason {
	case openai.FinishReasonToolCalls, openai.FinishReasonFunctionCall:
		result.StopReason = StopReasonToolUse
	case openai.FinishReasonLength:
		result.StopReason = StopReasonMaxTokens
	default:
		result.StopReason = StopReasonEndTurn
	}

	if len(acc.toolCalls) > 0 {
		result.StopReason = StopReasonToolUse
		result.ToolCalls = make([]ToolCall, 0, len(acc.toolCalls))
		for i := 0; i < len(acc.toolCalls); i++ {
			ptc, ok := acc.toolCalls[i]
			if !ok {
				continue
			}
			args := ptc.arguments
			if args == "" {
				args = "{}"
			}
			result.ToolCalls = append(result.ToolCalls, ToolCall{
				ID:        ptc.id,
				Name:      ptc.name,
				Arguments: json.RawMessage(args),
			})
		}
	}

	return result, nil
}

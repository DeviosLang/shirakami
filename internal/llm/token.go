package llm

import (
	"fmt"

	tiktoken "github.com/pkoukk/tiktoken-go"
)

// defaultEncoding is the token encoding used for counting.
// cl100k_base is used by GPT-4, GPT-3.5-turbo, and is also compatible with Claude models.
const defaultEncoding = "cl100k_base"

// TokenCount returns the approximate number of tokens for the given messages.
// It counts the text content of UserMessage, AssistantMessage, and ToolResultMessage types.
func TokenCount(messages []Message) int {
	enc, err := tiktoken.GetEncoding(defaultEncoding)
	if err != nil {
		// Fallback: rough estimate of 4 chars per token
		total := 0
		for _, m := range messages {
			total += len(messageText(m)) / 4
		}
		return total
	}

	total := 0
	for _, m := range messages {
		text := messageText(m)
		if text == "" {
			continue
		}
		tokens := enc.Encode(text, nil, nil)
		total += len(tokens)
		// Add per-message overhead (role + formatting tokens, ~4 tokens per message)
		total += 4
	}
	// Add reply priming overhead
	total += 3
	return total
}

// TokenRatio returns the fraction of maxTokens used by messages.
// Returns 0 if maxTokens <= 0.
func TokenRatio(messages []Message, maxTokens int) float64 {
	if maxTokens <= 0 {
		return 0
	}
	count := TokenCount(messages)
	return float64(count) / float64(maxTokens)
}

// messageText extracts the text content from a message for token counting.
func messageText(m Message) string {
	switch msg := m.(type) {
	case UserMessage:
		return fmt.Sprintf("user: %s", msg.Content)
	case AssistantMessage:
		text := fmt.Sprintf("assistant: %s", msg.Content)
		for _, tc := range msg.ToolCalls {
			text += fmt.Sprintf(" [tool_call:%s(%s)]", tc.Name, string(tc.Arguments))
		}
		return text
	case ToolResultMessage:
		return fmt.Sprintf("tool: %s", msg.Content)
	default:
		return ""
	}
}

package compress

import (
	"context"
	"fmt"
	"strings"

	"github.com/DeviosLang/shirakami/internal/llm"
)

// compactSystemPrompt is the system instruction for the compaction LLM call.
const compactSystemPrompt = `You are a conversation compressor. 
Summarize the conversation history into a structured summary that preserves 
call-chain nodes, tool call results' key information, and deletes raw code text.`

// compactUserPrompt is the prompt template appended before the history.
const compactUserPrompt = `请将以下对话历史压缩为结构化摘要，保留已分析的调用链节点、工具调用结果的关键信息，删除原始代码文本。

输出格式：
## 已分析节点
- <节点名>: <关键发现>

## 工具调用摘要
- <工具名>(<参数摘要>): <结果摘要>

## 重要决策
- <决策内容>

## 待处理事项
- <未完成任务>

对话历史：
`

// recentMessagesToKeep is the number of most-recent messages preserved verbatim
// after compaction. These are appended after the compact summary so the model
// retains immediate context.
const recentMessagesToKeep = 10

// compactMessages calls the LLM to compress the conversation history in msgs
// into a structured summary and then replaces the slice with:
//
//	[SystemPrompt, CompactSummaryMessage, <last recentMessagesToKeep messages>]
//
// On error the original messages are left unchanged.
func compactMessages(ctx context.Context, msgs *[]llm.Message, client *llm.Client) error {
	original := *msgs
	if len(original) == 0 {
		return nil
	}

	// Build a text representation of the full history for the compaction call.
	var historyBuilder strings.Builder
	for _, m := range original {
		switch msg := m.(type) {
		case llm.UserMessage:
			historyBuilder.WriteString("User: ")
			historyBuilder.WriteString(msg.Content)
			historyBuilder.WriteString("\n\n")
		case llm.AssistantMessage:
			historyBuilder.WriteString("Assistant: ")
			historyBuilder.WriteString(msg.Content)
			for _, tc := range msg.ToolCalls {
				fmt.Fprintf(&historyBuilder, "\n[Tool Call: %s(%s)]", tc.Name, string(tc.Arguments))
			}
			historyBuilder.WriteString("\n\n")
		case llm.ToolResultMessage:
			historyBuilder.WriteString("Tool Result: ")
			historyBuilder.WriteString(msg.Content)
			historyBuilder.WriteString("\n\n")
		}
	}

	compactionMsgs := []llm.Message{
		llm.UserMessage{Content: compactUserPrompt + historyBuilder.String()},
	}

	resp, err := client.Complete(ctx, compactionMsgs, nil)
	if err != nil {
		return fmt.Errorf("compactMessages: LLM call failed: %w", err)
	}

	summary := resp.Content
	if summary == "" {
		// Fall back gracefully – keep original messages.
		return nil
	}

	// Determine the tail to keep verbatim.
	tail := original
	if len(original) > recentMessagesToKeep {
		tail = original[len(original)-recentMessagesToKeep:]
	}

	// Build the replacement message slice.
	result := make([]llm.Message, 0, 2+len(tail))
	result = append(result, llm.UserMessage{Content: compactSystemPrompt})
	result = append(result, llm.AssistantMessage{Content: summary})
	result = append(result, tail...)

	*msgs = result
	return nil
}

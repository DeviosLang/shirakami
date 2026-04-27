package compress

import (
	"strings"

	"github.com/DeviosLang/shirakami/internal/llm"
)

// codeBlockSummaryMarker replaces a cleared large code block in a tool result.
const codeBlockSummaryMarker = "[code block cleared – analysis already completed]"

// minCodeBlockTokens is the rough token threshold above which a tool result is
// considered a "large code block". At ~4 chars/token, 500 tokens ≈ 2000 chars.
const minCodeBlockTokens = 500

// clearProcessedCodeBlocks scans msgs and clears raw code blocks from
// ToolResultMessage entries that have already been confirmed by a subsequent
// AssistantMessage. Structural extraction results (summaries, metadata) are
// retained; only the verbose raw-code payload is replaced by a marker.
//
// A tool result is considered "analysed" when it is followed by at least one
// AssistantMessage that references or continues from that context.
func clearProcessedCodeBlocks(msgs *[]llm.Message) {
	original := *msgs
	n := len(original)
	if n == 0 {
		return
	}

	// Build a set of indices of ToolResultMessages that are followed by an
	// AssistantMessage (i.e., the model has already processed them).
	processedIndices := make(map[int]bool)
	for i := 0; i < n; i++ {
		if _, ok := original[i].(llm.ToolResultMessage); !ok {
			continue
		}
		// Look for a subsequent AssistantMessage to confirm the result was consumed.
		for j := i + 1; j < n; j++ {
			if _, ok := original[j].(llm.AssistantMessage); ok {
				processedIndices[i] = true
				break
			}
		}
	}

	if len(processedIndices) == 0 {
		return
	}

	result := make([]llm.Message, n)
	copy(result, original)

	for idx := range processedIndices {
		tr := result[idx].(llm.ToolResultMessage)
		if !isLargeCodeBlock(tr.Content) {
			continue
		}
		result[idx] = llm.ToolResultMessage{
			ToolCallID: tr.ToolCallID,
			Content:    codeBlockSummaryMarker,
		}
	}

	*msgs = result
}

// isLargeCodeBlock returns true when the content looks like a large code block
// that is worth clearing. The heuristic uses a character-count proxy for token
// count (4 chars ≈ 1 token, matching llm.TokenCount's fallback).
func isLargeCodeBlock(content string) bool {
	if len(content) < minCodeBlockTokens*4 {
		return false
	}
	// Treat the content as a code block if it contains typical code markers.
	hasCode := strings.Contains(content, "\n") &&
		(strings.Contains(content, "func ") ||
			strings.Contains(content, "class ") ||
			strings.Contains(content, "def ") ||
			strings.Contains(content, "import ") ||
			strings.Contains(content, "package ") ||
			strings.Contains(content, "{") ||
			strings.Contains(content, "```"))
	return hasCode
}

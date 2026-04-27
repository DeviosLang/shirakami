package memory

import (
	"context"
	"fmt"
	"strings"
)

const (
	// maxSystemReminderTokens is the upper bound on tokens for the assembled
	// system reminder. Enforced via a conservative char-based estimate
	// (4 chars ≈ 1 token) consistent with llm.TokenCount's fallback heuristic.
	maxSystemReminderTokens = 2000
	charsPerToken           = 4
	maxSystemReminderChars  = maxSystemReminderTokens * charsPerToken

	// maxKnowledgeRecords is the maximum number of Layer1 records to include.
	maxKnowledgeRecords = 5
)

// Layer3 builds the system reminder injected before each LLM call.
// It draws knowledge from Layer1 (long-term PostgreSQL store) and task
// progress from Layer2 (Redis), then assembles a structured Markdown string
// limited to maxSystemReminderTokens tokens.
type Layer3 struct {
	l1 *Layer1
	l2 *Layer2
}

// NewLayer3 creates a Layer3 instance wired to the given Layer1 and Layer2.
func NewLayer3(l1 *Layer1, l2 *Layer2) *Layer3 {
	return &Layer3{l1: l1, l2: l2}
}

// BuildSystemReminder assembles the system reminder string for the given
// task and keyword context. It queries at most maxKnowledgeRecords relevant
// records from Layer1 (filtered to currentHead) and the current task progress
// from Layer2, then formats them as structured Markdown.
//
// The result is guaranteed to be at most maxSystemReminderTokens tokens
// (estimated at 4 chars per token). An empty string is returned when both
// layers yield no data.
func (l *Layer3) BuildSystemReminder(ctx context.Context, taskID, currentHead string, keywords []string) (string, error) {
	// --- Layer1: relevant historical knowledge ---
	records, err := l.l1.SearchRelevant(ctx, keywords, currentHead, maxKnowledgeRecords)
	if err != nil {
		// Non-fatal: proceed without historical knowledge rather than failing
		// the caller's LLM call.
		records = nil
	}

	// --- Layer2: current task progress ---
	progress, err := l.l2.GetProgress(ctx, taskID)
	if err != nil {
		// Non-fatal: proceed without progress context.
		progress = nil
	}

	// Nothing to inject.
	if len(records) == 0 && progress == nil {
		return "", nil
	}

	var sb strings.Builder

	// Section 1: relevant historical knowledge
	if len(records) > 0 {
		sb.WriteString("## 相关历史知识\n")
		for _, r := range records {
			line := fmt.Sprintf("- %s/%s：%s\n", r.RepoName, r.Symbol, r.Summary)
			if sb.Len()+len(line) > maxSystemReminderChars {
				break
			}
			sb.WriteString(line)
		}
	}

	// Section 2: current analysis progress
	if progress != nil {
		header := "\n## 当前分析进度\n"
		if sb.Len()+len(header) <= maxSystemReminderChars {
			sb.WriteString(header)

			if len(progress.AnalyzedNodes) > 0 {
				doneLabel := "- 已完成节点："
				doneList := strings.Join(progress.AnalyzedNodes, ", ")
				doneLine := doneLabel + doneList + "\n"
				if sb.Len()+len(doneLine) <= maxSystemReminderChars {
					sb.WriteString(doneLine)
				}
			}

			stepLine := fmt.Sprintf("- 当前步骤：%d\n", progress.CurrentStep)
			if sb.Len()+len(stepLine) <= maxSystemReminderChars {
				sb.WriteString(stepLine)
			}
		}
	}

	return sb.String(), nil
}

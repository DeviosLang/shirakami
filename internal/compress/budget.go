package compress

import (
	"context"

	"github.com/DeviosLang/shirakami/internal/llm"
	"github.com/DeviosLang/shirakami/internal/memory"
)

// Threshold constants for the four compression tiers.
const (
	ThresholdWarning   = 0.60 // Plan D: inject system-reminder
	ThresholdCritical  = 0.70 // Plan B: restrict LayeredReader to level <= 2
	ThresholdPreMsg    = 0.80 // Plan C: clear analyzed code blocks
	ThresholdEmergency = 0.92 // Plan A: compact full conversation via LLM
)

// TokenBudgetManager watches token usage before each LLM call and triggers
// the appropriate compression strategy based on four ascending thresholds.
type TokenBudgetManager struct {
	maxTokens int
	llmClient *llm.Client
	layer3    *memory.Layer3

	// layeredReader is updated when Plan B restricts reading to level <= 2.
	layeredReader *LayeredReader

	// taskID and currentHead are forwarded to Layer3.BuildSystemReminder.
	taskID      string
	currentHead string
	keywords    []string
}

// NewTokenBudgetManager creates a manager for the given context window.
// llmClient is used by Plan A to compress the conversation.
// layer3 is used by Plan D to build the system reminder.
func NewTokenBudgetManager(
	maxTokens int,
	llmClient *llm.Client,
	layer3 *memory.Layer3,
	layeredReader *LayeredReader,
	taskID, currentHead string,
	keywords []string,
) *TokenBudgetManager {
	return &TokenBudgetManager{
		maxTokens:     maxTokens,
		llmClient:     llmClient,
		layer3:        layer3,
		layeredReader: layeredReader,
		taskID:        taskID,
		currentHead:   currentHead,
		keywords:      keywords,
	}
}

// TokenRatio returns the fraction of maxTokens used by the provided messages.
// Returns 0 when maxTokens <= 0.
func (m *TokenBudgetManager) TokenRatio(msgs []llm.Message) float64 {
	if m.maxTokens <= 0 {
		return 0
	}
	count := llm.TokenCount(msgs)
	return float64(count) / float64(m.maxTokens)
}

// CheckAndCompress evaluates the current token ratio and applies the highest-
// priority strategy that the ratio demands. Strategies are not cumulative:
// once a strategy fires the function returns.
//
// Order (highest priority first):
//   92% → Plan A: compact history via LLM
//   80% → Plan C: clear analyzed code blocks
//   70% → Plan B: restrict LayeredReader to level ≤ 2
//   60% → Plan D: inject system-reminder
func (m *TokenBudgetManager) CheckAndCompress(ctx context.Context, msgs *[]llm.Message) error {
	ratio := m.TokenRatio(*msgs)

	switch {
	case ratio >= ThresholdEmergency:
		// Plan A: compress full conversation history via LLM.
		return compactMessages(ctx, msgs, m.llmClient)

	case ratio >= ThresholdPreMsg:
		// Plan C: clear code blocks from already-analysed nodes.
		clearProcessedCodeBlocks(msgs)
		return nil

	case ratio >= ThresholdCritical:
		// Plan B: restrict file reading to at most level 2.
		if m.layeredReader != nil {
			m.layeredReader.SetMaxLevel(2)
		}
		return nil

	case ratio >= ThresholdWarning:
		// Plan D: inject a condensed system reminder.
		if m.layer3 != nil {
			return injectSystemReminder(ctx, msgs, m.layer3, m.taskID, m.currentHead, m.keywords)
		}
		return nil

	default:
		return nil
	}
}

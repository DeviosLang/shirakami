package compress

import (
	"context"
	"fmt"

	"github.com/DeviosLang/shirakami/internal/llm"
	"github.com/DeviosLang/shirakami/internal/memory"
)

// injectSystemReminder builds a concise system reminder via memory.Layer3 and
// prepends it to msgs as a UserMessage acting as a system-role instruction.
//
// The reminder is guaranteed to be ≤ 2000 tokens by Layer3.BuildSystemReminder.
// If the layer returns an empty string (nothing relevant to surface) the
// function is a no-op.
func injectSystemReminder(
	ctx context.Context,
	msgs *[]llm.Message,
	layer3 *memory.Layer3,
	taskID, currentHead string,
	keywords []string,
) error {
	reminder, err := layer3.BuildSystemReminder(ctx, taskID, currentHead, keywords)
	if err != nil {
		// Non-fatal: log and continue without the reminder.
		return fmt.Errorf("injectSystemReminder: build reminder: %w", err)
	}
	if reminder == "" {
		return nil
	}

	// Prepend the reminder as a UserMessage that acts as an updated system
	// instruction. Many OpenAI-compatible APIs treat a leading user message as
	// authoritative context before the actual conversation turn.
	reminderMsg := llm.UserMessage{
		Content: "[SYSTEM REMINDER – condensed context]\n\n" + reminder,
	}

	updated := make([]llm.Message, 0, 1+len(*msgs))
	updated = append(updated, reminderMsg)
	updated = append(updated, *msgs...)
	*msgs = updated
	return nil
}

package feedback

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// FeedbackType represents the classification of user feedback for an analysis result.
type FeedbackType string

const (
	FeedbackFalsePositive FeedbackType = "false_positive"
	FeedbackFalseNegative FeedbackType = "false_negative"
	FeedbackCorrect       FeedbackType = "correct"
)

// Feedback holds a single feedback record as returned by List.
type Feedback struct {
	ID        string
	TaskID    string
	Type      FeedbackType
	Comment   string
	CreatedAt time.Time
}

// Collector handles persistence of user feedback and downstream side-effects.
type Collector struct {
	db *pgxpool.Pool
}

// NewCollector creates a Collector backed by the given connection pool.
func NewCollector(db *pgxpool.Pool) *Collector {
	return &Collector{db: db}
}

// Submit records a piece of user feedback for the given task.
// When fbType is FeedbackFalsePositive the associated knowledge_base entries
// for that task are marked unreliable.
func (c *Collector) Submit(ctx context.Context, taskID string, fbType FeedbackType, comment string) error {
	_, err := c.db.Exec(ctx,
		`INSERT INTO feedback (task_id, type, comment) VALUES ($1, $2, $3)`,
		taskID, string(fbType), comment,
	)
	if err != nil {
		return fmt.Errorf("feedback insert: %w", err)
	}

	if fbType == FeedbackFalsePositive {
		if err := c.markKnowledgeBaseUnreliable(ctx, taskID); err != nil {
			return err
		}
	}

	return nil
}

// List returns all feedback records for the given task ordered by creation time.
func (c *Collector) List(ctx context.Context, taskID string) ([]Feedback, error) {
	rows, err := c.db.Query(ctx,
		`SELECT id, task_id, type, comment, created_at
		   FROM feedback
		  WHERE task_id = $1
		  ORDER BY created_at ASC`,
		taskID,
	)
	if err != nil {
		return nil, fmt.Errorf("feedback query: %w", err)
	}
	defer rows.Close()

	var feedbacks []Feedback
	for rows.Next() {
		var f Feedback
		var fbType string
		if err := rows.Scan(&f.ID, &f.TaskID, &fbType, &f.Comment, &f.CreatedAt); err != nil {
			return nil, fmt.Errorf("feedback scan: %w", err)
		}
		f.Type = FeedbackType(fbType)
		feedbacks = append(feedbacks, f)
	}
	return feedbacks, rows.Err()
}

// markKnowledgeBaseUnreliable sets the unreliable flag on knowledge_base rows
// whose symbols appear in the call_chain of the referenced analysis result.
func (c *Collector) markKnowledgeBaseUnreliable(ctx context.Context, taskID string) error {
	// Ensure the knowledge_base table has an unreliable column.
	// The migration may not have added it yet; we use IF NOT EXISTS defensively.
	_, err := c.db.Exec(ctx,
		`ALTER TABLE knowledge_base ADD COLUMN IF NOT EXISTS unreliable BOOLEAN NOT NULL DEFAULT FALSE`,
	)
	if err != nil {
		return fmt.Errorf("alter knowledge_base: %w", err)
	}

	// Mark all knowledge_base entries whose symbol appears in the call_chain
	// JSONB array stored in analysis_results for this task.
	_, err = c.db.Exec(ctx, `
		UPDATE knowledge_base kb
		   SET unreliable = TRUE
		  FROM analysis_results ar,
		       jsonb_array_elements(ar.call_chain) AS node
		 WHERE ar.task_id = $1
		   AND node->>'symbol' = kb.symbol
		   AND node->>'repo' = kb.repo_name`,
		taskID,
	)
	if err != nil {
		return fmt.Errorf("mark knowledge_base unreliable: %w", err)
	}

	return nil
}

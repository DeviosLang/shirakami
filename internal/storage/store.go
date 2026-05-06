package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TaskStatus represents the status of an analysis task.
type TaskStatus string

const (
	TaskStatusPending   TaskStatus = "pending"
	TaskStatusRunning   TaskStatus = "running"
	TaskStatusCompleted TaskStatus = "completed"
	TaskStatusFailed    TaskStatus = "failed"
)

// InputType represents the type of analysis input.
type InputType string

const (
	InputTypeDiff        InputType = "diff"
	InputTypeDescription InputType = "description"
	InputTypeCombined    InputType = "combined"
)

// Task represents an analysis task record.
type Task struct {
	ID            string
	InputType     InputType
	InputDiff     string
	InputDesc     string
	CacheKey      string
	Status        TaskStatus
	CreatedAt     time.Time
	CompletedAt   *time.Time
	Modes         []string
	SourceRepo    string
	QueuePosition *int
}

// TaskResult represents an analysis result record.
type TaskResult struct {
	ID               string
	TaskID           string
	CallChain        json.RawMessage
	TestScenarios    string
	EntryPoints      json.RawMessage
	TokenUsage       int
	StepCount        int
	CreatedAt        time.Time
	UTSuggestions    string
	FunctionAnalyses json.RawMessage
	ImpactSummary    string
	CrossRepoHops    int
	Risk             string
	IndexCoverage    json.RawMessage
	Modes            []string
}

// ErrNotFound is returned when a task or result is not found.
var ErrNotFound = errors.New("not found")

// Store provides persistence operations for analysis tasks and results.
type Store struct {
	db *pgxpool.Pool
}

// New creates a new Store backed by the given connection pool.
func New(db *pgxpool.Pool) *Store {
	return &Store{db: db}
}

// CreateTask inserts a new analysis task and returns its ID.
func (s *Store) CreateTask(ctx context.Context, inputType InputType, inputDiff, inputDesc, cacheKey, sourceRepo string, modes []string) (*Task, error) {
	if len(modes) == 0 {
		modes = []string{"chain", "e2e", "ut"}
	}
	row := s.db.QueryRow(ctx,
		`INSERT INTO analysis_tasks (input_type, input_diff, input_desc, cache_key, status, source_repo, modes)
		 VALUES ($1, $2, $3, $4, 'pending', $5, $6)
		 RETURNING id, input_type, input_diff, input_desc, cache_key, status, created_at, completed_at,
		           modes, source_repo, queue_position`,
		string(inputType), inputDiff, inputDesc, cacheKey, sourceRepo, modes,
	)

	var task Task
	var itStr, statusStr string
	err := row.Scan(
		&task.ID, &itStr, &task.InputDiff, &task.InputDesc,
		&task.CacheKey, &statusStr, &task.CreatedAt, &task.CompletedAt,
		&task.Modes, &task.SourceRepo, &task.QueuePosition,
	)
	if err != nil {
		return nil, fmt.Errorf("create task: %w", err)
	}
	task.InputType = InputType(itStr)
	task.Status = TaskStatus(statusStr)
	return &task, nil
}

// GetTask retrieves a task by ID.
func (s *Store) GetTask(ctx context.Context, id string) (*Task, error) {
	row := s.db.QueryRow(ctx,
		`SELECT id, input_type, input_diff, input_desc, cache_key, status, created_at, completed_at,
		        modes, source_repo, queue_position
		   FROM analysis_tasks WHERE id = $1`,
		id,
	)

	var task Task
	var itStr, statusStr string
	err := row.Scan(
		&task.ID, &itStr, &task.InputDiff, &task.InputDesc,
		&task.CacheKey, &statusStr, &task.CreatedAt, &task.CompletedAt,
		&task.Modes, &task.SourceRepo, &task.QueuePosition,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get task: %w", err)
	}
	task.InputType = InputType(itStr)
	task.Status = TaskStatus(statusStr)
	return &task, nil
}

// ListTasks returns the most recent tasks up to limit.
func (s *Store) ListTasks(ctx context.Context, limit int) ([]Task, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.Query(ctx,
		`SELECT id, input_type, input_diff, input_desc, cache_key, status, created_at, completed_at,
		        modes, source_repo, queue_position
		   FROM analysis_tasks
		  ORDER BY created_at DESC
		  LIMIT $1`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	defer rows.Close()

	var tasks []Task
	for rows.Next() {
		var task Task
		var itStr, statusStr string
		if err := rows.Scan(
			&task.ID, &itStr, &task.InputDiff, &task.InputDesc,
			&task.CacheKey, &statusStr, &task.CreatedAt, &task.CompletedAt,
			&task.Modes, &task.SourceRepo, &task.QueuePosition,
		); err != nil {
			return nil, fmt.Errorf("scan task: %w", err)
		}
		task.InputType = InputType(itStr)
		task.Status = TaskStatus(statusStr)
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

// UpdateTaskStatus updates the status of a task and sets completed_at when status is completed or failed.
func (s *Store) UpdateTaskStatus(ctx context.Context, id string, status TaskStatus) error {
	var err error
	if status == TaskStatusCompleted || status == TaskStatusFailed {
		_, err = s.db.Exec(ctx,
			`UPDATE analysis_tasks SET status = $1, completed_at = NOW() WHERE id = $2`,
			string(status), id,
		)
	} else {
		_, err = s.db.Exec(ctx,
			`UPDATE analysis_tasks SET status = $1 WHERE id = $2`,
			string(status), id,
		)
	}
	if err != nil {
		return fmt.Errorf("update task status: %w", err)
	}
	return nil
}

// SaveResult inserts an analysis result for a task.
func (s *Store) SaveResult(ctx context.Context, result *TaskResult) error {
	_, err := s.db.Exec(ctx,
		`INSERT INTO analysis_results
		   (task_id, call_chain, test_scenarios, entry_points, token_usage, step_count,
		    ut_suggestions, function_analyses, impact_summary, cross_repo_hops, risk, index_coverage, modes)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		result.TaskID,
		result.CallChain,
		result.TestScenarios,
		result.EntryPoints,
		result.TokenUsage,
		result.StepCount,
		result.UTSuggestions,
		result.FunctionAnalyses,
		result.ImpactSummary,
		result.CrossRepoHops,
		result.Risk,
		result.IndexCoverage,
		result.Modes,
	)
	if err != nil {
		return fmt.Errorf("save result: %w", err)
	}
	return nil
}

// GetResult retrieves the analysis result for a task.
func (s *Store) GetResult(ctx context.Context, taskID string) (*TaskResult, error) {
	row := s.db.QueryRow(ctx,
		`SELECT id, task_id, call_chain, test_scenarios, entry_points, token_usage, step_count, created_at,
		        ut_suggestions, function_analyses, impact_summary, cross_repo_hops, risk, index_coverage, modes
		   FROM analysis_results WHERE task_id = $1 ORDER BY created_at DESC LIMIT 1`,
		taskID,
	)

	var r TaskResult
	err := row.Scan(
		&r.ID, &r.TaskID, &r.CallChain, &r.TestScenarios,
		&r.EntryPoints, &r.TokenUsage, &r.StepCount, &r.CreatedAt,
		&r.UTSuggestions, &r.FunctionAnalyses, &r.ImpactSummary,
		&r.CrossRepoHops, &r.Risk, &r.IndexCoverage, &r.Modes,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get result: %w", err)
	}
	return &r, nil
}

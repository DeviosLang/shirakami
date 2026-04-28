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
	ID          string
	InputType   InputType
	InputDiff   string
	InputDesc   string
	CacheKey    string
	Status      TaskStatus
	CreatedAt   time.Time
	CompletedAt *time.Time
}

// TaskResult represents an analysis result record.
type TaskResult struct {
	ID            string
	TaskID        string
	CallChain     json.RawMessage
	TestScenarios string
	EntryPoints   json.RawMessage
	TokenUsage    int
	StepCount     int
	CreatedAt     time.Time
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
func (s *Store) CreateTask(ctx context.Context, inputType InputType, inputDiff, inputDesc, cacheKey string) (*Task, error) {
	row := s.db.QueryRow(ctx,
		`INSERT INTO analysis_tasks (input_type, input_diff, input_desc, cache_key, status)
		 VALUES ($1, $2, $3, $4, 'pending')
		 RETURNING id, input_type, input_diff, input_desc, cache_key, status, created_at, completed_at`,
		string(inputType), inputDiff, inputDesc, cacheKey,
	)

	var task Task
	var itStr, statusStr string
	err := row.Scan(
		&task.ID, &itStr, &task.InputDiff, &task.InputDesc,
		&task.CacheKey, &statusStr, &task.CreatedAt, &task.CompletedAt,
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
		`SELECT id, input_type, input_diff, input_desc, cache_key, status, created_at, completed_at
		   FROM analysis_tasks WHERE id = $1`,
		id,
	)

	var task Task
	var itStr, statusStr string
	err := row.Scan(
		&task.ID, &itStr, &task.InputDiff, &task.InputDesc,
		&task.CacheKey, &statusStr, &task.CreatedAt, &task.CompletedAt,
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
		`SELECT id, input_type, input_diff, input_desc, cache_key, status, created_at, completed_at
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
		`INSERT INTO analysis_results (task_id, call_chain, test_scenarios, entry_points, token_usage, step_count)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		result.TaskID,
		result.CallChain,
		result.TestScenarios,
		result.EntryPoints,
		result.TokenUsage,
		result.StepCount,
	)
	if err != nil {
		return fmt.Errorf("save result: %w", err)
	}
	return nil
}

// GetResult retrieves the analysis result for a task.
func (s *Store) GetResult(ctx context.Context, taskID string) (*TaskResult, error) {
	row := s.db.QueryRow(ctx,
		`SELECT id, task_id, call_chain, test_scenarios, entry_points, token_usage, step_count, created_at
		   FROM analysis_results WHERE task_id = $1 ORDER BY created_at DESC LIMIT 1`,
		taskID,
	)

	var r TaskResult
	err := row.Scan(
		&r.ID, &r.TaskID, &r.CallChain, &r.TestScenarios,
		&r.EntryPoints, &r.TokenUsage, &r.StepCount, &r.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get result: %w", err)
	}
	return &r, nil
}

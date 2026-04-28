package integration_test

import (
	"context"
	"testing"

	"github.com/pressly/goose/v3"
)

func TestMigrations_AllTablesCreated(t *testing.T) {
	db := startPostgresSQL(t)
	ctx := context.Background()

	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("set dialect: %v", err)
	}
	if err := goose.UpContext(ctx, db, "../../migrations"); err != nil {
		t.Fatalf("goose up: %v", err)
	}

	tables := []string{
		"analysis_tasks",
		"analysis_results",
		"knowledge_base",
		"feedback",
		"metrics_daily",
	}
	for _, table := range tables {
		var exists bool
		err := db.QueryRowContext(ctx,
			"SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = $1)",
			table,
		).Scan(&exists)
		if err != nil {
			t.Fatalf("check table %s: %v", table, err)
		}
		if !exists {
			t.Errorf("table %s does not exist after goose up", table)
		}
	}
}

func TestAnalysisTasks_CRUD(t *testing.T) {
	db := startPostgresSQL(t)
	ctx := context.Background()

	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("set dialect: %v", err)
	}
	if err := goose.UpContext(ctx, db, "../../migrations"); err != nil {
		t.Fatalf("goose up: %v", err)
	}

	// Create
	var taskID string
	err := db.QueryRowContext(ctx,
		`INSERT INTO analysis_tasks (input_type, input_diff, cache_key)
		 VALUES ($1, $2, $3) RETURNING id`,
		"diff", "--- a/main.go\n+++ b/main.go", "abc123",
	).Scan(&taskID)
	if err != nil {
		t.Fatalf("insert analysis_task: %v", err)
	}
	if taskID == "" {
		t.Fatal("expected non-empty task ID")
	}

	// Read
	var inputType, status string
	err = db.QueryRowContext(ctx,
		"SELECT input_type, status FROM analysis_tasks WHERE id = $1",
		taskID,
	).Scan(&inputType, &status)
	if err != nil {
		t.Fatalf("select analysis_task: %v", err)
	}
	if inputType != "diff" {
		t.Errorf("input_type: got %s, want diff", inputType)
	}
	if status != "pending" {
		t.Errorf("status: got %s, want pending", status)
	}

	// Update
	_, err = db.ExecContext(ctx,
		"UPDATE analysis_tasks SET status = $1 WHERE id = $2",
		"running", taskID,
	)
	if err != nil {
		t.Fatalf("update analysis_task: %v", err)
	}

	err = db.QueryRowContext(ctx,
		"SELECT status FROM analysis_tasks WHERE id = $1",
		taskID,
	).Scan(&status)
	if err != nil {
		t.Fatalf("select after update: %v", err)
	}
	if status != "running" {
		t.Errorf("status after update: got %s, want running", status)
	}

	// Delete
	_, err = db.ExecContext(ctx, "DELETE FROM analysis_tasks WHERE id = $1", taskID)
	if err != nil {
		t.Fatalf("delete analysis_task: %v", err)
	}

	var count int
	err = db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM analysis_tasks WHERE id = $1",
		taskID,
	).Scan(&count)
	if err != nil {
		t.Fatalf("count after delete: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 rows after delete, got %d", count)
	}
}

func TestAnalysisTasks_CascadeDelete(t *testing.T) {
	db := startPostgresSQL(t)
	ctx := context.Background()

	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("set dialect: %v", err)
	}
	if err := goose.UpContext(ctx, db, "../../migrations"); err != nil {
		t.Fatalf("goose up: %v", err)
	}

	// Create a task
	var taskID string
	err := db.QueryRowContext(ctx,
		`INSERT INTO analysis_tasks (input_type) VALUES ($1) RETURNING id`,
		"description",
	).Scan(&taskID)
	if err != nil {
		t.Fatalf("insert task: %v", err)
	}

	// Create a dependent result
	_, err = db.ExecContext(ctx,
		`INSERT INTO analysis_results (task_id, token_usage, step_count) VALUES ($1, $2, $3)`,
		taskID, 100, 5,
	)
	if err != nil {
		t.Fatalf("insert analysis_result: %v", err)
	}

	// Delete the task – result should cascade
	_, err = db.ExecContext(ctx, "DELETE FROM analysis_tasks WHERE id = $1", taskID)
	if err != nil {
		t.Fatalf("delete task: %v", err)
	}

	var count int
	err = db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM analysis_results WHERE task_id = $1",
		taskID,
	).Scan(&count)
	if err != nil {
		t.Fatalf("count results: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 results after cascade delete, got %d", count)
	}
}

package config

import (
	"os"
	"testing"

	"github.com/spf13/viper"
)

// resetViper clears viper state between tests so env vars from one test
// do not bleed into the next.
func resetViper() {
	viper.Reset()
}

// TestLoad_P0StepBudget_FromEnv verifies that SHIRAKAMI_P0_STEP_BUDGET is
// read and stored in Config.P0StepBudget.
func TestLoad_P0StepBudget_FromEnv(t *testing.T) {
	resetViper()
	t.Setenv("SHIRAKAMI_P0_STEP_BUDGET", "200")
	// Required fields so validation passes.
	t.Setenv("SHIRAKAMI_LLM_API_KEY", "test-key")
	t.Setenv("SHIRAKAMI_DB_DSN", "postgres://test")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.P0StepBudget != 200 {
		t.Errorf("P0StepBudget = %d, want 200", cfg.P0StepBudget)
	}
}

// TestLoad_P0StepBudget_DefaultZero verifies that when the env var is absent
// the default is 0 (no cap — backward-compatible behaviour).
func TestLoad_P0StepBudget_DefaultZero(t *testing.T) {
	resetViper()
	os.Unsetenv("SHIRAKAMI_P0_STEP_BUDGET")
	t.Setenv("SHIRAKAMI_LLM_API_KEY", "test-key")
	t.Setenv("SHIRAKAMI_DB_DSN", "postgres://test")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.P0StepBudget != 0 {
		t.Errorf("P0StepBudget = %d, want 0 (default)", cfg.P0StepBudget)
	}
}

// TestLoad_P1StepBudget_FromEnv verifies the existing P1 env var still works
// alongside the new P0 variable (regression guard).
func TestLoad_P1StepBudget_FromEnv(t *testing.T) {
	resetViper()
	t.Setenv("SHIRAKAMI_P1_STEP_BUDGET", "150")
	t.Setenv("SHIRAKAMI_P0_STEP_BUDGET", "200")
	t.Setenv("SHIRAKAMI_LLM_API_KEY", "test-key")
	t.Setenv("SHIRAKAMI_DB_DSN", "postgres://test")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.P1StepBudget != 150 {
		t.Errorf("P1StepBudget = %d, want 150", cfg.P1StepBudget)
	}
	if cfg.P0StepBudget != 200 {
		t.Errorf("P0StepBudget = %d, want 200", cfg.P0StepBudget)
	}
}

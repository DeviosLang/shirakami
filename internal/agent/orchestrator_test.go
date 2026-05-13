package agent

import (
	"testing"
)

// TestOrchestrator_SetP0StepBudget verifies that SetP0StepBudget stores the
// value and that it is reflected in the p0StepBudget field.
func TestOrchestrator_SetP0StepBudget(t *testing.T) {
	orch := NewOrchestrator(nil, nil, nil, "", nil)

	// Default: 0 (no cap).
	if orch.p0StepBudget != 0 {
		t.Errorf("default p0StepBudget = %d, want 0", orch.p0StepBudget)
	}

	orch.SetP0StepBudget(200)
	if orch.p0StepBudget != 200 {
		t.Errorf("p0StepBudget = %d, want 200", orch.p0StepBudget)
	}
}

// TestOrchestrator_SetP1StepBudget_Regression ensures the existing P1 setter
// still works after the P0 field was added alongside it (regression guard).
func TestOrchestrator_SetP1StepBudget_Regression(t *testing.T) {
	orch := NewOrchestrator(nil, nil, nil, "", nil)
	orch.SetP1StepBudget(150)
	if orch.p1StepBudget != 150 {
		t.Errorf("p1StepBudget = %d, want 150", orch.p1StepBudget)
	}
}

// TestOrchestrator_BudgetAssignment verifies the priority-to-budget mapping
// logic extracted from runWorkerBatch.
//
// Instead of exercising the full runWorkerBatch (which requires real Workers
// and repos), we replicate the budget selection logic here so that any future
// change to the switch block breaks this test first.
func TestOrchestrator_BudgetAssignment(t *testing.T) {
	cases := []struct {
		name         string
		priority     string
		p0Budget     int
		p1Budget     int
		wantBudget   int
	}{
		// P2 is always hard-capped at 50 regardless of configured budgets.
		{"P2 always 50", "P2", 200, 150, 50},
		// P1 uses configured budget when > 0.
		{"P1 uses config", "P1", 200, 150, 150},
		// P1 falls back to 0 (=300) when p1Budget is 0.
		{"P1 zero fallback", "P1", 0, 0, 0},
		// P0 uses configured budget when > 0.
		{"P0 uses config", "P0", 200, 150, 200},
		// P0 falls back to 0 (=300) when p0Budget is 0 — backward compatible.
		{"P0 zero fallback", "P0", 0, 150, 0},
		// Unknown priority defaults to 0 (=300) — same as legacy behavior.
		{"unknown defaults 0", "default", 200, 150, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			orch := &Orchestrator{
				p0StepBudget: tc.p0Budget,
				p1StepBudget: tc.p1Budget,
			}
			got := orch.budgetForPriority(tc.priority)
			if got != tc.wantBudget {
				t.Errorf("budgetForPriority(%q) = %d, want %d", tc.priority, got, tc.wantBudget)
			}
		})
	}
}

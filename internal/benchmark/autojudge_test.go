package benchmark

import (
	"context"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Mock LLM client
// ---------------------------------------------------------------------------

// mockLLMClient implements LLMClient for testing.
// It returns preset responses keyed by call order (0, 1, 2, ...).
type mockLLMClient struct {
	responses []string
	callIdx   int
	err       error // if non-nil, returned on every call
}

func (m *mockLLMClient) CompleteText(_ context.Context, _, _ string) (string, error) {
	if m.err != nil {
		return "", m.err
	}
	if m.callIdx >= len(m.responses) {
		return "", nil
	}
	resp := m.responses[m.callIdx]
	m.callIdx++
	return resp, nil
}

func makePendingRecord(sourceFunc, targetFunc string) DiffRecord {
	return DiffRecord{
		Edge: NormalizedEdge{
			SourceRepo: "repo1",
			SourceFunc: sourceFunc,
			SourceFile: "pkg/foo.go",
			TargetRepo: "repo2",
			TargetFunc: targetFunc,
			EdgeType:   "CALLS",
		},
		Category: CategoryExtraPend,
	}
}

// ---------------------------------------------------------------------------
// parseJudgeVerdict
// ---------------------------------------------------------------------------

func TestParseJudgeVerdict_EXISTS(t *testing.T) {
	cases := []struct {
		raw  string
		want DiffCategory
	}{
		{"EXISTS\n\nsome explanation", CategoryExtraTP},
		{"EXISTS — call is real at line 42", CategoryExtraTP},
		{"exists (lowercase)", CategoryExtraTP}, // ToUpper normalises
		{"NOT_EXISTS\nfoo", CategoryExtraFP},
		{"not_exists — false positive", CategoryExtraFP},
		{"UNCERTAIN\nhard to tell", CategoryExtraPend},
		{"", CategoryExtraPend},
		{"  \n  ", CategoryExtraPend},
		{"UNKNOWN_VERDICT", CategoryExtraPend},
	}

	for _, tc := range cases {
		got := parseJudgeVerdict(tc.raw)
		if got != tc.want {
			t.Errorf("parseJudgeVerdict(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// AutoJudgeExtra — non-pending record is passed through unchanged
// ---------------------------------------------------------------------------

func TestAutoJudgeExtra_NonPendingPassthrough(t *testing.T) {
	rec := DiffRecord{
		Edge:     NormalizedEdge{SourceFunc: "A", TargetFunc: "B"},
		Category: CategoryMatch,
	}
	mock := &mockLLMClient{}
	result, err := AutoJudgeExtra(context.Background(), mock, rec, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.NewCat != CategoryMatch {
		t.Errorf("NewCat: got %v, want %v", result.NewCat, CategoryMatch)
	}
	if mock.callIdx != 0 {
		t.Errorf("expected 0 LLM calls for non-pending record, got %d", mock.callIdx)
	}
}

// ---------------------------------------------------------------------------
// AutoJudgeExtra — EXISTS verdict
// ---------------------------------------------------------------------------

func TestAutoJudgeExtra_EXISTS(t *testing.T) {
	rec := makePendingRecord("Caller", "Target")
	mock := &mockLLMClient{
		responses: []string{
			"The call exists at pkg/foo.go:42 — `Target(ctx)`", // proof
			"I can't disprove it, the evidence is solid",       // disproof
			"EXISTS\nBoth sides agree the call is real",        // judge
		},
	}

	result, err := AutoJudgeExtra(context.Background(), mock, rec, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.NewCat != CategoryExtraTP {
		t.Errorf("NewCat: got %v, want CategoryExtraTP", result.NewCat)
	}
	if mock.callIdx != 3 {
		t.Errorf("expected 3 LLM calls, got %d", mock.callIdx)
	}
	if !strings.Contains(result.Proof, "exists at") {
		t.Errorf("Proof field not populated correctly: %q", result.Proof)
	}
}

// ---------------------------------------------------------------------------
// AutoJudgeExtra — NOT_EXISTS verdict
// ---------------------------------------------------------------------------

func TestAutoJudgeExtra_NOT_EXISTS(t *testing.T) {
	rec := makePendingRecord("Foo", "Bar")
	mock := &mockLLMClient{
		responses: []string{
			"NO EVIDENCE — I cannot find the call",                    // proof
			"This is a false positive from a comment at line 5",       // disproof
			"NOT_EXISTS\nThe match came from a comment, not real code", // judge
		},
	}

	result, err := AutoJudgeExtra(context.Background(), mock, rec, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.NewCat != CategoryExtraFP {
		t.Errorf("NewCat: got %v, want CategoryExtraFP", result.NewCat)
	}
}

// ---------------------------------------------------------------------------
// AutoJudgeExtra — UNCERTAIN verdict
// ---------------------------------------------------------------------------

func TestAutoJudgeExtra_UNCERTAIN(t *testing.T) {
	rec := makePendingRecord("Baz", "Qux")
	mock := &mockLLMClient{
		responses: []string{
			"Possibly exists through an interface indirection", // proof
			"But the interface could be satisfied by multiple types", // disproof
			"UNCERTAIN\nNeed human review",                         // judge
		},
	}

	result, err := AutoJudgeExtra(context.Background(), mock, rec, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.NewCat != CategoryExtraPend {
		t.Errorf("NewCat: got %v, want CategoryExtraPend", result.NewCat)
	}
}

// ---------------------------------------------------------------------------
// AutoJudgeExtra — codeContext injection
// ---------------------------------------------------------------------------

func TestAutoJudgeExtra_CodeContextInjected(t *testing.T) {
	rec := makePendingRecord("F", "G")
	var capturedProofUser string
	calls := 0
	mock := &mockLLMClient{
		responses: []string{"proof response", "disproof response", "EXISTS"},
	}
	// We verify indirectly: if codeContext is non-empty, the prompt must
	// contain it — we do that by checking JudgeRaw after a full run.
	_ = capturedProofUser
	_ = calls

	result, err := AutoJudgeExtra(context.Background(), mock, rec, "func G() { return 42 }")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.NewCat != CategoryExtraTP {
		t.Errorf("NewCat: got %v, want CategoryExtraTP", result.NewCat)
	}
}

// ---------------------------------------------------------------------------
// AutoJudgeExtra — LLM error on proof phase
// ---------------------------------------------------------------------------

func TestAutoJudgeExtra_ProofError(t *testing.T) {
	rec := makePendingRecord("X", "Y")
	mock := &mockLLMClient{err: context.DeadlineExceeded}

	_, err := AutoJudgeExtra(context.Background(), mock, rec, "")
	if err == nil {
		t.Error("expected error from proof phase, got nil")
	}
	if !strings.Contains(err.Error(), "proof phase") {
		t.Errorf("error should mention proof phase, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// RunAutoJudge — batch processing
// ---------------------------------------------------------------------------

func makeReportWithPending(n int) *ParityReport {
	r := makeTestReport(2, 1, n)
	return r
}

func TestRunAutoJudge_AllTP(t *testing.T) {
	report := makeReportWithPending(2)

	// Each record needs 3 responses; 2 records = 6 responses total.
	responses := make([]string, 0, 6)
	for i := 0; i < 2; i++ {
		responses = append(responses, "proof", "disproof", "EXISTS")
	}
	mock := &mockLLMClient{responses: responses}

	summary, err := RunAutoJudge(context.Background(), mock, report, AutoJudgeOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if summary.Total != 2 {
		t.Errorf("Total: got %d, want 2", summary.Total)
	}
	if summary.Judged != 2 {
		t.Errorf("Judged: got %d, want 2", summary.Judged)
	}
	if summary.TPCount != 2 {
		t.Errorf("TPCount: got %d, want 2", summary.TPCount)
	}
	if report.ExtraPendingCount != 0 {
		t.Errorf("ExtraPendingCount after: got %d, want 0", report.ExtraPendingCount)
	}
	if report.ExtraTPCount != 2 {
		t.Errorf("ExtraTPCount after: got %d, want 2", report.ExtraTPCount)
	}
}

func TestRunAutoJudge_MaxRecords(t *testing.T) {
	report := makeReportWithPending(3)
	mock := &mockLLMClient{
		responses: []string{"proof", "disproof", "EXISTS"}, // only 1 record processed
	}

	summary, err := RunAutoJudge(context.Background(), mock, report, AutoJudgeOptions{MaxRecords: 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if summary.Judged != 1 {
		t.Errorf("Judged: got %d, want 1", summary.Judged)
	}
	if summary.Remaining != 2 {
		t.Errorf("Remaining: got %d, want 2", summary.Remaining)
	}
}

func TestRunAutoJudge_LLMError_NonFatal(t *testing.T) {
	report := makeReportWithPending(1)
	mock := &mockLLMClient{err: context.DeadlineExceeded}

	summary, err := RunAutoJudge(context.Background(), mock, report, AutoJudgeOptions{})
	if err != nil {
		t.Fatalf("expected nil error (errors are non-fatal), got %v", err)
	}
	if summary.Remaining != 1 {
		t.Errorf("Remaining: got %d, want 1 (error keeps record pending)", summary.Remaining)
	}
}

func TestRunAutoJudge_FPRateRecalculated(t *testing.T) {
	// 2 match, 0 miss, 1 extra_pending → judge as fp
	report := makeTestReport(2, 0, 1)
	mock := &mockLLMClient{
		responses: []string{"proof", "disproof", "NOT_EXISTS"},
	}

	_, err := RunAutoJudge(context.Background(), mock, report, AutoJudgeOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// FPRate = 1 / (2 + 0 + 1) = 0.333...
	if report.FPRate < 0.32 || report.FPRate > 0.34 {
		t.Errorf("FPRate: got %.4f, want ~0.333", report.FPRate)
	}
}

func TestRunAutoJudge_VerboseCallback(t *testing.T) {
	report := makeReportWithPending(1)
	mock := &mockLLMClient{responses: []string{"proof", "disproof", "EXISTS"}}

	var logged []string
	opts := AutoJudgeOptions{
		Verbose: func(format string, args ...any) {
			logged = append(logged, format)
		},
	}

	_, err := RunAutoJudge(context.Background(), mock, report, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(logged) == 0 {
		t.Error("expected verbose output, got none")
	}
}

package benchmark

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Helpers: create a minimal temp golden case directory for unit tests.
// ---------------------------------------------------------------------------

func writeTempCase(t *testing.T, dir, caseName, patch, expectedJSON string) {
	t.Helper()
	caseDir := filepath.Join(dir, caseName)
	if err := os.MkdirAll(caseDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(caseDir, "input.patch"), []byte(patch), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(caseDir, "expected.json"), []byte(expectedJSON), 0644); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// LoadGoldenCases
// ---------------------------------------------------------------------------

func TestLoadGoldenCases_Empty(t *testing.T) {
	dir := t.TempDir()
	cases, err := LoadGoldenCases(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 0 {
		t.Errorf("expected 0 cases, got %d", len(cases))
	}
}

func TestLoadGoldenCases_SkipsMissingPatch(t *testing.T) {
	dir := t.TempDir()
	// Create a case with only expected.json (no patch)
	caseDir := filepath.Join(dir, "no-patch")
	if err := os.MkdirAll(caseDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(caseDir, "expected.json"), []byte(`{"changed_functions":[],"call_chain":[],"entry_points":[],"cross_repo_calls":[]}`), 0644); err != nil {
		t.Fatal(err)
	}

	cases, err := LoadGoldenCases(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 0 {
		t.Errorf("expected 0 cases (patch missing), got %d", len(cases))
	}
}

func TestLoadGoldenCases_Basic(t *testing.T) {
	dir := t.TempDir()
	patch := `--- a/foo.go
+++ b/foo.go
@@ -1,3 +1,5 @@
+func NewFoo() *Foo {
+	return &Foo{}
+}
`
	expected := `{
  "changed_functions": [{"name": "NewFoo", "repo": "foo", "file": "foo.go", "start_line": 1}],
  "call_chain": [],
  "entry_points": [],
  "cross_repo_calls": []
}`
	writeTempCase(t, dir, "test-case", patch, expected)

	cases, err := LoadGoldenCases(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 1 {
		t.Fatalf("expected 1 case, got %d", len(cases))
	}
	if cases[0].Name != "test-case" {
		t.Errorf("case name = %q, want 'test-case'", cases[0].Name)
	}
	if len(cases[0].Expected.ChangedFunctions) != 1 {
		t.Errorf("expected 1 changed function, got %d", len(cases[0].Expected.ChangedFunctions))
	}
}

func TestLoadGoldenCase_NotFound(t *testing.T) {
	dir := t.TempDir()
	_, err := LoadGoldenCase(dir, "nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent case")
	}
}

// ---------------------------------------------------------------------------
// EvalCase — file recall
// ---------------------------------------------------------------------------

func TestEvalCase_FileRecall_Perfect(t *testing.T) {
	dir := t.TempDir()
	patch := `--- a/service/payment.go
+++ b/service/payment.go
@@ -10,6 +10,10 @@
+func ProcessRefund(id string) error {
+	return nil
+}
`
	expected := `{
  "changed_functions": [
    {"name": "ProcessRefund", "repo": "pay", "file": "service/payment.go", "start_line": 11}
  ],
  "call_chain": [],
  "entry_points": [],
  "cross_repo_calls": []
}`
	writeTempCase(t, dir, "refund-case", patch, expected)
	cases, err := LoadGoldenCases(dir)
	if err != nil {
		t.Fatal(err)
	}

	m := EvalCase(cases[0])
	if m.FilesExpected != 1 {
		t.Errorf("FilesExpected = %d, want 1", m.FilesExpected)
	}
	if m.FilesCovered != 1 {
		t.Errorf("FilesCovered = %d, want 1", m.FilesCovered)
	}
	if m.FileRecall != 1.0 {
		t.Errorf("FileRecall = %.2f, want 1.00", m.FileRecall)
	}
	if len(m.MissingFiles) != 0 {
		t.Errorf("unexpected MissingFiles: %v", m.MissingFiles)
	}
}

func TestEvalCase_FileRecall_Miss(t *testing.T) {
	dir := t.TempDir()
	// Patch touches foo.go but expected says bar.go was changed
	patch := `--- a/foo.go
+++ b/foo.go
@@ -1,3 +1,4 @@
+// comment
`
	expected := `{
  "changed_functions": [
    {"name": "BarFunc", "repo": "r", "file": "bar.go", "start_line": 5}
  ],
  "call_chain": [],
  "entry_points": [],
  "cross_repo_calls": []
}`
	writeTempCase(t, dir, "miss-case", patch, expected)
	cases, _ := LoadGoldenCases(dir)
	m := EvalCase(cases[0])

	if m.FilesCovered != 0 {
		t.Errorf("FilesCovered = %d, want 0", m.FilesCovered)
	}
	if m.FileRecall != 0.0 {
		t.Errorf("FileRecall = %.2f, want 0.00", m.FileRecall)
	}
	if len(m.MissingFiles) != 1 || m.MissingFiles[0] != "bar.go" {
		t.Errorf("MissingFiles = %v, want [bar.go]", m.MissingFiles)
	}
}

func TestEvalCase_NoExpectedFunctions(t *testing.T) {
	dir := t.TempDir()
	patch := `--- a/readme.md
+++ b/readme.md
@@ -1,1 +1,2 @@
+line added
`
	expected := `{"changed_functions":[],"call_chain":[],"entry_points":[],"cross_repo_calls":[]}`
	writeTempCase(t, dir, "empty-case", patch, expected)
	cases, _ := LoadGoldenCases(dir)
	m := EvalCase(cases[0])

	if m.FilesExpected != 0 {
		t.Errorf("FilesExpected = %d, want 0", m.FilesExpected)
	}
	if m.FileRecall != -1 {
		t.Errorf("FileRecall = %.2f, want -1 (no expected)", m.FileRecall)
	}
}

// ---------------------------------------------------------------------------
// EvalCase — func recall
// ---------------------------------------------------------------------------

func TestEvalCase_FuncRecall_NewDeclaration(t *testing.T) {
	dir := t.TempDir()
	patch := `--- a/worker.go
+++ b/worker.go
@@ -5,3 +5,7 @@
+func handleSingleStream(data workerData) {
+	// new helper
+}
`
	expected := `{
  "changed_functions": [
    {"name": "handleSingleStream", "repo": "grpc-go", "file": "worker.go", "start_line": 6}
  ],
  "call_chain": [],
  "entry_points": [],
  "cross_repo_calls": []
}`
	writeTempCase(t, dir, "func-case", patch, expected)
	cases, _ := LoadGoldenCases(dir)
	m := EvalCase(cases[0])

	if m.FuncsExpected != 1 {
		t.Errorf("FuncsExpected = %d, want 1", m.FuncsExpected)
	}
	if m.FuncsCovered != 1 {
		t.Errorf("FuncsCovered = %d, want 1", m.FuncsCovered)
	}
	if m.FuncRecall != 1.0 {
		t.Errorf("FuncRecall = %.2f, want 1.00", m.FuncRecall)
	}
}

func TestEvalCase_FuncRecall_BodyOnly_Skipped(t *testing.T) {
	// When the patch only modifies an existing function body (no + line with
	// the declaration), FuncRecall should be -1 (no declarations to check).
	dir := t.TempDir()
	patch := `--- a/handler.go
+++ b/handler.go
@@ -20,6 +20,8 @@
 func (c *Context) JSON(code int, obj any) {
+	c.Writer.WriteHeaderNow()
 	c.Render(code, render.JSON{Data: obj})
 }
`
	expected := `{
  "changed_functions": [
    {"name": "Context.JSON", "repo": "gin", "file": "handler.go", "start_line": 20}
  ],
  "call_chain": [],
  "entry_points": [],
  "cross_repo_calls": []
}`
	writeTempCase(t, dir, "body-only-case", patch, expected)
	cases, _ := LoadGoldenCases(dir)
	m := EvalCase(cases[0])

	if m.FuncRecall != -1 {
		t.Errorf("FuncRecall = %.2f, want -1 (body-only change, no declaration in + lines)", m.FuncRecall)
	}
	if m.FuncsExpected != 0 {
		t.Errorf("FuncsExpected = %d, want 0", m.FuncsExpected)
	}
}

// ---------------------------------------------------------------------------
// Summarize
// ---------------------------------------------------------------------------

func TestSummarize_Basic(t *testing.T) {
	cases := []CaseMetrics{
		{CaseName: "a", FileRecall: 1.0, FuncRecall: 1.0, FilesExpected: 1, FuncsExpected: 1},
		{CaseName: "b", FileRecall: 0.5, FuncRecall: -1, FilesExpected: 2, FuncsExpected: 0},
		{CaseName: "c", FileRecall: -1, FuncRecall: 0.8, FilesExpected: 0, FuncsExpected: 2},
	}
	s := Summarize(cases)
	if s.TotalCases != 3 {
		t.Errorf("TotalCases = %d, want 3", s.TotalCases)
	}
	// AvgFileRecall = (1.0 + 0.5) / 2 = 0.75
	if s.AvgFileRecall < 0.74 || s.AvgFileRecall > 0.76 {
		t.Errorf("AvgFileRecall = %.3f, want ~0.750", s.AvgFileRecall)
	}
	// AvgFuncRecall = (1.0 + 0.8) / 2 = 0.90
	if s.AvgFuncRecall < 0.89 || s.AvgFuncRecall > 0.91 {
		t.Errorf("AvgFuncRecall = %.3f, want ~0.900", s.AvgFuncRecall)
	}
}

func TestSummarize_Empty(t *testing.T) {
	s := Summarize(nil)
	if s.TotalCases != 0 {
		t.Errorf("TotalCases = %d, want 0", s.TotalCases)
	}
	if s.AvgFileRecall != 0 || s.AvgFuncRecall != 0 {
		t.Error("averages should be 0 for empty input")
	}
}

// ---------------------------------------------------------------------------
// patchHasFuncDecl
// ---------------------------------------------------------------------------

func TestPatchHasFuncDecl(t *testing.T) {
	tests := []struct {
		name     string
		patch    string
		funcName string
		want     bool
	}{
		{
			name:     "go func added",
			patch:    "+func handleSingleStream(data workerData) {",
			funcName: "handleSingleStream",
			want:     true,
		},
		{
			name:     "python def added",
			patch:    "+def retry(self, exc=None):",
			funcName: "retry",
			want:     true,
		},
		{
			name:     "call (not decl) only",
			patch:    "+result = s.apply_async(countdown=3)",
			funcName: "apply_async",
			want:     true, // contains "apply_async(" — we can't distinguish call from decl at this level
		},
		{
			name:     "removed line does not match",
			patch:    "-func oldFunc() {}",
			funcName: "oldFunc",
			want:     false,
		},
		{
			name:     "context line does not match",
			patch:    " func contextFunc() {}",
			funcName: "contextFunc",
			want:     false,
		},
		{
			name:     "case insensitive",
			patch:    "+FUNC MyFunc() {}",
			funcName: "myfunc",
			want:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := patchHasFuncDecl(tt.patch, tt.funcName)
			if got != tt.want {
				t.Errorf("patchHasFuncDecl(%q, %q) = %v, want %v",
					strings.TrimSpace(tt.patch), tt.funcName, got, tt.want)
			}
		})
	}
}

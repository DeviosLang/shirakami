package golden

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DeviosLang/shirakami/internal/tool"
)

// ExpectedResult is the golden test expected output structure.
type ExpectedResult struct {
	ChangedFunctions []ExpectedFunction `json:"changed_functions"`
	CallChain        []ExpectedEdge     `json:"call_chain"`
	EntryPoints      []ExpectedEntry    `json:"entry_points"`
	CrossRepoCalls   []ExpectedCross    `json:"cross_repo_calls"`
}

type ExpectedFunction struct {
	Name      string `json:"name"`
	Repo      string `json:"repo"`
	File      string `json:"file"`
	StartLine int    `json:"start_line"`
}

type ExpectedEdge struct {
	Source     string  `json:"source"`
	Target     string  `json:"target"`
	Type       string  `json:"type"`
	Depth      int     `json:"depth"`
	Confidence float64 `json:"confidence,omitempty"`
}

type ExpectedEntry struct {
	Repo     string `json:"repo"`
	Function string `json:"function"`
	File     string `json:"file"`
	Protocol string `json:"protocol"`
	Path     string `json:"path"`
}

type ExpectedCross struct {
	FromRepo   string  `json:"from_repo"`
	ToRepo     string  `json:"to_repo"`
	Function   string  `json:"function"`
	Confidence float64 `json:"confidence"`
}

// validProtocols lists the allowed Protocol values (mirrors schema.Protocol).
var validProtocols = map[string]bool{
	"HTTP":     true,
	"gRPC":     true,
	"MQ":       true,
	"Cron":     true,
	"CLI":      true,
	"JSON-RPC": true, // commonly used in vstation_compute
}

// validEdgeTypes lists the allowed call-chain edge types.
var validEdgeTypes = map[string]bool{
	"CALLS":      true,
	"IMPORTS":    true,
	"EXTENDS":    true,
	"IMPLEMENTS": true,
}

// loadCase reads and parses expected.json + input.patch for a named case.
func loadCase(t *testing.T, caseName string) (ExpectedResult, string) {
	t.Helper()
	caseDir := filepath.Join("cases", caseName)

	patchBytes, err := os.ReadFile(filepath.Join(caseDir, "input.patch"))
	if err != nil {
		t.Fatalf("read input.patch: %v", err)
	}

	expectedBytes, err := os.ReadFile(filepath.Join(caseDir, "expected.json"))
	if err != nil {
		t.Fatalf("read expected.json: %v", err)
	}

	var expected ExpectedResult
	if err := json.Unmarshal(expectedBytes, &expected); err != nil {
		t.Fatalf("parse expected.json: %v", err)
	}

	return expected, string(patchBytes)
}

// listCases returns all case directory names.
func listCases(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir("cases")
	if err != nil {
		t.Fatalf("read cases dir: %v", err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names
}

// ---------------------------------------------------------------------------
// Layer A: ParseDiffHunks — file-level recall (no DB or index required)
// ---------------------------------------------------------------------------

// TestParseDiffHunks_GoldenCases validates that ParseDiffHunks correctly
// extracts changed line ranges from each golden case's input.patch.
// This tests Layer A (pure diff parsing) — no DB or index required.
func TestParseDiffHunks_GoldenCases(t *testing.T) {
	for _, caseName := range listCases(t) {
		t.Run(caseName, func(t *testing.T) {
			expected, patch := loadCase(t, caseName)

			hunks := tool.ParseDiffHunks(patch)

			// Build set of files covered by hunks.
			filesCovered := make(map[string]bool)
			for _, h := range hunks {
				filesCovered[h.File] = true
			}

			// Compute file-level recall.
			uniqueExpected := uniqueFiles(expected.ChangedFunctions)
			if len(uniqueExpected) == 0 {
				t.Skip("no expected changed functions — skipping recall check")
				return
			}

			coveredFiles := 0
			for _, f := range uniqueExpected {
				if filesCovered[f] {
					coveredFiles++
				} else {
					t.Errorf("file %q not covered by any hunk", f)
				}
			}

			recall := float64(coveredFiles) / float64(len(uniqueExpected))
			t.Logf("case=%s hunks=%d expected_files=%d covered=%d recall=%.2f",
				caseName, len(hunks), len(uniqueExpected), coveredFiles, recall)

			if recall < 0.8 {
				t.Errorf("file recall %.2f < 0.80 threshold", recall)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Layer A+: ParseDiffFunctions — function-name recall (no DB or index required)
//
// Checks that functions declared in added (+) lines in the patch can be
// detected by the regex-based parser. This catches regressions in the
// language-specific function-declaration patterns.
//
// NOTE: This test only fires for cases where changed_functions have
// start_line > 0, because regex detection only works when the function
// declaration itself appears in a + line. Cases where existing function
// bodies are modified (but not their signatures) are excluded.
// ---------------------------------------------------------------------------

// TestParseDiffFunctions_GoldenCases validates function-name extraction
// from diffs where the function declaration appears in an added line.
func TestParseDiffFunctions_GoldenCases(t *testing.T) {
	for _, caseName := range listCases(t) {
		t.Run(caseName, func(t *testing.T) {
			expected, patch := loadCase(t, caseName)

			funcs := tool.ParseDiffFunctions(patch)

			// Build set of detected function names (lowercased for comparison).
			detected := make(map[string]bool)
			for _, f := range funcs {
				detected[strings.ToLower(f.FuncName)] = true
				// Also index the "method.name" form for Python class methods.
				if idx := strings.LastIndex(f.FuncName, "."); idx >= 0 {
					detected[strings.ToLower(f.FuncName[idx+1:])] = true
				}
			}

			// Only check functions that appear as new declarations in the patch.
			// A function is a "new declaration" if its simple name appears in a + line.
			var declarationFuncs []ExpectedFunction
			for _, ef := range expected.ChangedFunctions {
				simpleName := ef.Name
				if idx := strings.LastIndex(ef.Name, "."); idx >= 0 {
					simpleName = ef.Name[idx+1:]
				}
				if patchContainsFuncDecl(patch, simpleName) {
					declarationFuncs = append(declarationFuncs, ef)
				}
			}

			if len(declarationFuncs) == 0 {
				t.Logf("case=%s: no new function declarations in patch — skipping func-name recall check", caseName)
				return
			}

			covered := 0
			for _, ef := range declarationFuncs {
				simpleName := strings.ToLower(ef.Name)
				if idx := strings.LastIndex(ef.Name, "."); idx >= 0 {
					simpleName = strings.ToLower(ef.Name[idx+1:])
				}
				if detected[simpleName] {
					covered++
				} else {
					t.Logf("func %q (file %s) not detected by ParseDiffFunctions", ef.Name, ef.File)
				}
			}

			recall := float64(covered) / float64(len(declarationFuncs))
			t.Logf("case=%s decl_funcs=%d detected=%d recall=%.2f",
				caseName, len(declarationFuncs), covered, recall)

			if recall < 0.5 {
				t.Errorf("func-name recall %.2f < 0.50 threshold (only new declarations counted)", recall)
			}
		})
	}
}

// patchContainsFuncDecl returns true if any added (+) line in the patch
// contains the function name followed by common declaration syntax.
func patchContainsFuncDecl(patch, funcName string) bool {
	lower := strings.ToLower(funcName)
	for _, line := range strings.Split(patch, "\n") {
		if !strings.HasPrefix(line, "+") {
			continue
		}
		lineLower := strings.ToLower(line)
		// Match patterns like: +def funcname(, +func funcname(, +func (r *T) funcname(
		if strings.Contains(lineLower, lower+"(") ||
			strings.Contains(lineLower, lower+" (") {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Schema validation: expected.json structural correctness
//
// No code execution — purely validates the test fixtures themselves.
// Catches typos (wrong protocol values, empty required fields, etc.)
// before a real run would surface them.
// ---------------------------------------------------------------------------

// TestExpectedJSON_Schema validates the structural correctness of every
// expected.json file: required fields present, enum values valid.
func TestExpectedJSON_Schema(t *testing.T) {
	for _, caseName := range listCases(t) {
		t.Run(caseName, func(t *testing.T) {
			expected, _ := loadCase(t, caseName)

			// changed_functions: name and file must be non-empty.
			for i, fn := range expected.ChangedFunctions {
				if fn.Name == "" {
					t.Errorf("changed_functions[%d]: name is empty", i)
				}
				if fn.File == "" {
					t.Errorf("changed_functions[%d]: file is empty", i)
				}
				if fn.Repo == "" {
					t.Errorf("changed_functions[%d]: repo is empty", i)
				}
			}

			// entry_points: protocol must be a known value.
			for i, ep := range expected.EntryPoints {
				if ep.Protocol != "" && !validProtocols[ep.Protocol] {
					t.Errorf("entry_points[%d]: unknown protocol %q (known: %v)",
						i, ep.Protocol, sortedKeys(validProtocols))
				}
				if ep.Function == "" {
					t.Errorf("entry_points[%d]: function is empty", i)
				}
			}

			// call_chain: source/target must be non-empty; type must be valid.
			for i, edge := range expected.CallChain {
				if edge.Source == "" {
					t.Errorf("call_chain[%d]: source is empty", i)
				}
				if edge.Target == "" {
					t.Errorf("call_chain[%d]: target is empty", i)
				}
				if edge.Type != "" && !validEdgeTypes[edge.Type] {
					t.Errorf("call_chain[%d]: unknown edge type %q (known: %v)",
						i, edge.Type, sortedKeys(validEdgeTypes))
				}
			}

			// cross_repo_calls: from_repo and to_repo must be non-empty.
			for i, cr := range expected.CrossRepoCalls {
				if cr.FromRepo == "" {
					t.Errorf("cross_repo_calls[%d]: from_repo is empty", i)
				}
				if cr.ToRepo == "" {
					t.Errorf("cross_repo_calls[%d]: to_repo is empty", i)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func uniqueFiles(fns []ExpectedFunction) []string {
	seen := make(map[string]bool)
	var result []string
	for _, fn := range fns {
		if !seen[fn.File] {
			seen[fn.File] = true
			result = append(result, fn.File)
		}
	}
	return result
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

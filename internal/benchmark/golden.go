// Package benchmark provides golden case loading, metrics evaluation,
// shadow parity comparison, and result normalization utilities.
package benchmark

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/DeviosLang/shirakami/internal/tool"
)

// ---------------------------------------------------------------------------
// Golden Case data model
// ---------------------------------------------------------------------------

// GoldenCase holds a loaded golden test case (patch content + expected output).
type GoldenCase struct {
	Name     string        // directory name (e.g. "go-gin-context-json")
	Dir      string        // absolute path to the case directory
	Patch    string        // contents of input.patch
	Expected GoldenExpected // parsed expected.json
}

// GoldenExpected mirrors the ExpectedResult type in tests/golden/runner_test.go.
// Defined here so the internal/benchmark package can import it without
// depending on the test package.
type GoldenExpected struct {
	ChangedFunctions []GoldenFunction `json:"changed_functions"`
	CallChain        []GoldenEdge     `json:"call_chain"`
	EntryPoints      []GoldenEntry    `json:"entry_points"`
	CrossRepoCalls   []GoldenCross    `json:"cross_repo_calls"`
}

// GoldenFunction is one entry in changed_functions.
type GoldenFunction struct {
	Name      string `json:"name"`
	Repo      string `json:"repo"`
	File      string `json:"file"`
	StartLine int    `json:"start_line"`
}

// GoldenEdge is one edge in call_chain.
type GoldenEdge struct {
	Source     string  `json:"source"`
	Target     string  `json:"target"`
	Type       string  `json:"type"`
	Depth      int     `json:"depth"`
	Confidence float64 `json:"confidence,omitempty"`
}

// GoldenEntry is one entry in entry_points.
type GoldenEntry struct {
	Function string `json:"function"`
	Repo     string `json:"repo"`
	File     string `json:"file"`
	Protocol string `json:"protocol"`
}

// GoldenCross is one entry in cross_repo_calls.
type GoldenCross struct {
	FromRepo   string  `json:"from_repo"`
	ToRepo     string  `json:"to_repo"`
	Function   string  `json:"function"`
	Confidence float64 `json:"confidence"`
}

// ---------------------------------------------------------------------------
// Per-case metrics
// ---------------------------------------------------------------------------

// CaseMetrics holds the evaluation results for a single golden case.
type CaseMetrics struct {
	CaseName string

	// Layer A — ParseDiffHunks: file-level coverage
	FilesExpected int
	FilesCovered  int
	FileRecall    float64 // FilesCovered / FilesExpected (NaN → -1 when 0 expected)

	// Layer A+ — ParseDiffFunctions: function-name-level coverage
	// Only counts functions whose declarations appear in added (+) lines.
	FuncsExpected int     // number of expected funcs with a declaration in patch
	FuncsCovered  int     // how many of those were detected
	FuncRecall    float64 // FuncsCovered / FuncsExpected (-1 when no declarations found)

	// Detailed lists for debug output
	MissingFiles []string // expected files not covered by any hunk
	MissingFuncs []string // expected decl-funcs not detected by ParseDiffFunctions
	DetectedFuncs []string // all function names detected by ParseDiffFunctions
}

// SummaryMetrics aggregates CaseMetrics across multiple cases.
type SummaryMetrics struct {
	TotalCases    int
	AvgFileRecall float64 // mean over cases with FilesExpected > 0
	AvgFuncRecall float64 // mean over cases with FuncsExpected > 0
	Cases         []CaseMetrics
}

// ---------------------------------------------------------------------------
// Loading
// ---------------------------------------------------------------------------

// LoadGoldenCases reads all subdirectories of dir and returns cases that have
// both input.patch and expected.json. Cases missing either file are silently skipped.
func LoadGoldenCases(dir string) ([]GoldenCase, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read golden dir %s: %w", dir, err)
	}

	var cases []GoldenCase
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		caseDir := filepath.Join(dir, entry.Name())

		patchBytes, err := os.ReadFile(filepath.Join(caseDir, "input.patch"))
		if err != nil {
			continue // no patch — skip
		}

		expectedBytes, err := os.ReadFile(filepath.Join(caseDir, "expected.json"))
		if err != nil {
			continue // no expected — skip
		}

		var expected GoldenExpected
		if err := json.Unmarshal(expectedBytes, &expected); err != nil {
			return nil, fmt.Errorf("parse expected.json for case %s: %w", entry.Name(), err)
		}

		cases = append(cases, GoldenCase{
			Name:     entry.Name(),
			Dir:      caseDir,
			Patch:    string(patchBytes),
			Expected: expected,
		})
	}
	return cases, nil
}

// LoadGoldenCase loads exactly one case by name from dir.
func LoadGoldenCase(dir, name string) (*GoldenCase, error) {
	cases, err := LoadGoldenCases(dir)
	if err != nil {
		return nil, err
	}
	for _, c := range cases {
		if c.Name == name {
			cc := c
			return &cc, nil
		}
	}
	return nil, fmt.Errorf("golden case %q not found in %s", name, dir)
}

// ---------------------------------------------------------------------------
// Evaluation
// ---------------------------------------------------------------------------

// EvalCase computes Layer A and Layer A+ metrics for a single golden case.
func EvalCase(gc GoldenCase) CaseMetrics {
	m := CaseMetrics{CaseName: gc.Name, FileRecall: -1, FuncRecall: -1}

	// ── Layer A: file-level recall via ParseDiffHunks ─────────────────────

	hunks := tool.ParseDiffHunks(gc.Patch)
	filesCoveredSet := make(map[string]bool)
	for _, h := range hunks {
		filesCoveredSet[h.File] = true
	}

	// Unique expected files
	expectedFilesSet := make(map[string]bool)
	for _, fn := range gc.Expected.ChangedFunctions {
		if fn.File != "" {
			expectedFilesSet[fn.File] = true
		}
	}
	m.FilesExpected = len(expectedFilesSet)

	if m.FilesExpected > 0 {
		for f := range expectedFilesSet {
			if filesCoveredSet[f] {
				m.FilesCovered++
			} else {
				m.MissingFiles = append(m.MissingFiles, f)
			}
		}
		sort.Strings(m.MissingFiles)
		m.FileRecall = float64(m.FilesCovered) / float64(m.FilesExpected)
	}

	// ── Layer A+: function-name recall via ParseDiffFunctions ─────────────

	detectedFuncs := tool.ParseDiffFunctions(gc.Patch)
	detectedSet := make(map[string]bool)
	for _, f := range detectedFuncs {
		name := strings.ToLower(f.FuncName)
		detectedSet[name] = true
		m.DetectedFuncs = append(m.DetectedFuncs, f.FuncName)
		// Also index short name after last dot (for Python class methods)
		if idx := strings.LastIndex(name, "."); idx >= 0 {
			detectedSet[name[idx+1:]] = true
		}
	}
	sort.Strings(m.DetectedFuncs)

	// Only count expected functions whose declarations appear as + lines.
	var declFuncs []GoldenFunction
	for _, ef := range gc.Expected.ChangedFunctions {
		simpleName := ef.Name
		if idx := strings.LastIndex(ef.Name, "."); idx >= 0 {
			simpleName = ef.Name[idx+1:]
		}
		if patchHasFuncDecl(gc.Patch, simpleName) {
			declFuncs = append(declFuncs, ef)
		}
	}
	m.FuncsExpected = len(declFuncs)

	if m.FuncsExpected > 0 {
		for _, ef := range declFuncs {
			simpleName := strings.ToLower(ef.Name)
			if idx := strings.LastIndex(simpleName, "."); idx >= 0 {
				simpleName = simpleName[idx+1:]
			}
			if detectedSet[simpleName] {
				m.FuncsCovered++
			} else {
				m.MissingFuncs = append(m.MissingFuncs, ef.Name)
			}
		}
		sort.Strings(m.MissingFuncs)
		m.FuncRecall = float64(m.FuncsCovered) / float64(m.FuncsExpected)
	}

	return m
}

// patchHasFuncDecl returns true if any added (+) line in the patch
// contains funcName followed by "(" — the common declaration syntax across
// Go, Python, and other supported languages.
func patchHasFuncDecl(patch, funcName string) bool {
	lower := strings.ToLower(funcName)
	for _, line := range strings.Split(patch, "\n") {
		if !strings.HasPrefix(line, "+") {
			continue
		}
		lineLower := strings.ToLower(line)
		if strings.Contains(lineLower, lower+"(") || strings.Contains(lineLower, lower+" (") {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Summary
// ---------------------------------------------------------------------------

// Summarize aggregates a slice of CaseMetrics into a SummaryMetrics.
func Summarize(cases []CaseMetrics) SummaryMetrics {
	s := SummaryMetrics{
		TotalCases: len(cases),
		Cases:      cases,
	}

	var fileSum, fileCount float64
	var funcSum, funcCount float64
	for _, c := range cases {
		if c.FileRecall >= 0 {
			fileSum += c.FileRecall
			fileCount++
		}
		if c.FuncRecall >= 0 {
			funcSum += c.FuncRecall
			funcCount++
		}
	}
	if fileCount > 0 {
		s.AvgFileRecall = fileSum / fileCount
	}
	if funcCount > 0 {
		s.AvgFuncRecall = funcSum / funcCount
	}
	return s
}

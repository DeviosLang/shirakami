package golden

import (
	"encoding/json"
	"os"
	"path/filepath"
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

// TestParseDiffHunks_GoldenCases validates that ParseDiffHunks correctly
// extracts changed line ranges from each golden case's input.patch.
// This tests Layer A (pure diff parsing) — no DB or index required.
func TestParseDiffHunks_GoldenCases(t *testing.T) {
	casesDir := filepath.Join("cases")
	entries, err := os.ReadDir(casesDir)
	if err != nil {
		t.Fatalf("read cases dir: %v", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		caseName := entry.Name()
		t.Run(caseName, func(t *testing.T) {
			caseDir := filepath.Join(casesDir, caseName)

			// Read input.patch
			patchBytes, err := os.ReadFile(filepath.Join(caseDir, "input.patch"))
			if err != nil {
				t.Fatalf("read input.patch: %v", err)
			}

			// Read expected.json
			expectedBytes, err := os.ReadFile(filepath.Join(caseDir, "expected.json"))
			if err != nil {
				t.Fatalf("read expected.json: %v", err)
			}

			var expected ExpectedResult
			if err := json.Unmarshal(expectedBytes, &expected); err != nil {
				t.Fatalf("parse expected.json: %v", err)
			}

			// Run ParseDiffHunks
			hunks := tool.ParseDiffHunks(string(patchBytes))

			// Verify: every changed function's file should appear in hunks
			filesCovered := make(map[string]bool)
			for _, h := range hunks {
				filesCovered[h.File] = true
			}

			missingFiles := 0
			for _, fn := range expected.ChangedFunctions {
				if !filesCovered[fn.File] {
					t.Errorf("changed function %s in file %s not covered by any hunk", fn.Name, fn.File)
					missingFiles++
				}
			}

			// Compute file-level recall
			totalExpectedFiles := len(uniqueFiles(expected.ChangedFunctions))
			coveredFiles := 0
			for _, f := range uniqueFiles(expected.ChangedFunctions) {
				if filesCovered[f] {
					coveredFiles++
				}
			}

			recall := float64(coveredFiles) / float64(totalExpectedFiles)
			t.Logf("case=%s hunks=%d expected_files=%d covered=%d recall=%.2f",
				caseName, len(hunks), totalExpectedFiles, coveredFiles, recall)

			if recall < 0.8 {
				t.Errorf("file recall %.2f < 0.80 threshold", recall)
			}
		})
	}
}

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

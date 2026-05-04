package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/DeviosLang/shirakami/internal/tool"
)

// buildBenchmarkCmd creates the `shirakami benchmark` command group.
func buildBenchmarkCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "benchmark",
		Short: "Run benchmarks and verify analysis quality",
	}
	cmd.AddCommand(buildBenchmarkVerifyCmd())
	return cmd
}

// ---------------------------------------------------------------------------
// shirakami benchmark verify
// ---------------------------------------------------------------------------

func buildBenchmarkVerifyCmd() *cobra.Command {
	var (
		goldenDir string
		metric    string
		threshold float64
		guards    []string
	)

	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Verify ParseDiffHunks coverage against golden cases (single number output)",
		Long: `Outputs a single metric value to stdout and exits with code 0 (pass) or 1 (fail).

Designed for CI pipelines and autoresearch-style loops:
  shirakami benchmark verify --golden-dir tests/golden/cases/
  # stdout: 1.00
  # exit code: 0

  shirakami benchmark verify --threshold 0.95
  # stdout: 0.80
  # exit code: 1 (below threshold)`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if goldenDir == "" {
				goldenDir = "tests/golden/cases"
			}

			entries, err := os.ReadDir(goldenDir)
			if err != nil {
				return fmt.Errorf("read golden dir %s: %w", goldenDir, err)
			}

			totalCases := 0
			totalExpectedFiles := 0
			totalCoveredFiles := 0

			for _, entry := range entries {
				if !entry.IsDir() {
					continue
				}
				caseDir := filepath.Join(goldenDir, entry.Name())

				// Read input.patch
				patchBytes, err := os.ReadFile(filepath.Join(caseDir, "input.patch"))
				if err != nil {
					continue // skip cases without patch
				}

				// Read expected.json
				expectedBytes, err := os.ReadFile(filepath.Join(caseDir, "expected.json"))
				if err != nil {
					continue
				}

				var expected struct {
					ChangedFunctions []struct {
						File string `json:"file"`
					} `json:"changed_functions"`
				}
				if err := json.Unmarshal(expectedBytes, &expected); err != nil {
					continue
				}

				// Run ParseDiffHunks
				hunks := tool.ParseDiffHunks(string(patchBytes))
				filesCovered := make(map[string]bool)
				for _, h := range hunks {
					filesCovered[h.File] = true
				}

				// Count unique expected files
				expectedFiles := make(map[string]bool)
				for _, fn := range expected.ChangedFunctions {
					expectedFiles[fn.File] = true
				}

				covered := 0
				for f := range expectedFiles {
					if filesCovered[f] {
						covered++
					}
				}

				totalCases++
				totalExpectedFiles += len(expectedFiles)
				totalCoveredFiles += covered
			}

			if totalCases == 0 {
				fmt.Fprintf(os.Stderr, "no golden cases found in %s\n", goldenDir)
				os.Exit(1)
			}

			// Compute the metric
			var value float64
			switch metric {
			case "file_recall":
				value = float64(totalCoveredFiles) / float64(totalExpectedFiles)
			default:
				value = float64(totalCoveredFiles) / float64(totalExpectedFiles)
			}

			// Output single number (machine-readable)
			fmt.Printf("%.4f\n", value)

			// Check guards
			for _, guard := range guards {
				var guardMetric string
				var guardThreshold float64
				_, err := fmt.Sscanf(guard, "%s >= %f", &guardMetric, &guardThreshold)
				if err == nil && value < guardThreshold {
					fmt.Fprintf(os.Stderr, "FAIL guard: %s (value=%.4f < %.4f)\n", guard, value, guardThreshold)
					os.Exit(1)
				}
			}

			// Check threshold
			if threshold > 0 && value < threshold {
				fmt.Fprintf(os.Stderr, "FAIL: %.4f < threshold %.4f (%d cases, %d/%d files covered)\n",
					value, threshold, totalCases, totalCoveredFiles, totalExpectedFiles)
				os.Exit(1)
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&goldenDir, "golden-dir", "tests/golden/cases", "path to golden test cases directory")
	cmd.Flags().StringVar(&metric, "metric", "file_recall", "metric to output (default: file_recall)")
	cmd.Flags().Float64Var(&threshold, "threshold", 0, "minimum acceptable value (exit 1 if below)")
	cmd.Flags().StringArrayVar(&guards, "guard", nil, "guard expression (e.g. 'file_recall >= 0.80')")

	return cmd
}

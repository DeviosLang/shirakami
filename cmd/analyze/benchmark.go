package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/DeviosLang/shirakami/internal/benchmark"
	"github.com/DeviosLang/shirakami/internal/config"
	"github.com/DeviosLang/shirakami/internal/llm"
	"github.com/DeviosLang/shirakami/internal/tool"
)

// buildBenchmarkCmd creates the `shirakami benchmark` command group.
func buildBenchmarkCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "benchmark",
		Short: "Run benchmarks and verify analysis quality",
	}
	cmd.AddCommand(buildBenchmarkVerifyCmd())
	cmd.AddCommand(buildBenchmarkRunCmd())
	cmd.AddCommand(buildBenchmarkDebugCmd())
	cmd.AddCommand(buildBenchmarkJudgeCmd())
	cmd.AddCommand(buildBenchmarkAutoJudgeCmd())
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

// ---------------------------------------------------------------------------
// shirakami benchmark run
// ---------------------------------------------------------------------------

func buildBenchmarkRunCmd() *cobra.Command {
	var (
		goldenDir string
		format    string
		failBelow float64
	)

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run Layer A evaluation on all golden cases and print a summary table",
		Long: `Evaluates ParseDiffHunks (file-level) and ParseDiffFunctions (function-level)
against all golden cases and prints per-case metrics plus an aggregate summary.

Examples:
  shirakami benchmark run
  shirakami benchmark run --golden-dir tests/golden/cases --format json
  shirakami benchmark run --fail-below 0.90`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cases, err := benchmark.LoadGoldenCases(goldenDir)
			if err != nil {
				return err
			}
			if len(cases) == 0 {
				fmt.Fprintf(os.Stderr, "no golden cases found in %s\n", goldenDir)
				os.Exit(1)
			}

			metrics := make([]benchmark.CaseMetrics, 0, len(cases))
			for _, gc := range cases {
				metrics = append(metrics, benchmark.EvalCase(gc))
			}
			summary := benchmark.Summarize(metrics)

			if format == "json" {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(summary)
			}

			// Terminal table output
			const (
				colCase = 32
				colFR   = 12
				colFnR  = 12
				colMiss = 0 // unbounded
			)
			header := fmt.Sprintf("%-*s  %-*s  %-*s  %s",
				colCase, "CASE",
				colFR, "FILE_RECALL",
				colFnR, "FUNC_RECALL",
				"MISSING_FUNCS",
			)
			fmt.Println(header)
			fmt.Println(strings.Repeat("─", len(header)+20))

			for _, m := range summary.Cases {
				frStr := fmtRecall(m.FileRecall)
				fnrStr := fmtRecall(m.FuncRecall)
				missing := "-"
				if len(m.MissingFuncs) > 0 {
					missing = strings.Join(m.MissingFuncs, ", ")
				}
				fmt.Printf("%-*s  %-*s  %-*s  %s\n",
					colCase, m.CaseName,
					colFR, frStr,
					colFnR, fnrStr,
					missing,
				)
			}

			fmt.Println(strings.Repeat("─", len(header)+20))
			fmt.Printf("%-*s  %-*s  %-*s\n",
				colCase, fmt.Sprintf("AVERAGE (%d cases)", summary.TotalCases),
				colFR, fmt.Sprintf("%.2f", summary.AvgFileRecall),
				colFnR, fmt.Sprintf("%.2f", summary.AvgFuncRecall),
			)

			// Exit code 1 if below threshold
			if failBelow > 0 && summary.AvgFileRecall < failBelow {
				fmt.Fprintf(os.Stderr, "\nFAIL: avg_file_recall=%.4f < --fail-below=%.4f\n",
					summary.AvgFileRecall, failBelow)
				os.Exit(1)
			}
			// Also fail if any individual case is below 0.80 (CI gate)
			for _, m := range summary.Cases {
				if m.FileRecall >= 0 && m.FileRecall < 0.80 {
					fmt.Fprintf(os.Stderr, "\nFAIL: case %q file_recall=%.2f < 0.80\n",
						m.CaseName, m.FileRecall)
					os.Exit(1)
				}
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&goldenDir, "golden-dir", "tests/golden/cases", "path to golden test cases directory")
	cmd.Flags().StringVar(&format, "format", "terminal", "output format: terminal or json")
	cmd.Flags().Float64Var(&failBelow, "fail-below", 0, "exit 1 if avg_file_recall is below this value")

	return cmd
}

// fmtRecall formats a recall value, using "n/a" for -1.
func fmtRecall(v float64) string {
	if v < 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.2f", v)
}

// ---------------------------------------------------------------------------
// shirakami benchmark debug
// ---------------------------------------------------------------------------

func buildBenchmarkDebugCmd() *cobra.Command {
	var goldenDir string

	cmd := &cobra.Command{
		Use:   "debug <case-name>",
		Short: "Print detailed Layer A evaluation for a single golden case",
		Long: `Loads a single golden case and prints:
  1. Parsed diff hunks (file, line range)
  2. Functions detected by ParseDiffFunctions
  3. Expected changed_functions from expected.json
  4. Per-function match status (✓ detected / ✗ missing)

Example:
  shirakami benchmark debug go-gin-context-json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			caseName := args[0]
			gc, err := benchmark.LoadGoldenCase(goldenDir, caseName)
			if err != nil {
				return err
			}

			fmt.Printf("═══ Case: %s ═══\n\n", gc.Name)

			// 1. Diff hunks
			hunks := tool.ParseDiffHunks(gc.Patch)
			fmt.Printf("── ParseDiffHunks (%d hunks) ──────────────────────────\n", len(hunks))
			for _, h := range hunks {
				fmt.Printf("  %-40s  lines %d–%d\n", h.File, h.StartLine, h.EndLine)
			}
			fmt.Println()

			// 2. Detected functions
			detected := tool.ParseDiffFunctions(gc.Patch)
			fmt.Printf("── ParseDiffFunctions (%d detected) ────────────────────\n", len(detected))
			for _, f := range detected {
				fmt.Printf("  %-30s  %s:%d\n", f.FuncName, f.File, f.Line)
			}
			fmt.Println()

			// 3. Expected + match status
			m := benchmark.EvalCase(*gc)
			detectedSet := make(map[string]bool)
			for _, fn := range m.DetectedFuncs {
				detectedSet[strings.ToLower(fn)] = true
			}

			fmt.Printf("── Expected changed_functions (%d) ─────────────────────\n", len(gc.Expected.ChangedFunctions))
			for _, ef := range gc.Expected.ChangedFunctions {
				simpleName := ef.Name
				if idx := strings.LastIndex(ef.Name, "."); idx >= 0 {
					simpleName = ef.Name[idx+1:]
				}
				mark := "✓"
				if !detectedSet[strings.ToLower(simpleName)] {
					mark = "✗"
				}
				fmt.Printf("  %s  %-30s  %s:%d\n", mark, ef.Name, ef.File, ef.StartLine)
			}
			fmt.Println()

			// 4. Summary
			fmt.Printf("── Summary ──────────────────────────────────────────────\n")
			fmt.Printf("  file_recall : %s  (%d/%d files)\n",
				fmtRecall(m.FileRecall), m.FilesCovered, m.FilesExpected)
			fmt.Printf("  func_recall : %s  (%d/%d decl-funcs)\n",
				fmtRecall(m.FuncRecall), m.FuncsCovered, m.FuncsExpected)
			if len(m.MissingFiles) > 0 {
				fmt.Printf("  missing files: %s\n", strings.Join(m.MissingFiles, ", "))
			}
			if len(m.MissingFuncs) > 0 {
				fmt.Printf("  missing funcs: %s\n", strings.Join(m.MissingFuncs, ", "))
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&goldenDir, "golden-dir", "tests/golden/cases", "path to golden test cases directory")

	return cmd
}

// ---------------------------------------------------------------------------
// shirakami benchmark judge
// ---------------------------------------------------------------------------

// buildBenchmarkJudgeCmd creates the `shirakami benchmark judge` sub-command.
//
// Usage:
//
//	shirakami benchmark judge --run latest --symbol "Cache.Get" --verdict tp
//	shirakami benchmark judge --run latest --symbol "order-service:placeOrder→payment-service:Charge" --verdict fp
func buildBenchmarkJudgeCmd() *cobra.Command {
	var (
		parityDir string
		runID     string
		symbol    string
		verdict   string
		trendFile string
	)

	cmd := &cobra.Command{
		Use:   "judge",
		Short: "Apply a tp/fp verdict to an extra_pending shadow-parity record",
		Long: `Loads a saved shadow parity report, applies a tp (true positive) or fp
(false positive) verdict to the first extra_pending record matching the given
symbol key, persists the updated report, and appends a row to the trend CSV.

Examples:
  # Mark 'Cache.Get' as a true positive (v2 correctly found a caller v1 missed)
  shirakami benchmark judge --symbol "Cache.Get" --verdict tp

  # Mark by full edge key as false positive
  shirakami benchmark judge \
    --run 2026-05-06T12-00-00 \
    --symbol "order-service:placeOrder→payment-service:Charge[CALLS]" \
    --verdict fp`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if symbol == "" {
				return fmt.Errorf("--symbol is required")
			}
			if verdict == "" {
				return fmt.Errorf("--verdict is required (tp or fp)")
			}

			// Load the report.
			report, err := benchmark.LoadReport(parityDir, runID)
			if err != nil {
				return fmt.Errorf("load report: %w", err)
			}

			pendingBefore := report.ExtraPendingCount
			if err := benchmark.ApplyVerdict(report, symbol, verdict); err != nil {
				return err
			}

			fmt.Printf("Applied verdict=%s to run=%s (extra_pending: %d → %d)\n",
				strings.ToUpper(verdict), report.RunID, pendingBefore, report.ExtraPendingCount)
			fmt.Printf("  miss_rate=%.4f  fp_rate=%.4f\n", report.MissRate, report.FPRate)

			// Persist updated report (overwrite timestamped file + latest.json).
			if err := benchmark.SaveReport(report, parityDir); err != nil {
				return fmt.Errorf("save updated report: %w", err)
			}
			fmt.Printf("Saved updated report to %s/\n", parityDir)

			// Append trend row.
			if err := benchmark.AppendTrend(trendFile, report); err != nil {
				// Non-fatal — trend is informational.
				fmt.Fprintf(os.Stderr, "warn: failed to append trend: %v\n", err)
			} else {
				fmt.Printf("Appended trend row to %s\n", trendFile)
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&parityDir, "parity-dir", "reports/shadow-parity", "directory containing parity report JSON files")
	cmd.Flags().StringVar(&runID, "run", "latest", "run ID to load (use 'latest' for the most recent run)")
	cmd.Flags().StringVar(&symbol, "symbol", "", "symbol key to match in extra_pending records (required)")
	cmd.Flags().StringVar(&verdict, "verdict", "", "tp (true positive) or fp (false positive) (required)")
	cmd.Flags().StringVar(&trendFile, "trend-file", "reports/shadow-parity/parity-trend.csv", "path to the trend CSV file")

	_ = cmd.MarkFlagRequired("symbol")
	_ = cmd.MarkFlagRequired("verdict")

	return cmd
}

// ---------------------------------------------------------------------------
// shirakami benchmark autojudge
// ---------------------------------------------------------------------------

// buildBenchmarkAutoJudgeCmd creates the `shirakami benchmark autojudge` command.
//
// Usage:
//
//	shirakami benchmark autojudge --run latest
//	shirakami benchmark autojudge --run 2026-05-06T12-00-00 --max-records 5
func buildBenchmarkAutoJudgeCmd() *cobra.Command {
	var (
		parityDir  string
		runID      string
		trendFile  string
		maxRecords int
		verbose    bool
	)

	cmd := &cobra.Command{
		Use:   "autojudge",
		Short: "Auto-judge extra_pending records via adversarial LLM verification (§12.6)",
		Long: `Runs the three-phase Generate→Critic→Judge adversarial pipeline on every
extra_pending record in the specified shadow-parity report.

Each record gets three LLM calls:
  1. Proof   — argue the call EXISTS
  2. Disproof — argue the call does NOT exist
  3. Judge   — weigh both sides → EXISTS / NOT_EXISTS / UNCERTAIN

Records judged UNCERTAIN remain extra_pending for human review.
The report is updated in-place and a trend row is appended.

Examples:
  shirakami benchmark autojudge --run latest
  shirakami benchmark autojudge --run 2026-05-06T12-00-00 --max-records 3 --verbose`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Load LLM client from config.
			cfg, err := config.Load(cfgFile)
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			llmClient := llm.NewClient(llm.Config{
				BaseURL:   cfg.LLM.Endpoint,
				APIKey:    cfg.LLM.APIKey,
				Model:     cfg.LLM.Model,
				MaxTokens: 2048,
			})

			report, err := benchmark.LoadReport(parityDir, runID)
			if err != nil {
				return fmt.Errorf("load report: %w", err)
			}

			if report.ExtraPendingCount == 0 {
				fmt.Println("No extra_pending records — nothing to judge.")
				return nil
			}

			fmt.Printf("Auto-judging %d extra_pending records in run %s...\n",
				report.ExtraPendingCount, report.RunID)

			opts := benchmark.AutoJudgeOptions{
				MaxRecords: maxRecords,
			}
			if verbose {
				opts.Verbose = func(format string, args ...any) {
					fmt.Printf(format+"\n", args...)
				}
			}

			summary, err := benchmark.RunAutoJudge(context.Background(), llmClient, report, opts)
			if err != nil {
				return fmt.Errorf("auto-judge: %w", err)
			}

			// Print summary table.
			fmt.Printf("\n── Auto-Judge Summary ──────────────────────────────────\n")
			fmt.Printf("  Total extra_pending : %d\n", summary.Total)
			fmt.Printf("  Judged (definitive) : %d  (tp=%d, fp=%d)\n",
				summary.Judged, summary.TPCount, summary.FPCount)
			fmt.Printf("  Remaining (uncertain): %d\n", summary.Remaining)
			fmt.Printf("  miss_rate : %.4f    fp_rate : %.4f\n",
				report.MissRate, report.FPRate)

			// Save updated report.
			if err := benchmark.SaveReport(report, parityDir); err != nil {
				return fmt.Errorf("save report: %w", err)
			}
			fmt.Printf("Saved updated report to %s/\n", parityDir)

			// Append trend row.
			trendPath := trendFile
			if trendPath == "" {
				trendPath = filepath.Join(parityDir, "parity-trend.csv")
			}
			if err := benchmark.AppendTrend(trendPath, report); err != nil {
				fmt.Fprintf(os.Stderr, "warn: failed to append trend: %v\n", err)
			} else {
				fmt.Printf("Appended trend row to %s\n", trendPath)
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&parityDir, "parity-dir", "reports/shadow-parity", "directory containing parity report JSON files")
	cmd.Flags().StringVar(&runID, "run", "latest", "run ID to load (use 'latest' for the most recent run)")
	cmd.Flags().StringVar(&trendFile, "trend-file", "", "path to trend CSV (default: <parity-dir>/parity-trend.csv)")
	cmd.Flags().IntVar(&maxRecords, "max-records", 0, "cap on records to judge in one run (0 = no limit)")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "print per-record judgment details")

	return cmd
}

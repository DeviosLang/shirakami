// Package benchmark provides shadow parity comparison, golden test evaluation,
// and metrics computation for Shirakami v1 (LLM) vs v2 (deterministic) analysis.
package benchmark

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Normalized representation for comparison
// ---------------------------------------------------------------------------

// NormalizedEdge is the common structure both v1 and v2 results are projected to.
// Comparison is set-based (order-independent).
type NormalizedEdge struct {
	SourceRepo string `json:"source_repo"`
	SourceFile string `json:"source_file"`
	SourceFunc string `json:"source_func"`
	TargetRepo string `json:"target_repo"`
	TargetFile string `json:"target_file"`
	TargetFunc string `json:"target_func"`
	EdgeType   string `json:"edge_type"` // CALLS / IMPORTS / EXTENDS / IMPLEMENTS
}

// Key returns a deduplication key for set comparison (order-independent).
func (e NormalizedEdge) Key() string {
	return fmt.Sprintf("%s:%s:%s→%s:%s:%s[%s]",
		e.SourceRepo, e.SourceFile, e.SourceFunc,
		e.TargetRepo, e.TargetFile, e.TargetFunc,
		e.EdgeType)
}

// NormalizedEntryPoint is an entry point in normalized form.
type NormalizedEntryPoint struct {
	Repo     string `json:"repo"`
	File     string `json:"file"`
	Function string `json:"function"`
	Protocol string `json:"protocol"`
	Path     string `json:"path"`
}

// Key returns a deduplication key.
func (e NormalizedEntryPoint) Key() string {
	return fmt.Sprintf("%s:%s:%s", e.Repo, e.File, e.Function)
}

// NormalizedResult is the unified representation of an analysis output.
type NormalizedResult struct {
	ChangedFunctions []string               `json:"changed_functions"`
	Edges            []NormalizedEdge        `json:"edges"`
	EntryPoints      []NormalizedEntryPoint  `json:"entry_points"`
}

// ---------------------------------------------------------------------------
// Shadow Diff
// ---------------------------------------------------------------------------

// DiffCategory classifies a single edge comparison result.
type DiffCategory string

const (
	CategoryMatch       DiffCategory = "match"         // both sides found this edge
	CategoryMiss        DiffCategory = "miss"          // v2 missed (v1 has, v2 doesn't) — false negative
	CategoryExtraPend   DiffCategory = "extra_pending" // v2 extra (v2 has, v1 doesn't) — pending judgment
	CategoryExtraTP     DiffCategory = "extra_tp"      // confirmed true positive (v2 found real caller v1 missed)
	CategoryExtraFP     DiffCategory = "extra_fp"      // confirmed false positive (v2 hallucinated)
)

// DiffRecord is one edge-level comparison result.
type DiffRecord struct {
	Edge     NormalizedEdge `json:"edge"`
	Category DiffCategory   `json:"category"`
	Details  string         `json:"details,omitempty"`
}

// ---------------------------------------------------------------------------
// Shadow Parity Report
// ---------------------------------------------------------------------------

// ParityReport aggregates all DiffRecords from one analysis run.
type ParityReport struct {
	RunID         string                  `json:"run_id"`
	Timestamp     time.Time               `json:"timestamp"`
	SourceRepo    string                  `json:"source_repo"`
	Description   string                  `json:"description"`

	// Counts
	TotalEdgesV1      int `json:"total_edges_v1"`
	TotalEdgesV2      int `json:"total_edges_v2"`
	MatchCount        int `json:"match_count"`
	MissCount         int `json:"miss_count"`          // v2 missed (false negative)
	ExtraPendingCount int `json:"extra_pending_count"` // v2 extra, not yet judged
	ExtraTPCount      int `json:"extra_tp_count"`      // confirmed true positive
	ExtraFPCount      int `json:"extra_fp_count"`      // confirmed false positive

	// Entry points
	EntryPointsV1    int `json:"entry_points_v1"`
	EntryPointsV2    int `json:"entry_points_v2"`
	EntryPointMatch  int `json:"entry_point_match"`
	EntryPointMiss   int `json:"entry_point_miss"`

	// Quality metrics
	MissRate    float64 `json:"miss_rate"`    // MissCount / (MatchCount + MissCount)
	FPRate      float64 `json:"fp_rate"`      // ExtraFPCount / (MatchCount + MissCount + ExtraFPCount)

	// Detailed records
	Records []DiffRecord `json:"records"`
}

// ---------------------------------------------------------------------------
// Comparison Engine
// ---------------------------------------------------------------------------

// CompareResults computes the shadow parity between legacy (v1) and new (v2) results.
func CompareResults(v1, v2 *NormalizedResult, sourceRepo, description string) *ParityReport {
	report := &ParityReport{
		RunID:       fmt.Sprintf("%d", time.Now().UnixMilli()),
		Timestamp:   time.Now(),
		SourceRepo:  sourceRepo,
		Description: description,
		TotalEdgesV1: len(v1.Edges),
		TotalEdgesV2: len(v2.Edges),
	}

	// Build edge sets
	v1Set := make(map[string]NormalizedEdge)
	for _, e := range v1.Edges {
		v1Set[e.Key()] = e
	}
	v2Set := make(map[string]NormalizedEdge)
	for _, e := range v2.Edges {
		v2Set[e.Key()] = e
	}

	// Match: in both
	for key, edge := range v1Set {
		if _, found := v2Set[key]; found {
			report.Records = append(report.Records, DiffRecord{
				Edge:     edge,
				Category: CategoryMatch,
			})
			report.MatchCount++
		} else {
			report.Records = append(report.Records, DiffRecord{
				Edge:     edge,
				Category: CategoryMiss,
				Details:  "v1 found this edge but v2 did not",
			})
			report.MissCount++
		}
	}

	// Extra: in v2 but not v1
	for key, edge := range v2Set {
		if _, found := v1Set[key]; !found {
			report.Records = append(report.Records, DiffRecord{
				Edge:     edge,
				Category: CategoryExtraPend,
				Details:  "v2 found this edge but v1 did not — pending judgment",
			})
			report.ExtraPendingCount++
		}
	}

	// Entry point comparison
	v1EP := make(map[string]bool)
	for _, ep := range v1.EntryPoints {
		v1EP[ep.Key()] = true
	}
	v2EP := make(map[string]bool)
	for _, ep := range v2.EntryPoints {
		v2EP[ep.Key()] = true
	}
	report.EntryPointsV1 = len(v1.EntryPoints)
	report.EntryPointsV2 = len(v2.EntryPoints)
	for key := range v1EP {
		if v2EP[key] {
			report.EntryPointMatch++
		} else {
			report.EntryPointMiss++
		}
	}

	// Compute rates
	denom := report.MatchCount + report.MissCount
	if denom > 0 {
		report.MissRate = float64(report.MissCount) / float64(denom)
	}
	fpDenom := report.MatchCount + report.MissCount + report.ExtraFPCount
	if fpDenom > 0 {
		report.FPRate = float64(report.ExtraFPCount) / float64(fpDenom)
	}

	return report
}

// ---------------------------------------------------------------------------
// Persistence
// ---------------------------------------------------------------------------

// SaveReport writes a parity report to disk as JSON.
func SaveReport(report *ParityReport, outputDir string) error {
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}

	// Write timestamped file
	filename := fmt.Sprintf("%s.json", report.Timestamp.Format("2006-01-02T15-04-05"))
	path := filepath.Join(outputDir, filename)
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal report: %w", err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("write report: %w", err)
	}

	// Update latest.json symlink/copy
	latestPath := filepath.Join(outputDir, "latest.json")
	_ = os.Remove(latestPath)
	if err := os.WriteFile(latestPath, data, 0644); err != nil {
		return fmt.Errorf("write latest: %w", err)
	}

	return nil
}

// ---------------------------------------------------------------------------
// Terminal output
// ---------------------------------------------------------------------------

// FormatTerminalReport generates a human-readable summary for terminal output.
func FormatTerminalReport(report *ParityReport) string {
	var sb strings.Builder

	sb.WriteString("\n[Shadow Parity Report]\n")
	sb.WriteString(fmt.Sprintf("  Source: %s | %s\n", report.SourceRepo, report.Description))
	sb.WriteString(fmt.Sprintf("  Edges: v1=%d  v2=%d\n", report.TotalEdgesV1, report.TotalEdgesV2))
	sb.WriteString(fmt.Sprintf("  Match: %d (%.1f%%)  |  Miss: %d (%.1f%%)  |  Extra: %d (pending)\n",
		report.MatchCount, (1-report.MissRate)*100,
		report.MissCount, report.MissRate*100,
		report.ExtraPendingCount))
	sb.WriteString(fmt.Sprintf("  Entry Points: v1=%d  v2=%d  match=%d  miss=%d\n",
		report.EntryPointsV1, report.EntryPointsV2,
		report.EntryPointMatch, report.EntryPointMiss))

	// Show misses (most actionable)
	misses := filterRecords(report.Records, CategoryMiss)
	if len(misses) > 0 {
		sb.WriteString("\n  Misses (v2 未找到):\n")
		for _, r := range misses {
			if len(sb.String()) > 2000 {
				sb.WriteString(fmt.Sprintf("    ... and %d more\n", len(misses)-5))
				break
			}
			sb.WriteString(fmt.Sprintf("    - %s:%s → %s\n",
				r.Edge.SourceRepo, r.Edge.SourceFunc, r.Edge.TargetFunc))
		}
	}

	// Show extras
	extras := filterRecords(report.Records, CategoryExtraPend)
	if len(extras) > 0 {
		sb.WriteString("\n  Extras (v2 额外发现, 待评判):\n")
		for i, r := range extras {
			if i >= 5 {
				sb.WriteString(fmt.Sprintf("    ... and %d more\n", len(extras)-5))
				break
			}
			sb.WriteString(fmt.Sprintf("    + %s:%s → %s\n",
				r.Edge.SourceRepo, r.Edge.SourceFunc, r.Edge.TargetFunc))
		}
	}

	return sb.String()
}

func filterRecords(records []DiffRecord, category DiffCategory) []DiffRecord {
	var result []DiffRecord
	for _, r := range records {
		if r.Category == category {
			result = append(result, r)
		}
	}
	return result
}

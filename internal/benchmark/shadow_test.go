package benchmark

import (
	"testing"
)

func TestCompareResults_BasicParity(t *testing.T) {
	v1 := &NormalizedResult{
		Edges: []NormalizedEdge{
			{SourceRepo: "r1", SourceFunc: "A", TargetRepo: "r1", TargetFunc: "B", EdgeType: "CALLS"},
			{SourceRepo: "r1", SourceFunc: "B", TargetRepo: "r1", TargetFunc: "C", EdgeType: "CALLS"},
			{SourceRepo: "r1", SourceFunc: "C", TargetRepo: "r2", TargetFunc: "D", EdgeType: "CALLS"},
		},
		EntryPoints: []NormalizedEntryPoint{
			{Repo: "r2", File: "handler.go", Function: "D"},
		},
	}

	v2 := &NormalizedResult{
		Edges: []NormalizedEdge{
			{SourceRepo: "r1", SourceFunc: "A", TargetRepo: "r1", TargetFunc: "B", EdgeType: "CALLS"},
			{SourceRepo: "r1", SourceFunc: "B", TargetRepo: "r1", TargetFunc: "C", EdgeType: "CALLS"},
			// Missing C→D (miss)
			// Extra E→F (extra_pending)
			{SourceRepo: "r1", SourceFunc: "E", TargetRepo: "r1", TargetFunc: "F", EdgeType: "CALLS"},
		},
		EntryPoints: []NormalizedEntryPoint{
			{Repo: "r2", File: "handler.go", Function: "D"},
		},
	}

	report := CompareResults(v1, v2, "r1", "test parity")

	if report.TotalEdgesV1 != 3 {
		t.Errorf("TotalEdgesV1: got %d, want 3", report.TotalEdgesV1)
	}
	if report.TotalEdgesV2 != 3 {
		t.Errorf("TotalEdgesV2: got %d, want 3", report.TotalEdgesV2)
	}
	if report.MatchCount != 2 {
		t.Errorf("MatchCount: got %d, want 2", report.MatchCount)
	}
	if report.MissCount != 1 {
		t.Errorf("MissCount: got %d, want 1", report.MissCount)
	}
	if report.ExtraPendingCount != 1 {
		t.Errorf("ExtraPendingCount: got %d, want 1", report.ExtraPendingCount)
	}
	if report.EntryPointMatch != 1 {
		t.Errorf("EntryPointMatch: got %d, want 1", report.EntryPointMatch)
	}
	if report.EntryPointMiss != 0 {
		t.Errorf("EntryPointMiss: got %d, want 0", report.EntryPointMiss)
	}

	// MissRate = 1 / (2+1) = 0.333...
	if report.MissRate < 0.32 || report.MissRate > 0.34 {
		t.Errorf("MissRate: got %.3f, want ~0.333", report.MissRate)
	}
}

func TestCompareResults_EmptyResults(t *testing.T) {
	v1 := &NormalizedResult{}
	v2 := &NormalizedResult{}

	report := CompareResults(v1, v2, "r1", "empty")
	if report.MatchCount != 0 || report.MissCount != 0 || report.ExtraPendingCount != 0 {
		t.Errorf("expected all zeros for empty results, got match=%d miss=%d extra=%d",
			report.MatchCount, report.MissCount, report.ExtraPendingCount)
	}
}

func TestNormalizeV1Result(t *testing.T) {
	workers := map[string]*WorkerResultData{
		"repo1": {
			RepoName: "repo1",
			Nodes: []CallNode{
				{Repo: "repo1", Function: "foo", File: "a.go"},
				{Repo: "repo1", Function: "bar", File: "b.go"},
				{Repo: "repo1", Function: "baz", File: "c.go"},
			},
			CrossRepoCalls: []CrossRepoCallData{
				{
					TargetRepo:     "repo2",
					TargetFunction: "handler",
					CallerNode:     CallNode{Repo: "repo1", Function: "baz", File: "c.go"},
				},
			},
			EntryPoints: []CallNode{
				{Repo: "repo2", Function: "handler", File: "h.go"},
			},
		},
	}

	result := NormalizeV1Result("repo1", workers)
	if len(result.Edges) != 3 {
		t.Errorf("Edges: got %d, want 3 (2 chain + 1 cross-repo)", len(result.Edges))
	}
	if len(result.EntryPoints) != 1 {
		t.Errorf("EntryPoints: got %d, want 1", len(result.EntryPoints))
	}
}

func TestNormalizeV2Result(t *testing.T) {
	data := &GraphImpactData{
		AffectedSymbols: []AffectedSymbolData{
			{ID: "1", Name: "Caller1", FilePath: "x.go", Repo: "repo1", Depth: 1, EdgeType: "CALLS"},
			{ID: "2", Name: "Caller2", FilePath: "y.go", Repo: "repo2", Depth: 2, EdgeType: "IMPORTS"},
		},
		EntryPoints: []CallNode{
			{Repo: "repo2", Function: "Main", File: "main.go"},
		},
	}

	result := NormalizeV2Result("repo1", []string{"foo", "bar"}, data)
	if len(result.Edges) != 2 {
		t.Errorf("Edges: got %d, want 2", len(result.Edges))
	}
	if len(result.EntryPoints) != 1 {
		t.Errorf("EntryPoints: got %d, want 1", len(result.EntryPoints))
	}
	if len(result.ChangedFunctions) != 2 {
		t.Errorf("ChangedFunctions: got %d, want 2", len(result.ChangedFunctions))
	}
	// Check edge types are preserved
	if result.Edges[1].EdgeType != "IMPORTS" {
		t.Errorf("EdgeType: got %s, want IMPORTS", result.Edges[1].EdgeType)
	}
}

func TestExtractChangedFiles(t *testing.T) {
	fns := []string{
		"repo1/pkg/utils/helper.go::DoStuff",
		"repo1/pkg/utils/helper.go::DoMore",
		"repo1/cmd/main.go::Run",
		"nofile_format",
	}
	files := ExtractChangedFiles(fns)
	if len(files) != 2 {
		t.Errorf("got %d files, want 2 (deduped)", len(files))
	}
}

func TestFormatTerminalReport(t *testing.T) {
	report := &ParityReport{
		SourceRepo:  "test-repo",
		Description: "test run",
		TotalEdgesV1: 10,
		TotalEdgesV2: 8,
		MatchCount:   7,
		MissCount:    3,
		MissRate:     0.3,
		ExtraPendingCount: 1,
		EntryPointsV1: 2,
		EntryPointsV2: 2,
		EntryPointMatch: 2,
		Records: []DiffRecord{
			{Edge: NormalizedEdge{SourceRepo: "r", SourceFunc: "f", TargetFunc: "g"}, Category: CategoryMiss},
		},
	}
	output := FormatTerminalReport(report)
	if output == "" {
		t.Error("expected non-empty terminal report")
	}
	if !contains(output, "Miss: 3") {
		t.Errorf("expected 'Miss: 3' in output, got: %s", output)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsHelper(s, sub))
}

func containsHelper(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

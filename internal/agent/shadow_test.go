package agent

import (
	"testing"
)

func TestBuildShadowReport_BasicParity(t *testing.T) {
	o := &Orchestrator{}

	// Simulate LLM output with 3 edges: A→B, B→C, C→D
	llmOutput := &AnalysisOutput{
		WorkerOutputs: map[string]*WorkerResult{
			"repo1": {
				RepoName: "repo1",
				Nodes: []CallNode{
					{Repo: "repo1", Function: "A", File: "a.go"},
					{Repo: "repo1", Function: "B", File: "b.go"},
					{Repo: "repo1", Function: "C", File: "c.go"},
					{Repo: "repo1", Function: "D", File: "d.go"},
				},
				EntryPoints: []CallNode{
					{Repo: "repo1", Function: "D", File: "d.go"},
				},
			},
		},
		EntryPoints: []CallNode{
			{Repo: "repo1", Function: "D", File: "d.go"},
		},
	}

	// Simulate graph output with 2 edges: A→B, B→C (misses C→D)
	graphResult := &graphAnalysisResult{
		nodes: []CallNode{
			{Repo: "repo1", Function: "A", File: "a.go"},
			{Repo: "repo1", Function: "B", File: "b.go"},
			{Repo: "repo1", Function: "C", File: "c.go"},
		},
		entryPoints: []CallNode{
			{Repo: "repo1", Function: "D", File: "d.go"},
		},
	}

	input := AnalysisInput{SourceRepo: "repo1"}
	report := o.buildShadowReport(llmOutput, graphResult, input)

	if report == nil {
		t.Fatal("expected non-nil report")
	}

	// LLM edges: A→B, B→C, C→D (3 edges)
	// Graph edges: A→B, B→C (2 edges)
	// Match: A→B, B→C = 2
	// Miss (LLM has but graph doesn't): C→D = 1
	// Extra (graph has but LLM doesn't): 0
	if report.MatchCount != 2 {
		t.Errorf("MatchCount: got %d, want 2", report.MatchCount)
	}
	if report.MissCount != 1 {
		t.Errorf("MissCount: got %d, want 1", report.MissCount)
	}
	if report.ExtraPendingCount != 0 {
		t.Errorf("ExtraPendingCount: got %d, want 0", report.ExtraPendingCount)
	}

	// MissRate = 1 / (2+1) = 0.333...
	expectedRate := 1.0 / 3.0
	if report.MissRate < expectedRate-0.01 || report.MissRate > expectedRate+0.01 {
		t.Errorf("MissRate: got %.3f, want ~%.3f", report.MissRate, expectedRate)
	}

	// Entry points: LLM has D, graph has D → match=1, miss=0
	if report.EntryPointMatch != 1 {
		t.Errorf("EntryPointMatch: got %d, want 1", report.EntryPointMatch)
	}
	if report.EntryPointMiss != 0 {
		t.Errorf("EntryPointMiss: got %d, want 0", report.EntryPointMiss)
	}

	if report.Details == "" {
		t.Error("expected non-empty Details string")
	}
}

func TestBuildShadowReport_GraphFindsExtra(t *testing.T) {
	o := &Orchestrator{}

	// LLM finds A→B only
	llmOutput := &AnalysisOutput{
		WorkerOutputs: map[string]*WorkerResult{
			"repo1": {
				RepoName: "repo1",
				Nodes: []CallNode{
					{Repo: "repo1", Function: "A", File: "a.go"},
					{Repo: "repo1", Function: "B", File: "b.go"},
				},
			},
		},
	}

	// Graph finds A→B and B→C (extra: B→C)
	graphResult := &graphAnalysisResult{
		nodes: []CallNode{
			{Repo: "repo1", Function: "A", File: "a.go"},
			{Repo: "repo1", Function: "B", File: "b.go"},
			{Repo: "repo1", Function: "C", File: "c.go"},
		},
	}

	input := AnalysisInput{SourceRepo: "repo1"}
	report := o.buildShadowReport(llmOutput, graphResult, input)

	if report.MatchCount != 1 {
		t.Errorf("MatchCount: got %d, want 1", report.MatchCount)
	}
	if report.MissCount != 0 {
		t.Errorf("MissCount: got %d, want 0", report.MissCount)
	}
	if report.ExtraPendingCount != 1 {
		t.Errorf("ExtraPendingCount: got %d, want 1", report.ExtraPendingCount)
	}
}

func TestBuildShadowReport_CrossRepoCalls(t *testing.T) {
	o := &Orchestrator{}

	// LLM finds a cross-repo call
	llmOutput := &AnalysisOutput{
		WorkerOutputs: map[string]*WorkerResult{
			"repo1": {
				RepoName: "repo1",
				Nodes:    []CallNode{},
				CrossRepoCalls: []CrossRepoCall{
					{
						TargetRepo:     "repo2",
						TargetFunction: "Handler",
						CallerNode:     CallNode{Repo: "repo1", Function: "Client"},
					},
				},
			},
		},
	}

	// Graph has no nodes (empty)
	graphResult := &graphAnalysisResult{}

	input := AnalysisInput{SourceRepo: "repo1"}
	report := o.buildShadowReport(llmOutput, graphResult, input)

	// LLM has 1 edge (cross-repo), graph has 0
	if report.MatchCount != 0 {
		t.Errorf("MatchCount: got %d, want 0", report.MatchCount)
	}
	if report.MissCount != 1 {
		t.Errorf("MissCount: got %d, want 1 (graph missed the cross-repo call)", report.MissCount)
	}
}

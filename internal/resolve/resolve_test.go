package resolve

import (
	"testing"

	"github.com/DeviosLang/shirakami/internal/index"
)

// buildTestGraph constructs a small symbol graph for testing:
//
//	repo-a/pkg/service.go::ServiceA.Handle
//	  ← repo-a/pkg/middleware.go::AuthMiddleware  (depth 1)
//	      ← repo-b/cmd/main.go::main             (depth 2, cross-repo)
//	  ← repo-a/pkg/router.go::RegisterRoutes     (depth 1, entry-role)
func buildTestGraph() *index.InMemoryGraph {
	g := index.NewInMemoryGraph()

	nodes := []index.SymbolNode{
		{ID: "a:service.go:ServiceA.Handle", Repo: "repo-a", FilePath: "pkg/service.go", Name: "ServiceA.Handle", Kind: "method"},
		{ID: "a:middleware.go:AuthMiddleware", Repo: "repo-a", FilePath: "pkg/middleware.go", Name: "AuthMiddleware", Kind: "function"},
		{ID: "a:router.go:RegisterRoutes", Repo: "repo-a", FilePath: "pkg/router.go", Name: "RegisterRoutes", Kind: "function"},
		{ID: "b:main.go:main", Repo: "repo-b", FilePath: "cmd/main.go", Name: "main", Kind: "function"},
		{ID: "a:service.go:ServiceA.Validate", Repo: "repo-a", FilePath: "pkg/service.go", Name: "ServiceA.Validate", Kind: "method"},
	}

	edges := []index.SymbolEdge{
		// AuthMiddleware calls ServiceA.Handle
		{ID: "e1", SourceID: "a:middleware.go:AuthMiddleware", TargetID: "a:service.go:ServiceA.Handle", Type: "CALLS", Confidence: 1.0},
		// RegisterRoutes calls ServiceA.Handle
		{ID: "e2", SourceID: "a:router.go:RegisterRoutes", TargetID: "a:service.go:ServiceA.Handle", Type: "CALLS", Confidence: 1.0},
		// main calls AuthMiddleware (cross-repo hop)
		{ID: "e3", SourceID: "b:main.go:main", TargetID: "a:middleware.go:AuthMiddleware", Type: "CALLS", Confidence: 0.8},
		// ServiceA.Handle calls ServiceA.Validate (downstream)
		{ID: "e4", SourceID: "a:service.go:ServiceA.Handle", TargetID: "a:service.go:ServiceA.Validate", Type: "CALLS", Confidence: 1.0},
	}

	g.Load(nodes, edges)
	return g
}

func TestImpact_BasicUpstream(t *testing.T) {
	r := New(buildTestGraph())

	result := r.Impact(ImpactOptions{
		Target:    "ServiceA.Handle",
		Repo:      "repo-a",
		Direction: "upstream",
		MaxDepth:  3,
	})

	if len(result.StartNodes) == 0 {
		t.Fatal("expected start nodes, got none")
	}
	if result.TotalAffected == 0 {
		t.Fatal("expected affected symbols, got none")
	}

	// depth-1 should contain AuthMiddleware and RegisterRoutes
	depth1 := result.ByDepth[1]
	if len(depth1) != 2 {
		t.Errorf("expected 2 depth-1 callers, got %d", len(depth1))
	}

	names := make(map[string]bool)
	for _, s := range depth1 {
		names[s.Name] = true
	}
	if !names["AuthMiddleware"] {
		t.Error("expected AuthMiddleware in depth-1 callers")
	}
	if !names["RegisterRoutes"] {
		t.Error("expected RegisterRoutes in depth-1 callers")
	}
}

func TestImpact_EntryPointDetection(t *testing.T) {
	r := New(buildTestGraph())

	result := r.Impact(ImpactOptions{
		Target:         "ServiceA.Handle",
		Repo:           "repo-a",
		Direction:      "upstream",
		MaxDepth:       3,
		EntryRoleRepos: []string{"repo-b"},
	})

	if len(result.EntryPoints) == 0 {
		t.Fatal("expected entry points for repo-b, got none")
	}
	if result.EntryPoints[0].Symbol.Repo != "repo-b" {
		t.Errorf("expected entry point in repo-b, got %s", result.EntryPoints[0].Symbol.Repo)
	}
}

func TestImpact_CrossRepoHops(t *testing.T) {
	r := New(buildTestGraph())

	result := r.Impact(ImpactOptions{
		Target:    "ServiceA.Handle",
		Repo:      "repo-a",
		Direction: "upstream",
		MaxDepth:  3,
	})

	if len(result.CrossRepoHops) == 0 {
		t.Error("expected at least one cross-repo hop")
	}
}

func TestImpact_Uncovered(t *testing.T) {
	r := New(buildTestGraph())

	result := r.Impact(ImpactOptions{
		Target: "NonExistentFunction",
		Repo:   "repo-a",
	})

	if len(result.Uncovered) == 0 {
		t.Error("expected Uncovered to contain the unknown target")
	}
	if result.Uncovered[0] != "NonExistentFunction" {
		t.Errorf("expected Uncovered[0] = NonExistentFunction, got %s", result.Uncovered[0])
	}
}

func TestImpact_FileSpecSplit(t *testing.T) {
	r := New(buildTestGraph())

	// Use FILE::FUNC notation
	result := r.Impact(ImpactOptions{
		Target:    "pkg/service.go::ServiceA.Handle",
		Repo:      "repo-a",
		Direction: "upstream",
		MaxDepth:  1,
	})

	if len(result.StartNodes) == 0 {
		t.Fatal("expected start node from FILE::FUNC spec")
	}
	if result.StartNodes[0].Name != "ServiceA.Handle" {
		t.Errorf("expected ServiceA.Handle, got %s", result.StartNodes[0].Name)
	}
}

func TestImpact_Downstream(t *testing.T) {
	r := New(buildTestGraph())

	result := r.Impact(ImpactOptions{
		Target:    "ServiceA.Handle",
		Repo:      "repo-a",
		Direction: "downstream",
		MaxDepth:  2,
	})

	if result.TotalAffected == 0 {
		t.Fatal("expected downstream callees")
	}
	// ServiceA.Validate should be a callee
	found := false
	for _, syms := range result.ByDepth {
		for _, s := range syms {
			if s.Name == "ServiceA.Validate" {
				found = true
			}
		}
	}
	if !found {
		t.Error("expected ServiceA.Validate in downstream result")
	}
}

func TestRiskAssessment(t *testing.T) {
	cases := []struct {
		name         string
		directCount  int
		entryPoints  int
		crossRepoHops int
		want         RiskLevel
	}{
		{"entry_point_always_critical", 1, 1, 0, RiskCritical},
		{"direct_gt_20_critical", 21, 0, 0, RiskCritical},
		{"direct_gt_10_high", 11, 0, 0, RiskHigh},
		{"two_cross_repo_high", 2, 0, 2, RiskHigh},
		{"direct_gt_3_medium", 5, 0, 0, RiskMedium},
		{"direct_le_3_low", 3, 0, 0, RiskLow},
		{"zero_low", 0, 0, 0, RiskLow},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &ImpactResult{
				DirectCount:   tc.directCount,
				CrossRepoHops: make([]CrossRepoHop, tc.crossRepoHops),
				EntryPoints:   make([]EntryPoint, tc.entryPoints),
				ByDepth:       make(map[int][]index.AffectedSymbol),
			}
			got := assessRisk(r)
			if got != tc.want {
				t.Errorf("assessRisk(%+v) = %s, want %s", tc, got, tc.want)
			}
		})
	}
}

func TestImpactMany(t *testing.T) {
	r := New(buildTestGraph())

	opts := []ImpactOptions{
		{Target: "ServiceA.Handle", Repo: "repo-a", Direction: "upstream", MaxDepth: 2},
		{Target: "ServiceA.Validate", Repo: "repo-a", Direction: "upstream", MaxDepth: 2},
	}

	result := r.ImpactMany(opts)
	if result.TotalAffected == 0 {
		t.Error("expected merged affected symbols")
	}
}

func TestInferProtocol(t *testing.T) {
	tests := []struct {
		filePath string
		name     string
		want     string
	}{
		{"cmd/main_test.go", "TestFoo", "test"},
		{"internal/service_test.go", "TestBar", "test"},
		{"pkg/grpc/server.go", "HandleRPC", "grpc"},
		{"pkg/kafka/consumer.go", "ConsumeEvent", "mq"},
		{"internal/api/handler.go", "HandleRequest", "http"},
		{"internal/router/routes.go", "Register", "http"},
		{"pkg/util/helper.go", "HelperFunc", ""},
	}
	for _, tc := range tests {
		got := inferProtocol(tc.filePath, tc.name)
		if got != tc.want {
			t.Errorf("inferProtocol(%q, %q) = %q, want %q", tc.filePath, tc.name, got, tc.want)
		}
	}
}

func TestSplitTargetSpec(t *testing.T) {
	tests := []struct {
		input    string
		wantFunc string
		wantFile string
	}{
		{"pkg/service.go::MyFunc", "MyFunc", "pkg/service.go"},
		{"MyFunc", "MyFunc", ""},
		{"repo/path/to/file.py::ClassName.method", "ClassName.method", "repo/path/to/file.py"},
	}
	for _, tc := range tests {
		gotFunc, gotFile := splitTargetSpec(tc.input)
		if gotFunc != tc.wantFunc {
			t.Errorf("splitTargetSpec(%q) func = %q, want %q", tc.input, gotFunc, tc.wantFunc)
		}
		if gotFile != tc.wantFile {
			t.Errorf("splitTargetSpec(%q) file = %q, want %q", tc.input, gotFile, tc.wantFile)
		}
	}
}

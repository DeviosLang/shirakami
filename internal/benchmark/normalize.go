package benchmark

import (
	"strings"
)

// CallNode mirrors agent.CallNode for normalization without circular import.
type CallNode struct {
	Repo     string
	Package  string
	Function string
	File     string
	Line     int
}

// WorkerResultData holds the subset of agent.WorkerResult needed for normalization.
type WorkerResultData struct {
	RepoName       string
	Nodes          []CallNode
	CrossRepoCalls []CrossRepoCallData
	EntryPoints    []CallNode
}

// CrossRepoCallData mirrors agent.CrossRepoCall.
type CrossRepoCallData struct {
	TargetRepo     string
	TargetFunction string
	CallerNode     CallNode
}

// GraphImpactData holds the output of v2 graph-based analysis.
type GraphImpactData struct {
	AffectedSymbols []AffectedSymbolData
	EntryPoints     []CallNode
}

// AffectedSymbolData mirrors index.AffectedSymbol.
type AffectedSymbolData struct {
	ID       string
	Name     string
	FilePath string
	Repo     string
	Depth    int
	EdgeType string
}

// NormalizeV1Result converts v1 LLM Worker output into NormalizedResult.
func NormalizeV1Result(sourceRepo string, workers map[string]*WorkerResultData) *NormalizedResult {
	result := &NormalizedResult{}

	edgeSeen := make(map[string]bool)

	for _, wr := range workers {
		if wr == nil {
			continue
		}

		// Convert nodes to edges (consecutive nodes form a call chain)
		for i := 0; i < len(wr.Nodes)-1; i++ {
			src := wr.Nodes[i]
			tgt := wr.Nodes[i+1]
			edge := NormalizedEdge{
				SourceRepo: coalesce(src.Repo, wr.RepoName),
				SourceFile: src.File,
				SourceFunc: src.Function,
				TargetRepo: coalesce(tgt.Repo, wr.RepoName),
				TargetFile: tgt.File,
				TargetFunc: tgt.Function,
				EdgeType:   "CALLS",
			}
			if !edgeSeen[edge.Key()] {
				edgeSeen[edge.Key()] = true
				result.Edges = append(result.Edges, edge)
			}
		}

		// Convert cross-repo calls
		for _, cross := range wr.CrossRepoCalls {
			edge := NormalizedEdge{
				SourceRepo: cross.CallerNode.Repo,
				SourceFile: cross.CallerNode.File,
				SourceFunc: cross.CallerNode.Function,
				TargetRepo: cross.TargetRepo,
				TargetFunc: cross.TargetFunction,
				EdgeType:   "CALLS",
			}
			if !edgeSeen[edge.Key()] {
				edgeSeen[edge.Key()] = true
				result.Edges = append(result.Edges, edge)
			}
		}

		// Convert entry points
		for _, ep := range wr.EntryPoints {
			result.EntryPoints = append(result.EntryPoints, NormalizedEntryPoint{
				Repo:     ep.Repo,
				File:     ep.File,
				Function: ep.Function,
			})
		}
	}

	return result
}

// NormalizeV2Result converts v2 graph-based analysis output into NormalizedResult.
func NormalizeV2Result(sourceRepo string, changedFunctions []string, data *GraphImpactData) *NormalizedResult {
	result := &NormalizedResult{
		ChangedFunctions: changedFunctions,
	}

	// Each affected symbol represents a caller of the changed functions
	for _, sym := range data.AffectedSymbols {
		// The edge is: affected symbol → changed function (upstream direction)
		// In upstream BFS, affected symbols are callers of start symbols
		edge := NormalizedEdge{
			SourceRepo: sym.Repo,
			SourceFile: sym.FilePath,
			SourceFunc: sym.Name,
			TargetRepo: sourceRepo,
			TargetFunc: "", // connected to one of the changed functions
			EdgeType:   coalesce(sym.EdgeType, "CALLS"),
		}
		result.Edges = append(result.Edges, edge)
	}

	// Entry points
	for _, ep := range data.EntryPoints {
		result.EntryPoints = append(result.EntryPoints, NormalizedEntryPoint{
			Repo:     ep.Repo,
			File:     ep.File,
			Function: ep.Function,
		})
	}

	return result
}

func coalesce(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// ExtractChangedFiles extracts unique file paths from a list of changed function identifiers.
func ExtractChangedFiles(changedFunctions []string) []string {
	seen := make(map[string]bool)
	var files []string
	for _, fn := range changedFunctions {
		// Format: "repo/path/file.py::FuncName" or "repo/module.Func"
		if idx := strings.Index(fn, "::"); idx > 0 {
			path := fn[:idx]
			if slash := strings.Index(path, "/"); slash > 0 {
				path = path[slash+1:] // strip repo prefix
			}
			if !seen[path] {
				seen[path] = true
				files = append(files, path)
			}
		}
	}
	return files
}

// Package resolve provides business-level impact analysis on top of the
// in-memory symbol graph (index.InMemoryGraph).
//
// It wraps the graph's low-level BFS with:
//   - Symbolic name → node resolution (with file-path disambiguation)
//   - Risk assessment (LOW / MEDIUM / HIGH / CRITICAL)
//   - Entry-point detection (symbols in entry-role repos)
//   - Cross-repo hop tracking
//   - Uncovered symbol list (symbols not yet indexed → LLM fallback candidates)
//
// Architecture placement: Layer 2 of the hybrid model.
// Sits between index.InMemoryGraph (pure graph) and agent.Orchestrator
// (LLM orchestration). The orchestrator calls Resolver.Impact() first; any
// symbols in ImpactResult.Uncovered are handed to LLM Workers.
package resolve

import (
	"fmt"
	"strings"

	"github.com/DeviosLang/shirakami/internal/index"
)

// ---------------------------------------------------------------------------
// Public types
// ---------------------------------------------------------------------------

// ImpactOptions configures a single call to Resolver.Impact.
type ImpactOptions struct {
	// Target is the symbol name (qualified, e.g. "PaymentService.Handle") or
	// a FILE_PATH::FUNCTION_NAME string as produced by extractChangedFunctions.
	Target string

	// Repo is the repository the target lives in.
	// Used to disambiguate when multiple repos define a symbol with the same name.
	Repo string

	// FilePath optionally narrows resolution further (repo-relative path).
	FilePath string

	// Direction is "upstream" (find callers) or "downstream" (find callees).
	// Defaults to "upstream".
	Direction string

	// MaxDepth caps BFS depth. Defaults to 3.
	//   depth 1 → WILL BREAK (direct callers)
	//   depth 2 → LIKELY AFFECTED
	//   depth 3 → MAY NEED TESTING
	MaxDepth int

	// RelationTypes restricts which edge kinds are followed.
	// Nil / empty means follow all: CALLS, IMPORTS, EXTENDS, IMPLEMENTS.
	RelationTypes []string

	// MinConfidence filters out low-confidence edges (0.0 = follow all).
	MinConfidence float64

	// EntryRoleRepos is the set of repo names that are "entry-role"
	// (HTTP servers, test harnesses, route registries). Symbols in these
	// repos are marked as EntryPoints in the result.
	EntryRoleRepos []string
}

// ImpactResult is the structured output of Resolver.Impact.
type ImpactResult struct {
	// Risk is the overall risk classification.
	Risk RiskLevel

	// StartNodes are the resolved graph nodes for the target symbol.
	// Multiple nodes occur when the same name exists in several files.
	StartNodes []*index.SymbolNode

	// DirectCount is the number of depth-1 affected symbols.
	DirectCount int

	// TotalAffected is the total count across all depths.
	TotalAffected int

	// ByDepth maps depth level → affected symbols at that depth.
	ByDepth map[int][]index.AffectedSymbol

	// EntryPoints are affected symbols that live in entry-role repos.
	EntryPoints []EntryPoint

	// CrossRepoHops are edges that cross repository boundaries.
	CrossRepoHops []CrossRepoHop

	// Uncovered lists symbol names that could not be resolved in the graph.
	// These should be handed to LLM Workers as fallback targets.
	Uncovered []string
}

// RiskLevel classifies the blast-radius of a change.
type RiskLevel string

const (
	RiskLow      RiskLevel = "LOW"
	RiskMedium   RiskLevel = "MEDIUM"
	RiskHigh     RiskLevel = "HIGH"
	RiskCritical RiskLevel = "CRITICAL"
)

// EntryPoint is an affected symbol that lives in an entry-role repository.
type EntryPoint struct {
	Symbol index.AffectedSymbol
	// Protocol hints at the entry type, inferred from file path patterns.
	// e.g. "http", "grpc", "mq", "test", ""
	Protocol string
}

// CrossRepoHop records a single cross-repository call edge encountered during BFS.
type CrossRepoHop struct {
	FromRepo string
	FromFunc string
	ToRepo   string
	ToFunc   string
	Depth    int
	EdgeType string
}

// ---------------------------------------------------------------------------
// Resolver
// ---------------------------------------------------------------------------

// Resolver wraps an InMemoryGraph and provides business-level impact analysis.
type Resolver struct {
	graph *index.InMemoryGraph
}

// New creates a Resolver backed by the given graph.
// graph must not be nil.
func New(graph *index.InMemoryGraph) *Resolver {
	return &Resolver{graph: graph}
}

// Impact performs impact analysis for a single target symbol.
//
// Resolution strategy:
//  1. If opts.Target is in "FILE::FUNC" format, split and use file to narrow.
//  2. Resolve name → graph node(s), preferring opts.Repo + opts.FilePath.
//  3. BFS via graph.Impact(), collecting affected symbols.
//  4. Classify risk, detect entry points, record cross-repo hops.
func (r *Resolver) Impact(opts ImpactOptions) *ImpactResult {
	result := &ImpactResult{
		ByDepth: make(map[int][]index.AffectedSymbol),
	}

	// Apply defaults.
	if opts.Direction == "" {
		opts.Direction = "upstream"
	}
	if opts.MaxDepth <= 0 {
		opts.MaxDepth = 3
	}

	// -----------------------------------------------------------------------
	// Step 1 – resolve target symbol(s)
	// -----------------------------------------------------------------------
	funcName, filePath := splitTargetSpec(opts.Target)
	if filePath == "" {
		filePath = opts.FilePath
	}

	startNodes := r.resolveSymbol(funcName, opts.Repo, filePath)
	if len(startNodes) == 0 {
		// No node found — report as uncovered.
		result.Uncovered = []string{opts.Target}
		result.Risk = RiskLow
		return result
	}
	result.StartNodes = startNodes

	startIDs := make([]string, len(startNodes))
	for i, n := range startNodes {
		startIDs[i] = n.ID
	}

	// -----------------------------------------------------------------------
	// Step 2 – BFS traversal
	// -----------------------------------------------------------------------
	affected := r.graph.Impact(startIDs, opts.Direction, opts.MaxDepth, opts.MinConfidence)

	// Filter by relation types if specified.
	if len(opts.RelationTypes) > 0 {
		allowed := make(map[string]bool, len(opts.RelationTypes))
		for _, t := range opts.RelationTypes {
			allowed[strings.ToUpper(t)] = true
		}
		filtered := affected[:0]
		for _, sym := range affected {
			if allowed[strings.ToUpper(sym.EdgeType)] {
				filtered = append(filtered, sym)
			}
		}
		affected = filtered
	}

	// Group by depth.
	entryRoleSet := make(map[string]bool, len(opts.EntryRoleRepos))
	for _, r := range opts.EntryRoleRepos {
		entryRoleSet[r] = true
	}

	seenCrossRepo := make(map[string]bool)
	seenStartRepo := opts.Repo
	for _, sym := range affected {
		result.ByDepth[sym.Depth] = append(result.ByDepth[sym.Depth], sym)
		result.TotalAffected++

		// Entry-point detection.
		if entryRoleSet[sym.Repo] {
			ep := EntryPoint{
				Symbol:   sym,
				Protocol: inferProtocol(sym.FilePath, sym.Name),
			}
			result.EntryPoints = append(result.EntryPoints, ep)
		}

		// Cross-repo hop detection: when a symbol's repo differs from its caller.
		if sym.Repo != "" && sym.Repo != seenStartRepo {
			hopKey := fmt.Sprintf("%s→%s@%d", seenStartRepo, sym.Repo, sym.Depth)
			if !seenCrossRepo[hopKey] {
				seenCrossRepo[hopKey] = true
				hop := CrossRepoHop{
					FromRepo: seenStartRepo,
					ToRepo:   sym.Repo,
					ToFunc:   sym.Name,
					Depth:    sym.Depth,
					EdgeType: sym.EdgeType,
				}
				// For depth > 1, the "from" repo may itself have changed.
				result.CrossRepoHops = append(result.CrossRepoHops, hop)
			}
		}
	}

	// DirectCount = depth-1 symbols.
	result.DirectCount = len(result.ByDepth[1])

	// -----------------------------------------------------------------------
	// Step 3 – Risk assessment
	// -----------------------------------------------------------------------
	result.Risk = assessRisk(result)

	return result
}

// ImpactMany runs Impact for each target in opts (sharing EntryRoleRepos and
// graph traversal direction) and merges the results into one ImpactResult.
// Deduplication is performed across targets.
func (r *Resolver) ImpactMany(targets []ImpactOptions) *ImpactResult {
	merged := &ImpactResult{
		ByDepth: make(map[int][]index.AffectedSymbol),
	}

	seenID := make(map[string]bool)
	seenEP := make(map[string]bool)
	seenHop := make(map[string]bool)

	for _, opt := range targets {
		sub := r.Impact(opt)

		// Merge start nodes (no dedup needed — different targets).
		merged.StartNodes = append(merged.StartNodes, sub.StartNodes...)
		merged.Uncovered = append(merged.Uncovered, sub.Uncovered...)

		for depth, syms := range sub.ByDepth {
			for _, s := range syms {
				if !seenID[s.ID] {
					seenID[s.ID] = true
					merged.ByDepth[depth] = append(merged.ByDepth[depth], s)
					merged.TotalAffected++
					if depth == 1 {
						merged.DirectCount++
					}
				}
			}
		}

		for _, ep := range sub.EntryPoints {
			k := ep.Symbol.ID
			if !seenEP[k] {
				seenEP[k] = true
				merged.EntryPoints = append(merged.EntryPoints, ep)
			}
		}

		for _, hop := range sub.CrossRepoHops {
			k := fmt.Sprintf("%s→%s@%d", hop.FromRepo, hop.ToRepo, hop.Depth)
			if !seenHop[k] {
				seenHop[k] = true
				merged.CrossRepoHops = append(merged.CrossRepoHops, hop)
			}
		}
	}

	merged.Risk = assessRisk(merged)
	return merged
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// resolveSymbol finds graph nodes for the given symbol name, with optional
// repo and file-path disambiguation. Returns nil if no node found.
func (r *Resolver) resolveSymbol(name, repo, filePath string) []*index.SymbolNode {
	candidates := r.graph.FindNodesByName(name)
	if len(candidates) == 0 {
		return nil
	}
	if len(candidates) == 1 {
		return candidates
	}

	// Multiple candidates: prefer repo+filePath match, then repo match.
	var repoFileMatch, repoMatch []*index.SymbolNode
	for _, c := range candidates {
		repoOK := repo == "" || c.Repo == repo
		fileOK := filePath == "" || c.FilePath == filePath || strings.HasSuffix(c.FilePath, filePath)
		switch {
		case repoOK && fileOK:
			repoFileMatch = append(repoFileMatch, c)
		case repoOK:
			repoMatch = append(repoMatch, c)
		}
	}
	if len(repoFileMatch) > 0 {
		return repoFileMatch
	}
	if len(repoMatch) > 0 {
		return repoMatch
	}
	return candidates // no disambiguation possible, return all
}

// splitTargetSpec splits a "path/to/file.py::FuncName" spec into (funcName, filePath).
// If no "::" separator, returns (target, "").
func splitTargetSpec(target string) (funcName, filePath string) {
	if idx := strings.LastIndex(target, "::"); idx >= 0 {
		return target[idx+2:], target[:idx]
	}
	return target, ""
}

// assessRisk computes RiskLevel from the ImpactResult counts.
//
// Rules (from architecture-v2-design §3.2.3):
//
//	CRITICAL: depth=1 callers > 20 OR affected entry-role repos
//	HIGH:     depth=1 callers > 10 OR cross 2+ repos
//	MEDIUM:   depth=1 callers > 3
//	LOW:      depth=1 callers ≤ 3 and no cross-repo impact
func assessRisk(r *ImpactResult) RiskLevel {
	if len(r.EntryPoints) > 0 || r.DirectCount > 20 {
		return RiskCritical
	}
	if r.DirectCount > 10 || len(r.CrossRepoHops) >= 2 {
		return RiskHigh
	}
	if r.DirectCount > 3 {
		return RiskMedium
	}
	return RiskLow
}

// inferProtocol guesses the protocol type of an entry point from its
// file path and symbol name. Used to enrich EntryPoint.Protocol.
func inferProtocol(filePath, name string) string {
	lower := strings.ToLower(filePath)
	nameLower := strings.ToLower(name)
	switch {
	case strings.Contains(lower, "test") || strings.HasSuffix(lower, "_test.go") ||
		strings.HasPrefix(nameLower, "test"):
		return "test"
	case strings.Contains(lower, "grpc") || strings.Contains(lower, "proto") ||
		strings.Contains(nameLower, "grpc"):
		return "grpc"
	case strings.Contains(lower, "mq") || strings.Contains(lower, "kafka") ||
		strings.Contains(lower, "queue") || strings.Contains(lower, "consumer"):
		return "mq"
	case strings.Contains(lower, "handler") || strings.Contains(lower, "router") ||
		strings.Contains(lower, "server") || strings.Contains(lower, "api") ||
		strings.Contains(lower, "route"):
		return "http"
	default:
		return ""
	}
}

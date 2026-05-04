package agent

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/DeviosLang/shirakami/internal/checkpoint"
	"github.com/DeviosLang/shirakami/internal/logger"
)

// AnalysisInput is the input to an Orchestrator run.
type AnalysisInput struct {
	// Diff is the unified diff of the changes to analyse.
	// When multiple patches are combined, they are concatenated here with
	// "# === <description> ===" markers between them.
	Diff string
	// Description is a human-readable description of the change.
	Description string
	// SourceRepo is the repo name where the diff originates.
	// When set, unrecognised function names fall back to this repo instead of
	// the first repo in the list.
	SourceRepo string
	// PatchInfo carries metadata about each patch when the input was assembled
	// from multiple sources (e.g. YAML config). Empty when analysing a single diff.
	PatchInfo []PatchRef
	// ScopeOnlyRepos optionally restricts cross-repo tracing to these target repos.
	// When empty, all discovered cross-repo calls are followed.
	ScopeOnlyRepos []string
}

// PatchRef describes one patch in a multi-patch analysis.
type PatchRef struct {
	Path        string // original file path (for logging/reports)
	Description string // patch-specific description
	Bytes       int    // size of the patch content
}

// AnalysisOutput is the result returned by the Orchestrator.
type AnalysisOutput struct {
	// ChangedFunctions lists all functions identified as changed by the diff.
	ChangedFunctions []string
	// CallGraph is the complete multi-repo call chain.
	CallGraph []CallNode
	// EntryPoints are the route-registered / test functions in entry-role repos.
	EntryPoints []CallNode
	// FunctionAnalyses holds per-function constraint extraction and test scenario suggestions.
	FunctionAnalyses []FunctionAnalysis
	// WorkerOutputs holds per-repo raw results for debugging.
	WorkerOutputs map[string]*WorkerResult
}

// Orchestrator coordinates multi-repo call-chain analysis.
//
// It:
//  1. Parses the diff to extract changed functions.
//  2. Launches a WorkerAgent per involved repository concurrently.
//  3. Follows cross-repo calls iteratively until all chains reach an entry-
//     role repository or are exhausted.
//  4. Merges all WorkerResult values into a single AnalysisOutput.
type Orchestrator struct {
	llmClient    LLMClient
	tools        []Tool
	repos        []RepoInfo
	workspaceDir string
	cp           *checkpoint.FileCheckpointer
	// batchPriorities maps batchKey → priority metadata, populated by applyTriage.
	// Used by runWorkerBatch to schedule and budget Workers.
	batchPriorities map[string]BatchPriority
	// maxRounds caps the cross-repo hop count.
	// 0 = default (10, deep mode); lower values (e.g. 3) enable fast mode.
	maxRounds int
	// contractHints are pre-declared cross-repo relationships from config.
	// Formatted as human-readable strings injected into Worker prompts.
	contractHints []string
	// indexGraph is the in-memory symbol graph for deterministic call chain analysis.
	// When non-nil, enables hybrid mode: graph traversal first, LLM fallback for uncovered.
	indexGraph IndexGraph
	// indexMode controls how the index is used: "off", "shadow", "hybrid", "deterministic".
	indexMode string
	// importContext is a pre-built import graph summary (from Python indexer).
	// Injected into Worker prompts to reduce LLM search rounds.
	importContext string
}

// IndexGraph is the interface consumed by Orchestrator for deterministic analysis.
// Satisfied by *index.InMemoryGraph.
type IndexGraph interface {
	// Impact performs BFS traversal and returns affected symbols.
	Impact(startIDs []string, direction string, maxDepth int, minConfidence float64) []IndexAffectedSymbol
	// FindNodesByName returns nodes matching a name.
	FindNodesByName(name string) []IndexSymbolNode
	// NodeCount returns total nodes in graph.
	NodeCount() int
}

// IndexAffectedSymbol is the interface-compatible version of index.AffectedSymbol.
type IndexAffectedSymbol struct {
	ID         string
	Name       string
	FilePath   string
	Repo       string
	Depth      int
	Confidence float64
	EdgeType   string
}

// IndexSymbolNode is the interface-compatible version of index.SymbolNode (subset).
type IndexSymbolNode struct {
	ID        string
	Repo      string
	FilePath  string
	Name      string
	Kind      string
	StartLine int
	EndLine   int
}

// NewOrchestrator creates a new Orchestrator.
func NewOrchestrator(
	llmClient LLMClient,
	tools []Tool,
	repos []RepoInfo,
	workspaceDir string,
	cp *checkpoint.FileCheckpointer,
) *Orchestrator {
	return &Orchestrator{
		llmClient:       llmClient,
		tools:           tools,
		repos:           repos,
		workspaceDir:    workspaceDir,
		cp:              cp,
		batchPriorities: make(map[string]BatchPriority),
	}
}

// SetMaxRounds overrides the cross-repo iteration cap.
// Values ≤ 0 fall back to the default (10, deep mode).
// Use e.g. SetMaxRounds(3) for fast mode.
func (o *Orchestrator) SetMaxRounds(n int) {
	o.maxRounds = n
}

// SetContractHints provides pre-declared cross-repo relationships from config.
// Each hint is a formatted string like:
//
//	"cvm_api (POST /api/v1/instance/create) → vstation_compute_access (dispatch.VMachineCreate)"
//
// These are injected into Worker prompts so the LLM can confirm/deny
// known relationships without needing to discover them via ripgrep.
func (o *Orchestrator) SetContractHints(hints []string) {
	o.contractHints = hints
}

// SetIndexGraph injects the in-memory symbol graph for hybrid/deterministic mode.
// When set, the orchestrator will attempt graph-based analysis before falling back to LLM.
func (o *Orchestrator) SetIndexGraph(graph IndexGraph) {
	o.indexGraph = graph
}

// SetIndexMode sets how the index is used during analysis.
// Valid values: "off" (default, pure LLM), "shadow", "hybrid", "deterministic".
func (o *Orchestrator) SetIndexMode(mode string) {
	o.indexMode = mode
}

// SetImportContext provides a pre-built import graph summary for Worker prompts.
// Typically generated by index.BuildImportContext() from Python index data.
func (o *Orchestrator) SetImportContext(ctx string) {
	o.importContext = ctx
}

// Run analyses the provided diff and returns the complete call-chain graph.
func (o *Orchestrator) Run(ctx context.Context, input AnalysisInput) (*AnalysisOutput, error) {
	log := logger.S()
	runStart := time.Now()

	// Resolve max rounds early so we can log the mode.
	maxRounds := o.maxRounds
	if maxRounds <= 0 {
		maxRounds = 10
	}
	mode := "deep"
	if maxRounds < 10 {
		mode = "fast"
	}

	log.Infow("analyse.start",
		"source_repo", input.SourceRepo,
		"diff_bytes", len(input.Diff),
		"description", input.Description,
		"mode", mode,
		"max_rounds", maxRounds,
		"index_mode", o.indexMode,
	)

	// Step 1 – extract changed functions from the diff.
	step1 := time.Now()
	changed, err := o.extractChangedFunctions(ctx, input)
	if err != nil {
		log.Errorw("extract.failed", "err", err.Error(),
			"duration_ms", time.Since(step1).Milliseconds())
		return nil, fmt.Errorf("extract changed functions: %w", err)
	}
	log.Infow("extract.done",
		"fn_count", len(changed),
		"duration_ms", time.Since(step1).Milliseconds(),
	)

	output := &AnalysisOutput{
		ChangedFunctions: changed,
		WorkerOutputs:    make(map[string]*WorkerResult),
	}

	// Step 1b – Hybrid/Deterministic mode: try graph-based analysis first.
	// If the index graph is available, resolve call chains deterministically
	// before (or instead of) launching LLM Workers.
	if o.indexGraph != nil && o.indexGraph.NodeCount() > 0 &&
		(o.indexMode == "hybrid" || o.indexMode == "deterministic") {

		graphResult := o.runGraphAnalysis(changed, input.SourceRepo)

		if o.indexMode == "deterministic" || len(graphResult.uncovered) == 0 {
			// Deterministic mode OR full coverage: return graph result directly
			log.Infow("analyse.graph_complete",
				"mode", o.indexMode,
				"graph_nodes", len(graphResult.nodes),
				"graph_entries", len(graphResult.entryPoints),
				"uncovered", len(graphResult.uncovered),
				"duration_ms", time.Since(runStart).Milliseconds(),
			)
			output.CallGraph = graphResult.nodes
			output.EntryPoints = graphResult.entryPoints
			// In deterministic mode, skip LLM Workers entirely
			if o.indexMode == "deterministic" {
				return output, nil
			}
			// Hybrid with full coverage: still skip LLM
			return output, nil
		}

		// Hybrid mode with partial coverage: merge graph results + continue with LLM for uncovered
		output.CallGraph = append(output.CallGraph, graphResult.nodes...)
		output.EntryPoints = append(output.EntryPoints, graphResult.entryPoints...)
		// Override changed to only include uncovered functions (LLM handles only what graph missed)
		changed = graphResult.uncovered
		log.Infow("analyse.graph_partial",
			"graph_nodes", len(graphResult.nodes),
			"graph_entries", len(graphResult.entryPoints),
			"uncovered_for_llm", len(changed),
			"duration_ms", time.Since(step1).Milliseconds(),
		)
	}

	// Step 2 – group changed functions by repo, then by file within each repo.
	pending := o.groupByRepoAndFile(changed, input.SourceRepo)
	log.Infow("group.done", "batches", len(pending))

	// Step 2b – Go-level diff scan: ensure every changed file in the diff has a
	// Worker batch, even if the LLM missed some functions.
	beforeCoverage := len(pending)
	o.ensureDiffFileCoverage(pending, input)
	log.Infow("diff_coverage.done",
		"batches_before", beforeCoverage,
		"batches_after", len(pending),
		"sentinels_added", len(pending)-beforeCoverage,
	)

	// Step 2c – Triage: classify each pending batch by business priority (P0/P1/P2).
	step2c := time.Now()
	pending = o.applyTriage(ctx, pending, input)
	log.Infow("triage.done",
		"batches", len(pending),
		"duration_ms", time.Since(step2c).Milliseconds(),
	)

	// visited prevents infinite loops on cross-repo cycles.
	visited := make(map[string]bool)
	// repoVisited tracks repos that already ran a Worker in a previous round
	// so we can send subsequent cross-repo calls as ExternalCallers.
	repoVisited := make(map[string]bool)

	// Plateau detection: early-stop when no new nodes are discovered for
	// earlyStopPatience consecutive rounds (saves LLM calls on exhausted chains).
	const earlyStopPatience = 3
	consecutiveNoProgress := 0
	prevNodeCount := 0

	// Safety cap on cross-repo hops. Already resolved above (maxRounds).
	for round := 0; round < maxRounds && len(pending) > 0; round++ {
		roundStart := time.Now()
		log.Infow("round.start",
			"round", round,
			"pending_batches", len(pending),
		)
		nextPending := make(map[string][]string)

		results := o.runWorkerBatch(ctx, pending)
		totalNodes, totalCross, totalEntries := 0, 0, 0
		for repoName, result := range results {
			if result == nil {
				log.Warnw("worker.result_nil", "repo", repoName, "round", round)
				continue
			}
			output.WorkerOutputs[repoName] = result
			output.CallGraph = append(output.CallGraph, result.Nodes...)

			// Collect entry points regardless of ReachedEntry flag.
			if len(result.EntryPoints) > 0 {
				output.EntryPoints = append(output.EntryPoints, result.EntryPoints...)
			}

			// Collect constraint and test scenario analyses.
			output.FunctionAnalyses = append(output.FunctionAnalyses, result.FunctionAnalyses...)

			repoVisited[repoName] = true

			// ── Cross-repo call extraction: triple-path merge ──────────────
			// Path 1 (LLM primary): result.CrossRepoCalls — what LLM explicitly recorded
			// Path 2 (search_results): repos appearing in ripgrep result paths
			// Path 3 (nodes): cross-repo nodes the LLM recorded in its tree but
			//                 forgot to also emit as cross_repo_calls
			// All three are merged (deduped by target_repo+target_function).
			// This maximizes recall — a path missed by one source is often caught by another.
			llmCalls := result.CrossRepoCalls
			fromSearch := o.extractCrossRepoCallsFromSearch(repoName, result.SearchResults)
			fromNodes := o.extractCrossRepoCallsFromNodes(repoName, result.Nodes)
			crossCalls := mergeCrossRepoCalls(llmCalls, fromSearch, fromNodes)
			if len(crossCalls) > len(llmCalls) {
				log.Infow("round.cross_repo_enriched",
					"round", round,
					"repo", repoName,
					"from_llm", len(llmCalls),
					"from_search", len(fromSearch),
					"from_nodes", len(fromNodes),
					"merged_total", len(crossCalls),
				)
			}
			fallbackUsed := len(llmCalls) == 0 && len(crossCalls) > 0

			// Filter + FILE_CHANGED fallback for cross-repo calls.
			//   1. Skip calls where TargetRepo doesn't exist in our config
			//      (prevents LLM from routing to imaginary repos like aurora/vstation_nano)
			//   2. When TargetFunction is empty, fall back to a FILE_CHANGED sentinel
			//      using the caller_file so the next-hop Worker searches the file broadly
			skipped, added := 0, 0
			for _, cross := range crossCalls {
				if !o.repoExists(cross.TargetRepo) {
					skipped++
					continue
				}
				targetFunc := cross.TargetFunction
				if targetFunc == "" {
					// Use caller_file as a file-level sentinel.
					if cross.CallerNode.File != "" {
						targetFunc = "FILE_CHANGED:" + cross.TargetRepo + "/" + cross.CallerNode.File
					} else {
						skipped++
						continue
					}
				}

				key := cross.TargetRepo + ":" + targetFunc
				if !visited[key] {
					visited[key] = true
					nextPending[cross.TargetRepo] = append(
						nextPending[cross.TargetRepo], targetFunc,
					)
					added++
				}
			}
			if skipped > 0 {
				log.Infow("round.cross_repo_filtered",
					"round", round,
					"repo", repoName,
					"skipped", skipped,
					"added", added,
				)
			}

			totalNodes += len(result.Nodes)
			totalCross += len(crossCalls)
			totalEntries += len(result.EntryPoints)

			log.Infow("round.merged",
				"round", round,
				"repo", repoName,
				"nodes", len(result.Nodes),
				"cross_repo_calls", len(crossCalls),
				"entry_points", len(result.EntryPoints),
				"cross_fallback_used", fallbackUsed,
			)
		}

		log.Infow("round.done",
			"round", round,
			"results", len(results),
			"total_nodes", totalNodes,
			"total_cross", totalCross,
			"total_entries", totalEntries,
			"next_batches", len(nextPending),
			"duration_ms", time.Since(roundStart).Milliseconds(),
		)

		// Plateau detection: if no new nodes were added this round and no
		// new cross-repo batches were discovered, increment the stall counter.
		currentNodeCount := len(output.CallGraph)
		if currentNodeCount == prevNodeCount && len(nextPending) == 0 {
			consecutiveNoProgress++
		} else {
			consecutiveNoProgress = 0
		}
		prevNodeCount = currentNodeCount

		if consecutiveNoProgress >= earlyStopPatience {
			log.Infow("analyse.early_stop",
				"reason", "plateau_detected",
				"round", round,
				"consecutive_no_progress", consecutiveNoProgress,
				"total_nodes", currentNodeCount,
				"saved_rounds", maxRounds-round-1,
			)
			break
		}

		pending = nextPending
	}

	// Warn if we hit the cap while pending batches remained — indicates deeper
	// exploration was truncated (e.g. --fast limiting --deep-worthy analysis).
	if len(pending) > 0 {
		log.Warnw("analyse.max_rounds_reached",
			"max_rounds", maxRounds,
			"mode", mode,
			"unexplored_batches", len(pending),
		)
	}

	// Deduplicate entry points.
	output.EntryPoints = dedupeCallNodes(output.EntryPoints)

	log.Infow("analyse.done",
		"total_nodes", len(output.CallGraph),
		"entry_points", len(output.EntryPoints),
		"function_analyses", len(output.FunctionAnalyses),
		"workers", len(output.WorkerOutputs),
		"mode", mode,
		"max_rounds", maxRounds,
		"duration_ms", time.Since(runStart).Milliseconds(),
	)

	return output, nil
}

// extractChangedFunctions uses an AgentLoop to ask the LLM to parse the diff
// and return a list of changed function names with their file paths.
// Uses a minimal prompt focused ONLY on diff parsing — no call chain or constraint steps.
func (o *Orchestrator) extractChangedFunctions(ctx context.Context, input AnalysisInput) ([]string, error) {
	// Minimal system prompt for diff parsing only.
	// No hardcoded repo names, function names, or examples.
	sysPrompt := fmt.Sprintf(`You are a code diff parser. Your ONLY job is to extract changed production function names.

Workspace: %s
Source repo: %s

Rules:
- Read the diff carefully
- Return ONLY a newline-separated list of changed functions in format: FILE_PATH::FUNCTION_NAME
  e.g. compute/disk/encrypt_disk.py::read_ciphertext_from_dev
- Prefix the file path with the source repo name: %s/compute/disk/encrypt_disk.py::read_ciphertext_from_dev
- SKIP test functions: any function whose name starts with Test, test_, or is in a file ending with _test.py / test_*.py
- SKIP __init__, __str__, __repr__ and other dunder-only changes unless they contain real logic changes
- Do NOT call any tools, do NOT analyse call chains, do NOT add explanations
- Just the list`, o.workspaceDir, input.SourceRepo, input.SourceRepo)

	// Use an agent loop with NO tools — pure text generation.
	loop := NewAgentLoop(o.llmClient, nil, 0, o.cp, sysPrompt)

	task := fmt.Sprintf(
		"Extract changed production functions from the following diff.\n\nDescription: %s\n\nDiff:\n%s",
		input.Description, input.Diff,
	)

	result, err := loop.Run(ctx, "orchestrator-extract", task)
	if err != nil {
		return nil, err
	}

	fns := parseFileFunctionList(result.Content)
	// Go-level safety filter: drop any remaining test functions the LLM may have included.
	fns = filterTestFunctions(fns)
	return fns, nil
}

// maxWorkerConcurrency limits how many Worker goroutines run in parallel.
// High concurrency triggers LLM rate-limits and queueing; 6 is a safe value
// for this LLM backend and keeps context-tokens-in-flight bounded.
const maxWorkerConcurrency = 6

// runWorkerBatch launches WorkerAgents for each pending batch and collects results.
//
// Scheduling strategy:
//  1. Sort batches by triage priority: P0 → P1 → P2
//  2. Within each priority tier, run up to maxWorkerConcurrency workers in parallel
//  3. P2 workers use a tighter step budget (50 steps) — they just do shallow file search
//  4. Results from multiple batches of the same repo are merged into one WorkerResult
func (o *Orchestrator) runWorkerBatch(ctx context.Context, pending map[string][]string) map[string]*WorkerResult {
	log := logger.S()

	// Group batches by priority so we can run P0 → P1 → P2 serially.
	type batchJob struct {
		batchKey string
		funcs    []string
		priority string
		repo     string
	}
	priorityTiers := [][]batchJob{
		nil, nil, nil, nil, // 0=P0, 1=P1, 2=P2, 3=unknown/default
	}
	for batchKey, funcs := range pending {
		repoName := batchKeyToRepo(batchKey)
		priority := "P1" // default for follow-up rounds without triage metadata
		if meta, ok := o.batchPriorities[batchKey]; ok {
			priority = meta.Priority
		}
		job := batchJob{batchKey: batchKey, funcs: funcs, priority: priority, repo: repoName}
		switch priority {
		case "P0":
			priorityTiers[0] = append(priorityTiers[0], job)
		case "P1":
			priorityTiers[1] = append(priorityTiers[1], job)
		case "P2":
			priorityTiers[2] = append(priorityTiers[2], job)
		default:
			priorityTiers[3] = append(priorityTiers[3], job)
		}
	}

	var mu sync.Mutex
	batchResults := make(map[string]*WorkerResult, len(pending))

	// Run each priority tier serially; inside a tier, workers run with limited concurrency.
	tierNames := []string{"P0", "P1", "P2", "default"}
	for tierIdx, jobs := range priorityTiers {
		if len(jobs) == 0 {
			continue
		}
		tierStart := time.Now()
		log.Infow("worker_tier.start",
			"priority", tierNames[tierIdx],
			"jobs", len(jobs),
			"concurrency", maxWorkerConcurrency,
		)

		sem := make(chan struct{}, maxWorkerConcurrency)
		var wg sync.WaitGroup

		for _, j := range jobs {
			j := j
			// Step budget: P2 gets tighter budget (50) to cap shallow scans.
			budget := 0 // default (300)
			if j.priority == "P2" {
				budget = 50
			}

			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()

				repoPath := o.repoPath(j.repo)
				worker := NewWorkerAgentWithBudget(
					o.llmClient, o.tools, o.cp, o.repos, o.workspaceDir, budget,
				)
				res, err := worker.Analyse(ctx, WorkerTask{
					RepoName:         j.repo,
					RepoPath:         repoPath,
					ChangedFunctions: j.funcs,
					Priority:         j.priority,
					ContractHints:    o.contractHints,
					ImportContext:    o.importContext,
				})
				mu.Lock()
				if err != nil {
					batchResults[j.batchKey] = nil
				} else {
					batchResults[j.batchKey] = res
				}
				mu.Unlock()
			}()
		}
		wg.Wait()
		log.Infow("worker_tier.done",
			"priority", tierNames[tierIdx],
			"jobs", len(jobs),
			"duration_ms", time.Since(tierStart).Milliseconds(),
		)
	}

	// Merge batch results: multiple batches of the same repo → one WorkerResult.
	merged := make(map[string]*WorkerResult)
	for batchKey, res := range batchResults {
		repoName := batchKeyToRepo(batchKey)
		if res == nil {
			if _, exists := merged[repoName]; !exists {
				merged[repoName] = nil
			}
			continue
		}
		if existing, exists := merged[repoName]; !exists || existing == nil {
			merged[repoName] = res
		} else {
			existing.Nodes = append(existing.Nodes, res.Nodes...)
			existing.CrossRepoCalls = append(existing.CrossRepoCalls, res.CrossRepoCalls...)
			existing.EntryPoints = append(existing.EntryPoints, res.EntryPoints...)
			existing.FunctionAnalyses = append(existing.FunctionAnalyses, res.FunctionAnalyses...)
			existing.EntryScenarios = append(existing.EntryScenarios, res.EntryScenarios...)
			existing.SearchResults = append(existing.SearchResults, res.SearchResults...)
			if res.ReachedEntry {
				existing.ReachedEntry = true
			}
		}
	}
	return merged
}

// groupByRepo attempts to map fully-qualified function names to repository
// names.  Names are expected in the form "repoName/pkg.Func" or simply
// "Func" (assigned to sourceRepo, or the first repo as last-resort fallback).
func (o *Orchestrator) groupByRepo(functions []string, sourceRepo string) map[string][]string {
	grouped := make(map[string][]string)
	for _, fn := range functions {
		repo := o.inferRepo(fn, sourceRepo)
		grouped[repo] = append(grouped[repo], fn)
	}
	return grouped
}

// groupByRepoAndFile groups changed functions first by repo, then by source file.
// Each (repo, file) pair becomes a separate Worker batch key so that Workers
// operate on a coherent set of functions from the same module.
//
// Function format expected: "repo/path/to/file.py::FuncName"
// Falls back to groupByRepo behaviour for old-style "repo/module.Func" names.
//
// If a single file has more than maxFuncsPerFileBatch functions, it is split
// into numbered sub-batches to keep each Worker's context manageable.
func (o *Orchestrator) groupByRepoAndFile(functions []string, sourceRepo string) map[string][]string {
	const maxFuncsPerFileBatch = 20

	// file key → list of functions.
	type fileGroup struct {
		repo  string
		file  string
		funcs []string
	}
	// Use ordered slice to keep deterministic batch keys.
	fileKeyOrder := make([]string, 0)
	fileGroups := make(map[string]*fileGroup)

	for _, fn := range functions {
		repo, filePath, funcName := parseFileFuncTriple(fn, sourceRepo, o)
		fileKey := repo + "::" + filePath

		if _, exists := fileGroups[fileKey]; !exists {
			fileKeyOrder = append(fileKeyOrder, fileKey)
			fileGroups[fileKey] = &fileGroup{repo: repo, file: filePath}
		}
		_ = funcName // funcName is embedded in fn; pass fn as-is to Worker
		fileGroups[fileKey].funcs = append(fileGroups[fileKey].funcs, fn)
	}

	// Build pending map: batchKey → []string of functions.
	pending := make(map[string][]string)
	for _, fileKey := range fileKeyOrder {
		fg := fileGroups[fileKey]
		if len(fg.funcs) <= maxFuncsPerFileBatch {
			// Single batch for this file.
			batchKey := fg.repo + "::file:" + sanitizeKey(fg.file)
			if fg.file == "" {
				batchKey = fg.repo
			}
			pending[batchKey] = append(pending[batchKey], fg.funcs...)
		} else {
			// Split into numbered sub-batches.
			for i := 0; i < len(fg.funcs); i += maxFuncsPerFileBatch {
				end := i + maxFuncsPerFileBatch
				if end > len(fg.funcs) {
					end = len(fg.funcs)
				}
				batchKey := fmt.Sprintf("%s::file:%s::batch%d",
					fg.repo, sanitizeKey(fg.file), i/maxFuncsPerFileBatch)
				pending[batchKey] = fg.funcs[i:end]
			}
		}
	}
	return pending
}

// parseFileFuncTriple extracts (repo, filePath, funcName) from a function entry.
// Expected formats:
//   - "repo/path/file.py::FuncName"   (new format with file path)
//   - "repo/module.ClassName.method"  (old format without file path)
func parseFileFuncTriple(fn, sourceRepo string, o *Orchestrator) (repo, filePath, funcName string) {
	if idx := strings.Index(fn, "::"); idx > 0 {
		// New format: "repo/path/file.py::FuncName"
		pathPart := fn[:idx]
		funcName = fn[idx+2:]
		// First segment of pathPart is the repo name.
		if slash := strings.Index(pathPart, "/"); slash > 0 {
			repo = pathPart[:slash]
			filePath = pathPart[slash+1:] // path within repo
		} else {
			repo = o.inferRepo(pathPart, sourceRepo)
			filePath = pathPart
		}
		if !o.repoExists(repo) {
			repo = o.inferRepo(fn, sourceRepo)
			filePath = ""
		}
		return
	}
	// Old format: fall back to inferRepo, no file path.
	repo = o.inferRepo(fn, sourceRepo)
	filePath = ""
	funcName = fn
	return
}

// batchKeyToRepo strips the "::file:..." and "::batch..." suffixes to get the repo name.
func batchKeyToRepo(batchKey string) string {
	if idx := strings.Index(batchKey, "::"); idx > 0 {
		return batchKey[:idx]
	}
	return batchKey
}

// sanitizeKey replaces path separators with underscores for use in map keys.
func sanitizeKey(s string) string {
	return strings.NewReplacer("/", "_", ".", "_").Replace(s)
}

// inferRepo tries to identify the repo for a given function name.
//
// Strategy (in order of precedence):
//  1. If the function name contains a path separator, the first segment is
//     the repo name (e.g. "vstation_compute/compute/disk.read_ciphertext").
//  2. Prefix-match the function name against known repo names
//     (e.g. "vstation_compute.disk.read" matches repo "vstation_compute").
//  3. Fall back to sourceRepo (the repo that owns the diff).
//  4. Last resort: first repo in the list.
func (o *Orchestrator) inferRepo(fn string, sourceRepo string) string {
	// Strategy 1: path-style name — first segment is repo name.
	if idx := strings.Index(fn, "/"); idx > 0 {
		candidate := fn[:idx]
		if o.repoExists(candidate) {
			return candidate
		}
	}

	// Strategy 2: prefix match against known repo names.
	for _, r := range o.repos {
		if strings.HasPrefix(fn, r.Name+"/") ||
			strings.HasPrefix(fn, r.Name+".") ||
			strings.HasPrefix(fn, r.Name+"_") {
			return r.Name
		}
	}

	// Strategy 3: use explicitly declared source repo.
	if sourceRepo != "" && o.repoExists(sourceRepo) {
		return sourceRepo
	}

	// Strategy 4: last resort.
	if len(o.repos) > 0 {
		return o.repos[0].Name
	}
	return "unknown"
}

// repoFromFilePath extracts the repository name from a file path returned by
// ripgrep.  ripgrep results use workspace-relative paths of the form:
//
//	{repo_name}/{internal/path}:{line}:{content}
//
// This function takes the first path segment as the repo name and verifies it
// exists in the known repo list.  This is the primary routing mechanism for
// cross-repo call chain tracing — no name-guessing needed.
func (o *Orchestrator) repoFromFilePath(filePath string) string {
	// Strip line/content suffix (e.g. "repo/file.py:45:content" → "repo/file.py")
	if idx := strings.Index(filePath, ":"); idx > 0 {
		filePath = filePath[:idx]
	}
	// First path segment = repo name.
	if idx := strings.Index(filePath, "/"); idx > 0 {
		candidate := filePath[:idx]
		if o.repoExists(candidate) {
			return candidate
		}
	}
	return ""
}

// repoExists returns true if a repo with the given name is in the known list.
func (o *Orchestrator) repoExists(name string) bool {
	for _, r := range o.repos {
		if r.Name == name {
			return true
		}
	}
	return false
}

// extractCrossRepoCallsFromSearch derives cross-repo calls from SearchResult
// file paths, independent of LLM cross_repo_calls field.
// Used both as fallback and as supplement (merged with LLM-provided calls).
//
// Rules:
//   - File path first segment = repo name (must exist in config)
//   - Skip paths that don't cross a repo boundary
//   - If SearchResult.Function is non-empty, use it as TargetFunction
//   - If SearchResult.Function is empty, emit a FILE_CHANGED sentinel
//     so the next-hop Worker still has something to search
//   - Deduplicate by (repo, target_function)
func (o *Orchestrator) extractCrossRepoCallsFromSearch(
	currentRepo string,
	results []SearchResult,
) []CrossRepoCall {
	var calls []CrossRepoCall
	seen := make(map[string]bool)

	for _, r := range results {
		// File path first segment = repo name (deterministic, no LLM needed).
		filePath := r.File
		if idx := strings.Index(filePath, ":"); idx > 0 {
			filePath = filePath[:idx]
		}
		parts := strings.SplitN(filePath, "/", 2)
		if len(parts) < 2 || parts[0] == "" {
			continue
		}
		callerRepo := parts[0]

		if callerRepo == currentRepo {
			continue
		}
		if !o.repoExists(callerRepo) {
			continue
		}

		// Target function: use caller function name when known, else FILE_CHANGED sentinel.
		targetFunc := r.Function
		if targetFunc == "" {
			targetFunc = "FILE_CHANGED:" + callerRepo + "/" + parts[1]
		}

		key := callerRepo + ":" + targetFunc
		if seen[key] {
			continue
		}
		seen[key] = true

		calls = append(calls, CrossRepoCall{
			TargetRepo:     callerRepo,
			TargetFunction: targetFunc,
			CallerNode: CallNode{
				Repo:     currentRepo,
				File:     r.File,
				Line:     r.Line,
				Function: r.Function,
			},
		})
	}
	return calls
}

// extractCrossRepoCallsFromNodes derives cross-repo calls from Worker-reported
// nodes[] entries whose repo differs from the current repo.
//
// This catches cases where the LLM correctly identified a cross-repo caller in
// its `nodes` output but forgot to also fill `cross_repo_calls`.
func (o *Orchestrator) extractCrossRepoCallsFromNodes(
	currentRepo string,
	nodes []CallNode,
) []CrossRepoCall {
	var calls []CrossRepoCall
	seen := make(map[string]bool)

	for _, n := range nodes {
		nodeRepo := n.Repo
		// If node's explicit repo is missing, try to infer from file path.
		if nodeRepo == "" && n.File != "" {
			if idx := strings.Index(n.File, "/"); idx > 0 {
				candidate := n.File[:idx]
				if o.repoExists(candidate) {
					nodeRepo = candidate
				}
			}
		}
		if nodeRepo == "" || nodeRepo == currentRepo {
			continue
		}
		if !o.repoExists(nodeRepo) {
			continue
		}

		targetFunc := n.Function
		if targetFunc == "" && n.File != "" {
			// File path sentinel fallback.
			fp := n.File
			if idx := strings.Index(fp, nodeRepo+"/"); idx >= 0 {
				fp = fp[idx:]
			} else {
				fp = nodeRepo + "/" + fp
			}
			targetFunc = "FILE_CHANGED:" + fp
		}
		if targetFunc == "" {
			continue
		}

		key := nodeRepo + ":" + targetFunc
		if seen[key] {
			continue
		}
		seen[key] = true

		calls = append(calls, CrossRepoCall{
			TargetRepo:     nodeRepo,
			TargetFunction: targetFunc,
			CallerNode: CallNode{
				Repo:     currentRepo,
				File:     n.File,
				Line:     n.Line,
				Function: n.Function,
			},
		})
	}
	return calls
}

// mergeCrossRepoCalls combines multiple CrossRepoCall slices, deduplicating by
// (TargetRepo, TargetFunction). Preserves the CallerNode from the first occurrence.
func mergeCrossRepoCalls(slices ...[]CrossRepoCall) []CrossRepoCall {
	seen := make(map[string]bool)
	merged := make([]CrossRepoCall, 0)
	for _, s := range slices {
		for _, c := range s {
			key := c.TargetRepo + ":" + c.TargetFunction
			if seen[key] {
				continue
			}
			seen[key] = true
			merged = append(merged, c)
		}
	}
	return merged
}

// repoPath looks up the on-disk path for a repo name.
func (o *Orchestrator) repoPath(name string) string {
	for _, r := range o.repos {
		if r.Name == name {
			return r.Path
		}
	}
	// If not found by exact name, try workspace dir + name as fallback.
	if o.workspaceDir != "" && name != "" {
		return o.workspaceDir + "/" + name
	}
	return ""
}

// parseFunctionList splits a newline / comma-separated list of function names
// returned by the LLM into a clean slice.
func parseFunctionList(raw string) []string {
	var result []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "-")
		line = strings.TrimPrefix(line, "*")
		line = strings.TrimSpace(line)
		if line != "" {
			result = append(result, line)
		}
	}
	return result
}

// parseFileFunctionList parses LLM output in "file_path::func_name" format.
// Falls back to parseFunctionList for lines that don't contain "::".
// The returned strings preserve the "repo/file.py::func" format so that
// groupByFileModule can extract the file path for grouping.
func parseFileFunctionList(raw string) []string {
	var result []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "-")
		line = strings.TrimPrefix(line, "*")
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		result = append(result, line)
	}
	return result
}

// filterTestFunctions removes test functions from a list.
// A function is considered a test if:
//   - Its base name (after the last "::") starts with "Test" or "test_"
//   - It lives in a file whose name matches test_*.py or *_test.py
func filterTestFunctions(fns []string) []string {
	out := make([]string, 0, len(fns))
	for _, fn := range fns {
		if isTestFunction(fn) {
			continue
		}
		out = append(out, fn)
	}
	return out
}

// isTestFunction returns true if the function name or its containing file
// indicates it is a test artifact that should be skipped during tracing.
func isTestFunction(fn string) bool {
	// Extract the function name part (after last "::" if present).
	name := fn
	if idx := strings.LastIndex(fn, "::"); idx >= 0 {
		name = fn[idx+2:]
	} else if idx := strings.LastIndex(fn, "/"); idx >= 0 {
		// Fallback: last path segment for old-style "repo/module.Func" names.
		name = fn[idx+1:]
	}
	name = strings.TrimSpace(name)

	// Extract file path part (before "::").
	filePath := ""
	if idx := strings.Index(fn, "::"); idx >= 0 {
		filePath = fn[:idx]
	}

	// Test by function name prefix.
	if strings.HasPrefix(name, "Test") || strings.HasPrefix(name, "test_") {
		return true
	}
	// Test by class.method where class starts with Test.
	if idx := strings.Index(name, "."); idx > 0 {
		className := name[:idx]
		if strings.HasPrefix(className, "Test") {
			return true
		}
	}

	// Test by file path pattern.
	if filePath != "" {
		base := filePath
		if idx := strings.LastIndex(filePath, "/"); idx >= 0 {
			base = filePath[idx+1:]
		}
		if strings.HasPrefix(base, "test_") || strings.HasSuffix(base, "_test.py") ||
			strings.HasSuffix(base, "_test.go") {
			return true
		}
	}

	return false
}

// dedupeCallNodes removes duplicate CallNode entries by repo+file+function key.
func dedupeCallNodes(nodes []CallNode) []CallNode {
	seen := make(map[string]bool)
	out := make([]CallNode, 0, len(nodes))
	for _, n := range nodes {
		key := n.Repo + ":" + n.File + ":" + n.Function
		if !seen[key] {
			seen[key] = true
			out = append(out, n)
		}
	}
	return out
}

// diffFileRe matches "--- a/path" or "+++ b/path" lines in unified diffs.
var diffFileRe = regexp.MustCompile(`(?m)^\+\+\+ b/(.+)$`)

// extractDiffFiles returns the set of source-relative file paths that appear
// in the unified diff (i.e. the files that were actually changed).
// Only production files are returned — test files are excluded.
func extractDiffFiles(diff string) []string {
	matches := diffFileRe.FindAllStringSubmatch(diff, -1)
	seen := make(map[string]bool)
	var files []string
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		fp := strings.TrimSpace(m[1])
		if fp == "" || fp == "/dev/null" {
			continue
		}
		if seen[fp] {
			continue
		}
		// Skip test files at the Go level too.
		if isTestFunction(fp) {
			continue
		}
		seen[fp] = true
		files = append(files, fp)
	}
	return files
}

// ensureDiffFileCoverage scans the raw diff for changed files and adds any
// file that isn't already covered by a pending Worker batch.
// The sentinel function name "FILE_CHANGED" signals to the Worker that it
// should search the file broadly rather than trace a specific named function.
func (o *Orchestrator) ensureDiffFileCoverage(pending map[string][]string, input AnalysisInput) {
	diffFiles := extractDiffFiles(input.Diff)

	for _, filePath := range diffFiles {
		// Determine repo from the first path segment.
		repo := ""
		rest := filePath
		if idx := strings.Index(filePath, "/"); idx > 0 {
			candidate := filePath[:idx]
			if o.repoExists(candidate) {
				repo = candidate
				rest = filePath[idx+1:]
			}
		}
		if repo == "" {
			if input.SourceRepo != "" {
				repo = input.SourceRepo
				rest = filePath
			} else {
				continue
			}
		}

		// Build the batch key this file would map to.
		batchKey := repo + "::file:" + sanitizeKey(rest)

		// Check if any existing pending key already covers this file.
		alreadyCovered := false
		for k := range pending {
			if strings.HasPrefix(k, repo+"::file:"+sanitizeKey(rest)) {
				alreadyCovered = true
				break
			}
		}
		if alreadyCovered {
			continue
		}

		// Add a sentinel entry so a Worker is launched for this file.
		// "FILE_CHANGED:repo/path" tells the Worker to search the file broadly.
		sentinel := "FILE_CHANGED:" + repo + "/" + rest
		pending[batchKey] = append(pending[batchKey], sentinel)
	}
}

// BatchPriority carries the priority classification for a Worker batch.
// Set by applyTriage; consumed by runWorkerBatch to schedule serially
// (P0 first, then P1, then P2) and to apply per-priority step budgets.
type BatchPriority struct {
	Priority string // "P0", "P1", "P2"
	Repo     string
}

// applyTriage runs the TriageAgent to classify changed files by priority, then:
//  1. P2 files → demoted to FILE_CHANGED sentinel (shallow tracing)
//  2. Small batches (<maxMergePerBatch funcs) of the same (repo, priority) are
//     merged into one Worker batch, reducing 35 Workers → ~8 Workers
//  3. Priority metadata is stored in o.batchPriorities (for scheduler use)
func (o *Orchestrator) applyTriage(ctx context.Context, pending map[string][]string, input AnalysisInput) map[string][]string {
	// Collect unique file paths across all pending batches.
	fileSet := make(map[string]bool)
	for _, funcs := range pending {
		for _, fn := range funcs {
			if strings.HasPrefix(fn, "FILE_CHANGED:") {
				fp := strings.TrimPrefix(fn, "FILE_CHANGED:")
				if idx := strings.Index(fp, "/"); idx > 0 {
					fp = fp[idx+1:]
				}
				fileSet[fp] = true
			} else if idx := strings.Index(fn, "::"); idx > 0 {
				fp := fn[:idx]
				if slash := strings.Index(fp, "/"); slash > 0 {
					fp = fp[slash+1:]
				}
				fileSet[fp] = true
			}
		}
	}

	files := make([]string, 0, len(fileSet))
	for f := range fileSet {
		files = append(files, f)
	}

	if len(files) == 0 {
		return pending
	}

	// Run triage.
	triageAgent := NewTriageAgent(o.llmClient)
	triageResult := triageAgent.Triage(ctx, files, input.Description)

	// Build a file → priority map.
	filePriority := make(map[string]string, len(triageResult.Items))
	for _, item := range triageResult.Items {
		filePriority[item.File] = item.Priority
	}

	// Per-batch priority + file extraction.
	type batchMeta struct {
		priority string
		file     string
		repo     string
		funcs    []string
	}
	batches := make([]batchMeta, 0, len(pending))
	p0Count, p1Count, p2Count, p2Demoted := 0, 0, 0, 0

	for batchKey, funcs := range pending {
		fileFromKey := ""
		if idx := strings.Index(batchKey, "::file:"); idx > 0 {
			suffix := batchKey[idx+7:]
			if bi := strings.Index(suffix, "::batch"); bi > 0 {
				suffix = suffix[:bi]
			}
			fileFromKey = suffix
		}

		priority := "P1" // default — changed from P0 (less aggressive)
		for filePath, p := range filePriority {
			if sanitizeKey(filePath) == fileFromKey ||
				strings.Contains(fileFromKey, sanitizeKey(filePath)) ||
				strings.Contains(sanitizeKey(filePath), fileFromKey) {
				priority = p
				break
			}
		}

		repo := batchKeyToRepo(batchKey)
		bm := batchMeta{priority: priority, file: fileFromKey, repo: repo, funcs: funcs}

		switch priority {
		case "P0":
			p0Count++
		case "P1":
			p1Count++
		case "P2":
			p2Count++
			// Demote: replace all functions with a single FILE_CHANGED sentinel.
			fileSentinel := "FILE_CHANGED:" + repo + "/" + strings.ReplaceAll(fileFromKey, "_", "/")
			bm.funcs = []string{fileSentinel}
			p2Demoted++
		}
		batches = append(batches, bm)
	}

	// Merge small batches of the same (repo, priority) into a single Worker batch.
	// This collapses 35 one-function batches into ~8 Worker tasks, drastically
	// reducing LLM concurrency pressure.
	const maxMergePerBatch = 10
	merged := make(map[string][]string)
	o.batchPriorities = make(map[string]BatchPriority)
	groupBuckets := make(map[string][]batchMeta) // key = repo|priority

	for _, bm := range batches {
		gk := bm.repo + "|" + bm.priority
		groupBuckets[gk] = append(groupBuckets[gk], bm)
	}

	for gk, group := range groupBuckets {
		parts := strings.SplitN(gk, "|", 2)
		repo, priority := parts[0], parts[1]

		// Split group into chunks of maxMergePerBatch batches.
		for i := 0; i < len(group); i += maxMergePerBatch {
			end := i + maxMergePerBatch
			if end > len(group) {
				end = len(group)
			}
			chunk := group[i:end]
			// Collect all functions in this chunk.
			var combinedFuncs []string
			for _, bm := range chunk {
				combinedFuncs = append(combinedFuncs, bm.funcs...)
			}
			// Build merged batch key.
			mergedKey := fmt.Sprintf("%s::priority:%s::merged:%d", repo, priority, i/maxMergePerBatch)
			merged[mergedKey] = combinedFuncs
			o.batchPriorities[mergedKey] = BatchPriority{Priority: priority, Repo: repo}
		}
	}

	logger.S().Infow("triage.classified",
		"total_files", len(files),
		"p0_batches", p0Count,
		"p1_batches", p1Count,
		"p2_batches", p2Count,
		"p2_demoted", p2Demoted,
		"before_merge", len(pending),
		"after_merge", len(merged),
		"skipped", len(triageResult.Skipped),
	)
	return merged
}

// ---------------------------------------------------------------------------
// Graph-based analysis (hybrid/deterministic mode)
// ---------------------------------------------------------------------------

// graphAnalysisResult holds the output from deterministic graph traversal.
type graphAnalysisResult struct {
	nodes       []CallNode
	entryPoints []CallNode
	uncovered   []string // function names not found in the index (need LLM)
}

// runGraphAnalysis performs deterministic call chain analysis using the index graph.
// For each changed function, it resolves the symbol in the graph and runs BFS
// to find all upstream callers (up to depth 3).
func (o *Orchestrator) runGraphAnalysis(changedFunctions []string, sourceRepo string) *graphAnalysisResult {
	log := logger.S()
	result := &graphAnalysisResult{}

	var startIDs []string

	for _, fn := range changedFunctions {
		// Try to resolve function name to a graph node
		// Extract the function name part (after :: if present)
		funcName := fn
		if idx := strings.LastIndex(fn, "::"); idx >= 0 {
			funcName = fn[idx+2:]
		}
		// Strip repo prefix if present (e.g. "vstation_compute/compute/disk/encrypt_disk.py::func")
		if idx := strings.Index(funcName, "/"); idx >= 0 {
			parts := strings.SplitN(fn, "/", 2)
			if len(parts) == 2 && o.repoExists(parts[0]) {
				// Already handled by funcName extraction above
			}
		}

		nodes := o.indexGraph.FindNodesByName(funcName)
		if len(nodes) == 0 {
			// Try with simple name (last segment after .)
			if dotIdx := strings.LastIndex(funcName, "."); dotIdx >= 0 {
				simpleName := funcName[dotIdx+1:]
				nodes = o.indexGraph.FindNodesByName(simpleName)
			}
		}

		if len(nodes) == 0 {
			result.uncovered = append(result.uncovered, fn)
			continue
		}

		// Use first matching node (TODO: disambiguate by file path)
		for _, n := range nodes {
			startIDs = append(startIDs, n.ID)
		}
	}

	if len(startIDs) == 0 {
		result.uncovered = changedFunctions
		return result
	}

	log.Infow("graph.analysis_start",
		"start_symbols", len(startIDs),
		"uncovered", len(result.uncovered),
	)

	// BFS upstream (find callers)
	affected := o.indexGraph.Impact(startIDs, "upstream", 3, 0.0)

	// Convert to CallNodes
	for _, a := range affected {
		node := CallNode{
			Repo:     a.Repo,
			File:     a.FilePath,
			Function: a.Name,
		}
		result.nodes = append(result.nodes, node)

		// Check if this node is in an entry-role repo
		for _, r := range o.repos {
			if strings.EqualFold(r.Role, "entry") && r.Name == a.Repo {
				result.entryPoints = append(result.entryPoints, node)
				break
			}
		}
	}

	log.Infow("graph.analysis_done",
		"affected_symbols", len(affected),
		"entry_points", len(result.entryPoints),
		"uncovered", len(result.uncovered),
	)

	return result
}

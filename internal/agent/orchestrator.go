package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/DeviosLang/shirakami/internal/checkpoint"
)

// AnalysisInput is the input to an Orchestrator run.
type AnalysisInput struct {
	// Diff is the unified diff of the changes to analyse.
	Diff string
	// Description is a human-readable description of the change.
	Description string
}

// AnalysisOutput is the result returned by the Orchestrator.
type AnalysisOutput struct {
	// ChangedFunctions lists all functions identified as changed by the diff.
	ChangedFunctions []string
	// CallGraph is the complete multi-repo call chain.
	CallGraph []CallNode
	// EntryPoints are the route-registered / test functions in entry-role repos
	// that form the integration-test entry points.
	EntryPoints []CallNode
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
		llmClient:    llmClient,
		tools:        tools,
		repos:        repos,
		workspaceDir: workspaceDir,
		cp:           cp,
	}
}

// Run analyses the provided diff and returns the complete call-chain graph.
func (o *Orchestrator) Run(ctx context.Context, input AnalysisInput) (*AnalysisOutput, error) {
	// Step 1 – extract changed functions from the diff.
	changed, err := o.extractChangedFunctions(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("extract changed functions: %w", err)
	}

	output := &AnalysisOutput{
		ChangedFunctions: changed,
		WorkerOutputs:    make(map[string]*WorkerResult),
	}

	// Step 2 – group changed functions by repo and fan out to workers.
	// We iterate upward until no new cross-repo calls are produced.
	// Each iteration maps repoName → functions that need tracing.
	pending := o.groupByRepo(changed)

	visited := make(map[string]bool)
	for len(pending) > 0 {
		nextPending := make(map[string][]string)

		results := o.runWorkerBatch(ctx, pending)
		for repoName, result := range results {
			if result == nil {
				continue
			}
			output.WorkerOutputs[repoName] = result
			output.CallGraph = append(output.CallGraph, result.Nodes...)
			if result.ReachedEntry {
				output.EntryPoints = append(output.EntryPoints, result.EntryPoints...)
			}

			// Follow cross-repo calls that haven't been visited yet.
			for _, cross := range result.CrossRepoCalls {
				key := cross.TargetRepo + ":" + cross.TargetFunction
				if !visited[key] {
					visited[key] = true
					nextPending[cross.TargetRepo] = append(
						nextPending[cross.TargetRepo], cross.TargetFunction,
					)
				}
			}
		}

		pending = nextPending
	}

	return output, nil
}

// extractChangedFunctions uses an AgentLoop to ask the LLM to parse the diff
// and return a list of changed function names.
func (o *Orchestrator) extractChangedFunctions(ctx context.Context, input AnalysisInput) ([]string, error) {
	toolNames := make([]string, 0, len(o.tools))
	for _, t := range o.tools {
		toolNames = append(toolNames, t.Definition().Name)
	}

	sysPrompt, err := BuildSystemPrompt(PromptData{
		WorkspaceDir:   o.workspaceDir,
		Repos:          o.repos,
		AnalysisGoal:   "Parse the diff and return a newline-separated list of fully-qualified changed function names.",
		AvailableTools: toolNames,
	})
	if err != nil {
		return nil, err
	}

	loop := NewAgentLoop(o.llmClient, o.tools, 0, o.cp, sysPrompt)

	task := fmt.Sprintf(
		"Extract changed functions from the following diff.\n\nDescription: %s\n\nDiff:\n%s",
		input.Description, input.Diff,
	)

	result, err := loop.Run(ctx, "orchestrator-extract", task)
	if err != nil {
		return nil, err
	}

	return parseFunctionList(result.Content), nil
}

// runWorkerBatch launches one WorkerAgent per entry in pending concurrently
// and collects results.
func (o *Orchestrator) runWorkerBatch(ctx context.Context, pending map[string][]string) map[string]*WorkerResult {
	var mu sync.Mutex
	results := make(map[string]*WorkerResult, len(pending))
	var wg sync.WaitGroup

	for repoName, funcs := range pending {
		repoName, funcs := repoName, funcs
		repoPath := o.repoPath(repoName)

		wg.Add(1)
		go func() {
			defer wg.Done()
			worker := NewWorkerAgent(o.llmClient, o.tools, o.cp, o.repos, o.workspaceDir)
			res, err := worker.Analyse(ctx, WorkerTask{
				RepoName:         repoName,
				RepoPath:         repoPath,
				ChangedFunctions: funcs,
			})
			if err != nil {
				// Non-fatal: record nil and continue.
				mu.Lock()
				results[repoName] = nil
				mu.Unlock()
				return
			}
			mu.Lock()
			results[repoName] = res
			mu.Unlock()
		}()
	}

	wg.Wait()
	return results
}

// groupByRepo attempts to map fully-qualified function names to repository
// names.  Names are expected in the form "repoName/pkg.Func" or simply
// "Func" (assigned to the first non-entry repo as a heuristic).
func (o *Orchestrator) groupByRepo(functions []string) map[string][]string {
	grouped := make(map[string][]string)
	for _, fn := range functions {
		repo := o.inferRepo(fn)
		grouped[repo] = append(grouped[repo], fn)
	}
	return grouped
}

// inferRepo tries to identify the repo for a given function name by prefix
// matching.  Falls back to the first available repo.
func (o *Orchestrator) inferRepo(fn string) string {
	for _, r := range o.repos {
		if strings.HasPrefix(fn, r.Name+"/") || strings.HasPrefix(fn, r.Name+".") {
			return r.Name
		}
	}
	if len(o.repos) > 0 {
		return o.repos[0].Name
	}
	return "unknown"
}

// repoPath looks up the on-disk path for a repo name.
func (o *Orchestrator) repoPath(name string) string {
	for _, r := range o.repos {
		if r.Name == name {
			return r.Path
		}
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

package agent

import (
	"context"
	"fmt"

	"github.com/DeviosLang/shirakami/internal/checkpoint"
)

// CallNode represents one function in a call chain.
type CallNode struct {
	Repo     string // repository name
	Package  string // Go package import path or module-relative path
	Function string // qualified function name, e.g. "(*Handler).ServeHTTP"
	File     string // file path relative to repo root
	Line     int    // line number (0 if unknown)
}

// WorkerTask is the input handed to a WorkerAgent.
type WorkerTask struct {
	// RepoName is the target repository to analyse.
	RepoName string
	// RepoPath is the absolute on-disk path to the repository.
	RepoPath string
	// ChangedFunctions is the set of functions changed by the incoming diff
	// that need to be traced upward within this repo.
	ChangedFunctions []string
	// ExternalCallers are qualified function names from other repos that call
	// into this repo, also needing upstream tracing.
	ExternalCallers []string
}

// WorkerResult is returned by a WorkerAgent after analysis.
type WorkerResult struct {
	// RepoName is the repository that was analysed.
	RepoName string
	// Nodes are all call-chain nodes discovered in this repo.
	Nodes []CallNode
	// CrossRepoCalls are calls from this repo into other repos that the
	// Orchestrator should fan out to next.
	CrossRepoCalls []CrossRepoCall
	// ReachedEntry is true when at least one call chain reached a route-
	// registered function in an entry-role repository.
	ReachedEntry bool
	// EntryPoints lists the entry-role functions that were reached.
	EntryPoints []CallNode
}

// CrossRepoCall describes a call that crosses a repository boundary.
type CrossRepoCall struct {
	// TargetRepo is the repository name being called.
	TargetRepo string
	// TargetFunction is the function being called in the target repo.
	TargetFunction string
	// CallerNode is the node in the current repo making the call.
	CallerNode CallNode
}

// WorkerAgent performs local call-chain analysis inside a single repository.
// It runs an AgentLoop backed by repo-specific analysis tools.
type WorkerAgent struct {
	loop *AgentLoop
}

// NewWorkerAgent creates a WorkerAgent for a specific repository.
// tools should include repo-local analysis tools (e.g. grep, AST callers).
func NewWorkerAgent(
	llmClient LLMClient,
	tools []Tool,
	cp *checkpoint.FileCheckpointer,
	repos []RepoInfo,
	workspaceDir string,
) *WorkerAgent {
	toolNames := make([]string, 0, len(tools))
	for _, t := range tools {
		toolNames = append(toolNames, t.Definition().Name)
	}

	prompt, _ := BuildSystemPrompt(PromptData{
		WorkspaceDir:   workspaceDir,
		Repos:          repos,
		AnalysisGoal:   "Trace changed functions upward to entry-role route handlers within a single repository.",
		AvailableTools: toolNames,
	})

	loop := NewAgentLoop(llmClient, tools, 0, cp, prompt)
	return &WorkerAgent{loop: loop}
}

// Analyse runs the worker's agent loop for the supplied task.
// The LLM is expected to produce a structured text response that this
// function parses into a WorkerResult.
//
// In this implementation the raw LLM output is returned as-is in a
// WorkerResult so that the Orchestrator can decide how to aggregate it.
// Production implementations would parse structured JSON from the LLM.
func (w *WorkerAgent) Analyse(ctx context.Context, task WorkerTask) (*WorkerResult, error) {
	prompt := fmt.Sprintf(
		"Repository: %s\nPath: %s\nChanged functions: %v\nExternal callers: %v\n\n"+
			"Trace all changed and externally-called functions upward. "+
			"Identify callers within this repo and any calls that cross into other repos. "+
			"Return results as a structured summary.",
		task.RepoName,
		task.RepoPath,
		task.ChangedFunctions,
		task.ExternalCallers,
	)

	taskID := "worker-" + task.RepoName
	result, err := w.loop.Run(ctx, taskID, prompt)
	if err != nil {
		return nil, fmt.Errorf("worker %s: %w", task.RepoName, err)
	}

	// In a production system we would parse result.Content (structured JSON)
	// back into Nodes, CrossRepoCalls, etc.  For now we return the raw
	// content so callers can inspect it and the acceptance tests can verify
	// the message chain was constructed correctly.
	return &WorkerResult{
		RepoName: task.RepoName,
		Nodes: []CallNode{{
			Repo:     task.RepoName,
			Function: result.Content,
		}},
	}, nil
}

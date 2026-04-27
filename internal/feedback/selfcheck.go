package feedback

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"

	"github.com/DeviosLang/shirakami/internal/agent"
	"github.com/DeviosLang/shirakami/internal/llm"
)

// SelfCheckResult holds the outcome of both self-check phases.
type SelfCheckResult struct {
	// VerifiedNodes are call-chain nodes that passed Phase 1 verification.
	VerifiedNodes []agent.CallNode
	// UnverifiedNodes are nodes removed because the function was not found.
	UnverifiedNodes []agent.CallNode
	// ReviewNotes is the Phase 2 LLM review commentary.
	ReviewNotes string
}

// LLMCompleter is satisfied by *llm.Client and any test double.
type LLMCompleter interface {
	Complete(ctx context.Context, messages []llm.Message, tools []llm.ToolDefinition) (*llm.Response, error)
}

// SelfChecker performs the two-phase self-check on an analysis output.
type SelfChecker struct {
	llmClient    LLMCompleter
	workspaceDir string
}

// NewSelfChecker creates a SelfChecker.
func NewSelfChecker(llmClient LLMCompleter, workspaceDir string) *SelfChecker {
	return &SelfChecker{
		llmClient:    llmClient,
		workspaceDir: workspaceDir,
	}
}

// Check runs Phase 1 and Phase 2 on the supplied call graph.
// It should be called after the Orchestrator has produced a final AnalysisOutput.
func (sc *SelfChecker) Check(ctx context.Context, output *agent.AnalysisOutput) (*SelfCheckResult, error) {
	result := &SelfCheckResult{}

	// Phase 1 – verify each node exists in the file system via ripgrep.
	for _, node := range output.CallGraph {
		found, err := sc.verifyNodeExists(ctx, node)
		if err != nil {
			// Treat search errors as unverified, not fatal.
			result.UnverifiedNodes = append(result.UnverifiedNodes, node)
			continue
		}
		if found {
			result.VerifiedNodes = append(result.VerifiedNodes, node)
		} else {
			result.UnverifiedNodes = append(result.UnverifiedNodes, node)
		}
	}

	// Phase 2 – LLM review of the verified call chain.
	reviewNotes, err := sc.reviewCallChain(ctx, result.VerifiedNodes)
	if err != nil {
		// Non-fatal; attach error as a note.
		result.ReviewNotes = fmt.Sprintf("review error: %v", err)
	} else {
		result.ReviewNotes = reviewNotes
	}

	return result, nil
}

// verifyNodeExists uses ripgrep to check that the function name appears in the
// expected file (or anywhere in the workspace if File is empty).
func (sc *SelfChecker) verifyNodeExists(ctx context.Context, node agent.CallNode) (bool, error) {
	if node.Function == "" {
		return false, nil
	}

	args := []string{"--quiet", "--fixed-strings", node.Function}

	if node.File != "" {
		// Search the specific file (resolved relative to repo root).
		repoPath := filepath.Join(sc.workspaceDir, node.Repo)
		target := filepath.Join(repoPath, node.File)
		args = append(args, target)
	} else {
		// Fall back to searching the whole workspace.
		args = append(args, sc.workspaceDir)
	}

	cmd := exec.CommandContext(ctx, "rg", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		// Exit code 1 = no match; anything else is a real error.
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return false, nil
		}
		return false, fmt.Errorf("ripgrep: %w", err)
	}
	return true, nil
}

// reviewCallChain sends the verified call graph to the LLM for a completeness
// review and returns the LLM's commentary.
func (sc *SelfChecker) reviewCallChain(ctx context.Context, nodes []agent.CallNode) (string, error) {
	if len(nodes) == 0 {
		return "No verified nodes to review.", nil
	}

	chain := formatCallChain(nodes)
	prompt := fmt.Sprintf(
		"请审查以下调用链，检查从变更函数到入口仓库的路径是否完整，是否有遗漏的跨仓调用:\n\n%s",
		chain,
	)

	messages := []llm.Message{
		llm.UserMessage{Content: prompt},
	}

	resp, err := sc.llmClient.Complete(ctx, messages, nil)
	if err != nil {
		return "", fmt.Errorf("llm review: %w", err)
	}

	return resp.Content, nil
}

// formatCallChain formats a slice of CallNodes into a human-readable string
// suitable for the LLM review prompt.
func formatCallChain(nodes []agent.CallNode) string {
	var buf bytes.Buffer
	for i, n := range nodes {
		fmt.Fprintf(&buf, "%d. [%s] %s", i+1, n.Repo, n.Function)
		if n.File != "" {
			fmt.Fprintf(&buf, " (%s", n.File)
			if n.Line > 0 {
				fmt.Fprintf(&buf, ":%d", n.Line)
			}
			buf.WriteByte(')')
		}
		buf.WriteByte('\n')
	}
	return buf.String()
}

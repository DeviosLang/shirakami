package tool

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// RipgrepTool searches file contents using the system rg (ripgrep) command.
type RipgrepTool struct {
	// WorkspaceDir is the root workspace containing all repositories.
	// Searches are restricted to this directory tree to prevent path traversal.
	WorkspaceDir string
}

// NewRipgrepTool creates a RipgrepTool constrained to workspaceDir.
func NewRipgrepTool(workspaceDir string) *RipgrepTool {
	return &RipgrepTool{WorkspaceDir: workspaceDir}
}

func (t *RipgrepTool) Name() string { return "ripgrep" }

func (t *RipgrepTool) Description() string {
	return "Search file contents using ripgrep. Use 'repo' to restrict search to a specific repository directory (e.g. 'vstation_compute'). Returns matching lines in 'repo/file:line:content' format."
}

func (t *RipgrepTool) InputSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"pattern": map[string]interface{}{
				"type":        "string",
				"description": "Regular expression pattern to search for",
			},
			"repo": map[string]interface{}{
				"type":        "string",
				"description": "Repository name to restrict search to (e.g. 'vstation_compute', 'vstation_api'). Omit to search all repositories.",
			},
			"glob": map[string]interface{}{
				"type":        "string",
				"description": "File glob filter (e.g. **/*.py). Optional.",
			},
			"max_results": map[string]interface{}{
				"type":        "integer",
				"description": "Maximum number of matching lines to return. Defaults to 100.",
			},
		},
		"required": []string{"pattern"},
	}
}

// ripgrepInput is the parsed input for the ripgrep tool.
type ripgrepInput struct {
	Pattern    string `json:"pattern"`
	Repo       string `json:"repo"`
	Glob       string `json:"glob"`
	MaxResults int    `json:"max_results"`
}

// ripgrepJSONMatch represents a single match from rg --json output.
type ripgrepJSONMatch struct {
	Type string `json:"type"`
	Data struct {
		Path struct {
			Text string `json:"text"`
		} `json:"path"`
		Lines struct {
			Text string `json:"text"`
		} `json:"lines"`
		LineNumber  int `json:"line_number"`
		Submatches []struct {
			Match struct {
				Text string `json:"text"`
			} `json:"match"`
		} `json:"submatches"`
	} `json:"data"`
}

func (t *RipgrepTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var inp ripgrepInput
	if err := json.Unmarshal(input, &inp); err != nil {
		return "", fmt.Errorf("ripgrep: invalid input: %w", err)
	}
	if inp.Pattern == "" {
		return "", fmt.Errorf("ripgrep: pattern is required")
	}
	maxResults := inp.MaxResults
	if maxResults <= 0 {
		maxResults = 100
	}

	// Determine search directory: workspace root or a specific repo subdir.
	searchDir := t.WorkspaceDir
	if searchDir == "" {
		searchDir = "."
	}
	if inp.Repo != "" {
		searchDir = filepath.Join(searchDir, filepath.Clean(inp.Repo))
	}

	// Prevent path traversal: ensure searchDir is within WorkspaceDir.
	absWorkspace, err := filepath.Abs(t.WorkspaceDir)
	if err != nil {
		return "", fmt.Errorf("ripgrep: cannot resolve workspace dir: %w", err)
	}
	absDir, err := filepath.Abs(searchDir)
	if err != nil {
		return "", fmt.Errorf("ripgrep: cannot resolve search dir: %w", err)
	}
	if !strings.HasPrefix(absDir, absWorkspace) {
		return "", fmt.Errorf("ripgrep: search dir %q is outside workspace", inp.Repo)
	}

	// Build rg arguments
	args := []string{
		"--json",
		"--max-count", fmt.Sprintf("%d", maxResults),
	}
	if inp.Glob != "" {
		args = append(args, "--glob", inp.Glob)
	}
	if strings.HasPrefix(inp.Pattern, "-") {
		args = append(args, "-e", inp.Pattern)
	} else {
		args = append(args, inp.Pattern)
	}
	args = append(args, absDir)

	cmd := exec.CommandContext(ctx, "rg", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// rg exits with code 1 when no matches found — that's not an error
	_ = cmd.Run()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}

	// Parse JSON output
	var results []string
	scanner := bufio.NewScanner(&stdout)
	count := 0
	for scanner.Scan() && count < maxResults {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var match ripgrepJSONMatch
		if err := json.Unmarshal([]byte(line), &match); err != nil {
			continue
		}
		if match.Type != "match" {
			continue
		}
		// Normalize path relative to workspace root (includes repo name as prefix).
		filePath := match.Data.Path.Text
		if rel, err := filepath.Rel(absWorkspace, filePath); err == nil {
			filePath = rel
		}
		lineNum := match.Data.LineNumber
		content := strings.TrimRight(match.Data.Lines.Text, "\n\r")
		results = append(results, fmt.Sprintf("%s:%d:%s", filePath, lineNum, content))
		count++
	}

	if len(results) == 0 {
		return "No matches found.", nil
	}
	return strings.Join(results, "\n"), nil
}

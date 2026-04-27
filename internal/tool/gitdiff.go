package tool

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// ChangedFunction represents a function that was added or modified in a diff.
type ChangedFunction struct {
	File     string `json:"file"`
	Line     int    `json:"line"`
	FuncName string `json:"func_name"`
	ChangeType string `json:"change_type"` // "added" or "modified"
}

// GitDiffTool parses unified diff format and extracts changed function information.
type GitDiffTool struct{}

func NewGitDiffTool() *GitDiffTool { return &GitDiffTool{} }

func (t *GitDiffTool) Name() string { return "gitdiff" }

func (t *GitDiffTool) Description() string {
	return "Parse a unified diff and extract the list of added/modified functions. Returns file path, line number, and function name."
}

func (t *GitDiffTool) InputSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"diff": map[string]interface{}{
				"type":        "string",
				"description": "Unified diff content (output of git diff or diff -u)",
			},
		},
		"required": []string{"diff"},
	}
}

type gitdiffInput struct {
	Diff string `json:"diff"`
}

// goFuncPattern matches Go function and method declarations.
var goFuncPattern = regexp.MustCompile(`^[+](?:func\s+(?:\([^)]+\)\s+)?(\w+)\s*\()`)

// genericFuncPatterns covers common languages (Python, Java, TypeScript/JavaScript, C/C++).
var genericFuncPatterns = []*regexp.Regexp{
	// Python: def funcname(
	regexp.MustCompile(`^[+]def\s+(\w+)\s*\(`),
	// JS/TS: function funcname( or async function funcname(
	regexp.MustCompile(`^[+](?:async\s+)?function\s+(\w+)\s*\(`),
	// JS/TS: const/let/var funcname = (... =>  or function
	regexp.MustCompile(`^[+](?:export\s+)?(?:const|let|var)\s+(\w+)\s*=\s*(?:async\s*)?\(`),
	// Java/C#/C++: type funcname(
	regexp.MustCompile(`^[+]\s*(?:public|private|protected|static|virtual|override|inline)?\s*\w[\w<>*&\[\]]*\s+(\w+)\s*\(`),
	// Go method/function (already above, fallback)
	goFuncPattern,
}

func (t *GitDiffTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var inp gitdiffInput
	if err := json.Unmarshal(input, &inp); err != nil {
		return "", fmt.Errorf("gitdiff: invalid input: %w", err)
	}
	if inp.Diff == "" {
		return "", fmt.Errorf("gitdiff: diff is required")
	}

	funcs := parseDiff(inp.Diff)
	if len(funcs) == 0 {
		return "No changed functions found.", nil
	}

	out, err := json.MarshalIndent(funcs, "", "  ")
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// parseDiff parses a unified diff and returns changed functions.
func parseDiff(diff string) []ChangedFunction {
	var results []ChangedFunction
	scanner := bufio.NewScanner(strings.NewReader(diff))

	currentFile := ""
	currentNewLine := 0

	for scanner.Scan() {
		line := scanner.Text()

		// New file header: +++ b/path/to/file
		if strings.HasPrefix(line, "+++ ") {
			path := strings.TrimPrefix(line, "+++ ")
			// Strip leading "b/" prefix from git diff
			path = strings.TrimPrefix(path, "b/")
			// Handle /dev/null (deleted files)
			if path == "/dev/null" {
				currentFile = ""
			} else {
				currentFile = path
			}
			currentNewLine = 0
			continue
		}

		// Hunk header: @@ -oldstart,oldcount +newstart,newcount @@
		if strings.HasPrefix(line, "@@") {
			newStart := parseHunkNewStart(line)
			if newStart > 0 {
				currentNewLine = newStart - 1 // will be incremented on first +/context line
			}
			continue
		}

		// Skip --- lines (old file header)
		if strings.HasPrefix(line, "--- ") {
			continue
		}

		// Added line
		if strings.HasPrefix(line, "+") {
			currentNewLine++
			if currentFile == "" {
				continue
			}
			// Check if this line contains a function declaration
			funcName := extractFuncName(line)
			if funcName != "" {
				results = append(results, ChangedFunction{
					File:       currentFile,
					Line:       currentNewLine,
					FuncName:   funcName,
					ChangeType: "added",
				})
			}
			continue
		}

		// Removed line: doesn't advance new-file line counter
		if strings.HasPrefix(line, "-") {
			continue
		}

		// Context line: advance new line counter
		if !strings.HasPrefix(line, "diff ") && !strings.HasPrefix(line, "index ") &&
			!strings.HasPrefix(line, "new file") && !strings.HasPrefix(line, "deleted file") {
			currentNewLine++
		}
	}

	return results
}

// parseHunkNewStart extracts the new-file start line number from a hunk header.
// Format: @@ -old_start[,old_count] +new_start[,new_count] @@
func parseHunkNewStart(line string) int {
	// Find the +N part
	start := strings.Index(line, "+")
	if start < 0 {
		return 0
	}
	rest := line[start+1:]
	// Read digits until , or space
	end := strings.IndexAny(rest, ", \t@")
	if end < 0 {
		end = len(rest)
	}
	numStr := rest[:end]
	var n int
	fmt.Sscanf(numStr, "%d", &n)
	return n
}

// extractFuncName returns the function name from an added diff line, or empty string.
func extractFuncName(line string) string {
	// Try Go pattern first (most specific)
	if m := goFuncPattern.FindStringSubmatch(line); len(m) > 1 {
		return m[1]
	}
	// Try generic patterns
	for _, pat := range genericFuncPatterns {
		if m := pat.FindStringSubmatch(line); len(m) > 1 {
			return m[1]
		}
	}
	return ""
}

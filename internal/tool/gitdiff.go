package tool

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// classContextRE matches a Python class definition line used as @@ hunk context,
// e.g. "class CDfwUpdateVmNetType(CDfwOp):" or "class Foo:".
// When the @@ context is a class rather than a function, we need to scan
// the hunk's context lines to find the enclosing method (def xxx).
var classContextRE = regexp.MustCompile(`^\s*class\s+(\w+)`)

// contextDefRE extracts a method/function name from a context line (no +/- prefix).
// Matches Python "def name(" and similar patterns.
var contextDefRE = regexp.MustCompile(`^\s+(?:async\s+)?def\s+(\w+)\s*\(`)

// isClassOnlyContext returns true when the @@ context string looks like a class
// definition rather than a function/method definition. Used to trigger
// method-context scanning in the diff body.
func isClassOnlyContext(ctx string) bool {
	return classContextRE.MatchString(ctx)
}

// DiffHunk represents a contiguous block of changed lines in one file.
// Used by the deterministic diff parser (Layer A) to map diff hunks to
// symbol definitions without LLM involvement.
type DiffHunk struct {
	File        string // file path (repo-relative, from +++ b/path)
	StartLine   int    // first changed line number (new side)
	EndLine     int    // last changed line number (new side)
	FuncContext string // enclosing function name from @@ line context (may be empty)
}

// ParseDiffHunks extracts all changed-line ranges from a unified diff.
// Pure text parsing — no external dependencies, no LLM, no DB.
// Each returned DiffHunk covers one contiguous block of added/modified lines
// within a single file.
//
// This is Layer A of the DiffToSymbols pipeline (see architecture-v2-design §3.4):
//   - Layer A (this function): pure diff parsing, always available
//   - Layer B (DiffToSymbols):  line→symbol mapping, requires index
func ParseDiffHunks(diff string) []DiffHunk {
	var hunks []DiffHunk
	scanner := bufio.NewScanner(strings.NewReader(diff))

	currentFile := ""
	currentNewLine := 0
	currentFuncCtx := "" // FuncContext extracted from most recent @@ line
	hunkStart := 0       // start of current contiguous changed block
	hunkEnd := 0         // end of current contiguous changed block
	hunkFuncCtx := ""    // FuncContext for current contiguous block
	inChange := false    // tracking a contiguous block of + lines

	// currentContextMethod tracks the most recently seen "def xxx" in hunk
	// context lines. Used to resolve the enclosing method when the @@ context
	// is a class name (Python template-method / multi-level inheritance pattern).
	currentContextMethod := ""
	// hunkCtxIsClass is true when the current @@ context string looks like a
	// class definition rather than a function/method definition.
	hunkCtxIsClass := false

	flushHunk := func() {
		if inChange && currentFile != "" && hunkStart > 0 {
			hunks = append(hunks, DiffHunk{
				File:        currentFile,
				StartLine:   hunkStart,
				EndLine:     hunkEnd,
				FuncContext: hunkFuncCtx,
			})
		}
		inChange = false
		hunkStart = 0
		hunkEnd = 0
		hunkFuncCtx = ""
	}

	for scanner.Scan() {
		line := scanner.Text()

		// New file header: +++ b/path/to/file
		if strings.HasPrefix(line, "+++ ") {
			flushHunk()
			path := strings.TrimPrefix(line, "+++ ")
			path = strings.TrimPrefix(path, "b/")
			if path == "/dev/null" {
				currentFile = ""
			} else {
				currentFile = path
			}
			currentNewLine = 0
			currentFuncCtx = ""
			currentContextMethod = ""
			hunkCtxIsClass = false
			continue
		}

		// Hunk header: @@ -oldstart,oldcount +newstart,newcount @@ [func context]
		if strings.HasPrefix(line, "@@") {
			flushHunk()
			newStart := parseHunkNewStart(line)
			if newStart > 0 {
				currentNewLine = newStart - 1
			}
			currentFuncCtx = parseHunkFuncContext(line)
			currentContextMethod = ""
			hunkCtxIsClass = isClassOnlyContext(currentFuncCtx)
			continue
		}

		// Skip --- lines, diff headers, index lines
		if strings.HasPrefix(line, "--- ") ||
			strings.HasPrefix(line, "diff ") ||
			strings.HasPrefix(line, "index ") ||
			strings.HasPrefix(line, "new file") ||
			strings.HasPrefix(line, "deleted file") {
			continue
		}

		// Added line: part of the change
		if strings.HasPrefix(line, "+") {
			currentNewLine++
			if currentFile == "" {
				continue
			}
			if !inChange {
				inChange = true
				hunkStart = currentNewLine
				// Resolve effective function context:
				// When @@ context is a class and we've seen a "def xxx" in
				// preceding context lines, use "ClassName.method_name".
				if hunkCtxIsClass && currentContextMethod != "" {
					hunkFuncCtx = currentFuncCtx + "." + currentContextMethod
				} else {
					hunkFuncCtx = currentFuncCtx
				}
			}
			hunkEnd = currentNewLine
			continue
		}

		// Removed line: does not advance new-file line counter
		// but breaks a contiguous + block
		if strings.HasPrefix(line, "-") {
			// Don't flush — interleaved -/+ lines are part of the same logical change
			continue
		}

		// Context line: breaks a contiguous change block, advances line counter.
		// When @@ context is a class, scan for the nearest enclosing "def xxx"
		// so that subsequent added lines can resolve "ClassName.method_name".
		flushHunk()
		currentNewLine++
		if hunkCtxIsClass {
			if m := contextDefRE.FindStringSubmatch(line); len(m) > 1 {
				currentContextMethod = m[1]
			}
		}
	}

	// Flush any trailing hunk
	flushHunk()

	return hunks
}

// ChangedFunction represents a function that was added or modified in a diff.
type ChangedFunction struct {
	File       string `json:"file"`
	Line       int    `json:"line"`
	FuncName   string `json:"func_name"`
	ChangeType string `json:"change_type"` // "added" or "modified"
}

// ParseDiffFunctions extracts changed function names from a unified diff.
// Unlike ParseDiffHunks (Layer A, line ranges only), this function also
// detects function declarations in added lines using language-specific regex.
// Used by golden tests for function-name recall validation.
func ParseDiffFunctions(diff string) []ChangedFunction {
	return parseDiff(diff)
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
// Two detection strategies are combined:
//  1. Explicit function declarations in added (+) lines (ChangeType: "added").
//  2. @@ hunk header function context — when git records the enclosing function
//     name after the closing @@, that function is marked as "modified". This
//     handles the common case where only internal statements are changed (no new
//     `def`/`func` line appears) yet the enclosing function is clearly modified.
func parseDiff(diff string) []ChangedFunction {
	var results []ChangedFunction
	scanner := bufio.NewScanner(strings.NewReader(diff))

	currentFile := ""
	currentNewLine := 0
	currentHunkFuncCtx := "" // function name from current @@ line
	hunkFuncEmitted := false // whether we've already emitted a "modified" entry for this hunk

	// currentContextMethod tracks the most recently seen "def xxx" line in hunk
	// context lines (lines without +/- prefix). Used to resolve the enclosing
	// method when the @@ context is a class name (template-method / multi-level
	// inheritance pattern, common in Python).
	currentContextMethod := ""
	// hunkCtxIsClass is true when the current @@ context string looks like a
	// class definition rather than a function/method definition.
	hunkCtxIsClass := false

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
			currentHunkFuncCtx = ""
			hunkFuncEmitted = false
			currentContextMethod = ""
			hunkCtxIsClass = false
			continue
		}

		// Hunk header: @@ -oldstart,oldcount +newstart,newcount @@ [func context]
		if strings.HasPrefix(line, "@@") {
			newStart := parseHunkNewStart(line)
			if newStart > 0 {
				currentNewLine = newStart - 1 // will be incremented on first +/context line
			}
			currentHunkFuncCtx = parseHunkFuncContext(line)
			hunkFuncEmitted = false
			currentContextMethod = ""
			hunkCtxIsClass = isClassOnlyContext(currentHunkFuncCtx)
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

			// Resolve the effective function context:
			// - When @@ context is a class and we've seen a "def xxx" in context
			//   lines, use "ClassName.method_name" to be precise.
			// - Otherwise fall back to the raw @@ context name.
			effectiveCtx := currentHunkFuncCtx
			if hunkCtxIsClass && currentContextMethod != "" {
				effectiveCtx = currentHunkFuncCtx + "." + currentContextMethod
			}

			// Strategy 2: emit the @@ context function as "modified" on first
			// added line in the hunk (if no explicit declaration found yet).
			// We defer to strategy 1 if this added line itself is a declaration.
			funcName := extractFuncName(line)
			if funcName != "" {
				// Explicit declaration found — emit "added" and suppress "modified" duplicate.
				results = append(results, ChangedFunction{
					File:       currentFile,
					Line:       currentNewLine,
					FuncName:   funcName,
					ChangeType: "added",
				})
				hunkFuncEmitted = true // declaration supersedes the @@ context entry
			} else if !hunkFuncEmitted && effectiveCtx != "" {
				// No declaration on this line; emit the resolved context as "modified".
				results = append(results, ChangedFunction{
					File:       currentFile,
					Line:       currentNewLine,
					FuncName:   effectiveCtx,
					ChangeType: "modified",
				})
				hunkFuncEmitted = true
			}
			continue
		}

		// Removed line: doesn't advance new-file line counter
		if strings.HasPrefix(line, "-") {
			continue
		}

		// Context line: advance new line counter, and track enclosing def for
		// class-context resolution.
		if !strings.HasPrefix(line, "diff ") && !strings.HasPrefix(line, "index ") &&
			!strings.HasPrefix(line, "new file") && !strings.HasPrefix(line, "deleted file") {
			currentNewLine++
			// When the hunk @@ context is a class, scan context lines for the
			// nearest enclosing method definition ("def xxx").
			// This resolves the Class.method name even when only internal
			// statements are changed (no +def line appears in the hunk).
			if hunkCtxIsClass && !hunkFuncEmitted {
				if m := contextDefRE.FindStringSubmatch(line); len(m) > 1 {
					currentContextMethod = m[1]
				}
			}
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

// hunkFuncContextRE matches the function context that git appends after the
// closing @@ of a hunk header, e.g.:
//
//	@@ -209,10 +209,10 @@ def get_data(self):
//	                       ^^^^^^^^^^^^^^^^^^^^^^^^^^
//
// The regex captures everything after "@@ " that is not blank.
var hunkFuncContextRE = regexp.MustCompile(`@@[^@]*@@\s+(\S.*)$`)

// parseHunkFuncContext extracts the enclosing function name from a @@ hunk header.
// git diff automatically appends the nearest enclosing function signature after
// the closing @@ delimiter. We strip common language decorators to return just
// the bare function name.
//
// Examples:
//
//	"@@ -1,3 +1,3 @@ def get_data(self):"          → "get_data"
//	"@@ -10,5 +10,5 @@ func (r *Router) Handle("     → "Handle"
//	"@@ -5,4 +5,4 @@ public void processRequest("    → "processRequest"
//	"@@ -1,2 +1,2 @@"                                → ""
func parseHunkFuncContext(line string) string {
	m := hunkFuncContextRE.FindStringSubmatch(line)
	if m == nil {
		return ""
	}
	ctx := strings.TrimSpace(m[1])
	if ctx == "" {
		return ""
	}
	return extractBareFunc(ctx)
}

// extractBareFunc strips language-specific prefixes/decorators and returns
// just the bare function name from a @@ context string.
func extractBareFunc(ctx string) string {
	// Patterns to extract function name from common language signatures:
	// Python:  "def funcname(" or "async def funcname("
	// Go:      "func funcname(" or "func (r *T) funcname("
	// Java/C#: "public void funcname(" etc.
	// JS/TS:   "function funcname(" or "const funcname = ("
	for _, pat := range []*regexp.Regexp{
		// Python: def funcname( or async def funcname(
		regexp.MustCompile(`(?:async\s+)?def\s+(\w+)\s*\(`),
		// Go: func (receiver) funcname( or func funcname(
		regexp.MustCompile(`func\s+(?:\([^)]+\)\s+)?(\w+)\s*\(`),
		// JS/TS: function funcname( or async function funcname(
		regexp.MustCompile(`(?:async\s+)?function\s+(\w+)\s*\(`),
		// JS/TS arrow: const/let/var funcname = (
		regexp.MustCompile(`(?:const|let|var)\s+(\w+)\s*=`),
		// Java/C#/C++: ... returnType funcname(
		regexp.MustCompile(`\b(\w+)\s*\(`),
	} {
		if m := pat.FindStringSubmatch(ctx); len(m) > 1 {
			name := m[1]
			// Filter out language keywords that could match
			switch name {
			case "if", "for", "while", "switch", "return", "new", "class", "interface", "struct", "enum":
				continue
			}
			return name
		}
	}
	return ""
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

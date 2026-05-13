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
// Group 1: class name. Group 2 (optional): parent class name inside parentheses.
var classContextRE = regexp.MustCompile(`^\s*class\s+(\w+)(?:\((\w+)\))?`)

// contextDefRE extracts a method/function name from a context line (no +/- prefix).
// Matches Python "def name(" and similar patterns.
var contextDefRE = regexp.MustCompile(`^\s+(?:async\s+)?def\s+(\w+)\s*\(`)

// isClassOnlyContext returns true when the @@ context string looks like a class
// definition rather than a function/method definition. Used to trigger
// method-context scanning in the diff body.
func isClassOnlyContext(ctx string) bool {
	return classContextRE.MatchString(ctx)
}

// FuncHierarchy holds the innermost and outermost enclosing function
// discovered by scanning backward from a changed line.
// When the changed line is inside a nested function, Inner is the nested
// function name (e.g. "worker") and Outer is the module-level function that
// contains it (e.g. "concurrency_worker_executor").
// When the changed line is directly inside a module-level function, Inner and
// Outer are the same value.
// Both fields are empty when no enclosing function is found.
type FuncHierarchy struct {
	Inner string // nearest enclosing function (may be indented/nested)
	Outer string // outermost enclosing function at indent=0 (same as Inner when not nested)
}

// ResolveFuncHierarchy scans backward from startLine (1-based) in lines to
// find both the innermost and outermost enclosing function definition.
//
// Algorithm:
//  1. Scan upward from startLine; on the first function def found, record as
//     Inner and note its indentation depth.
//  2. Validate that the candidate function actually encloses startLine: scan
//     forward from the def line to startLine and verify no same-or-lower
//     indented non-blank non-comment line terminates the function body before
//     reaching startLine. If the function ended before startLine, discard it
//     (the changed line is at module scope, not inside any function).
//  3. If Inner has indentation > 0 (it is a nested/inner function), continue
//     scanning upward to find the first def with indentation == 0, which is
//     the Outer function.
//  4. If Inner has indentation == 0, Outer = Inner (already at module level).
//
// lines must contain raw file content (no diff +/- prefix).
// maxScan is the maximum number of lines to scan upward.
func ResolveFuncHierarchy(lines []string, startLine, maxScan int) FuncHierarchy {
	if startLine < 1 {
		startLine = 1
	}
	idx := startLine - 1 // convert to 0-based
	if idx >= len(lines) {
		idx = len(lines) - 1
	}
	limit := idx - maxScan
	if limit < 0 {
		limit = 0
	}

	var h FuncHierarchy
	innerIndent := -1 // indentation of the Inner function line (-1 = not found yet)

	for i := idx; i >= limit; i-- {
		line := lines[i]
		for _, re := range sourceFuncDefREs {
			if m := re.FindStringSubmatch(line); len(m) > 1 {
				name := m[1]
				indent := len(line) - len(strings.TrimLeft(line, " \t"))

				if h.Inner == "" {
					// First (innermost) candidate — validate it actually encloses startLine.
					// Scan forward from the def line (i) to idx; if we encounter a
					// non-blank, non-comment line with indentation <= indent that is NOT
					// the def line itself and is NOT a continuation of the function body
					// (i.e. it starts at the same or lower indentation as the def), the
					// function ended before startLine so this def does not enclose it.
					if funcEndsBeforeIdx(lines, i, idx, indent) {
						// This def does not enclose startLine — it came before it at
						// module level. The changed line is at module scope with no
						// enclosing function.
						return FuncHierarchy{}
					}
					h.Inner = name
					innerIndent = indent
					if indent == 0 {
						// Already at module level — no outer to find
						h.Outer = name
						return h
					}
					// Inner is nested; continue searching for Outer
				} else {
					// Already found Inner; looking for the first unindented def
					if indent == 0 {
						h.Outer = name
						return h
					}
					// Still indented; keep scanning (could be a deeper outer)
					// but only accept if this def's indent < innerIndent,
					// which means it is a closer enclosing scope.
					if indent < innerIndent {
						innerIndent = indent // update to closer enclosing level
					}
				}
				break // only one pattern can match per line
			}
		}
	}

	// If we found Inner but never found an unindented Outer, use Inner as Outer.
	if h.Inner != "" && h.Outer == "" {
		h.Outer = h.Inner
	}
	return h
}

// funcEndsBeforeIdx returns true when the function defined at defIdx (0-based)
// with bodyIndent (the def line's indentation) ends before targetIdx.
//
// A Python/Go function body ends when a non-blank, non-comment line appears
// at indentation <= bodyIndent AND that line is not the def line itself.
// We scan forward from defIdx+1 to targetIdx-1; if any such line is found,
// the function body terminated before targetIdx.
func funcEndsBeforeIdx(lines []string, defIdx, targetIdx, bodyIndent int) bool {
	for j := defIdx + 1; j < targetIdx; j++ {
		raw := lines[j]
		trimmed := strings.TrimLeft(raw, " \t")
		if trimmed == "" {
			continue // blank line: function body continues (Python blank lines are fine)
		}
		// Single-line comments: Python '#', Go '//', C-style '/*'
		if strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "/*") {
			continue
		}
		lineIndent := len(raw) - len(trimmed)
		if lineIndent <= bodyIndent {
			// A real statement at the same or lower indentation than the def —
			// the function has ended.
			return true
		}
	}
	return false
}

// DiffHunk represents a contiguous block of changed lines in one file.
// Used by the deterministic diff parser (Layer A) to map diff hunks to
// symbol definitions without LLM involvement.
type DiffHunk struct {
	File        string // file path (repo-relative, from +++ b/path)
	StartLine   int    // first changed line number (new side)
	EndLine     int    // last changed line number (new side)
	FuncContext string // enclosing function name from @@ line context (may be empty)
	// OuterFuncName holds the outermost (module-level) enclosing function when
	// FuncContext points to a nested inner function. Set by the Orchestrator's
	// Layer A+ disk fallback (resolveFuncHierarchyFromDisk). Empty when
	// FuncContext is already a top-level function or when disk scan is unavailable.
	OuterFuncName string
	// RawLines holds the +/- body lines of the hunk (e.g. "+x = 1", "-x = 0").
	// Used to inject diff snippets into the Worker prompt so the LLM understands
	// what changed, not just where. Limited to 40 lines to keep token usage bounded.
	RawLines []string
	// GlobalVar holds the name of a module-level variable definition detected
	// in the changed lines (e.g. "TIMEOUT = 30" → GlobalVar = "TIMEOUT").
	// When set, the Orchestrator emits a FILE_CHANGED_VAR sentinel so the Worker
	// can search specifically for usage sites of that variable.
	GlobalVar string
	// ClassName holds the class name when the @@ hunk context is a class definition
	// (e.g. "class CDfwUpdateVmNetType(CDfwOp):"). Always populated when the hunk
	// context looks like a class — used together with ParentClass for 6-P2 hints.
	ClassName string
	// ParentClass holds the parent class name from "class Child(Parent):" syntax.
	// Empty when the class has no explicit parent or the context is not a class.
	ParentClass string
}

// globalVarDefRE matches module-level variable assignments in Python, Go, and JS/TS.
// These are NOT function definitions, but their usage sites matter for call-chain
// analysis (e.g. a changed timeout constant affects all callers that reference it).
//
// Key design: the pattern requires the variable name to start immediately after the
// '+' diff prefix (no spaces), which means it only matches TOP-LEVEL (module-scope)
// assignments. Indented assignments (inside functions or classes) will have leading
// spaces after '+', so they won't match this pattern.
//
// Examples that DO match (module-level):
//   +WHITE_SET = {...}           (Python constant)
//   +white_set = {...}           (Python module-level set)
//   +timeout = 30                (Python/Go module-level variable)
//   +var MaxRetry = 10           (Go package-level var)
//   +const DefaultTimeout = 30   (Go/JS constant)
//
// Examples that do NOT match (indented, i.e. inside functions/classes):
//   +    timeout = 20            (Python/Go local variable — leading spaces after '+')
//   +  self.white_set = {...}    (Python attribute assignment — leading spaces)
//
// Group 1 captures the variable name.
var globalVarDefRE = regexp.MustCompile(`^[+](?:(?:var|const|let)\s+)?([A-Za-z_]\w*)\s*(?::=|=)[^=]`)

// sourceFuncDefREs holds language-specific patterns for finding function/method
// definitions when scanning backward from a changed line (改进 1: Layer A+ disk fallback).
// Each pattern must have at least one capture group — group 1 is the function name.
// Patterns are applied to lines without the diff +/- prefix (raw file content).
var sourceFuncDefREs = []*regexp.Regexp{
	// Python: def funcname( or async def funcname(
	regexp.MustCompile(`^\s*(?:async\s+)?def\s+(\w+)\s*\(`),
	// Go: func FuncName( or func (r *T) FuncName(
	regexp.MustCompile(`^func\s+(?:\([^)]+\)\s+)?(\w+)\s*\(`),
	// Java/Kotlin/C#/C++: access-modifier returnType funcName(
	regexp.MustCompile(`^\s*(?:public|private|protected|static|virtual|override|final|async|abstract)\s+[\w<>\[\]]+\s+(\w+)\s*\(`),
	// JS/TS: function funcName( or async function funcName(
	regexp.MustCompile(`^\s*(?:export\s+)?(?:async\s+)?function\s+(\w+)\s*\(`),
	// JS/TS: const/let/var funcName = (args) => or = function(
	regexp.MustCompile(`^\s*(?:export\s+)?(?:const|let|var)\s+(\w+)\s*=\s*(?:async\s*)?\(`),
	// Ruby: def funcname
	regexp.MustCompile(`^\s*def\s+(\w+)`),
	// Rust: fn funcname(
	regexp.MustCompile(`^\s*(?:pub\s+)?(?:async\s+)?fn\s+(\w+)\s*\(`),
}

// ResolveFuncAtLine scans backward from startLine (1-based) in lines to find the
// enclosing function name using language-specific regex patterns.
// It stops at the first match or after scanning maxScan lines.
// Returns the function name, or "" if none found.
// lines must contain the raw file content (no diff +/- prefix).
func ResolveFuncAtLine(lines []string, startLine, maxScan int) string {
	if startLine < 1 {
		startLine = 1
	}
	idx := startLine - 1 // convert to 0-based index
	if idx >= len(lines) {
		idx = len(lines) - 1
	}
	limit := idx - maxScan
	if limit < 0 {
		limit = 0
	}
	for i := idx; i >= limit; i-- {
		line := lines[i]
		for _, re := range sourceFuncDefREs {
			if m := re.FindStringSubmatch(line); len(m) > 1 {
				return m[1]
			}
		}
	}
	return ""
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
	// currentClassName and currentParentClass hold the class/parent from the @@ context.
	currentClassName := ""
	currentParentClass := ""

	// rawLinesBuf accumulates +/- lines for the current contiguous block.
	// Capped at maxRawLines to limit token cost when injected into prompts.
	const maxRawLines = 40
	rawLinesBuf := make([]string, 0, 8)
	// currentGlobalVar is the module-level variable name detected in the changed lines.
	currentGlobalVar := ""

	flushHunk := func() {
		if inChange && currentFile != "" && hunkStart > 0 {
			h := DiffHunk{
				File:        currentFile,
				StartLine:   hunkStart,
				EndLine:     hunkEnd,
				FuncContext: hunkFuncCtx,
				ClassName:   currentClassName,
				ParentClass: currentParentClass,
				GlobalVar:   currentGlobalVar,
			}
			if len(rawLinesBuf) > 0 {
				cp := make([]string, len(rawLinesBuf))
				copy(cp, rawLinesBuf)
				h.RawLines = cp
			}
			hunks = append(hunks, h)
		}
		inChange = false
		hunkStart = 0
		hunkEnd = 0
		hunkFuncCtx = ""
		rawLinesBuf = rawLinesBuf[:0]
		currentGlobalVar = ""
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
			currentClassName = ""
			currentParentClass = ""
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
			// Use the raw (unstripped) context string for class detection so that
			// "class Foo(Bar):" syntax is preserved for classContextRE to match.
			rawCtx := parseHunkRawContext(line)
			hunkCtxIsClass = isClassOnlyContext(rawCtx)
			// Extract ClassName and ParentClass from the @@ context when it is a class.
			if hunkCtxIsClass {
				if m := classContextRE.FindStringSubmatch(rawCtx); len(m) > 1 {
					currentClassName = m[1]
					if len(m) > 2 {
						currentParentClass = m[2]
					} else {
						currentParentClass = ""
					}
				}
			} else {
				currentClassName = ""
				currentParentClass = ""
			}
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
			// Accumulate raw line (capped).
			if len(rawLinesBuf) < maxRawLines {
				rawLinesBuf = append(rawLinesBuf, line)
			}
			// Detect module-level global variable assignment in added lines.
			// globalVarDefRE only matches unindented assignments (no leading whitespace
			// after the '+' prefix), so it is inherently restricted to module-scope.
			//
			// We intentionally run this check even when hunkFuncCtx is non-empty.
			// Git's @@ hunk context heuristic uses the *nearest* enclosing "def"
			// above the changed lines, which can be a function that ended before the
			// module-level variable — a false attribution. When the changed line itself
			// is unindented, the git context is wrong and we override it:
			//   - Set currentGlobalVar to the variable name.
			//   - Clear hunkFuncCtx so the Orchestrator emits a FILE_CHANGED_VAR
			//     sentinel (variable usage search) instead of a function-caller search.
			if currentGlobalVar == "" {
				if m := globalVarDefRE.FindStringSubmatch(line); len(m) > 1 {
					currentGlobalVar = m[1]
					// Override any misattributed function context from the @@ hunk header.
					// Module-level assignment cannot be inside the attributed function.
					hunkFuncCtx = ""
				}
			}
			continue
		}

		// Removed line: does not advance new-file line counter
		// but breaks a contiguous + block
		if strings.HasPrefix(line, "-") {
			// Accumulate removed lines in raw buf too (for context).
			if inChange && len(rawLinesBuf) < maxRawLines {
				rawLinesBuf = append(rawLinesBuf, line)
			}
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
			rawCtx := parseHunkRawContext(line)
			hunkCtxIsClass = isClassOnlyContext(rawCtx)
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

// parseHunkRawContext returns the raw (unprocessed) context string after the
// closing @@ delimiter, without stripping language decorators. Used for class
// detection which requires the full "class Foo(Bar):" syntax to be intact.
func parseHunkRawContext(line string) string {
	m := hunkFuncContextRE.FindStringSubmatch(line)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[1])
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

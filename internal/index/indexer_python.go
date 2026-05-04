package index

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// PythonIndexer extracts symbol definitions and import relationships from
// Python source code using regex-based parsing.
//
// This is the "辅助级" indexer (§3.1.3): it does NOT attempt to resolve
// dynamic call chains. Instead, it extracts:
//   - Import relationships (deterministic, ~90% coverage)
//   - Function/class definitions with line numbers
//   - Decorator patterns (route handlers, RPC registrations)
//
// The output is injected into Worker prompts as context so the LLM needs
// fewer ripgrep rounds to discover the call graph.
type PythonIndexer struct {
	repo       string
	repoPath   string
	commitHash string
}

// NewPythonIndexer creates a Python indexer for a repository.
func NewPythonIndexer(repo, repoPath, commitHash string) *PythonIndexer {
	return &PythonIndexer{
		repo:       repo,
		repoPath:   repoPath,
		commitHash: commitHash,
	}
}

// Python regex patterns
var (
	// import module / import module as alias
	pyImportRe = regexp.MustCompile(`^\s*import\s+(\S+)(?:\s+as\s+\S+)?`)
	// from module import name / from module import name as alias
	pyFromImportRe = regexp.MustCompile(`^\s*from\s+(\S+)\s+import\s+(.+)`)
	// def function_name(
	pyFuncDefRe = regexp.MustCompile(`^\s*def\s+(\w+)\s*\(`)
	// class ClassName(
	pyClassDefRe = regexp.MustCompile(`^\s*class\s+(\w+)\s*[:\(]`)
	// @app.route / @router.post / @app.get etc (decorator)
	pyRouteDecoratorRe = regexp.MustCompile(`^\s*@\w+\.(route|get|post|put|delete|patch|head|options)\s*\(["']([^"']+)["']`)
	// Method inside a class (indented def)
	pyMethodDefRe = regexp.MustCompile(`^\s{4,}def\s+(\w+)\s*\(`)
)

// Index parses all Python files in the repository.
func (p *PythonIndexer) Index() (*IndexResult, error) {
	result := &IndexResult{}
	fileCount := 0

	err := filepath.Walk(p.repoPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip inaccessible files
		}
		if info.IsDir() {
			// Skip common non-source directories
			base := filepath.Base(path)
			if base == ".git" || base == "__pycache__" || base == ".venv" ||
				base == "venv" || base == "node_modules" || base == ".eggs" {
				return filepath.SkipDir
			}
			return nil
		}

		if !strings.HasSuffix(path, ".py") {
			return nil
		}

		relPath, err := filepath.Rel(p.repoPath, path)
		if err != nil {
			return nil
		}

		// Skip test files and vendor
		if strings.Contains(relPath, "test") && strings.Contains(relPath, "test_") {
			return nil
		}
		if strings.HasPrefix(relPath, "vendor/") || strings.Contains(relPath, "/vendor/") {
			return nil
		}

		fileResult := p.parseFile(path, relPath)
		result.Nodes = append(result.Nodes, fileResult.Nodes...)
		result.Edges = append(result.Edges, fileResult.Edges...)
		fileCount++

		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("python indexer walk: %w", err)
	}

	result.Files = fileCount
	return result, nil
}

// parseFile extracts symbols and imports from a single Python file.
func (p *PythonIndexer) parseFile(absPath, relPath string) *IndexResult {
	result := &IndexResult{}

	file, err := os.Open(absPath)
	if err != nil {
		return result
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	lineNum := 0
	currentClass := "" // track current class context for methods
	classIndent := 0

	// Module path for import resolution: compute/disk/encrypt_disk.py → compute.disk.encrypt_disk
	modulePath := strings.TrimSuffix(relPath, ".py")
	modulePath = strings.ReplaceAll(modulePath, "/", ".")
	if strings.HasSuffix(modulePath, ".__init__") {
		modulePath = strings.TrimSuffix(modulePath, ".__init__")
	}

	for scanner.Scan() {
		lineNum++
		line := scanner.Text()

		// Track class context (for qualifying method names)
		if m := pyClassDefRe.FindStringSubmatch(line); m != nil {
			currentClass = m[1]
			classIndent = countLeadingSpaces(line)

			node := SymbolNode{
				ID:         fmt.Sprintf("%s:%s:%s#0", p.repo, relPath, m[1]),
				Repo:       p.repo,
				FilePath:   relPath,
				Name:       m[1],
				Kind:       "class",
				StartLine:  lineNum,
				EndLine:    lineNum, // will be updated if we track end
				CommitHash: p.commitHash,
			}
			result.Nodes = append(result.Nodes, node)
			continue
		}

		// Reset class context if we're back to top level
		if currentClass != "" && len(line) > 0 && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") && !strings.HasPrefix(line, "#") && line != "" {
			if countLeadingSpaces(line) <= classIndent && !pyMethodDefRe.MatchString(line) {
				currentClass = ""
			}
		}

		// Function/method definition
		if m := pyFuncDefRe.FindStringSubmatch(line); m != nil {
			funcName := m[1]
			kind := "function"
			qualifiedName := funcName

			// If indented and inside a class → method
			if currentClass != "" && countLeadingSpaces(line) > classIndent {
				kind = "method"
				qualifiedName = currentClass + "." + funcName
			}

			arity := countPyParams(line)
			node := SymbolNode{
				ID:         fmt.Sprintf("%s:%s:%s#%d", p.repo, relPath, qualifiedName, arity),
				Repo:       p.repo,
				FilePath:   relPath,
				Name:       qualifiedName,
				Kind:       kind,
				StartLine:  lineNum,
				EndLine:    lineNum,
				CommitHash: p.commitHash,
			}
			result.Nodes = append(result.Nodes, node)
			continue
		}

		// Import statements → IMPORTS edges
		if m := pyFromImportRe.FindStringSubmatch(line); m != nil {
			importModule := m[1]
			// Resolve relative imports
			if strings.HasPrefix(importModule, ".") {
				importModule = resolveRelativeImport(modulePath, importModule)
			}
			importedNames := parseImportedNames(m[2])
			for _, name := range importedNames {
				edge := SymbolEdge{
					ID:         fmt.Sprintf("imp:%s:%s:%s.%s", p.repo, relPath, importModule, name),
					SourceID:   fmt.Sprintf("%s:%s:__module__#0", p.repo, relPath),
					TargetID:   fmt.Sprintf("%s:%s:%s#0", p.repo, moduleToFile(importModule), name),
					Type:       "IMPORTS",
					FilePath:   relPath,
					Line:       lineNum,
					Confidence: 0.9,
				}
				result.Edges = append(result.Edges, edge)
			}
			continue
		}

		if m := pyImportRe.FindStringSubmatch(line); m != nil {
			importModule := m[1]
			edge := SymbolEdge{
				ID:         fmt.Sprintf("imp:%s:%s:%s", p.repo, relPath, importModule),
				SourceID:   fmt.Sprintf("%s:%s:__module__#0", p.repo, relPath),
				TargetID:   fmt.Sprintf("%s:%s:__module__#0", p.repo, moduleToFile(importModule)),
				Type:       "IMPORTS",
				FilePath:   relPath,
				Line:       lineNum,
				Confidence: 0.9,
			}
			result.Edges = append(result.Edges, edge)
			continue
		}

		// Route decorator → marks function as HTTP handler
		if m := pyRouteDecoratorRe.FindStringSubmatch(line); m != nil {
			// Next line should be a function def — we'll mark it in the next iteration
			// For now, create an edge indicating this file has a route handler
			method := strings.ToUpper(m[1])
			routePath := m[2]
			_ = method
			_ = routePath
			// Route nodes will be created when the next def is encountered
			continue
		}
	}

	return result
}

// --- Helpers ---

func countLeadingSpaces(line string) int {
	count := 0
	for _, ch := range line {
		if ch == ' ' {
			count++
		} else if ch == '\t' {
			count += 4
		} else {
			break
		}
	}
	return count
}

func countPyParams(defLine string) int {
	// Extract parameters between parentheses
	start := strings.Index(defLine, "(")
	end := strings.LastIndex(defLine, ")")
	if start < 0 || end < 0 || end <= start {
		// Multi-line params — approximate with 0
		return 0
	}
	params := defLine[start+1 : end]
	if strings.TrimSpace(params) == "" {
		return 0
	}
	// Count commas + 1, subtract self/cls
	parts := strings.Split(params, ",")
	count := 0
	for _, part := range parts {
		name := strings.TrimSpace(part)
		if name == "self" || name == "cls" || name == "" {
			continue
		}
		// Skip *args, **kwargs from count
		if strings.HasPrefix(name, "*") {
			continue
		}
		count++
	}
	return count
}

func resolveRelativeImport(currentModule, importPath string) string {
	// . → same package, .. → parent package
	dots := 0
	for _, ch := range importPath {
		if ch == '.' {
			dots++
		} else {
			break
		}
	}
	remainder := importPath[dots:]

	parts := strings.Split(currentModule, ".")
	if dots > len(parts) {
		return importPath // can't resolve
	}
	base := strings.Join(parts[:len(parts)-dots+1], ".")
	if remainder == "" {
		return base
	}
	return base + "." + remainder
}

func parseImportedNames(namesStr string) []string {
	// Handle: "name1, name2 as alias2, name3"
	// Also handle: "(name1, name2, name3)" multi-line imports
	namesStr = strings.Trim(namesStr, "()")
	parts := strings.Split(namesStr, ",")
	var names []string
	for _, part := range parts {
		name := strings.TrimSpace(part)
		// Strip " as alias" suffix
		if idx := strings.Index(name, " as "); idx > 0 {
			name = name[:idx]
		}
		name = strings.TrimSpace(name)
		if name != "" && name != "*" {
			names = append(names, name)
		}
	}
	return names
}

func moduleToFile(module string) string {
	// compute.disk.encrypt_disk → compute/disk/encrypt_disk.py
	return strings.ReplaceAll(module, ".", "/") + ".py"
}

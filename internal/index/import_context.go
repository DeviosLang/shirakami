package index

import (
	"fmt"
	"strings"
)

// BuildImportContext generates a human-readable import graph summary for
// injection into LLM Worker prompts. This reduces the number of ripgrep
// rounds needed — the LLM already knows which files import what.
//
// Output format (Markdown-style, compact):
//
//	## Known Import Graph for vstation_compute
//	- compute/disk/encrypt_disk.py imports:
//	    compute.common.utils (execute, log_info)
//	    compute.common.plog (log_error, log_exception)
//	- compute/access.py imports:
//	    compute.disk.encrypt_disk (init_sm4_dev, refresh_sm4_dev)
//	    compute.service.dispatch (dispatch)
func BuildImportContext(nodes []SymbolNode, edges []SymbolEdge, repo string, changedFiles []string) string {
	if len(edges) == 0 {
		return ""
	}

	// Build file → imports mapping (only for changed files and their direct dependents)
	relevantFiles := make(map[string]bool)
	for _, f := range changedFiles {
		relevantFiles[f] = true
	}

	// Also find files that import the changed files (one hop upstream)
	changedModules := make(map[string]bool)
	for _, f := range changedFiles {
		mod := strings.TrimSuffix(f, ".py")
		mod = strings.ReplaceAll(mod, "/", ".")
		changedModules[mod] = true
	}

	// Collect import edges relevant to changed files
	type importInfo struct {
		fromFile string
		toModule string
		names    []string
	}

	fileImports := make(map[string][]importInfo)

	for _, e := range edges {
		if e.Type != "IMPORTS" {
			continue
		}
		// Parse source file from edge
		srcFile := e.FilePath
		if srcFile == "" {
			// Try to extract from SourceID: "{repo}:{file}:__module__#0"
			parts := strings.SplitN(e.SourceID, ":", 3)
			if len(parts) >= 3 && parts[0] == repo {
				srcFile = parts[1]
			}
		}
		if srcFile == "" {
			continue
		}

		// Parse target module from TargetID
		targetModule := ""
		parts := strings.SplitN(e.TargetID, ":", 3)
		if len(parts) >= 3 {
			targetFile := parts[1]
			targetModule = strings.TrimSuffix(targetFile, ".py")
			targetModule = strings.ReplaceAll(targetModule, "/", ".")
		}
		if targetModule == "" {
			continue
		}

		// Include if: source is a changed file, OR target is a changed module
		if relevantFiles[srcFile] || changedModules[targetModule] {
			// Extract imported name from target
			importedName := ""
			if len(parts) >= 3 {
				namePart := parts[2]
				if namePart != "__module__#0" {
					importedName = strings.TrimSuffix(namePart, "#0")
					// Strip arity suffix
					if idx := strings.LastIndex(importedName, "#"); idx > 0 {
						importedName = importedName[:idx]
					}
				}
			}

			info := importInfo{
				fromFile: srcFile,
				toModule: targetModule,
			}
			if importedName != "" {
				info.names = []string{importedName}
			}
			fileImports[srcFile] = append(fileImports[srcFile], info)
		}
	}

	if len(fileImports) == 0 {
		return ""
	}

	// Format output
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("## Known Import Graph for %s\n", repo))
	sb.WriteString("(Use this to avoid unnecessary ripgrep searches — these relationships are confirmed)\n\n")

	for file, imports := range fileImports {
		// Merge imports by target module
		moduleNames := make(map[string][]string)
		for _, imp := range imports {
			moduleNames[imp.toModule] = append(moduleNames[imp.toModule], imp.names...)
		}

		sb.WriteString(fmt.Sprintf("- %s imports:\n", file))
		for mod, names := range moduleNames {
			if len(names) > 0 && names[0] != "" {
				sb.WriteString(fmt.Sprintf("    %s (%s)\n", mod, strings.Join(dedupeStrings(names), ", ")))
			} else {
				sb.WriteString(fmt.Sprintf("    %s\n", mod))
			}
		}
	}

	return sb.String()
}

func dedupeStrings(ss []string) []string {
	seen := make(map[string]bool)
	var result []string
	for _, s := range ss {
		if s != "" && !seen[s] {
			seen[s] = true
			result = append(result, s)
		}
	}
	return result
}

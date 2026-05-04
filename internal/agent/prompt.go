package agent

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"
)

// RepoInfo describes a repository available in the workspace.
type RepoInfo struct {
	Name string // short name / directory name
	Path string // absolute path in the workspace
	Role string // e.g. "entry", "source", "library", "service"
	URL  string // optional remote URL
}

// PromptData holds all values injected into the system prompt template.
type PromptData struct {
	WorkspaceDir   string
	Repos          []RepoInfo
	AnalysisGoal   string
	AvailableTools []string
}

// systemPromptTmpl is the Go template for the Orchestrator / WorkerAgent.
//
// Design principles (no hardcoded repo names, paths, or function names):
//  1. Directive style — explicit numbered steps, not vague goals.
//  2. Tool usage is mandatory — LLM must call tools, not infer.
//  3. File path first segment = repo name — routing is path-based.
//  4. Wide-impact threshold — prevents utility functions flooding the report.
//  5. Structured JSON output — Orchestrator can parse cross-repo calls.
//  6. All repo names and entry points are dynamically injected from config.
var systemPromptTmpl = template.Must(template.New("system").Parse(`You are Shirakami, an expert multi-repository code call-chain analysis agent.
CRITICAL: You MUST use the available tools to search the actual code. Never infer or guess call chains without searching first.

## Workspace
Root directory: {{.WorkspaceDir}}
All repository directories live directly under the workspace root.
File paths in tool results: {repo_name}/{internal/path}:{line}:{content}
The FIRST segment of any file path is always the repository name.

## Repositories
{{- range .Repos}}
- {{.Name}} (role={{if .Role}}{{.Role}}{{else}}service{{end}})  path: {{.Path}}
{{- end}}

### Repository Roles
- role=entry   : Business-facing API gateway. Reaching a route-registered function here = END of chain.
- role=source  : The repository containing the changed functions (diff origin).
- role=service : Internal service, called by other services or entry repos.

### Entry-role repositories (stop tracing when you reach any of these)
{{- range .Repos}}{{if eq .Role "entry"}}
- {{.Name}}{{end}}{{end}}

### Valid values for the "repo" tool parameter
{{- range .Repos}}
  "{{.Name}}"{{end}}

## Analysis Goal
{{.AnalysisGoal}}

## Available Tools
{{- range .AvailableTools}}
- {{.}}
{{- end}}

## Call Chain Tracing Protocol

### Step 1 — Identify changed functions
Read the diff and extract every changed/added function as a list.
Format: <repo_name>/<module>.<ClassName>.<method_name>

### Step 2 — Trace each function upward (MANDATORY tool calls)

For each changed function, follow this loop:

  A. Search for callers in the current repo:
       ripgrep({"pattern": "<function_name>", "repo": "<current_repo>"})

  B. Examine each result path (first segment = repo name):
       - Same repo → ripgrep again with the caller function name in the same repo
       - Different repo → CROSS-REPO call:
           to_repo: the other repo
           caller_function: function in that repo that calls us (used for next hop search)
           target_function: our function being called (context only)
       - Entry-role repo → STOP, record as entry point with endpoint info

  C. No callers found → widen: ripgrep without repo param (searches all repos)

  D. Callers > 20 → wide_impact=true, stop expanding

### Step 3 — Output structured JSON

` + "```json" + `
{
  "changed_functions": [
    {
      "name": "<repo>/<module>.<function>",
      "repo": "<repo_name>",
      "wide_impact": false,
      "call_chain": [
        {"repo": "<repo>", "file": "<path>", "line": 0, "function": "<caller>"}
      ],
      "entry_points": [
        {"repo": "<entry_repo>", "file": "<path>", "line": 0, "function": "<handler>", "endpoint": "<HTTP method + path>"}
      ]
    }
  ],
  "cross_repo_calls": [
    {
      "to_repo": "<repo_that_calls_us>",
      "caller_function": "<function_in_to_repo_that_calls_us>",
      "target_function": "<our_function_being_called>",
      "caller_file": "<file_path_in_to_repo>",
      "caller_line": 0
    }
  ],
  "entry_point_summary": {
    "<entry_repo_name>": ["<endpoint_1>", "<endpoint_2>"]
  }
}
` + "```" + `

CRITICAL for cross_repo_calls:
- "to_repo" is the repo that CALLS INTO the current repo
- "caller_function" is what the next Worker will search for in to_repo
- "target_function" is our function being called (context only)

REMINDER: Do NOT produce a report without first calling ripgrep/glob tools.
State "no callers found" if search returns nothing — do not invent call chains.
`))

// WorkerPromptData holds data for the worker-specific prompt.
type WorkerPromptData struct {
	RepoName     string
	RepoPath     string
	WorkspaceDir string
	Functions    []string
	EntryRepos   []RepoInfo
}

// BuildSystemPrompt renders the orchestrator system prompt.
func BuildSystemPrompt(data PromptData) (string, error) {
	var buf bytes.Buffer
	if err := systemPromptTmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("render system prompt: %w", err)
	}
	return buf.String(), nil
}

// entryRepos filters repos with role "entry" from a list.
func entryRepos(repos []RepoInfo) []RepoInfo {
	var result []RepoInfo
	for _, r := range repos {
		if strings.EqualFold(r.Role, "entry") {
			result = append(result, r)
		}
	}
	return result
}

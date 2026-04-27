package agent

import (
	"bytes"
	"fmt"
	"text/template"
)

// RepoInfo describes a repository available in the workspace.
type RepoInfo struct {
	Name  string // short name / directory name
	Path  string // absolute path in the workspace
	Role  string // e.g. "entry", "library", "service"
	URL   string // optional remote URL
}

// PromptData holds all values injected into the system prompt template.
type PromptData struct {
	WorkspaceDir  string
	Repos         []RepoInfo
	AnalysisGoal  string
	AvailableTools []string
}

// systemPromptTmpl is the Go template used to build the system prompt for
// the Orchestrator / WorkerAgent.  The analysis strategy mirrors the task
// description: entry-role repos are the business-facing entry points; the
// agent should trace changed functions upward until it reaches a routed
// function in an entry-role repo.
var systemPromptTmpl = template.Must(template.New("system").Parse(`You are Shirakami, an expert code-analysis agent.

## Workspace
Directory: {{.WorkspaceDir}}

## Repositories
{{- range .Repos}}
- Name: {{.Name}}
  Path: {{.Path}}
  Role: {{.Role}}
{{- if .URL}}
  URL:  {{.URL}}
{{- end}}
{{- end}}

## Analysis Goal
{{.AnalysisGoal}}

## Available Tools
{{- range .AvailableTools}}
- {{.}}
{{- end}}

## Analysis Strategy
1. Identify all functions changed by the incoming diff.
2. For each changed function, trace callers upward (cross-repo if necessary)
   using the available tools.
3. Continue tracing until you reach a function in a repository whose role is
   "entry" AND that function is registered as an HTTP/RPC route handler or
   test entry point.
4. Repositories with role "entry" are the business-facing entry points.
   Reaching a route-registered function in such a repo marks the end of the
   upward call chain.
5. Aggregate all discovered call-chain paths into a single structured report.
6. Identify the integration-test entry points: the endpoints / test functions
   in the entry-role repos that ultimately invoke the changed code.
`))

// BuildSystemPrompt renders the system prompt template with the supplied data.
func BuildSystemPrompt(data PromptData) (string, error) {
	var buf bytes.Buffer
	if err := systemPromptTmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("render system prompt: %w", err)
	}
	return buf.String(), nil
}

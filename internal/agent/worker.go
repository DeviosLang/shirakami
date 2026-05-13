package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/DeviosLang/shirakami/internal/checkpoint"
	"github.com/DeviosLang/shirakami/internal/logger"
	"github.com/DeviosLang/shirakami/internal/tool"
	itrace "github.com/DeviosLang/shirakami/internal/trace"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// CallNode represents one function in a call chain.
type CallNode struct {
	Repo     string // repository name
	Package  string // Go package import path or module-relative path
	Function string // qualified function name
	File     string // file path relative to repo root
	Line     int    // line number (0 if unknown)
	// Source indicates how this node was discovered.
	// "worker" = directly found by a WorkerAgent search.
	// "graph"  = resolved by the deterministic symbol graph (Layer B).
	// Empty string = unknown / legacy.
	Source string
}

// WorkerTask is the input handed to a WorkerAgent.
type WorkerTask struct {
	RepoName         string
	RepoPath         string
	// WorkspaceDir is the root directory containing all cloned repositories.
	// When set, filterGhostNodes can rescue cross-repo ghost nodes by searching
	// any repo directory under this root (e.g. /workspace/cvm_api/).
	WorkspaceDir     string
	ChangedFunctions []string
	ExternalCallers  []string
	// Priority is the triage classification ("P0", "P1", "P2"); empty = default.
	// P2 tasks use a tighter step budget to cap their cost.
	Priority string
	// ContractHints are known cross-repo relationships declared in shirakami.yaml.
	// Injected into the Worker prompt so the LLM can confirm/deny without searching.
	ContractHints []string
	// ImportContext is a pre-built import graph summary for the repo (Python).
	// Reduces LLM search rounds by providing known import relationships upfront.
	ImportContext string
	// Modes controls which follow-up passes to run after main tracing.
	// Valid values: "chain", "e2e", "ut". Empty slice means run all.
	// "e2e" enables runScenarioAnalysis; "ut" enables runUTAnalysis.
	Modes []string
	// ExtraPrompt is optional business-context text appended to the e2e scenario
	// and UT follow-up prompts. Helps the LLM generate more accurate test cases
	// when domain knowledge is not inferable from the code alone.
	ExtraPrompt string
	// LineHints maps "file::funcname" → approximate line number where the change
	// occurs in the new file (from the diff hunk). When provided, the Worker
	// prompt includes "(around line N)" hints so the LLM can distinguish between
	// multiple same-named functions in the same file.
	LineHints map[string]int
	// DiffSnippets maps "file::funcname" → the actual +/- lines from the hunk (改进 4).
	// Injected into the Worker prompt so the LLM understands WHAT changed, not just
	// WHERE. Limited to 40 lines per function to keep token cost bounded.
	DiffSnippets map[string][]string
	// NewFunctions lists function names that are brand-new additions in the diff
	// (ChangeType == "added" from ParseDiffFunctions). These functions do not exist
	// in the master workspace but ARE present in a feature-branch worktree.
	// When non-empty, they are injected into the prompt so the LLM treats them as
	// real, and their nodes are exempted from the filterGhostNodes disk check.
	NewFunctions []string
}

// SearchResult holds one raw ripgrep hit — file path, line, and caller name.
// It is stored verbatim so the Orchestrator's fallback path can determine
// cross-repo calls from the file-path prefix without relying on LLM judgment.
type SearchResult struct {
	File     string // e.g. "vstation_compute_access/dispatch.py"
	Line     int
	Function string // caller function name at this location
}

// WorkerResult is returned by a WorkerAgent after analysis.
type WorkerResult struct {
	RepoName       string
	Nodes          []CallNode
	CrossRepoCalls []CrossRepoCall
	ReachedEntry   bool
	EntryPoints    []CallNode
	// FunctionAnalyses holds per-function constraint and scenario data.
	FunctionAnalyses []FunctionAnalysis
	// EntryScenarios holds per-entry-point test scenario suggestions (from scenario follow-up).
	EntryScenarios []EntryPointScenario
	// UTAnalyses holds per-changed-function unit-test suggestions (from UT follow-up).
	UTAnalyses []UTAnalysis
	// SearchResults holds raw ripgrep hits for the fallback cross-repo extractor.
	SearchResults []SearchResult
	// WideImpactFunctions lists function names for which the LLM set wide_impact=true,
	// meaning ripgrep found more callers than the expansion threshold and tracing was
	// stopped early. Used by the Orchestrator to rescue same-repo entry points that
	// were truncated.
	WideImpactFunctions []string
	// RawContent holds the full LLM text output for display / debugging.
	RawContent string
	// GhostFiles holds file paths that were referenced by the LLM output but
	// do not exist on disk. Populated by filterGhostNodes after parseWorkerOutput.
	// Used to emit actionable warnings in API responses.
	GhostFiles []string
}

// UTAnalysis is the unit-test suggestion for one changed function, produced
// by the UT follow-up call. It focuses on function-internal behaviour
// (constraints, mock setups, exception paths) rather than end-to-end flow.
type UTAnalysis struct {
	FuncName      string
	FilePath      string
	Summary       string
	Constraints   []string
	ExistingTests []string
	Scenarios     []UTScenario
}

// UTScenario is a single unit-test scenario for a changed function.
type UTScenario struct {
	Priority    string // P0/P1/P2
	Type        string // normal / boundary / exception / compat / fallback
	Description string
	MockSetup   string
	Assertions  string
}

// EntryPointScenario holds test scenario suggestions for a single entry point,
// generated by the scenario follow-up pass after the main call-chain tracing.
type EntryPointScenario struct {
	EntryFunction string       // matches EntryPoint.Node.FuncName
	EntryFile     string       // matches EntryPoint.Node.FilePath
	ChangedVia    []string     // diff-changed functions in the call path to this entry
	Preconditions []string     // prerequisite system state
	TypicalInputs string       // description of typical input parameters
	Scenarios     []TestScenario
}

// FunctionAnalysis holds constraint extraction and test scenario suggestions
// for a single changed function.
type FunctionAnalysis struct {
	Name               string
	Repo               string
	File               string
	Constraints        []Constraint
	ExistingTests      []string
	SuggestedScenarios []TestScenario
}

// Constraint describes a code-level constraint found in a function body.
type Constraint struct {
	Type      string // "size_limit", "whitelist", "python_compat", "type_check", "null_check"
	Condition string // e.g. "block_size <= MAX_BLOCK_SIZE"
	File      string
	Line      int
	Note      string
}

// TestScenario is a suggested test case derived from constraints and call chain.
type TestScenario struct {
	Type        string // "normal", "boundary", "exception", "compat", "whitelist"
	Description string
	Input       string
	Expected    string
	Priority    string // "P0", "P1", "P2"
	Oracles     []TestOracle
}

// TestOracle identifies a verification target for a scenario.
type TestOracle struct {
	Type      string // db / file_state / host_process / mq / rpc / log / metric / external_api
	Target    string // table.field / file path / topic / source etc.
	Assertion string // expected value or observable condition
}

// CrossRepoCall describes a call that crosses a repository boundary.
type CrossRepoCall struct {
	TargetRepo     string
	TargetFunction string
	CallerNode     CallNode
}

// --- JSON shapes the LLM must output -----------------------------------

type llmCallNode struct {
	Repo     string `json:"repo"`
	File     string `json:"file"`
	Line     int    `json:"line"`
	Function string `json:"function"`
	Endpoint string `json:"endpoint,omitempty"`
}

// llmCrossRepoCall records a call that crosses a repository boundary.
//
// Field semantics (critical for correct Orchestrator routing):
//   to_repo         : the repository that contains the CALLER
//   caller_function : the function in to_repo that makes the call
//                     (this is what the next Worker should search for)
//   target_function : the function in the current repo being called
//                     (context only; not used for next-hop routing)
//   caller_file     : file path in to_repo where the call is made
//   caller_line     : line number in caller_file
type llmCrossRepoCall struct {
	ToRepo         string `json:"to_repo"`
	CallerFunction string `json:"caller_function"` // function in to_repo that calls us
	TargetFunction string `json:"target_function"` // our function being called (context)
	CallerFile     string `json:"caller_file"`
	CallerLine     int    `json:"caller_line"`
	// Legacy field alias — some LLMs output via_function instead of caller_function
	ViaFunction string `json:"via_function,omitempty"`
}

type llmChangedFunction struct {
	Name             string              `json:"name"`
	Repo             string              `json:"repo"`
	WideImpact       bool                `json:"wide_impact"`
	CallChain        []llmCallNode       `json:"call_chain"`
	EntryPoints      []llmCallNode       `json:"entry_points"`
	Constraints      []llmConstraint     `json:"constraints,omitempty"`
	ExistingTests    []string            `json:"existing_tests,omitempty"`
	SuggestedScenarios []llmScenario     `json:"suggested_scenarios,omitempty"`
}

// llmConstraint describes a code-level constraint found in the function body.
type llmConstraint struct {
	Type      string `json:"type"`      // e.g. "size_limit", "whitelist", "python_compat", "type_check"
	Condition string `json:"condition"` // e.g. "block_size <= MAX_BLOCK_SIZE"
	File      string `json:"file"`
	Line      int    `json:"line"`
	Note      string `json:"note,omitempty"` // human-readable explanation
}

// llmScenario describes a suggested test scenario derived from constraints and call chain.
type llmScenario struct {
	Type        string         `json:"type"`        // "normal", "boundary", "exception", "compat", "whitelist"
	Description string         `json:"description"` // e.g. "块设备大小恰好等于限制值时读取密文"
	Input       string         `json:"input,omitempty"`
	Expected    string         `json:"expected,omitempty"`
	Priority    string         `json:"priority,omitempty"`
	Oracles     []llmTestOracle `json:"oracles,omitempty"`
}

// llmTestOracle mirrors TestOracle for JSON parsing.
type llmTestOracle struct {
	Type      string `json:"type"`
	Target    string `json:"target"`
	Assertion string `json:"assertion"`
}

type llmWorkerOutput struct {
	Repo                string               `json:"repo"`
	Nodes               []llmCallNode        `json:"nodes"`
	CrossRepoCalls      []llmCrossRepoCall   `json:"cross_repo_calls"`
	EntryPoints         []llmCallNode        `json:"entry_points"`
	WideImpactFunctions []string             `json:"wide_impact_functions"`
	ChangedFunctions    []llmChangedFunction `json:"changed_functions"`
	// search_results: raw ripgrep hits the LLM found.
	// Used by the fallback cross-repo extractor — LLM just lists what it found,
	// Go code decides which are cross-repo.
	SearchResults []llmSearchResult `json:"search_results"`
}

// llmSearchResult is one raw ripgrep hit reported by the LLM.
type llmSearchResult struct {
	File     string `json:"file"`
	Line     int    `json:"line"`
	Function string `json:"function"` // caller function at this location
}

// llmEntryScenario is the per-entry-point scenario block returned by the scenario follow-up.
type llmEntryScenario struct {
	EntryFunction string        `json:"entry_function"` // corresponds to entry_points[].function
	EntryFile     string        `json:"entry_file"`
	ChangedVia    []string      `json:"changed_via"`    // diff-changed functions in the call path
	Preconditions []string      `json:"preconditions"`
	TypicalInputs string        `json:"typical_inputs"`
	Scenarios     []llmScenario `json:"scenarios"`
}

// llmEntryScenariosOutput is the top-level JSON returned by the scenario follow-up call.
type llmEntryScenariosOutput struct {
	EntryScenarios []llmEntryScenario `json:"entry_scenarios"`
}

// fencedJSONStartRe finds the opening { of a ```json fenced block.
// Used by extractJSON (strategy 1) as an anchor; the actual JSON object
// is then extracted via depth-counted brace matching so that:
//   (a) nested JSON objects are handled correctly, and
//   (b) multiple fenced blocks in the same response are not merged into
//       invalid JSON (which the old greedy regex (?s)`{.+}` caused).
var fencedJSONStartRe = regexp.MustCompile("```json[ \\t]*\\r?\\n?[ \\t]*(\\{)")

// WorkerAgent performs local call-chain analysis inside a single repository.
type WorkerAgent struct {
	loop        *AgentLoop
	entryRepos  []string // names of entry-role repos, dynamically injected from config
	metrics     MetricsRecorder
	metricsModel string // LLM model name forwarded to loop.WithMetrics
}

// NewWorkerAgent creates a WorkerAgent for a specific repository.
func NewWorkerAgent(
	llmClient LLMClient,
	tools []Tool,
	cp *checkpoint.FileCheckpointer,
	repos []RepoInfo,
	workspaceDir string,
) *WorkerAgent {
	return NewWorkerAgentWithBudget(llmClient, tools, cp, repos, workspaceDir, 0)
}

// NewWorkerAgentWithBudget creates a WorkerAgent with a specific step budget.
// A budget of 0 means use the default from the agent loop.
func NewWorkerAgentWithBudget(
	llmClient LLMClient,
	tools []Tool,
	cp *checkpoint.FileCheckpointer,
	repos []RepoInfo,
	workspaceDir string,
	budget int,
) *WorkerAgent {
	toolNames := make([]string, 0, len(tools))
	for _, t := range tools {
		toolNames = append(toolNames, t.Definition().Name)
	}

	// Collect entry-role repo names from config — never hardcode.
	entryRepos := make([]string, 0)
	for _, r := range repos {
		if strings.EqualFold(r.Role, "entry") {
			entryRepos = append(entryRepos, r.Name)
		}
	}

	prompt, _ := BuildSystemPrompt(PromptData{
		WorkspaceDir:   workspaceDir,
		Repos:          repos,
		AnalysisGoal:   "Trace changed functions upward through callers until reaching entry-role repositories.",
		AvailableTools: toolNames,
	})

	loop := NewAgentLoop(llmClient, tools, budget, cp, prompt)
	return &WorkerAgent{loop: loop, entryRepos: entryRepos}
}

// WithMetrics attaches a Prometheus metrics recorder to the worker.
// Call this after NewWorkerAgentWithBudget; model is the LLM model name label.
// Returns the worker so calls can be chained.
func (w *WorkerAgent) WithMetrics(m MetricsRecorder, model string) *WorkerAgent {
	w.metrics = m
	w.metricsModel = model
	w.loop.WithMetrics(m, model, "worker")
	return w
}

// Analyse runs the worker's agent loop and parses the structured JSON result.
func (w *WorkerAgent) Analyse(ctx context.Context, task WorkerTask) (*WorkerResult, error) {
	// Build entry repo list dynamically from config — no hardcoding.
	entryRepoList := strings.Join(w.entryRepos, ", ")
	if entryRepoList == "" {
		entryRepoList = "(none configured — trace all the way up)"
	}

	// Resolve FILE_CHANGED sentinels: convert "FILE_CHANGED:repo/path/file.py" into
	// a human-readable instruction for the Worker to search the file broadly.
	// Also handle FILE_CHANGED_VAR sentinels (改进 5) which target a specific
	// global variable usage site rather than the whole file.
	resolvedFuncs := make([]string, 0, len(task.ChangedFunctions))
	fileOnlyTasks := make([]string, 0)
	varOnlyTasks := make([]string, 0) // "filePath:varName" pairs
	for _, fn := range task.ChangedFunctions {
		if strings.HasPrefix(fn, "FILE_CHANGED_VAR:") {
			// Format: "FILE_CHANGED_VAR:repo/file.py:VAR_NAME"
			rest := strings.TrimPrefix(fn, "FILE_CHANGED_VAR:")
			// Split on last ":" to get filePath + varName
			lastColon := strings.LastIndex(rest, ":")
			if lastColon > 0 {
				varOnlyTasks = append(varOnlyTasks, rest[:lastColon]+"\x00"+rest[lastColon+1:])
			} else {
				fileOnlyTasks = append(fileOnlyTasks, rest)
			}
		} else if strings.HasPrefix(fn, "FILE_CHANGED:") {
			filePath := strings.TrimPrefix(fn, "FILE_CHANGED:")
			fileOnlyTasks = append(fileOnlyTasks, filePath)
		} else {
			resolvedFuncs = append(resolvedFuncs, fn)
		}
	}
	// Build a combined function list: named functions first, then file-level sentinels
	// phrased as search instructions.
	combinedFuncs := resolvedFuncs
	for _, fp := range fileOnlyTasks {
		combinedFuncs = append(combinedFuncs,
			fmt.Sprintf("[search all changed functions in file: %s]", fp))
	}
	// 改进 5: FILE_CHANGED_VAR — target a specific global variable usage site.
	for _, pair := range varOnlyTasks {
		parts := strings.SplitN(pair, "\x00", 2)
		if len(parts) == 2 {
			combinedFuncs = append(combinedFuncs,
				fmt.Sprintf("[search all callers of global variable %s in file: %s]", parts[1], parts[0]))
		}
	}
	if len(combinedFuncs) == 0 {
		combinedFuncs = task.ChangedFunctions
	}

	// Annotate function entries with line-number hints and diff snippets when available.
	// Line hints help the LLM disambiguate same-named functions in the same file.
	// Diff snippets (改进 4) show WHAT changed so the LLM understands context without
	// having to read the file. Format:
	//   "repo/file.py::funcname (changed around line 209)\n  Diff:\n  +x = 1\n  -x = 0"
	annotatedFuncs := make([]string, 0, len(combinedFuncs))
	for _, fn := range combinedFuncs {
		// Strip any pre-existing annotation before looking up hints
		// (sentinels like "[search all...]" are passed through unchanged).
		lookupKey := fn
		if idx := strings.Index(fn, " (changed around line"); idx > 0 {
			lookupKey = fn[:idx]
		}

		annotated := fn
		if task.LineHints != nil {
			if lineNum, ok := task.LineHints[lookupKey]; ok && lineNum > 0 {
				annotated = fmt.Sprintf("%s (changed around line %d)", fn, lineNum)
			}
		}
		// Append diff snippet if available (改进 4).
		if task.DiffSnippets != nil {
			if lines, ok := task.DiffSnippets[lookupKey]; ok && len(lines) > 0 {
				snippet := strings.Join(lines, "\n  ")
				annotated = annotated + "\n  Diff:\n  " + snippet
			}
		}
		annotatedFuncs = append(annotatedFuncs, annotated)
	}

	// Build contract hints section (from shirakami.yaml contracts declarations).
	contractHintsSection := ""
	if len(task.ContractHints) > 0 {
		contractHintsSection = "KNOWN CROSS-REPO RELATIONSHIPS (from configuration — use these as starting points):\n" +
			formatList(task.ContractHints) +
			"These are pre-declared relationships. When tracing call chains, CHECK these first before\n" +
			"doing broad ripgrep searches. If a changed function matches a provider/consumer above,\n" +
			"record the cross-repo call directly (confidence is high for declared contracts).\n\n"
	}

	// Build import context section (from Python index — reduces search rounds).
	importContextSection := ""
	if task.ImportContext != "" {
		importContextSection = task.ImportContext + "\n" +
			"Use the import graph above to trace callers WITHOUT ripgrep when possible.\n" +
			"If file A imports function X from file B, and X is in the changed functions list,\n" +
			"then A is a direct caller — record it in nodes[] without needing to search.\n\n"
	}

	// Build newly-added functions section (Plan B fallback for bare diff or worktree failure).
	// These functions are brand-new in the diff — they do NOT exist in the master workspace,
	// but may exist in a feature-branch worktree (if worktree was created successfully).
	// The section tells the LLM to treat them as real even if ripgrep returns 0 results.
	newFunctionsSection := ""
	if len(task.NewFunctions) > 0 {
		newFunctionsSection = "NEWLY ADDED FUNCTIONS (exist only in this feature branch, NOT in master):\n" +
			formatList(task.NewFunctions) +
			"These functions are brand-new ('+def'/'+func' in diff). They may not appear in ripgrep\n" +
			"results if the master workspace is used. Still include them as nodes[] in your output\n" +
			"(with file/line from the diff or empty if unknown) — do NOT skip them.\n\n"
	}

	prompt := fmt.Sprintf(
		"SOURCE REPOSITORY: %s\n"+
			"REPOSITORY PATH: %s\n"+
			"CHANGED FUNCTIONS TO TRACE:\n%s"+
			"EXTERNAL CALLERS (from other repos calling into this repo):\n%s\n"+
			"%s"+
			"%s"+
			"%s"+
			"ENTRY-ROLE REPOSITORIES (stop tracing when you reach these): %s\n\n"+
			"## Your Goal\n\n"+
			"Trace the complete call chain for every CHANGED FUNCTION above.\n"+
			"Find all entry points in entry-role repositories (HTTP handlers / gRPC endpoints)\n"+
			"that can ultimately reach the changed code.\n\n"+
			"If an entry says '[search all changed functions in file: X]', first use ripgrep to\n"+
			"discover all function definitions in that file, then trace each one.\n\n"+
			"## Tools Available\n\n"+
			"- ripgrep: search actual source code for callers\n"+
			"- file_reader: read file contents to understand context (use when ripgrep results are ambiguous)\n"+
			"- lsp_call_hierarchy: precise call hierarchy queries — incomingCalls, outgoingCalls, findImplementations\n\n"+
			"## Search Constraints (MUST follow)\n\n"+
			"- Every ripgrep call MUST be executed; never infer callers from memory.\n"+
			"- If a function name is generic (handler/run/execute/main/process/start),\n"+
			"  search the MODULE name instead.\n"+
			"  e.g. 'compute.procedure.get_cbs_ciphertext.handler' → search 'get_cbs_ciphertext'\n"+
			"- When ripgrep returns 0 results, exhaust this fallback sequence before giving up:\n"+
			"    1. Naming-style conversion: try both CamelCase and snake_case variants.\n"+
			"       e.g. 'ResetInstanceNetType' → also try 'reset_instance_net_type', 'reset_net_type'\n"+
			"    2. Partial keyword search: extract the most distinctive 1-2 words.\n"+
			"       e.g. 'ResetInstanceNetType' → try 'NetType', 'reset_net', 'ResetNet'\n"+
			"    3. Remove the repo restriction — search ALL repos:\n"+
			"       ripgrep({\"pattern\": \"<keyword>\"})  ← no \"repo\" param\n"+
			"    4. Only after ALL steps above return 0 results: record the node with\n"+
			"       \"file\": \"\", \"line\": 0. DO NOT invent a path.\n"+
			"- Callers > 20 → set wide_impact=true and stop expanding that function.\n"+
			"- Record ALL ripgrep result file paths in search_results[].\n\n"+
			"## Cross-Repository Constraints\n\n"+
			"- When a ripgrep result path starts with a DIFFERENT repo name, record a cross_repo_calls entry.\n"+
			"- When a ripgrep result is in an entry-role repo, stop tracing and record as entry_point.\n"+
			"- For entry-role repos not yet reached via direct search, probe them:\n"+
			"  search for '%s' (the source repo name) or the changed function's module/package name\n"+
			"  inside each entry-role repo using ripgrep.\n"+
			"  If still no results, try 'dispatch' or 'handler' patterns referencing the source domain.\n\n"+
			"## Inheritance & Polymorphism\n\n"+
			"- If a modified function belongs to a class, check for subclasses:\n"+
			"  ripgrep({\"pattern\": \"class \\\\w+\\\\(ClassName\\\\)\", \"repo\": \"%s\"})\n"+
			"- If a subclass overrides the same method, treat each override as an additional changed function\n"+
			"  and trace it upward as well.\n"+
			"- If callers reference the base-class type (polymorphic dispatch), all subclass implementations\n"+
			"  may execute at runtime — record all of them.\n"+
			"- For interface/abstract methods, use lsp_call_hierarchy with operation=\"findImplementations\"\n"+
			"  to find all concrete implementations.\n\n"+
			"## Output Integrity Rules\n\n"+
			"- cross_repo_calls.to_repo: MUST be the first path segment seen in an actual ripgrep result;\n"+
			"  never guess a repo name.\n"+
			"- cross_repo_calls.caller_function: MUST be the real function name from the ripgrep result line;\n"+
			"  if unclear, leave it empty — the system will fall back to file-based search.\n"+
			"- cross_repo_calls.target_function: the function in THIS repo (%s) being called.\n"+
			"- nodes[].file and entry_points[].file: MUST be verbatim paths from ripgrep results;\n"+
			"  if not seen in output, leave \"file\": \"\" and \"line\": 0.\n"+
			"- DO NOT record cross_repo_calls for repos seen only in comments or documentation.\n\n"+
			"## Output Format\n\n"+
			"When done, output a single JSON block and nothing else:\n"+
			"```json\n"+
			"{\n"+
			"  \"repo\": \"%s\",\n"+
			"  \"nodes\": [{\"repo\":\"\",\"file\":\"\",\"line\":0,\"function\":\"\"}],\n"+
			"  \"cross_repo_calls\": [{\"to_repo\":\"\",\"caller_function\":\"\",\"target_function\":\"\",\"caller_file\":\"\",\"caller_line\":0}],\n"+
			"  \"entry_points\": [{\"repo\":\"\",\"file\":\"\",\"line\":0,\"function\":\"\",\"endpoint\":\"\"}],\n"+
			"  \"wide_impact_functions\": [],\n"+
			"  \"search_results\": [{\"file\":\"\",\"line\":0,\"function\":\"\"}]\n"+
			"}\n"+
			"```",
		task.RepoName,
		task.RepoPath,
		formatList(annotatedFuncs),
		formatList(task.ExternalCallers),
		contractHintsSection,
		importContextSection,
		newFunctionsSection,
		entryRepoList,
		task.RepoName, // cross-repo probe: source repo name
		task.RepoName, // inheritance: class search repo param
		task.RepoName, // output integrity: target_function repo name
		task.RepoName, // JSON template: repo field
	)

	taskID := "worker-" + task.RepoName
	log := logger.S()
	workerStart := time.Now()

	// Start a span for the entire per-repo analysis pass.
	ctx, span := itrace.Start(ctx, itrace.OpWorkerAnalyze,
		attribute.String(itrace.AttrRepo, task.RepoName),
		attribute.Int(itrace.AttrFuncCount, len(task.ChangedFunctions)),
		attribute.String(itrace.AttrTriageTier, task.Priority),
	)
	defer span.End()

	log.Infow("worker.start",
		"repo", task.RepoName,
		"funcs", len(task.ChangedFunctions),
		"external_callers", len(task.ExternalCallers),
	)

	result, err := w.loop.Run(ctx, taskID, prompt)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		log.Errorw("worker.trace_failed",
			"repo", task.RepoName,
			"err", err.Error(),
			"duration_ms", time.Since(workerStart).Milliseconds(),
		)
		return nil, fmt.Errorf("worker %s: %w", task.RepoName, err)
	}
	span.SetAttributes(
		attribute.Int(itrace.AttrStepCount, result.StepCount),
	)
	log.Infow("worker.trace_done",
		"repo", task.RepoName,
		"steps", result.StepCount,
		"truncated", result.Truncated,
		"content_bytes", len(result.Content),
		"duration_ms", time.Since(workerStart).Milliseconds(),
	)
	if w.metrics != nil {
		w.metrics.RecordSteps(float64(result.StepCount))
	}

	// If the output doesn't contain a JSON block, ask the LLM to output it now.
	// This handles cases where the LLM gives a prose summary instead of JSON.
	// Use RunFollowUpNoTools — JSON reformatting doesn't need tools.
	//
	// Skip follow-up if the primary content is tiny (<150 bytes) — that means
	// the LLM produced essentially nothing (e.g. "no results found"), and a
	// JSON-reformatting prompt would just loop without useful input.
	//
	// Skip follow-up when the loop was truncated (hit maxSteps). The content is
	// incomplete by definition; asking the LLM to reformat it produces a JSON
	// block built from a partial analysis, which in turn yields zero entry points
	// and causes the scenario-generation pass to be skipped entirely.
	// parseWorkerOutput will extract whatever partial structure exists from the
	// truncated content without risking a second wasted LLM call.
	if !result.Truncated && extractJSON(result.Content) == "" && len(strings.TrimSpace(result.Content)) >= 150 {
		log.Infow("worker.followup_json",
			"repo", task.RepoName,
			"reason", "no JSON block found in primary output",
		)
		followUp := "Your analysis above is good, but you MUST output the final result as a JSON block.\n" +
			"Please output ONLY the JSON block now (```json ... ```) with all findings from your analysis above.\n" +
			"Do not repeat the analysis — just the JSON."
		result2, err2 := w.loop.RunFollowUpNoTools(ctx, taskID, result.Content, followUp)
		if err2 == nil && result2 != nil {
			result = result2
		} else if err2 != nil {
			log.Warnw("worker.followup_json_failed",
				"repo", task.RepoName,
				"err", err2.Error(),
			)
		}
	}

	// Parse the structured JSON output from the LLM.
	workerResult := parseWorkerOutput(task.RepoName, result.Content)
	workerResult.RawContent = result.Content

	// Backfill File="" nodes: LLMs sometimes emit nodes/entry-points with a valid
	// Function name but omit the File field entirely.  backfillEmptyFiles uses
	// ripgrep (via the same toolSearchSymbol helper as filterGhostNodes) to locate
	// the file on disk and fill it in before filterGhostNodes runs.
	// This must run BEFORE filterGhostNodes so that rescued nodes are not
	// incorrectly exempted by the fileExists("") == true short-circuit.
	if task.RepoPath != "" {
		backfillEmptyFiles(workerResult, task.RepoPath, task.WorkspaceDir)
	}

	// Filter hallucinated file paths: any node whose File field points to a path
	// that does not exist on disk is almost certainly an LLM fabrication.
	// filterGhostNodes records the ghost paths in workerResult.GhostFiles so
	// the API layer can surface actionable warnings, then removes those nodes.
	if task.RepoPath != "" {
		filterGhostNodes(workerResult, task.RepoPath, task.WorkspaceDir, task.NewFunctions)
	}

	log.Infow("worker.parsed",
		"repo", task.RepoName,
		"nodes", len(workerResult.Nodes),
		"cross_repo_calls", len(workerResult.CrossRepoCalls),
		"entry_points", len(workerResult.EntryPoints),
		"search_results", len(workerResult.SearchResults),
		"ghost_files_filtered", len(workerResult.GhostFiles),
	)

	// Scenario follow-up: if entry points were found, ask the LLM to generate
	// per-entry-point test scenarios based on the diff-changed functions.
	// This is a best-effort call — failure does not affect the main result.
	//
	// Uses RunFollowUpNoTools to avoid the LLM wasting time on tool calls
	// (v38 observed 3-8 minutes per scenario when tools were exposed vs. ~20s
	// without tools — the LLM wouldn't know when to stop calling ripgrep).
	//
	// Batching: entry points are split into chunks of maxScenarioChunkSize to
	// prevent JSON truncation.  When a chunk produces 0 scenarios, it is retried
	// once with an explicit "strict JSON" reminder (mirrors runUTAnalysis).
	if len(workerResult.EntryPoints) > 0 && workerModeEnabled(task.Modes, "e2e") {
		workerResult.EntryScenarios = w.runScenarioAnalysis(
			ctx, taskID, result.Content, task.ChangedFunctions, workerResult.EntryPoints, task.ExtraPrompt,
		)
		log.Infow("worker.scenarios_done",
			"repo", task.RepoName,
			"entry_points", len(workerResult.EntryPoints),
			"scenarios", len(workerResult.EntryScenarios),
		)
	}

	// UT follow-up: for each diff-changed function this Worker handled,
	// generate unit-test suggestions (constraints + mock-based scenarios).
	// Uses no-tools RunFollowUp to keep this cheap (~20-40s per batch).
	// Skipped for tasks with only FILE_CHANGED sentinels (no named functions).
	realFuncs := make([]string, 0, len(task.ChangedFunctions))
	for _, fn := range task.ChangedFunctions {
		if !strings.HasPrefix(fn, "FILE_CHANGED:") &&
			!strings.HasPrefix(fn, "[search all changed functions in file:") {
			realFuncs = append(realFuncs, fn)
		}
	}
	if len(realFuncs) > 0 && workerModeEnabled(task.Modes, "ut") {
		workerResult.UTAnalyses = w.runUTAnalysis(ctx, taskID, result.Content, realFuncs, task.ExtraPrompt)
	}

	return workerResult, nil
}

// runScenarioAnalysis generates per-entry-point test scenarios for the given entry points.
//
// Resilience strategy (mirrors runUTAnalysis):
//  1. Split entry points into chunks of maxScenarioChunkSize (5) — 18 entry points at once
//     caused JSON truncation in shadow-v1 run (worker.scenarios_done scenarios=0).
//  2. Run each chunk via a no-tools follow-up.
//  3. Retry once per chunk if the chunk returned 0 scenarios (JSON parse failure).
//  4. Deduplicate by (entry_function, entry_file) across all chunks.
func (w *WorkerAgent) runScenarioAnalysis(
	ctx context.Context,
	taskID string,
	priorContent string,
	changedFunctions []string,
	entryPoints []CallNode,
	extraPrompt string,
) []EntryPointScenario {
	log := logger.S()
	const maxScenarioChunkSize = 5

	// Split entry points into chunks.
	chunks := make([][]CallNode, 0, (len(entryPoints)+maxScenarioChunkSize-1)/maxScenarioChunkSize)
	for i := 0; i < len(entryPoints); i += maxScenarioChunkSize {
		end := i + maxScenarioChunkSize
		if end > len(entryPoints) {
			end = len(entryPoints)
		}
		chunks = append(chunks, entryPoints[i:end])
	}

	allResults := make([]EntryPointScenario, 0, len(entryPoints))
	// Track by (function, file) to deduplicate across chunks.
	type epKey struct{ fn, file string }
	seen := make(map[epKey]bool)

	for idx, chunk := range chunks {
		chunkStart := time.Now()
		prompt := buildScenarioFollowUp(changedFunctions, chunk, extraPrompt)
		resp, err := w.loop.RunFollowUpNoTools(ctx, taskID, priorContent, prompt)
		if err != nil {
			log.Warnw("worker.scenario_chunk_failed",
				"repo", taskID,
				"chunk", idx,
				"err", err.Error(),
			)
			continue
		}

		parsed := parseEntryScenarios(resp.Content)

		// Retry once if chunk returned nothing.
		if len(parsed) == 0 {
			retryPrompt := prompt + "\n\nIMPORTANT: Your previous response was not valid JSON. " +
				"Output ONLY a JSON object in a ```json fenced block. No prose. " +
				fmt.Sprintf("Cover ALL %d entry points listed above.", len(chunk))
			retryResp, retryErr := w.loop.RunFollowUpNoTools(ctx, taskID, priorContent, retryPrompt)
			if retryErr == nil && retryResp != nil {
				parsed = parseEntryScenarios(retryResp.Content)
				log.Infow("worker.scenario_chunk_retried",
					"repo", taskID,
					"chunk", idx,
					"recovered", len(parsed),
				)
			}
		}

		for _, s := range parsed {
			k := epKey{s.EntryFunction, s.EntryFile}
			if seen[k] {
				continue
			}
			seen[k] = true
			allResults = append(allResults, s)
		}

		log.Infow("worker.scenario_chunk_done",
			"chunk", idx,
			"entry_points", len(chunk),
			"returned", len(parsed),
			"duration_ms", time.Since(chunkStart).Milliseconds(),
		)
	}

	log.Infow("worker.scenario_analysis_done",
		"total_entry_points", len(entryPoints),
		"total_scenarios", len(allResults),
		"chunks", len(chunks),
	)
	return allResults
}

// runUTAnalysis generates UT suggestions for the given changed functions.
//
// Resilience strategy:
//  1. Split the function list into chunks of maxUTChunkSize (8) — large lists
//     caused the LLM to output truncated JSON (observed with 13 funcs → 0 results).
//  2. Run each chunk via a no-tools follow-up.
//  3. Parse results; if a chunk returned 0 analyses, do ONE retry with an
//     explicit "output STRICT JSON" prompt.
//  4. Identify any funcs still missing after all chunks; do one final retry
//     targeting only those functions.
//  5. Deduplicate by function name across all results.
func (w *WorkerAgent) runUTAnalysis(
	ctx context.Context,
	taskID string,
	priorContent string,
	realFuncs []string,
	extraPrompt string,
) []UTAnalysis {
	log := logger.S()
	const maxUTChunkSize = 8

	// Split into chunks.
	chunks := make([][]string, 0, (len(realFuncs)+maxUTChunkSize-1)/maxUTChunkSize)
	for i := 0; i < len(realFuncs); i += maxUTChunkSize {
		end := i + maxUTChunkSize
		if end > len(realFuncs) {
			end = len(realFuncs)
		}
		chunks = append(chunks, realFuncs[i:end])
	}

	allResults := make([]UTAnalysis, 0)
	// seenFuncs keys on the QUALIFIED function name (full entry from realFuncs,
	// e.g. "vstation_compute/compute/service/dispatch.py::handler") rather than
	// just the base name.  This prevents false dedup when two different repos both
	// export a function with the same short name (e.g. "handler", "init", "next").
	seenFuncs := make(map[string]bool)
	// baseToQualified maps the base function name back to its full qualified form
	// so that LLM-returned UTAnalysis (which carries only the short name) can be
	// matched back to the right qualified key.
	baseToQualified := make(map[string]string)
	for _, fn := range realFuncs {
		base := fn
		if i := strings.LastIndex(fn, "::"); i >= 0 {
			base = fn[i+2:]
		}
		// If two repos have the same base name, prefer the first (arbitrary but
		// deterministic). The sweep below uses the qualified key; duplicates are
		// tolerated because both will be emitted.
		if _, exists := baseToQualified[base]; !exists {
			baseToQualified[base] = fn
		}
	}

	for idx, chunk := range chunks {
		chunkStart := time.Now()
		prompt := buildUTFollowUp(chunk, extraPrompt)
		resp, err := w.loop.RunFollowUpNoTools(ctx, taskID, priorContent, prompt)
		if err != nil {
			log.Warnw("worker.ut_chunk_failed",
				"repo", taskID,
				"chunk", idx,
				"err", err.Error(),
			)
			continue
		}

		parsed := parseUTAnalyses(resp.Content)

		// Retry once if chunk returned nothing — LLM likely gave prose instead of JSON.
		if len(parsed) == 0 {
			retryPrompt := prompt + "\n\nIMPORTANT: Your previous response was not valid JSON. " +
				"Output ONLY a JSON object in a ```json fenced block. No prose. " +
				"Cover ALL " + fmt.Sprintf("%d", len(chunk)) + " functions listed above."
			retryResp, retryErr := w.loop.RunFollowUpNoTools(ctx, taskID, priorContent, retryPrompt)
			if retryErr == nil && retryResp != nil {
				parsed = parseUTAnalyses(retryResp.Content)
				log.Infow("worker.ut_chunk_retried",
					"repo", taskID,
					"chunk", idx,
					"recovered", len(parsed),
				)
			}
		}

		for _, a := range parsed {
			// Resolve the qualified key for this result: prefer the full qualified
			// form from realFuncs to avoid collisions between repos sharing base names.
			qualKey := a.FuncName
			if q, ok := baseToQualified[a.FuncName]; ok {
				qualKey = q
			}
			if seenFuncs[qualKey] {
				continue
			}
			seenFuncs[qualKey] = true
			allResults = append(allResults, a)
		}

		log.Infow("worker.ut_chunk_done",
			"chunk", idx,
			"funcs", len(chunk),
			"returned", len(parsed),
			"duration_ms", time.Since(chunkStart).Milliseconds(),
		)
	}

	// Final sweep: any requested func without analysis → one last targeted call.
	var missing []string
	for _, fn := range realFuncs {
		// Use the qualified key directly — seenFuncs is now keyed on qualified names.
		if !seenFuncs[fn] {
			missing = append(missing, fn)
		}
	}
	if len(missing) > 0 && len(missing) <= maxUTChunkSize {
		sweepStart := time.Now()
		prompt := buildUTFollowUp(missing, extraPrompt)
		resp, err := w.loop.RunFollowUpNoTools(ctx, taskID, priorContent, prompt)
		if err == nil && resp != nil {
			parsed := parseUTAnalyses(resp.Content)
			for _, a := range parsed {
				qualKey := a.FuncName
				if q, ok := baseToQualified[a.FuncName]; ok {
					qualKey = q
				}
				if seenFuncs[qualKey] {
					continue
				}
				seenFuncs[qualKey] = true
				allResults = append(allResults, a)
			}
			log.Infow("worker.ut_sweep_done",
				"missing_before", len(missing),
				"recovered", len(parsed),
				"duration_ms", time.Since(sweepStart).Milliseconds(),
			)
		}
	}

	log.Infow("worker.ut_done",
		"funcs", len(realFuncs),
		"ut_analyses", len(allResults),
		"chunks", len(chunks),
	)
	return allResults
}

// workerModeEnabled returns true when the given mode should run.
// An empty or nil modes slice means "run all" (default full analysis).
func workerModeEnabled(modes []string, mode string) bool {
	if len(modes) == 0 {
		return true
	}
	for _, m := range modes {
		if strings.EqualFold(m, mode) {
			return true
		}
	}
	return false
}

// parseWorkerOutput extracts structured data from LLM output.
// Strategy order:
//  1. Extract ```json ... ``` fenced block and unmarshal.
//  2. Find a bare JSON object in the output.
//  3. Parse prose output heuristically (extract file paths, function names).
func parseWorkerOutput(repoName, content string) *WorkerResult {
	log := logger.S()
	out := &WorkerResult{RepoName: repoName}

	raw := extractJSON(content)
	if raw == "" {
		// Fallback: no JSON block found.
		// Store a single node with a short summary (truncated to avoid noise).
		summary := content
		if len(summary) > 300 {
			summary = summary[:300] + "..."
		}
		out.Nodes = []CallNode{{Repo: repoName, Function: summary}}
		return out
	}

	var parsed llmWorkerOutput
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		// JSON block was found but could not be parsed (e.g. trailing comma, unescaped
		// newline inside a string value). Do NOT write the raw content as a Function —
		// that would produce a node whose Function field is a multi-kilobyte blob.
		// Instead, log and return an empty result so the caller can treat this Worker
		// as producing no structured output, which is far less harmful than injecting
		// garbage nodes into the call graph.
		log.Warnw("worker.parse_json_failed",
			"repo", repoName,
			"err", err.Error(),
			"raw_len", len(raw),
		)
		return out
	}

	// Convert nodes — skip pseudo-nodes where the LLM emitted an import
	// statement or bare module name instead of a real function symbol.
	// Heuristics: function name starts with "import " (Python import stmt)
	// or contains only lowercase module-path tokens with no parens/brackets
	// and line == 0 (LLM hallucinated position).
	for _, n := range parsed.Nodes {
		if isPseudoNode(n.Function) {
			continue
		}
		out.Nodes = append(out.Nodes, CallNode{
			Repo:     coalesce(n.Repo, repoName),
			File:     n.File,
			Line:     n.Line,
			Function: n.Function,
		})
	}

	// Convert cross-repo calls.
	// IMPORTANT: TargetFunction in the next Worker's task must be the
	// CallerFunction (the function in the target repo that makes the call),
	// NOT the function in the current repo being called.
	for _, c := range parsed.CrossRepoCalls {
		if c.ToRepo == "" || c.ToRepo == repoName {
			continue
		}
		// Resolve the caller function name: prefer caller_function, fall back to via_function.
		callerFunc := c.CallerFunction
		if callerFunc == "" {
			callerFunc = c.ViaFunction
		}
		out.CrossRepoCalls = append(out.CrossRepoCalls, CrossRepoCall{
			TargetRepo:     c.ToRepo,
			TargetFunction: callerFunc, // next Worker will search for THIS in to_repo
			CallerNode: CallNode{
				Repo:     repoName,
				File:     c.CallerFile,
				Line:     c.CallerLine,
				Function: c.TargetFunction, // the function in current repo that was called
			},
		})
	}

	// Convert entry points.
	for _, e := range parsed.EntryPoints {
		if e.Repo == "" {
			continue
		}
		out.EntryPoints = append(out.EntryPoints, CallNode{
			Repo:     e.Repo,
			File:     e.File,
			Line:     e.Line,
			Function: coalesce(e.Endpoint, e.Function),
		})
		out.ReachedEntry = true
	}

	// Also handle the orchestrator-style changed_functions format.
	for _, cf := range parsed.ChangedFunctions {
		// Add call chain nodes to the global node list.
		for _, n := range cf.CallChain {
			if isPseudoNode(n.Function) {
				continue
			}
			out.Nodes = append(out.Nodes, CallNode{
				Repo:     coalesce(n.Repo, repoName),
				File:     n.File,
				Line:     n.Line,
				Function: n.Function,
			})
			// If this node is in a different repo, it's a cross-repo call.
			if n.Repo != "" && n.Repo != repoName {
				out.CrossRepoCalls = append(out.CrossRepoCalls, CrossRepoCall{
					TargetRepo:     n.Repo,
					TargetFunction: n.Function,
					// CallerNode describes where in the current repo the call is made.
					// Use n.File / n.Line — the LLM records the call site there, not on cf.
					CallerNode: CallNode{
						Repo:     repoName,
						File:     n.File,
						Line:     n.Line,
						Function: cf.Name,
					},
				})
			}
		}
		// Add entry points.
		for _, ep := range cf.EntryPoints {
			if ep.Repo == "" {
				continue
			}
			out.EntryPoints = append(out.EntryPoints, CallNode{
				Repo:     ep.Repo,
				File:     ep.File,
				Line:     ep.Line,
				Function: coalesce(ep.Endpoint, ep.Function),
			})
			out.ReachedEntry = true
		}

		// Parse constraints and test scenarios.
		fa := FunctionAnalysis{
			Name:          cf.Name,
			Repo:          coalesce(cf.Repo, repoName),
			ExistingTests: cf.ExistingTests,
		}
		for _, c := range cf.Constraints {
			fa.Constraints = append(fa.Constraints, Constraint{
				Type:      c.Type,
				Condition: c.Condition,
				File:      c.File,
				Line:      c.Line,
				Note:      c.Note,
			})
		}
		for _, s := range cf.SuggestedScenarios {
			fa.SuggestedScenarios = append(fa.SuggestedScenarios, TestScenario{
				Type:        s.Type,
				Description: s.Description,
				Input:       s.Input,
				Expected:    s.Expected,
				Priority:    s.Priority,
			})
		}
		if len(fa.Constraints) > 0 || len(fa.SuggestedScenarios) > 0 || len(fa.ExistingTests) > 0 {
			out.FunctionAnalyses = append(out.FunctionAnalyses, fa)
		}
	}

	// Parse raw search results for the fallback cross-repo extractor.
	for _, sr := range parsed.SearchResults {
		if sr.File == "" {
			continue
		}
		out.SearchResults = append(out.SearchResults, SearchResult{
			File:     sr.File,
			Line:     sr.Line,
			Function: sr.Function,
		})
	}
	// Also collect search hits from call_chain nodes in changed_functions
	// (LLM sometimes puts them there instead of search_results).
	for _, cf := range parsed.ChangedFunctions {
		for _, n := range cf.CallChain {
			if n.File != "" {
				out.SearchResults = append(out.SearchResults, SearchResult{
					File:     n.File,
					Line:     n.Line,
					Function: n.Function,
				})
			}
		}
	}

	// Propagate wide_impact function names so the Orchestrator can rescue
	// same-repo entry points that were truncated by the expansion threshold.
	out.WideImpactFunctions = append(out.WideImpactFunctions, parsed.WideImpactFunctions...)
	// Also collect per-function wide_impact flags from changed_functions.
	for _, cf := range parsed.ChangedFunctions {
		if cf.WideImpact && cf.Name != "" {
			out.WideImpactFunctions = append(out.WideImpactFunctions, cf.Name)
		}
	}

	return out
}

// isPseudoNode returns true when a function name is not a real callable symbol
// but rather an import statement, a module-level variable, or some other
// non-function construct that the LLM mistakenly emitted as a call-chain node.
//
// Detected patterns (all case-insensitive start checks):
//   - "import "         → Python/Go import statement verbatim
//   - "from "           → Python "from X import Y" statement
//   - "require("        → Node.js require() at module level
//   - "#"               → comment line
//
// Additionally any name that is entirely whitespace or empty is filtered.
func isPseudoNode(name string) bool {
	if name == "" {
		return true
	}
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return true
	}
	lower := strings.ToLower(trimmed)
	return strings.HasPrefix(lower, "import ") ||
		strings.HasPrefix(lower, "from ") ||
		strings.HasPrefix(lower, "require(") ||
		strings.HasPrefix(lower, "#")
}

// extractJSON finds a JSON object in LLM output.
// First tries ```json ... ``` fenced blocks, then bare { ... }.
//
// Both strategies use brace-depth counting rather than greedy regex / LastIndex
// so that multiple JSON blocks in one response are not merged into invalid JSON.
func extractJSON(content string) string {
	// Strategy 1: fenced code block — anchor on the opening { right after ```json.
	if m := fencedJSONStartRe.FindStringSubmatchIndex(content); len(m) >= 4 {
		braceStart := m[2] // index of the '{' captured by group 1
		if obj := extractBalancedJSON(content, braceStart); obj != "" {
			return obj
		}
	}

	// Strategy 2: bare JSON — first { to its matching }.
	start := strings.Index(content, "{")
	if start >= 0 {
		if obj := extractBalancedJSON(content, start); obj != "" {
			return obj
		}
	}

	return ""
}

// extractBalancedJSON returns the JSON object starting at content[startIdx]
// (which must be '{') by counting brace depth until the matching '}' is found.
// Returns "" if no valid JSON object is found or if the extracted string
// fails json.Unmarshal.
func extractBalancedJSON(content string, startIdx int) string {
	if startIdx < 0 || startIdx >= len(content) || content[startIdx] != '{' {
		return ""
	}
	depth := 0
	inStr := false
	escaped := false
	for i := startIdx; i < len(content); i++ {
		c := content[i]
		if escaped {
			escaped = false
			continue
		}
		if c == '\\' && inStr {
			escaped = true
			continue
		}
		if c == '"' {
			inStr = !inStr
			continue
		}
		if inStr {
			continue
		}
		if c == '{' {
			depth++
		} else if c == '}' {
			depth--
			if depth == 0 {
				candidate := content[startIdx : i+1]
				var tmp map[string]interface{}
				if json.Unmarshal([]byte(candidate), &tmp) == nil {
					return candidate
				}
				return ""
			}
		}
	}
	return ""
}

func coalesce(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func formatList(items []string) string {
	if len(items) == 0 {
		return "  (none)\n"
	}
	var sb strings.Builder
	for _, item := range items {
		sb.WriteString("  - ")
		sb.WriteString(item)
		sb.WriteString("\n")
	}
	return sb.String()
}

// extraPromptSection returns a formatted extra-prompt block to append to follow-up
// prompts, or an empty string when extraPrompt is blank.
func extraPromptSection(extraPrompt string) string {
	if extraPrompt == "" {
		return ""
	}
	return "\n\n--- Business context (provided by the caller, use to improve scenario accuracy) ---\n" + extraPrompt + "\n--- End of business context ---"
}

// buildScenarioFollowUp constructs the follow-up prompt asking the LLM to generate
// per-entry-point test scenarios based on the diff-changed functions and found entry points.
func buildScenarioFollowUp(changedFunctions []string, entryPoints []CallNode, extraPrompt string) string {
	// Build entry point list.
	var epList strings.Builder
	for _, ep := range entryPoints {
		fmt.Fprintf(&epList, "  - %s (%s:%d)\n", ep.Function, ep.File, ep.Line)
	}
	// Build changed functions list.
	var cfList strings.Builder
	for _, fn := range changedFunctions {
		fmt.Fprintf(&cfList, "  - %s\n", fn)
	}

	return fmt.Sprintf(`Based on your call-chain analysis above, you found these entry points:
%s
The diff changed these functions:
%s
For EACH entry point above, generate test scenarios that specifically cover the diff changes reachable via that entry.

Guidelines:
- changed_via: list only the diff-changed functions in the call path to this entry (subset of the changed functions above)
- preconditions: required system state before calling this entry (e.g. "instance exists", "encryption key valid")
- typical_inputs: key parameter description for a normal call through this entry
- scenarios: P0=core flow (must test), P1=boundary/error/null-check, P2=compat/edge
  Derive scenarios from the actual diff changes above — do NOT invent scenarios unrelated to the changed code.

For EACH scenario, MUST include an "oracles" array listing verification points.
An oracle tells the test engineer WHERE to verify the expected outcome — not just
"assert success". Look at the code you traced: writes to DB, file operations,
libvirt XML changes, MQ publishes, log lines, metrics — each of these is an oracle.

Oracle types to use:
- db           : database row/field check              (target: "cvm.instance.encrypt_status")
- file_state   : file/device attribute                 (target: "/dev/vdb size")
- host_process : host process/cmd observable           (target: "virsh list")
- libvirt_xml  : libvirt domain XML changes            (target: "/etc/libvirt/qemu/<id>.xml")
- mq           : MQ message published                  (target: "host.terminate topic")
- rpc          : RPC call observed in downstream       (target: "vstation_compute_access.dispatch:VMachineCreate")
- log          : log line emitted                      (target: "encrypt_disk.py:init_sm4_dev log_info 'SM4 key loaded'")
- metric       : monitoring counter/gauge              (target: "cvm_encrypt_success_total")
- external_api : external system state                 (target: "CBS disk.status")

Each oracle must have: {type, target, assertion}. Give concrete targets from the
code you traced — NOT generic phrases like "check the result".

Output ONLY this JSON (no explanation):
`+"```json"+`
{
  "entry_scenarios": [
    {
      "entry_function": "<entry function name>",
      "entry_file": "<entry file path>",
      "changed_via": ["<changed function 1>", "<changed function 2>"],
      "preconditions": ["<precondition 1>"],
      "typical_inputs": "<key params description>",
      "scenarios": [
        {
          "priority":"P0","type":"normal",
          "description":"<desc>","input":"<key input>","expected":"<expected result>",
          "oracles":[
            {"type":"db","target":"cvm.instance.status","assertion":"= 'running' within 30s"},
            {"type":"log","target":"encrypt_disk.py:init_sm4_dev","assertion":"log_info 'SM4 dev initialized' emitted once"}
          ]
        }
      ]
    }
  ]
}
`+"```"+extraPromptSection(extraPrompt),
		epList.String(),
		cfList.String(),
	)
}

// parseEntryScenarios extracts entry-point scenario data from LLM output.
// Returns an empty slice if parsing fails (graceful degradation).
func parseEntryScenarios(content string) []EntryPointScenario {
	raw := extractJSON(content)
	if raw == "" {
		return nil
	}
	var parsed llmEntryScenariosOutput
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil
	}
	result := make([]EntryPointScenario, 0, len(parsed.EntryScenarios))
	for _, es := range parsed.EntryScenarios {
		eps := EntryPointScenario{
			EntryFunction: es.EntryFunction,
			EntryFile:     es.EntryFile,
			ChangedVia:    es.ChangedVia,
			Preconditions: es.Preconditions,
			TypicalInputs: es.TypicalInputs,
		}
		for _, s := range es.Scenarios {
			ts := TestScenario{
				Type:        s.Type,
				Description: s.Description,
				Input:       s.Input,
				Expected:    s.Expected,
				Priority:    s.Priority,
			}
			for _, o := range s.Oracles {
				ts.Oracles = append(ts.Oracles, TestOracle{
					Type:      o.Type,
					Target:    o.Target,
					Assertion: o.Assertion,
				})
			}
			eps.Scenarios = append(eps.Scenarios, ts)
		}
		result = append(result, eps)
	}
	return result
}

// ---------------------------------------------------------------------------
// UT (unit-test) follow-up — generates per-function UT suggestions focused on
// the function's internal behaviour rather than end-to-end flow.
// ---------------------------------------------------------------------------

// llmUTScenario is the JSON shape expected from the UT follow-up LLM call.
type llmUTScenario struct {
	Priority    string `json:"priority"`
	Type        string `json:"type"`
	Description string `json:"description"`
	MockSetup   string `json:"mock_setup,omitempty"`
	Assertions  string `json:"assertions,omitempty"`
}

type llmUTAnalysis struct {
	Name          string          `json:"name"`
	File          string          `json:"file"`
	Summary       string          `json:"summary,omitempty"`
	Constraints   []string        `json:"constraints,omitempty"`
	ExistingTests []string        `json:"existing_tests,omitempty"`
	Scenarios     []llmUTScenario `json:"scenarios,omitempty"`
}

type llmUTOutput struct {
	UTAnalyses []llmUTAnalysis `json:"ut_analyses"`
}

// buildUTFollowUp asks the LLM for unit-test suggestions focused on each
// changed function's internal logic.
func buildUTFollowUp(changedFunctions []string, extraPrompt string) string {
	var cfList strings.Builder
	for _, fn := range changedFunctions {
		fmt.Fprintf(&cfList, "  - %s\n", fn)
	}

	return fmt.Sprintf(`You traced call chains above. Now step back from the call graph and focus on the
CHANGED FUNCTIONS THEMSELVES (their code bodies you saw via ripgrep).

Changed functions to cover (%d total — you MUST produce one analysis block per function):
%s

CRITICAL REQUIREMENTS:
- Produce EXACTLY one entry in "ut_analyses" for EACH function above.
- Do NOT skip functions. If you cannot generate high-quality scenarios, still
  produce an entry with "summary" and at least 2 scenarios.
- If a function name contains "::", use only the part AFTER "::" as the "name"
  field value (e.g. "vstation_compute/compute/disk/encrypt_disk.py::_get_block_dev_size"
  → name="_get_block_dev_size", file="vstation_compute/compute/disk/encrypt_disk.py").

For EACH function, produce unit-test suggestions that focus on:
- Branches introduced by the diff (new conditionals, fallback paths)
- Exception paths the new code handles (errors, None/null guards, etc.)
- Compatibility or platform-specific branches only if they appear in the actual diff
- Boundary values of relevant parameters (size, length, count, etc.)
- Correct interaction with external dependencies the function touches
  (infer from the actual code: DB calls, file I/O, RPC, subprocess, etc.)

UT scenarios should use MOCK setups (not real integration). Show concrete mocks
based on the actual code — do NOT invent mocks for APIs not present in the diff.

Output ONLY this JSON in a ` + "```json" + ` fenced block — no prose before or after:
` + "```json" + `
{
  "ut_analyses": [
    {
      "name": "<function name>",
      "file": "<file path>",
      "summary": "<one-line purpose of the change>",
      "constraints": [
        "<key constraint or invariant introduced by the diff>",
        "<another constraint if applicable>"
      ],
      "existing_tests": ["<existing test locations if any>"],
      "scenarios": [
        {
          "priority":"P0","type":"normal",
          "description":"<describe the happy-path scenario for this function>",
          "mock_setup":"<concrete mock setup derived from the actual code>",
          "assertions":"<what to assert and where>"
        },
        {
          "priority":"P1","type":"exception",
          "description":"<describe the error/edge scenario>",
          "mock_setup":"<mock setup for error path>",
          "assertions":"<what to assert>"
        }
      ]
    }
  ]
}
`+"```"+extraPromptSection(extraPrompt),
		len(changedFunctions),
		cfList.String(),
	)
}

// parseUTAnalyses extracts UT scenario data from LLM output.
// Returns an empty slice if parsing fails (graceful degradation).
func parseUTAnalyses(content string) []UTAnalysis {
	raw := extractJSON(content)
	if raw == "" {
		return nil
	}
	var parsed llmUTOutput
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil
	}
	out := make([]UTAnalysis, 0, len(parsed.UTAnalyses))
	for _, a := range parsed.UTAnalyses {
		ut := UTAnalysis{
			FuncName:      a.Name,
			FilePath:      a.File,
			Summary:       a.Summary,
			Constraints:   a.Constraints,
			ExistingTests: a.ExistingTests,
		}
		for _, s := range a.Scenarios {
			ut.Scenarios = append(ut.Scenarios, UTScenario{
				Priority:    s.Priority,
				Type:        s.Type,
				Description: s.Description,
				MockSetup:   s.MockSetup,
				Assertions:  s.Assertions,
			})
		}
		out = append(out, ut)
	}
	return out
}

// backfillEmptyFiles fills in the File field for nodes and entry-points whose
// File is empty ("") but whose Function name is non-empty.  The LLM sometimes
// knows the function name but omits the file path, leaving File="".
//
// For each such node, backfillEmptyFiles runs a ripgrep search (using
// namingVariants for robustness) across the repo directory.  When a match is
// found, File and Line are updated in-place.  Nodes that still have File=""
// after this pass are left as-is; filterGhostNodes will subsequently exempt
// them via the fileExists("") == true short-circuit, so they are never dropped.
//
// This function must run BEFORE filterGhostNodes to avoid that short-circuit
// masking File="" nodes that could otherwise be resolved.
func backfillEmptyFiles(result *WorkerResult, repoPath, workspaceDir string) {
	log := logger.S()
	repoBase := filepath.Base(repoPath)

	// Shared 8-second timeout across all backfill searches.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	// tryFill attempts to locate funcName on disk and returns the updated
	// (File, Line) pair.  It searches repoPath first, then workspaceDir.
	tryFill := func(funcName, nodeRepo string) (string, int, bool) {
		if funcName == "" {
			return "", 0, false
		}
		// Determine search roots.  Cross-repo nodes are more likely to be found
		// under workspaceDir than under the current repoPath.
		searchDirs := []string{repoPath}
		if workspaceDir != "" && workspaceDir != repoPath {
			if nodeRepo != "" && nodeRepo != result.RepoName {
				searchDirs = []string{workspaceDir}
			} else {
				searchDirs = append(searchDirs, workspaceDir)
			}
		}
		for _, pattern := range namingVariants(funcName) {
			if ctx.Err() != nil {
				return "", 0, false
			}
			for _, dir := range searchDirs {
				file, line, ok := toolSearchSymbol(ctx, pattern, dir)
				if ok {
					// Convert to a consistent workspace-relative path.
					relFile := file
					if dir != workspaceDir {
						relFile = filepath.Join(repoBase, file)
					}
					return relFile, line, true
				}
			}
		}
		return "", 0, false
	}

	// Backfill Nodes.
	for i := range result.Nodes {
		if result.Nodes[i].File != "" {
			continue
		}
		if relFile, line, ok := tryFill(result.Nodes[i].Function, result.Nodes[i].Repo); ok {
			log.Infow("worker.backfill_empty_file",
				"repo", result.RepoName,
				"func", result.Nodes[i].Function,
				"file", relFile,
			)
			result.Nodes[i].File = relFile
			result.Nodes[i].Line = line
		}
	}

	// Backfill EntryPoints.
	for i := range result.EntryPoints {
		if result.EntryPoints[i].File != "" {
			continue
		}
		if relFile, line, ok := tryFill(result.EntryPoints[i].Function, result.EntryPoints[i].Repo); ok {
			log.Infow("worker.backfill_empty_file",
				"repo", result.RepoName,
				"func", result.EntryPoints[i].Function,
				"file", relFile,
			)
			result.EntryPoints[i].File = relFile
			result.EntryPoints[i].Line = line
		}
	}
}

// filterGhostNodes removes CallNode entries whose File field refers to a path
// that does not exist under repoPath on disk. Such entries are almost always
// LLM hallucinations — the model invented a plausible-sounding file path
// (e.g. "api/ResetInstanceNetType.py") instead of admitting it could not find
// the function via ripgrep.
//
// Nodes with an empty File field are kept unconditionally: they are legitimate
// results where the LLM correctly omitted a file path rather than guessing.
//
// Ghost nodes are first attempted for rescue via deterministic ripgrep searches
// using naming variants of the function name (exact → snake_case → partial keyword).
// The search covers:
//  1. The current repo directory (repoPath) for same-repo ghost nodes.
//  2. The full workspace directory (workspaceDir) for cross-repo ghost nodes,
//     which lets us recover hallucinated paths like "cvm_api/api/Foo.py" whose
//     function actually lives in a different subdirectory under the workspace.
//
// Rescued nodes have their File/Line updated to the real path found on disk.
// Only nodes that cannot be rescued after all variants are tried are added to
// result.GhostFiles so the API layer can surface actionable warnings.
// GhostFiles is deduplicated so each path appears at most once.
func filterGhostNodes(result *WorkerResult, repoPath, workspaceDir string, newFuncNames []string) {
	log := logger.S()
	repoBase := filepath.Base(repoPath)

	// Build a set of newly-added function names so we can exempt them from the ghost
	// check. These functions exist only in a feature-branch worktree (or may have an
	// empty file path), so they must not be silently dropped.
	newFuncSet := make(map[string]bool, len(newFuncNames))
	for _, fn := range newFuncNames {
		newFuncSet[fn] = true
	}
	// isNewFunc returns true when the node's Function field is a newly-added function.
	// We strip any "file::funcname" qualifier to match the bare function name.
	isNewFunc := func(funcName string) bool {
		if len(newFuncSet) == 0 {
			return false
		}
		bare := funcName
		if idx := strings.LastIndex(funcName, "::"); idx >= 0 {
			bare = funcName[idx+2:]
		}
		return newFuncSet[bare] || newFuncSet[funcName]
	}

	// cleanPath strips a leading "repoName/" prefix so we don't double-join.
	// e.g. "cvm_api/api/Foo.py" under repoPath "/workspace/cvm_api"
	//   → strip "cvm_api/" → "api/Foo.py"
	//   → filepath.Join("/workspace/cvm_api", "api/Foo.py")
	cleanPath := func(file string) string {
		if idx := strings.Index(file, "/"); idx > 0 && file[:idx] == repoBase {
			return file[idx+1:]
		}
		return file
	}

	// fileExists checks whether a non-empty path resolves to a real file.
	// It checks both under repoPath and (when workspaceDir is set) under workspaceDir.
	fileExists := func(file string) bool {
		if file == "" {
			return true // no path → keep
		}
		// Check under current repo first.
		if _, err := os.Stat(filepath.Join(repoPath, cleanPath(file))); err == nil {
			return true
		}
		// Also check as a workspace-relative path (covers cross-repo prefixed paths).
		if workspaceDir != "" {
			if _, err := os.Stat(filepath.Join(workspaceDir, file)); err == nil {
				return true
			}
		}
		return false
	}

	// --- Step 1: Separate valid nodes from ghost nodes ---

	var ghostNodes []CallNode
	filtered := result.Nodes[:0]
	for _, n := range result.Nodes {
		if fileExists(n.File) || isNewFunc(n.Function) {
			filtered = append(filtered, n)
			if !fileExists(n.File) && isNewFunc(n.Function) {
				log.Debugw("worker.ghost_exempt_new_func",
					"repo", result.RepoName,
					"func", n.Function,
					"file", n.File,
				)
			}
		} else {
			log.Warnw("worker.ghost_file_detected",
				"repo", result.RepoName,
				"func", n.Function,
				"file", n.File,
			)
			// All ghost nodes are eligible for rescue; cross-repo nodes use workspaceDir.
			ghostNodes = append(ghostNodes, n)
		}
	}
	result.Nodes = filtered

	var ghostEPs []CallNode
	filteredEP := result.EntryPoints[:0]
	for _, ep := range result.EntryPoints {
		if fileExists(ep.File) || isNewFunc(ep.Function) {
			filteredEP = append(filteredEP, ep)
			if !fileExists(ep.File) && isNewFunc(ep.Function) {
				log.Debugw("worker.ghost_exempt_new_func",
					"repo", result.RepoName,
					"func", ep.Function,
					"file", ep.File,
				)
			}
		} else {
			log.Warnw("worker.ghost_file_detected",
				"repo", result.RepoName,
				"func", ep.Function,
				"file", ep.File,
			)
			ghostEPs = append(ghostEPs, ep)
		}
	}
	result.EntryPoints = filteredEP

	// --- Step 2: Rescue search ---
	// Skip if nothing to rescue.
	if len(ghostNodes)+len(ghostEPs) == 0 {
		return
	}

	// Limit total rescue time to 10 seconds so we don't block the Worker.
	rescueCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// rescue attempts to find the real file for a ghost node by searching naming
	// variants of the function name. Returns the updated node and true on success.
	// Search order: repoPath first (most likely), then workspaceDir (cross-repo).
	rescue := func(n CallNode) (CallNode, bool) {
		// Determine which search roots to try. Cross-repo nodes (prefixed with a
		// different repo name or owned by another repo) need workspaceDir.
		searchDirs := []string{repoPath}
		if workspaceDir != "" && workspaceDir != repoPath {
			// Infer the owning repo from the file path prefix when available.
			// e.g. "cvm_api/api/Foo.py" → try workspaceDir (covers all repos).
			if n.Repo != "" && n.Repo != result.RepoName {
				// Known cross-repo: skip current repoPath, go straight to workspace.
				searchDirs = []string{workspaceDir}
			} else if strings.Contains(n.File, "/") {
				prefix := n.File[:strings.Index(n.File, "/")]
				if prefix != repoBase {
					// File has a different repo prefix: add workspaceDir as secondary search.
					searchDirs = append(searchDirs, workspaceDir)
				}
			}
		}

		for _, pattern := range namingVariants(n.Function) {
			if rescueCtx.Err() != nil {
				return n, false
			}
			for _, dir := range searchDirs {
				file, line, ok := toolSearchSymbol(rescueCtx, pattern, dir)
				if ok {
					// Build a workspace-relative path so it is consistent across repos.
					relFile := file
					if dir == workspaceDir {
						relFile = file // already relative to workspaceDir = workspace-relative
					} else {
						// repoPath search returns path relative to repoPath; prefix with repoBase.
						relFile = filepath.Join(repoBase, file)
					}
					log.Infow("worker.ghost_rescued",
						"repo", result.RepoName,
						"func", n.Function,
						"ghost_file", n.File,
						"rescued_file", relFile,
						"pattern", pattern,
					)
					n.File = relFile
					n.Line = line
					n.Source = "ghost_rescued"
					return n, true
				}
			}
		}
		return n, false
	}

	// ghostFilesSet deduplicates GhostFiles entries.
	ghostFilesSet := make(map[string]bool, len(result.GhostFiles))
	for _, f := range result.GhostFiles {
		ghostFilesSet[f] = true
	}
	addGhost := func(file string) {
		if !ghostFilesSet[file] {
			ghostFilesSet[file] = true
			result.GhostFiles = append(result.GhostFiles, file)
		}
	}

	for _, n := range ghostNodes {
		if rescued, ok := rescue(n); ok {
			result.Nodes = append(result.Nodes, rescued)
		} else {
			log.Warnw("worker.ghost_file_filtered",
				"repo", result.RepoName,
				"func", n.Function,
				"file", n.File,
				"repo_path", repoPath,
			)
			addGhost(n.File)
		}
	}
	for _, ep := range ghostEPs {
		if rescued, ok := rescue(ep); ok {
			result.EntryPoints = append(result.EntryPoints, rescued)
		} else {
			log.Warnw("worker.ghost_file_filtered",
				"repo", result.RepoName,
				"func", ep.Function,
				"file", ep.File,
				"repo_path", repoPath,
			)
			addGhost(ep.File)
		}
	}
}

// camelToSnake converts CamelCase (or mixedCase) identifiers to snake_case.
// e.g. "ResetInstanceNetType" → "reset_instance_net_type"
func camelToSnake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if i > 0 && isUpperASCII(r) {
			b.WriteByte('_')
		}
		b.WriteRune(toLowerASCII(r))
	}
	return b.String()
}

func isUpperASCII(r rune) bool { return r >= 'A' && r <= 'Z' }
func toLowerASCII(r rune) rune {
	if isUpperASCII(r) {
		return r + ('a' - 'A')
	}
	return r
}

// namingVariants returns a prioritised list of search patterns for funcName.
// Order: exact → snake_case → last-two-CamelCase-words → last-word-alone.
// Duplicates are suppressed.
func namingVariants(funcName string) []string {
	if funcName == "" {
		return nil
	}
	seen := map[string]bool{funcName: true}
	out := []string{funcName}

	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}

	// snake_case variant
	add(camelToSnake(funcName))

	// Split CamelCase into words for partial-keyword variants.
	var words []string
	start := 0
	for i := 1; i < len(funcName); i++ {
		if isUpperASCII(rune(funcName[i])) {
			words = append(words, funcName[start:i])
			start = i
		}
	}
	words = append(words, funcName[start:])

	if len(words) >= 2 {
		// Last two CamelCase words joined (e.g. "NetType" from "ResetInstanceNetType")
		add(strings.Join(words[len(words)-2:], ""))
		// Last single word, only if meaningful (> 3 chars)
		last := words[len(words)-1]
		if len(last) > 3 {
			add(last)
		}
	}
	return out
}

// toolSearchSymbol is a thin wrapper around tool.SearchSymbol so that
// worker_test.go can substitute it in tests without importing the tool package.
// In production this simply delegates to tool.SearchSymbol.
var toolSearchSymbol = func(ctx context.Context, pattern, dir string) (string, int, bool) {
	return searchSymbolImpl(ctx, pattern, dir)
}

// searchSymbolImpl delegates to tool.SearchSymbol.
func searchSymbolImpl(ctx context.Context, pattern, dir string) (string, int, bool) {
	return tool.SearchSymbol(ctx, pattern, dir)
}

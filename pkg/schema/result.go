package schema

// NodeType represents the role of a node in the call chain.
type NodeType string

const (
	NodeTypeEntry  NodeType = "entry"
	NodeTypeMiddle NodeType = "middle"
	NodeTypeLeaf   NodeType = "leaf"
)

// Direction indicates whether the call chain traces upward (to entry) or downward (to implementation).
type Direction string

const (
	DirectionUpward   Direction = "upward"
	DirectionDownward Direction = "downward"
)

// Protocol is the type of entry point protocol.
type Protocol string

const (
	ProtocolHTTP Protocol = "HTTP"
	ProtocolGRPC Protocol = "gRPC"
	ProtocolMQ   Protocol = "MQ"
	ProtocolCron Protocol = "Cron"
	ProtocolCLI  Protocol = "CLI"
)

// NodeSource indicates how a node was discovered during analysis.
type NodeSource string

const (
	// NodeSourceWorker means the node was directly identified by a Worker agent
	// searching within a repository.
	NodeSourceWorker NodeSource = "worker"
	// NodeSourceInferred means the node was inferred by the Orchestrator from
	// cross-repository call contracts (not directly observed by a Worker).
	NodeSourceInferred NodeSource = "inferred"
)

// CallNode represents a single function node in a call chain.
type CallNode struct {
	FuncName string     `json:"func_name"`
	FilePath string     `json:"file_path"`
	Line     int        `json:"line"`
	Repo     string     `json:"repo"`
	NodeType NodeType   `json:"node_type"`
	// Source indicates how this node was discovered: "worker" (directly observed
	// by a WorkerAgent) or "inferred" (deduced from cross-repo contracts by the
	// Orchestrator). Empty string means source is unknown (legacy data).
	Source   NodeSource `json:"source,omitempty"`
}

// CallEdge represents a directed edge between two call nodes.
type CallEdge struct {
	From CallNode `json:"from"`
	To   CallNode `json:"to"`
}

// CallChain represents a directed call chain with nodes and edges.
type CallChain struct {
	Nodes     []CallNode `json:"nodes"`
	Edges     []CallEdge `json:"edges"`
	Direction Direction  `json:"direction"`
}

// EntryPoint represents an integration test entry point.
type EntryPoint struct {
	Node          CallNode `json:"node"`
	Protocol      Protocol `json:"protocol"`
	Path          string   `json:"path"` // e.g. "POST /api/v1/payment/process"
	TestScenarios []string `json:"test_scenarios"`
	// Source indicates how this entry point was identified: "worker" (directly
	// found by a WorkerAgent inspecting the entry repo) or "inferred"
	// (deduced by the Orchestrator from cross-repo call contracts).
	Source NodeSource `json:"source,omitempty"`
	// Fields populated by scenario follow-up analysis.
	// ChangedVia lists the diff-changed functions in the call path to this entry.
	ChangedVia         []string                `json:"changed_via,omitempty"`
	Preconditions      []string                `json:"preconditions,omitempty"`
	TypicalInputs      string                  `json:"typical_inputs,omitempty"`
	SuggestedScenarios []SuggestedTestScenario `json:"suggested_scenarios,omitempty"`
}

// ImpactSummary summarizes the impact scope of the change.
type ImpactSummary struct {
	SourceRepo      string   `json:"source_repo"`       // repo where the diff originates
	DirectFunctions []string `json:"direct_functions"`  // functions directly impacted within same repo
	CrossRepoImpact []string `json:"cross_repo_impact"` // impacted callers in other repos
	DirectCount     int      `json:"direct_count"`
	CrossRepoCount  int      `json:"cross_repo_count"`
}

// TestScenario represents a suggested integration test scenario.
type TestScenario struct {
	EntryProtocol Protocol `json:"entry_protocol"`
	EntryPath     string   `json:"entry_path"`
	Description   string   `json:"description"`
}

// InputType describes what was provided as the analysis input.
type InputType string

const (
	InputTypeFuncName InputType = "func_name"
	InputTypeDiff     InputType = "diff"
	InputTypeFileLine InputType = "file_line"
)

// FunctionConstraint describes a code-level constraint found in a function body.
type FunctionConstraint struct {
	Type      string `json:"type"`      // "size_limit", "whitelist", "python_compat", "type_check", "null_check"
	Condition string `json:"condition"` // e.g. "block_size <= MAX_BLOCK_SIZE"
	FilePath  string `json:"file_path"`
	Line      int    `json:"line"`
	Note      string `json:"note,omitempty"`
}

// SuggestedTestScenario is a test scenario derived from constraint analysis.
type SuggestedTestScenario struct {
	Type        string   `json:"type"`        // "normal", "boundary", "exception", "compat", "whitelist"
	Description string   `json:"description"`
	Input       string   `json:"input,omitempty"`
	Expected    string   `json:"expected,omitempty"`
	Priority    string   `json:"priority"` // "P0", "P1", "P2"
	// Oracles lists observation points for this scenario — places a test
	// engineer should check to confirm the expected outcome. Each oracle
	// describes a concrete verification target (DB field, file path, process,
	// log line, MQ topic, metric, etc.).
	Oracles []TestOracle `json:"oracles,omitempty"`
}

// TestOracle identifies a verification target for a test scenario.
// Oracle types commonly encountered in this codebase:
//   - db: database row/field assertion (e.g. "cvm.instance.encrypt_status = 'active'")
//   - file_state: file/device attribute (e.g. "/dev/vdb size > 0")
//   - host_process: process/cmd observable on host (e.g. "virsh list contains ins-xxx")
//   - mq: MQ topic message publish
//   - rpc: RPC request visible in target service
//   - log: log line emitted (e.g. "log_info('SM4 key loaded')")
//   - metric: monitoring counter/gauge (e.g. "cvm_encrypt_success_total")
//   - external_api: external system state (e.g. "CBS disk.status = 'mounted'")
type TestOracle struct {
	Type      string `json:"type"`      // category (db / file_state / host_process / mq / rpc / log / metric / external_api)
	Target    string `json:"target"`    // what to inspect (table.field, path, topic, log source, etc.)
	Assertion string `json:"assertion"` // expected value or condition to observe
}

// FunctionAnalysis holds constraint and test scenario data for a single changed function.
type FunctionAnalysis struct {
	FuncName           string                  `json:"func_name"`
	Repo               string                  `json:"repo"`
	FilePath           string                  `json:"file_path"`
	Constraints        []FunctionConstraint    `json:"constraints,omitempty"`
	ExistingTests      []string                `json:"existing_tests,omitempty"`
	SuggestedScenarios []SuggestedTestScenario `json:"suggested_scenarios,omitempty"`
}

// UTSuggestion holds unit-test suggestions for a single changed function.
// Unlike SuggestedTestScenario (which is E2E and entry-point–oriented), UT
// scenarios focus on the function itself: mocked inputs, boundary values,
// exception paths, and compatibility branches within the function body.
type UTSuggestion struct {
	FuncName      string          `json:"func_name"`
	Repo          string          `json:"repo"`
	FilePath      string          `json:"file_path"`
	Summary       string          `json:"summary,omitempty"`        // one-line purpose of the change
	Constraints   []string        `json:"constraints,omitempty"`    // human-readable constraints (fallback path, null-guard, etc.)
	ExistingTests []string        `json:"existing_tests,omitempty"` // where existing tests live
	Scenarios     []UTScenario    `json:"scenarios,omitempty"`
}

// UTScenario is a single unit-test scenario for a changed function.
// The Input column carries concrete mock setup (e.g. "mock os.path.getsize=0,
// mock os.lseek=1073741824") rather than high-level request body.
type UTScenario struct {
	Priority    string `json:"priority"`    // P0/P1/P2
	Type        string `json:"type"`        // normal / boundary / exception / compat / fallback
	Description string `json:"description"`
	MockSetup   string `json:"mock_setup,omitempty"` // concrete mocks / stubs needed
	Assertions  string `json:"assertions,omitempty"` // what to verify (return value, side effects, error raised)
}

// CrossRepoHop records a single cross-repository call edge found during
// deterministic graph traversal. Each hop represents a boundary crossing
// from one repository to another in the BFS traversal.
type CrossRepoHop struct {
	FromRepo string `json:"from_repo"`
	FromFunc string `json:"from_func,omitempty"`
	ToRepo   string `json:"to_repo"`
	ToFunc   string `json:"to_func"`
	Depth    int    `json:"depth"`
	EdgeType string `json:"edge_type,omitempty"`
}

// AnalysisResult is the top-level output of a Shirakami analysis run.
type AnalysisResult struct {
	TaskID           string             `json:"task_id"`
	InputType        InputType          `json:"input_type"`
	DownwardChain    CallChain          `json:"downward_chain"`
	UpwardChains     []CallChain        `json:"upward_chains"`
	EntryPoints      []EntryPoint       `json:"entry_points"`
	ImpactSummary    ImpactSummary      `json:"impact_summary"`
	TestScenarios    []TestScenario     `json:"test_scenarios"`
	// FunctionAnalyses holds per-function constraint extraction and test scenario suggestions.
	FunctionAnalyses []FunctionAnalysis `json:"function_analyses,omitempty"`
	// UTSuggestions holds unit-test suggestions per changed function.
	UTSuggestions    []UTSuggestion     `json:"ut_suggestions,omitempty"`
	SelfCheckReport  string             `json:"self_check_report"`

	// Risk is the overall blast-radius classification for the change.
	// Set by the deterministic graph path (resolve.Resolver).
	// Values: "LOW", "MEDIUM", "HIGH", "CRITICAL". Empty when index is not active.
	Risk string `json:"risk,omitempty"`

	// IndexCoverage is the fraction of changed functions that were resolved
	// by the deterministic index (0.0–1.0). Only set in hybrid/deterministic mode.
	IndexCoverage float64 `json:"index_coverage,omitempty"`

	// CrossRepoHops records repository-boundary edges found during graph traversal.
	// Each hop represents a CALLS/IMPORTS edge that crosses a repo boundary.
	// Only populated in hybrid/deterministic mode; empty in pure-LLM mode.
	CrossRepoHops []CrossRepoHop `json:"cross_repo_hops,omitempty"`
}

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

// CallNode represents a single function node in a call chain.
type CallNode struct {
	FuncName string   `json:"func_name"`
	FilePath string   `json:"file_path"`
	Line     int      `json:"line"`
	Repo     string   `json:"repo"`
	NodeType NodeType `json:"node_type"`
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
}

// ImpactSummary summarizes the impact scope of the change.
type ImpactSummary struct {
	DirectFunctions []string `json:"direct_functions"` // functions directly impacted within same repo
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

// AnalysisResult is the top-level output of a Shirakami analysis run.
type AnalysisResult struct {
	TaskID         string         `json:"task_id"`
	InputType      InputType      `json:"input_type"`
	DownwardChain  CallChain      `json:"downward_chain"`   // changed func → leaf implementations
	UpwardChains   []CallChain    `json:"upward_chains"`    // changed func → entry points (multiple paths)
	EntryPoints    []EntryPoint   `json:"entry_points"`     // integration test entry points
	ImpactSummary  ImpactSummary  `json:"impact_summary"`
	TestScenarios  []TestScenario `json:"test_scenarios"`
	SelfCheckReport string        `json:"self_check_report"`
}

package agent

// MetricsRecorder is the minimal Prometheus sink consumed by AgentLoop and Orchestrator.
// It is satisfied by *feedback.Metrics; pass nil to disable all metrics recording.
// Defining the interface here (rather than importing internal/feedback) breaks the
// import cycle: internal/feedback/selfcheck.go already imports internal/agent.
type MetricsRecorder interface {
	// RecordLLMTokens records a per-step LLM token count labelled by model and task type.
	RecordLLMTokens(model, taskType string, totalTokens int)
	// RecordGhostNode increments the ghost-node outcome counter.
	// outcome must be "rescued" or "lost".
	RecordGhostNode(outcome string)
	// RecordWorkerDuration records a single Worker run duration for the given tier and repo.
	RecordWorkerDuration(tier, repo string, seconds float64)
	// RecordCheckpointResumed increments the checkpoint-resume counter.
	RecordCheckpointResumed()
}

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/DeviosLang/shirakami/internal/checkpoint"
	"github.com/DeviosLang/shirakami/internal/compress"
	"github.com/DeviosLang/shirakami/internal/llm"
	"github.com/DeviosLang/shirakami/internal/logger"
)

const maxSteps = 300

// Sliding window truncation constants.
// When the message history exceeds slidingWindowThreshold entries, the loop
// trims the middle to keep context size bounded before each LLM call.
const (
	slidingWindowToolResults = 40  // keep last N ToolResultMessage entries from tail
	slidingWindowConv        = 20  // keep last K AssistantMessage entries from tail
	slidingWindowThreshold   = 80  // only truncate when message count exceeds this
)

// Tool is the interface that any executable tool must satisfy.
type Tool interface {
	// Definition returns the LLM-facing description of this tool.
	Definition() llm.ToolDefinition
	// Execute runs the tool with the given JSON arguments and returns a result string.
	Execute(ctx context.Context, arguments []byte) (string, error)
}

// Result holds the outcome of a completed agent loop run.
type Result struct {
	// Content is the final text produced by the LLM (from the last end_turn response).
	Content string
	// StepCount is the number of LLM turns consumed.
	StepCount int
	// Truncated is true when the loop was force-stopped at maxSteps.
	Truncated bool
}

// LLMClient is the interface consumed by AgentLoop (satisfied by *llm.Client).
type LLMClient interface {
	Complete(ctx context.Context, messages []llm.Message, tools []llm.ToolDefinition) (*llm.Response, error)
}

// AgentLoop implements the core Claude-Code-style end_turn state machine.
//
// Lifecycle:
//  1. Try to restore from checkpoint (supports break-point resume).
//  2. Send messages + tools to LLM.
//  3. stop_reason == end_turn  → done.
//  4. stop_reason == tool_use  → execute tools concurrently, append results, goto 2.
//  5. stepCount >= maxSteps    → force-terminate and return partial result.
//
// Each step is persisted via checkpointer so the loop can resume after a crash.
// Before each LLM call the TokenBudgetManager checks token usage and applies
// the appropriate compression strategy (Plan A–D) when thresholds are exceeded.
type AgentLoop struct {
	llm          LLMClient
	tools        []Tool
	budget       int // max steps override (0 = use default maxSteps)
	checkpointer *checkpoint.FileCheckpointer
	systemPrompt string
	// budgetMgr is optional; when set it is invoked before each LLM call to
	// apply token compression strategies (Plan A–D) as context usage grows.
	budgetMgr *compress.TokenBudgetManager
	// metrics is optional; when set, per-step and lifecycle events are recorded.
	metrics MetricsRecorder
	// metricsModel is the model name label used when recording LLM token metrics.
	metricsModel string
	// metricsTaskType is the task_type label (e.g. "worker", "triage") for token metrics.
	metricsTaskType string
}

// NewAgentLoop constructs a new AgentLoop.
// systemPrompt is prepended as the first user message if messages are empty.
func NewAgentLoop(
	llmClient LLMClient,
	tools []Tool,
	budget int,
	cp *checkpoint.FileCheckpointer,
	systemPrompt string,
) *AgentLoop {
	return &AgentLoop{
		llm:          llmClient,
		tools:        tools,
		budget:       budget,
		checkpointer: cp,
		systemPrompt: systemPrompt,
	}
}

// WithBudgetManager attaches a TokenBudgetManager to the loop so that
// four-tier token compression (Plan A–D) is applied before each LLM call.
// Call this after NewAgentLoop when a compress.TokenBudgetManager is available.
func (a *AgentLoop) WithBudgetManager(mgr *compress.TokenBudgetManager) *AgentLoop {
	a.budgetMgr = mgr
	return a
}

// WithMetrics attaches Prometheus metrics to the loop.
// model is the LLM model name (e.g. "gpt-4o") used as the metric label.
// taskType is a short label describing the loop's role (e.g. "worker", "triage", "followup").
func (a *AgentLoop) WithMetrics(m MetricsRecorder, model, taskType string) *AgentLoop {
	a.metrics = m
	a.metricsModel = model
	a.metricsTaskType = taskType
	return a
}

// Run executes the agent loop for the given task string.
// It attempts to load a checkpoint first; on completion it deletes the checkpoint.
func (a *AgentLoop) Run(ctx context.Context, taskID string, task string) (*Result, error) {
	limit := maxSteps
	if a.budget > 0 && a.budget < limit {
		limit = a.budget
	}

	// Build tool definitions slice once.
	toolDefs := make([]llm.ToolDefinition, 0, len(a.tools))
	for _, t := range a.tools {
		toolDefs = append(toolDefs, t.Definition())
	}

	// Try to restore from a previous checkpoint.
	messages, stepCount, err := a.loadCheckpoint(taskID, task)
	if err != nil {
		return nil, fmt.Errorf("load checkpoint: %w", err)
	}

	// Record checkpoint resume when we are continuing a previously saved run.
	if stepCount > 0 && a.metrics != nil {
		a.metrics.RecordCheckpointResumed()
	}

	// Accumulates total tokens across all steps for the per-run histogram.
	totalTokens := 0

	// Main loop.
	log := logger.S()
	for {
		if stepCount >= limit {
			log.Warnw("loop.truncated",
				"task_id", taskID,
				"step", stepCount,
				"limit", limit,
			)
			if a.metrics != nil && totalTokens > 0 {
				a.metrics.RecordTaskTokens(a.metricsModel, a.metricsTaskType, totalTokens)
			}
			return &Result{
				Content:   lastContent(messages),
				StepCount: stepCount,
				Truncated: true,
			}, nil
		}

		// Sliding window: drop middle messages to bound context size.
		if len(messages) > slidingWindowThreshold {
			before := len(messages)
			messages = slidingWindowTruncate(messages)
			log.Debugw("loop.sliding_window",
				"task_id", taskID,
				"step", stepCount,
				"before", before,
				"after", len(messages),
			)
		}

		// Apply token budget compression strategies (Plan A–D) before each LLM call.
		// This prevents context overflow without requiring the caller to manage token counts.
		if a.budgetMgr != nil {
			if compressErr := a.budgetMgr.CheckAndCompress(ctx, &messages); compressErr != nil {
				log.Warnw("loop.compress_failed",
					"task_id", taskID,
					"step", stepCount,
					"err", compressErr.Error(),
				)
				// Non-fatal: proceed without compression — next step may still succeed.
			}
		}

		stepStart := time.Now()
		resp, err := a.llm.Complete(ctx, messages, toolDefs)
		if err != nil {
			log.Errorw("loop.llm_failed",
				"task_id", taskID,
				"step", stepCount,
				"err", err.Error(),
				"duration_ms", time.Since(stepStart).Milliseconds(),
			)
			return nil, fmt.Errorf("step %d llm complete: %w", stepCount, err)
		}
		stepCount++

		log.Debugw("loop.step",
			"task_id", taskID,
			"step", stepCount,
			"stop_reason", string(resp.StopReason),
			"tool_calls", len(resp.ToolCalls),
			"content_bytes", len(resp.Content),
			"prompt_tokens", resp.Usage.PromptTokens,
			"completion_tokens", resp.Usage.CompletionTokens,
			"total_tokens", resp.Usage.TotalTokens,
			"duration_ms", time.Since(stepStart).Milliseconds(),
		)

		// Record per-step LLM token consumption.
		if a.metrics != nil && resp.Usage.TotalTokens > 0 {
			a.metrics.RecordLLMTokens(a.metricsModel, a.metricsTaskType, resp.Usage.TotalTokens)
		}
		// Accumulate for per-task total and record prompt-cache hit/miss split.
		totalTokens += resp.Usage.TotalTokens
		if a.metrics != nil && resp.Usage.PromptTokens > 0 {
			uncached := resp.Usage.PromptTokens - resp.Usage.CachedTokens
			a.metrics.RecordCacheTokens(a.metricsModel, a.metricsTaskType, resp.Usage.CachedTokens, uncached)
		}

		// Append assistant turn to history.
		assistantMsg := llm.AssistantMessage{
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		}
		messages = append(messages, assistantMsg)

		// Persist state after each step.
		if a.checkpointer != nil {
			if saveErr := a.checkpointer.Save(taskID, stepCount, messages); saveErr != nil {
				// Non-fatal: checkpoint failure doesn't break the analysis, but log it
				// so operators know crash recovery won't work for this task.
				log.Warnw("loop.checkpoint_save_failed",
					"task_id", taskID,
					"step", stepCount,
					"err", saveErr.Error(),
				)
			}
		}

		switch resp.StopReason {
		case llm.StopReasonEndTurn, llm.StopReasonMaxTokens:
			// Clean up checkpoint on successful completion.
			if a.checkpointer != nil {
				_ = a.checkpointer.Delete(taskID)
			}
			if a.metrics != nil && totalTokens > 0 {
				a.metrics.RecordTaskTokens(a.metricsModel, a.metricsTaskType, totalTokens)
			}
			return &Result{
				Content:   resp.Content,
				StepCount: stepCount,
			}, nil

		case llm.StopReasonToolUse:
			toolResults := a.executeTools(ctx, resp.ToolCalls)
			for _, tr := range toolResults {
				messages = append(messages, tr)
			}

		default:
			// Unknown stop reason – treat as end_turn.
			if a.checkpointer != nil {
				_ = a.checkpointer.Delete(taskID)
			}
			if a.metrics != nil && totalTokens > 0 {
				a.metrics.RecordTaskTokens(a.metricsModel, a.metricsTaskType, totalTokens)
			}
			return &Result{
				Content:   resp.Content,
				StepCount: stepCount,
			}, nil
		}
	}
}

// RunFollowUp continues a conversation after a previous Run, appending
// the assistant's prior response and a new user follow-up message.
// This is used to ask the LLM to produce JSON when it gave prose instead.
func (a *AgentLoop) RunFollowUp(ctx context.Context, taskID string, priorAssistantContent, followUpMsg string) (*Result, error) {
	toolDefs := make([]llm.ToolDefinition, 0, len(a.tools))
	for _, t := range a.tools {
		toolDefs = append(toolDefs, t.Definition())
	}

	// Build a minimal message history: system + prior assistant + follow-up user.
	messages := []llm.Message{
		llm.UserMessage{Content: a.systemPrompt},
		llm.AssistantMessage{Content: priorAssistantContent},
		llm.UserMessage{Content: followUpMsg},
	}

	resp, err := a.llm.Complete(ctx, messages, toolDefs)
	if err != nil {
		return nil, fmt.Errorf("follow-up llm complete: %w", err)
	}
	return &Result{Content: resp.Content, StepCount: 1}, nil
}

// RunFollowUpNoTools is like RunFollowUp but passes an empty tool list to the LLM.
// Use this for follow-up tasks that should be pure text generation (e.g. scenario
// generation, JSON reformatting) where giving the LLM access to tools like ripgrep
// causes it to waste time on unnecessary tool calls.
//
// Performance insight: scenario follow-ups observed on v38 took 3-8 minutes when
// tools were exposed, but ~20 seconds when the LLM had no tool option. Forcing
// no-tools mode makes the path deterministic.
func (a *AgentLoop) RunFollowUpNoTools(ctx context.Context, taskID string, priorAssistantContent, followUpMsg string) (*Result, error) {
	log := logger.S()
	start := time.Now()

	// Build a minimal message history: system + prior assistant + follow-up user.
	messages := []llm.Message{
		llm.UserMessage{Content: a.systemPrompt},
		llm.AssistantMessage{Content: priorAssistantContent},
		llm.UserMessage{Content: followUpMsg},
	}

	// Explicitly pass nil for tool defs — the LLM must answer in prose/JSON.
	resp, err := a.llm.Complete(ctx, messages, nil)
	if err != nil {
		return nil, fmt.Errorf("follow-up-no-tools llm complete: %w", err)
	}
	log.Debugw("loop.followup_no_tools",
		"task_id", taskID,
		"content_bytes", len(resp.Content),
		"stop_reason", string(resp.StopReason),
		"prompt_tokens", resp.Usage.PromptTokens,
		"completion_tokens", resp.Usage.CompletionTokens,
		"duration_ms", time.Since(start).Milliseconds(),
	)
	return &Result{Content: resp.Content, StepCount: 1}, nil
}

// executeTools runs all tool calls in the slice concurrently and returns
// one ToolResultMessage per call in the same order as the input slice.
func (a *AgentLoop) executeTools(ctx context.Context, toolCalls []llm.ToolCall) []llm.Message {
	results := make([]llm.Message, len(toolCalls))
	var wg sync.WaitGroup

	log := logger.S()
	for i, tc := range toolCalls {
		i, tc := i, tc // capture loop vars
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					log.Errorw("loop.tool_panic",
						"tool", tc.Name,
						"panic", fmt.Sprintf("%v", r),
					)
					panicErr := fmt.Errorf("tool %s panicked: %v", tc.Name, r)
					results[i] = llm.ToolResultMessage{
						ToolCallID: tc.ID,
						Content:    toolErrorResponse(tc.Name, panicErr),
					}
				}
			}()
			content, err := a.runTool(ctx, tc)
			if err != nil {
				log.Warnw("loop.tool_error",
					"tool", tc.Name,
					"err", err.Error(),
				)
				content = toolErrorResponse(tc.Name, err)
			}
			results[i] = llm.ToolResultMessage{
				ToolCallID: tc.ID,
				Content:    content,
			}
		}()
	}

	wg.Wait()
	return results
}

// runTool finds the tool by name and executes it.
func (a *AgentLoop) runTool(ctx context.Context, tc llm.ToolCall) (string, error) {
	for _, t := range a.tools {
		if t.Definition().Name == tc.Name {
			return t.Execute(ctx, tc.Arguments)
		}
	}
	return "", fmt.Errorf("unknown tool %q", tc.Name)
}

// toolErrorResponse converts a tool execution error into a structured JSON hint
// so the LLM can distinguish error categories and choose the right recovery strategy.
//
// Error categories and their hints:
//   - not_found:   file/symbol missing → try ripgrep to locate the correct path
//   - timeout:     search/LSP call timed out → reduce scope or use a simpler query
//   - permission:  access denied → check file permissions or workspace configuration
//   - unknown_tool: tool name not registered → check available tool list
//   - tool_panic:  internal panic → report to operator, do not retry with same args
//   - error:       all other errors → generic hint
func toolErrorResponse(toolName string, err error) string {
	msg := err.Error()
	lower := strings.ToLower(msg)

	type errResponse struct {
		Error string `json:"error"`
		Hint  string `json:"hint"`
	}

	var resp errResponse
	switch {
	case strings.Contains(lower, "no such file") ||
		strings.Contains(lower, "file not found") ||
		strings.Contains(lower, "does not exist") ||
		strings.Contains(lower, "not found") ||
		strings.Contains(lower, "no matches"):
		resp = errResponse{
			Error: "not_found",
			Hint:  "use ripgrep to search for the correct file path before retrying",
		}
	case strings.Contains(lower, "timeout") ||
		strings.Contains(lower, "context deadline exceeded") ||
		strings.Contains(lower, "deadline exceeded"):
		resp = errResponse{
			Error: "timeout",
			Hint:  "reduce search scope, use --max-count or a narrower pattern",
		}
	case strings.Contains(lower, "permission denied") ||
		strings.Contains(lower, "access denied"):
		resp = errResponse{
			Error: "permission",
			Hint:  "this file is not accessible in the current workspace; skip it",
		}
	case strings.Contains(lower, "unknown tool"):
		resp = errResponse{
			Error: "unknown_tool",
			Hint:  fmt.Sprintf("tool %q is not available; choose from the registered tool list", toolName),
		}
	default:
		resp = errResponse{
			Error: "error",
			Hint:  msg,
		}
	}

	b, _ := json.Marshal(resp)
	return string(b)
}

// loadCheckpoint tries to restore messages and stepCount from a saved checkpoint.
// If no checkpoint exists it returns a fresh messages slice seeded with the task.
func (a *AgentLoop) loadCheckpoint(taskID, task string) ([]llm.Message, int, error) {
	if a.checkpointer == nil {
		return a.seedMessages(task), 0, nil
	}

	state, err := a.checkpointer.Load(taskID)
	if err != nil {
		return nil, 0, err
	}
	if state == nil {
		return a.seedMessages(task), 0, nil
	}

	msgs, err := checkpoint.RestoreMessages(state.Messages)
	if err != nil {
		// Corrupted checkpoint – start fresh.
		return a.seedMessages(task), 0, nil
	}
	return msgs, state.StepCount, nil
}

// seedMessages builds the initial message list for a new run.
func (a *AgentLoop) seedMessages(task string) []llm.Message {
	msgs := make([]llm.Message, 0, 2)
	if a.systemPrompt != "" {
		msgs = append(msgs, llm.UserMessage{Content: a.systemPrompt})
	}
	msgs = append(msgs, llm.UserMessage{Content: task})
	return msgs
}

// slidingWindowTruncate trims a message history to keep context size bounded.
//
// Strategy:
//  1. Extract "seed" messages — all leading messages before the first
//     AssistantMessage (i.e. system prompt + initial task user message).
//  2. From the remaining messages keep only the last
//     (slidingWindowToolResults + slidingWindowConv) entries.
//  3. Return seed + tail, discarding the middle.
//
// This guarantees the LLM always sees the original task context plus the most
// recent working memory, regardless of how many steps have elapsed.
func slidingWindowTruncate(msgs []llm.Message) []llm.Message {
	// Step 1: find the end of the seed prefix (everything before the first
	// AssistantMessage).
	seedEnd := 0
	for i, m := range msgs {
		if _, ok := m.(llm.AssistantMessage); ok {
			seedEnd = i
			break
		}
		// If no AssistantMessage found, seedEnd stays 0 and we treat the
		// entire slice as tail (nothing to seed-protect separately).
		seedEnd = i + 1
	}
	seed := msgs[:seedEnd]
	rest := msgs[seedEnd:]

	// Step 2: keep the last (slidingWindowToolResults + slidingWindowConv) entries.
	tailSize := slidingWindowToolResults + slidingWindowConv
	if len(rest) > tailSize {
		rest = rest[len(rest)-tailSize:]
	}

	// Step 3: combine.
	result := make([]llm.Message, 0, len(seed)+len(rest))
	result = append(result, seed...)
	result = append(result, rest...)
	return result
}

// lastContent extracts the Content from the last AssistantMessage in the slice,
// or returns an empty string if none exists.
func lastContent(messages []llm.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if am, ok := messages[i].(llm.AssistantMessage); ok {
			return am.Content
		}
	}
	return ""
}

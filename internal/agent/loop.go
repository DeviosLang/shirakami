package agent

import (
	"context"
	"fmt"
	"sync"

	"github.com/DeviosLang/shirakami/internal/checkpoint"
	"github.com/DeviosLang/shirakami/internal/llm"
)

const maxSteps = 100

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
type AgentLoop struct {
	llm          LLMClient
	tools        []Tool
	budget       int // max steps override (0 = use default maxSteps)
	checkpointer *checkpoint.FileCheckpointer
	systemPrompt string
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

	// Main loop.
	for {
		if stepCount >= limit {
			return &Result{
				Content:   lastContent(messages),
				StepCount: stepCount,
				Truncated: true,
			}, nil
		}

		resp, err := a.llm.Complete(ctx, messages, toolDefs)
		if err != nil {
			return nil, fmt.Errorf("step %d llm complete: %w", stepCount, err)
		}
		stepCount++

		// Append assistant turn to history.
		assistantMsg := llm.AssistantMessage{
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		}
		messages = append(messages, assistantMsg)

		// Persist state after each step.
		if a.checkpointer != nil {
			if saveErr := a.checkpointer.Save(taskID, stepCount, messages); saveErr != nil {
				// Non-fatal: log and continue.
				_ = saveErr
			}
		}

		switch resp.StopReason {
		case llm.StopReasonEndTurn, llm.StopReasonMaxTokens:
			// Clean up checkpoint on successful completion.
			if a.checkpointer != nil {
				_ = a.checkpointer.Delete(taskID)
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
			return &Result{
				Content:   resp.Content,
				StepCount: stepCount,
			}, nil
		}
	}
}

// executeTools runs all tool calls in the slice concurrently and returns
// one ToolResultMessage per call in the same order as the input slice.
func (a *AgentLoop) executeTools(ctx context.Context, toolCalls []llm.ToolCall) []llm.Message {
	results := make([]llm.Message, len(toolCalls))
	var wg sync.WaitGroup

	for i, tc := range toolCalls {
		i, tc := i, tc // capture loop vars
		wg.Add(1)
		go func() {
			defer wg.Done()
			content, err := a.runTool(ctx, tc)
			if err != nil {
				content = fmt.Sprintf("error: %s", err.Error())
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

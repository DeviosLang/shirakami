package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/DeviosLang/shirakami/internal/llm"
)

// ---------------------------------------------------------------------------
// extractBalancedJSON / extractJSON
// ---------------------------------------------------------------------------

func TestExtractBalancedJSON_SimpleObject(t *testing.T) {
	got := extractBalancedJSON(`{"key":"val"}`, 0)
	if got != `{"key":"val"}` {
		t.Errorf("unexpected: %q", got)
	}
}

func TestExtractBalancedJSON_Nested(t *testing.T) {
	input := `{"outer":{"inner":"v"},"arr":[1,2]}`
	got := extractBalancedJSON(input, 0)
	if got != input {
		t.Errorf("unexpected: %q", got)
	}
}

func TestExtractBalancedJSON_NotAtStart(t *testing.T) {
	input := `prefix {"key":"val"} suffix`
	got := extractBalancedJSON(input, 7)
	if got != `{"key":"val"}` {
		t.Errorf("unexpected: %q", got)
	}
}

func TestExtractBalancedJSON_BraceInString(t *testing.T) {
	// The brace inside the string value must not confuse depth counting.
	input := `{"msg":"has { brace inside"}`
	got := extractBalancedJSON(input, 0)
	if got != input {
		t.Errorf("unexpected: %q", got)
	}
}

func TestExtractBalancedJSON_EscapedQuote(t *testing.T) {
	input := `{"msg":"say \"hello\""}`
	got := extractBalancedJSON(input, 0)
	if got != input {
		t.Errorf("unexpected: %q", got)
	}
}

func TestExtractBalancedJSON_InvalidJSON(t *testing.T) {
	// Structurally balanced but semantically invalid JSON → should return "".
	got := extractBalancedJSON(`{bad: json}`, 0)
	if got != "" {
		t.Errorf("expected empty for invalid JSON, got %q", got)
	}
}

func TestExtractBalancedJSON_WrongStartChar(t *testing.T) {
	got := extractBalancedJSON(`[1,2,3]`, 0)
	if got != "" {
		t.Errorf("expected empty when startIdx points to non-{, got %q", got)
	}
}

func TestExtractBalancedJSON_OutOfBounds(t *testing.T) {
	got := extractBalancedJSON(`{"k":"v"}`, 100)
	if got != "" {
		t.Errorf("expected empty for out-of-bounds index, got %q", got)
	}
}

func TestExtractJSON_FencedBlock(t *testing.T) {
	content := "Some prose.\n```json\n{\"nodes\":[1,2,3]}\n```\nMore prose."
	got := extractJSON(content)
	if got != `{"nodes":[1,2,3]}` {
		t.Errorf("unexpected: %q", got)
	}
}

func TestExtractJSON_FencedBlock_OnlyFirstBlock(t *testing.T) {
	// Old greedy regex merged both blocks; new implementation returns only the first.
	content := "```json\n{\"a\":1}\n```\nsome text\n```json\n{\"b\":2}\n```"
	got := extractJSON(content)
	if got != `{"a":1}` {
		t.Errorf("expected first block only, got %q", got)
	}
}

func TestExtractJSON_BareObject_FallsBackWhenNoFence(t *testing.T) {
	content := `prefix {"key":"value"} suffix`
	got := extractJSON(content)
	if got != `{"key":"value"}` {
		t.Errorf("unexpected: %q", got)
	}
}

func TestExtractJSON_BareObject_MultipleObjectsReturnFirst(t *testing.T) {
	// When there are two bare JSON objects, we extract the first one.
	content := `{"a":1} some text {"b":2}`
	got := extractJSON(content)
	if got != `{"a":1}` {
		t.Errorf("expected first object, got %q", got)
	}
}

func TestExtractJSON_Empty(t *testing.T) {
	got := extractJSON("no json here")
	if got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestExtractJSON_FencedBlock_WithTrailingContent(t *testing.T) {
	// Fenced block followed by explanation text should not be merged.
	content := "```json\n{\"result\":\"ok\"}\n```\nExplanation of the result follows."
	got := extractJSON(content)
	if got != `{"result":"ok"}` {
		t.Errorf("unexpected: %q", got)
	}
}

// ---------------------------------------------------------------------------
// scopeContains
// ---------------------------------------------------------------------------

func TestScopeContains_Found(t *testing.T) {
	if !scopeContains([]string{"cvm_api", "vstation_compute"}, "cvm_api") {
		t.Error("expected true")
	}
}

func TestScopeContains_NotFound(t *testing.T) {
	if scopeContains([]string{"cvm_api", "vstation_compute"}, "cxm_api") {
		t.Error("expected false")
	}
}

func TestScopeContains_EmptyScope(t *testing.T) {
	if scopeContains([]string{}, "cvm_api") {
		t.Error("expected false for empty scope")
	}
}

func TestScopeContains_ExactMatch(t *testing.T) {
	// Must not match "cvm" when scope contains "cvm_api".
	if scopeContains([]string{"cvm_api"}, "cvm") {
		t.Error("substring should not match")
	}
}

// ---------------------------------------------------------------------------
// runScenarioAnalysis
// ---------------------------------------------------------------------------

// buildScenarioLLMResponse constructs a valid JSON response that parseEntryScenarios
// can decode, simulating what the LLM returns for a scenario follow-up call.
func buildScenarioLLMResponse(entries []struct{ fn, file string }) string {
	var sb strings.Builder
	sb.WriteString("```json\n{\"entry_scenarios\":[\n")
	for i, e := range entries {
		if i > 0 {
			sb.WriteString(",\n")
		}
		sb.WriteString(fmt.Sprintf(
			`{"entry_function":%q,"entry_file":%q,"changed_via":[],"preconditions":[],"typical_inputs":"","scenarios":[]}`,
			e.fn, e.file,
		))
	}
	sb.WriteString("\n]}\n```")
	return sb.String()
}

func makeWorkerAgentWithMock(responses []*llm.Response) *WorkerAgent {
	mock := &mockLLMClient{responses: responses}
	loop := NewAgentLoop(mock, nil, 0, nil, "")
	return &WorkerAgent{loop: loop, entryRepos: []string{}}
}

func TestRunScenarioAnalysis_SingleChunk(t *testing.T) {
	// 3 entry points → fits in one chunk (maxScenarioChunkSize=5).
	entries := []struct{ fn, file string }{
		{"handler_a", "svc/a.py"},
		{"handler_b", "svc/b.py"},
		{"handler_c", "svc/c.py"},
	}
	respBody := buildScenarioLLMResponse(entries)
	w := makeWorkerAgentWithMock([]*llm.Response{
		{Content: respBody, StopReason: llm.StopReasonEndTurn},
	})

	eps := []CallNode{
		{Function: "handler_a", File: "svc/a.py"},
		{Function: "handler_b", File: "svc/b.py"},
		{Function: "handler_c", File: "svc/c.py"},
	}
	got := w.runScenarioAnalysis(context.Background(), "t1", "", nil, eps)
	if len(got) != 3 {
		t.Errorf("expected 3 scenarios, got %d", len(got))
	}
}

func TestRunScenarioAnalysis_MultipleChunks(t *testing.T) {
	// 7 entry points → splits into 2 chunks (5 + 2) with maxScenarioChunkSize=5.
	allEntries := make([]struct{ fn, file string }, 7)
	for i := range allEntries {
		allEntries[i] = struct{ fn, file string }{
			fmt.Sprintf("handler_%d", i),
			fmt.Sprintf("svc/%d.py", i),
		}
	}
	chunk1 := buildScenarioLLMResponse(allEntries[:5])
	chunk2 := buildScenarioLLMResponse(allEntries[5:])

	w := makeWorkerAgentWithMock([]*llm.Response{
		{Content: chunk1, StopReason: llm.StopReasonEndTurn},
		{Content: chunk2, StopReason: llm.StopReasonEndTurn},
	})

	eps := make([]CallNode, 7)
	for i := range eps {
		eps[i] = CallNode{Function: fmt.Sprintf("handler_%d", i), File: fmt.Sprintf("svc/%d.py", i)}
	}
	got := w.runScenarioAnalysis(context.Background(), "t2", "", nil, eps)
	if len(got) != 7 {
		t.Errorf("expected 7 scenarios, got %d", len(got))
	}
}

func TestRunScenarioAnalysis_DeduplicateAcrossChunks(t *testing.T) {
	// Both chunks return the same entry — second occurrence must be dropped.
	entry := []struct{ fn, file string }{{"handler_dup", "svc/dup.py"}}
	resp := buildScenarioLLMResponse(entry)

	w := makeWorkerAgentWithMock([]*llm.Response{
		{Content: resp, StopReason: llm.StopReasonEndTurn},
		{Content: resp, StopReason: llm.StopReasonEndTurn},
	})

	// Pass 6 entry points (2 chunks) where both chunks claim the same fn/file.
	eps := make([]CallNode, 6)
	for i := range eps {
		eps[i] = CallNode{Function: "handler_dup", File: "svc/dup.py"}
	}
	got := w.runScenarioAnalysis(context.Background(), "t3", "", nil, eps)
	if len(got) != 1 {
		t.Errorf("expected 1 deduplicated scenario, got %d", len(got))
	}
}

func TestRunScenarioAnalysis_RetriesOnEmpty(t *testing.T) {
	// First LLM call returns no JSON; second (retry) returns valid data.
	entry := []struct{ fn, file string }{{"handler_retry", "svc/r.py"}}
	w := makeWorkerAgentWithMock([]*llm.Response{
		{Content: "sorry, I cannot help", StopReason: llm.StopReasonEndTurn}, // no JSON
		{Content: buildScenarioLLMResponse(entry), StopReason: llm.StopReasonEndTurn},
	})

	eps := []CallNode{{Function: "handler_retry", File: "svc/r.py"}}
	got := w.runScenarioAnalysis(context.Background(), "t4", "", nil, eps)
	if len(got) != 1 {
		t.Errorf("expected 1 scenario after retry, got %d", len(got))
	}
}

func TestRunScenarioAnalysis_EmptyEntryPoints(t *testing.T) {
	w := makeWorkerAgentWithMock(nil)
	got := w.runScenarioAnalysis(context.Background(), "t5", "", nil, nil)
	if got == nil {
		t.Error("expected non-nil empty slice")
	}
	if len(got) != 0 {
		t.Errorf("expected 0 scenarios for empty input, got %d", len(got))
	}
}

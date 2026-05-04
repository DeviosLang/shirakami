package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/DeviosLang/shirakami/internal/logger"
)

// TriageItem is one file classification returned by the Triage LLM call.
type TriageItem struct {
	File        string `json:"file"`         // repo-relative path, e.g. "compute/disk/encrypt_disk.py"
	Priority    string `json:"priority"`     // "P0", "P1", or "P2"
	ImpactScope string `json:"impact_scope"` // "cross_repo" or "local"
	Reason      string `json:"reason"`       // short human-readable rationale
}

// TriageResult is the full output of a Triage pass.
type TriageResult struct {
	Items   []TriageItem `json:"triage"`
	Skipped []string     `json:"skipped"` // files intentionally skipped (pure test / infra)
}

// TriageAgent performs a fast, no-tools LLM classification of changed files
// before the Worker phase.  It outputs a priority list (P0/P1/P2) that the
// Orchestrator uses to decide which files get deep tracing vs. shallow tracing.
type TriageAgent struct {
	llmClient LLMClient
}

// NewTriageAgent creates a TriageAgent.
func NewTriageAgent(llmClient LLMClient) *TriageAgent {
	return &TriageAgent{llmClient: llmClient}
}

// Triage classifies the changed files found in the diff.
// changedFiles is the list extracted by extractDiffFiles (test files already filtered).
// Returns TriageResult; on error returns a default result that treats all files as P0.
func (t *TriageAgent) Triage(ctx context.Context, changedFiles []string, description string) *TriageResult {
	log := logger.S()
	start := time.Now()

	if len(changedFiles) == 0 {
		log.Infow("triage.skip", "reason", "no changed files")
		return &TriageResult{}
	}

	log.Infow("triage.start", "files", len(changedFiles))

	// Build the file list for the prompt.
	var fileList strings.Builder
	for _, f := range changedFiles {
		fmt.Fprintf(&fileList, "  - %s\n", f)
	}

	sysPrompt := `You are a code change triage specialist. Given a list of changed files, classify each one by business impact priority. Output ONLY valid JSON — no explanation, no markdown outside the JSON block.`

	triagePrompt := fmt.Sprintf(`Changed files in this diff:
%s
Description: %s

Classify each file by priority. Follow the rules STRICTLY — do NOT mark everything as P0.

PRIORITY RULES (target distribution: P0 ≤ 30%%, P1 ~40%%, P2 ≥ 30%%):

P0 (strict): Direct business API implementation only
  - API handlers, controllers, view functions that serve external traffic
  - Core data persistence / transaction / state machine logic
  - Security / authentication / encryption primitives

P1 (moderate): Supporting paths
  - Helpers called by P0 code (utilities, parsers, validators)
  - Internal coordinators / dispatchers / schedulers
  - Service initialization, configuration loading

P2 (default for anything non-critical): Low business impact
  - Scripts in scripts/, tools/, bin/, scripts/*, utilities/*
  - Monitoring helpers (pico_manager, find_unused_*, xml_*_diff, host_info collectors)
  - Log / metrics / report generators
  - Pure data containers with only getters/setters
  - Init glue code that rarely changes business behaviour

PATH HEURISTICS (use path as primary signal):
- Contains "api/", "handler/", "controller/", "business/", "service/" → likely P0
- Contains "util", "common/utils", "helper" → likely P1
- Contains "scripts/", "tools/", "bin/", "cmd/", "monitor/", "report/" → P2
- Contains "pico", "xml_qemu", "find_unused", "host_stat", "test/" → P2
- Files with "< 50 lines" in a typical utility role → P2

If unsure, default to P1 (NOT P0). Err on the side of lower priority.

Output JSON only:
`+"```json"+`
{
  "triage": [
    {"file": "compute/disk/encrypt_disk.py", "priority": "P0", "impact_scope": "cross_repo", "reason": "SM4 encryption core path"},
    {"file": "compute/common/utils.py", "priority": "P1", "impact_scope": "cross_repo", "reason": "utility helpers called by encryption"},
    {"file": "compute/tools/find_unused_baseimage.py", "priority": "P2", "impact_scope": "local", "reason": "cleanup script, not a runtime path"}
  ],
  "skipped": []
}
`+"```",
		fileList.String(),
		description,
	)

	// Single no-tools LLM call.
	loop := NewAgentLoop(t.llmClient, nil, 0, nil, sysPrompt)
	result, err := loop.Run(ctx, "triage", triagePrompt)
	if err != nil {
		log.Warnw("triage.llm_failed",
			"err", err.Error(),
			"duration_ms", time.Since(start).Milliseconds(),
		)
		return defaultTriageResult(changedFiles)
	}

	parsed := parseTriageResult(result.Content)
	if parsed == nil || len(parsed.Items) == 0 {
		log.Warnw("triage.parse_failed",
			"content_bytes", len(result.Content),
			"duration_ms", time.Since(start).Milliseconds(),
		)
		return defaultTriageResult(changedFiles)
	}

	p0, p1, p2 := 0, 0, 0
	for _, it := range parsed.Items {
		switch it.Priority {
		case "P0":
			p0++
		case "P1":
			p1++
		case "P2":
			p2++
		}
	}
	log.Infow("triage.llm_done",
		"total", len(parsed.Items),
		"p0", p0, "p1", p1, "p2", p2,
		"skipped", len(parsed.Skipped),
		"duration_ms", time.Since(start).Milliseconds(),
	)
	return parsed
}

// parseTriageResult extracts TriageResult from LLM output.
func parseTriageResult(content string) *TriageResult {
	raw := extractJSON(content)
	if raw == "" {
		return nil
	}
	var result TriageResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return nil
	}
	return &result
}

// defaultTriageResult treats all files as P1 when triage fails.
// P1 (not P0) keeps analysis moderate — avoids unbounded deep tracing for
// every file if the triage LLM call is unavailable.
func defaultTriageResult(files []string) *TriageResult {
	items := make([]TriageItem, 0, len(files))
	for _, f := range files {
		items = append(items, TriageItem{
			File:        f,
			Priority:    "P1",
			ImpactScope: "cross_repo",
			Reason:      "triage unavailable — defaulting to P1",
		})
	}
	return &TriageResult{Items: items}
}

// PriorityOrder returns a sort key for priority strings (P0 < P1 < P2).
func PriorityOrder(p string) int {
	switch p {
	case "P0":
		return 0
	case "P1":
		return 1
	default:
		return 2
	}
}

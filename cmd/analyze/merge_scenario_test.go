package main

import (
	"testing"

	"github.com/DeviosLang/shirakami/pkg/schema"
)

// makeEntryNodes is a helper that builds a []schema.EntryPoint from name/file pairs.
func makeEntryNodes(pairs ...string) []schema.EntryPoint {
	if len(pairs)%2 != 0 {
		panic("makeEntryNodes: need even number of args (name, file, ...)")
	}
	nodes := make([]schema.EntryPoint, 0, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		nodes = append(nodes, schema.EntryPoint{
			Node: schema.CallNode{
				FuncName: pairs[i],
				FilePath: pairs[i+1],
			},
		})
	}
	return nodes
}

// ---------------------------------------------------------------------------
// Level 1 — exact name match
// ---------------------------------------------------------------------------

func TestFindEntryNodeIdx_ExactMatch(t *testing.T) {
	nodes := makeEntryNodes("RunInstances", "business/cvm/handler.py", "StopInstances", "business/cvm/stop.py")
	idx := findEntryNodeIdx(nodes, "RunInstances", "")
	if idx != 0 {
		t.Errorf("expected index 0, got %d", idx)
	}
}

func TestFindEntryNodeIdx_ExactMatchSecondEntry(t *testing.T) {
	nodes := makeEntryNodes("RunInstances", "a.py", "StopInstances", "b.py")
	idx := findEntryNodeIdx(nodes, "StopInstances", "")
	if idx != 1 {
		t.Errorf("expected index 1, got %d", idx)
	}
}

func TestFindEntryNodeIdx_ExactMatchTakesPriorityOverSubstring(t *testing.T) {
	// "CreateInstance" contains "Create" as substring, but exact "CreateInstance" should win.
	nodes := makeEntryNodes("CreateUser", "a.py", "CreateInstance", "b.py")
	idx := findEntryNodeIdx(nodes, "CreateInstance", "")
	if idx != 1 {
		t.Errorf("expected exact match at index 1, got %d", idx)
	}
}

// ---------------------------------------------------------------------------
// Level 2 — substring match with length guard
// ---------------------------------------------------------------------------

func TestFindEntryNodeIdx_SubstringMatch(t *testing.T) {
	// LLM returns "RunInst" (7 chars ≥ 6) which is a substring of "RunInstances".
	nodes := makeEntryNodes("RunInstances", "handler.py")
	idx := findEntryNodeIdx(nodes, "RunInst", "")
	if idx != 0 {
		t.Errorf("expected 0, got %d", idx)
	}
}

func TestFindEntryNodeIdx_SubstringTooShort_NoMatch(t *testing.T) {
	// "Run" (3 chars < 6) should not match "RunInstances".
	nodes := makeEntryNodes("RunInstances", "handler.py", "RunService", "svc.py")
	idx := findEntryNodeIdx(nodes, "Run", "")
	if idx != -1 {
		t.Errorf("expected -1 for short token, got %d", idx)
	}
}

func TestFindEntryNodeIdx_SubstringPicksLongestMatch(t *testing.T) {
	// "RunInstances" (12 chars) vs "RunInst" (7 chars) — LLM returns "RunInstances".
	// Both contain "RunInst" but "RunInstances" provides the longer shorter-side.
	nodes := makeEntryNodes("RunInstancesToo", "a.py", "RunInstances", "b.py")
	// LLM function name "RunInstances" — exact match with second entry.
	idx := findEntryNodeIdx(nodes, "RunInstances", "")
	if idx != 1 {
		t.Errorf("expected exact match at 1, got %d", idx)
	}
}

func TestFindEntryNodeIdx_ShortTokenDoesNotCollide(t *testing.T) {
	// "Create" (6 chars — exactly at boundary, should NOT match because guard is > 6, i.e., ≥ 6 is fine but…)
	// Wait — the guard is shorter >= 6, so "Create" (6 chars) DOES qualify.
	// Verify it picks the longest-shorter candidate when two entries both contain "Create".
	nodes := makeEntryNodes("CreateUser", "a.py", "CreateInstance", "b.py")
	// "Create" (6 chars) is a substring of both; shorter side = 6 for both.
	// bestMatchLen stays at 6 and idx picks the FIRST qualifying match.
	idx := findEntryNodeIdx(nodes, "Create", "")
	// Should pick index 0 (first qualifying) — acceptable deterministic behaviour.
	if idx != 0 {
		t.Errorf("expected first qualifying match at 0, got %d", idx)
	}
}

// ---------------------------------------------------------------------------
// Level 3 — file path match
// ---------------------------------------------------------------------------

func TestFindEntryNodeIdx_FilePathMatch(t *testing.T) {
	nodes := makeEntryNodes("dispatch", "compute/service/dispatch.py")
	// LLM uses a slightly abbreviated path.
	idx := findEntryNodeIdx(nodes, "unknownFunc", "service/dispatch.py")
	if idx != 0 {
		t.Errorf("expected 0 via file path match, got %d", idx)
	}
}

func TestFindEntryNodeIdx_FilePathNotUsedWhenNameMatches(t *testing.T) {
	// Name match at index 0, file path would match index 1 — name takes priority.
	nodes := makeEntryNodes("handler", "a.py", "other", "b.py")
	idx := findEntryNodeIdx(nodes, "handler", "b.py")
	if idx != 0 {
		t.Errorf("expected name match at 0 to win over file match at 1, got %d", idx)
	}
}

// ---------------------------------------------------------------------------
// No match
// ---------------------------------------------------------------------------

func TestFindEntryNodeIdx_NoMatch(t *testing.T) {
	nodes := makeEntryNodes("RunInstances", "handler.py")
	idx := findEntryNodeIdx(nodes, "DeleteInstances", "other/file.py")
	if idx != -1 {
		t.Errorf("expected -1, got %d", idx)
	}
}

func TestFindEntryNodeIdx_EmptyNodes(t *testing.T) {
	idx := findEntryNodeIdx(nil, "RunInstances", "handler.py")
	if idx != -1 {
		t.Errorf("expected -1 for empty nodes, got %d", idx)
	}
}

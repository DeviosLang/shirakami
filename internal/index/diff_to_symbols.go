package index

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DiffHunk mirrors tool.DiffHunk — a contiguous block of changed lines.
// Duplicated here to avoid circular import (tool → index → tool).
type DiffHunk struct {
	File      string
	StartLine int
	EndLine   int
}

// DiffToSymbolsResult holds the output of mapping diff hunks to indexed symbols.
type DiffToSymbolsResult struct {
	// Matched: hunks that were successfully mapped to symbols in the index.
	Matched []MatchedSymbol
	// Uncovered: hunks for which no symbol was found in the index.
	Uncovered []DiffHunk
}

// MatchedSymbol pairs a diff hunk with the symbol it maps to.
type MatchedSymbol struct {
	Hunk   DiffHunk
	Symbol SymbolNode
}

// DiffToSymbols maps diff hunks to indexed symbols by line-range overlap.
// This is Layer B of the pipeline (requires symbol_nodes table populated).
//
// For each hunk, it queries: symbols WHERE start_line <= hunk.EndLine AND end_line >= hunk.StartLine
// (i.e. the symbol's line range overlaps with the hunk's line range).
//
// Returns matched symbols and uncovered hunks (for LLM fallback).
func DiffToSymbols(ctx context.Context, pool *pgxpool.Pool, repo string, hunks []DiffHunk) (*DiffToSymbolsResult, error) {
	if len(hunks) == 0 {
		return &DiffToSymbolsResult{}, nil
	}

	store := NewStore(pool)
	result := &DiffToSymbolsResult{}

	for _, hunk := range hunks {
		nodes, err := store.FindSymbolsByLineRange(ctx, repo, hunk.File, hunk.StartLine, hunk.EndLine)
		if err != nil {
			return nil, fmt.Errorf("diff to symbols query %s:%d-%d: %w", hunk.File, hunk.StartLine, hunk.EndLine, err)
		}

		if len(nodes) == 0 {
			result.Uncovered = append(result.Uncovered, hunk)
		} else {
			// Deduplicate: same symbol may be matched by multiple overlapping hunks
			for _, node := range nodes {
				result.Matched = append(result.Matched, MatchedSymbol{
					Hunk:   hunk,
					Symbol: node,
				})
			}
		}
	}

	// Deduplicate matched symbols (same symbol matched by multiple hunks)
	result.Matched = dedupeMatched(result.Matched)

	return result, nil
}

// dedupeMatched removes duplicate symbol matches (same symbol ID).
func dedupeMatched(matches []MatchedSymbol) []MatchedSymbol {
	seen := make(map[string]bool)
	var deduped []MatchedSymbol
	for _, m := range matches {
		if !seen[m.Symbol.ID] {
			seen[m.Symbol.ID] = true
			deduped = append(deduped, m)
		}
	}
	return deduped
}

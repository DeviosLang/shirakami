package contract

import (
	"fmt"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// Deduplication and merging
// ---------------------------------------------------------------------------

// Deduplicate removes duplicate contracts (same caller+provider+path).
func Deduplicate(contracts []FoundContract) []FoundContract {
	seen := make(map[string]bool)
	var result []FoundContract
	for _, c := range contracts {
		key := fmt.Sprintf("%s|%s|%s|%s", c.CallerRepo, c.ProviderRepo, c.ProviderPath, c.Kind)
		if !seen[key] {
			seen[key] = true
			result = append(result, c)
		}
	}
	return result
}

// FilterResolved returns only contracts where the provider repo was resolved.
func FilterResolved(contracts []FoundContract) []FoundContract {
	var result []FoundContract
	for _, c := range contracts {
		if c.ProviderRepo != "" {
			result = append(result, c)
		}
	}
	return result
}

// ---------------------------------------------------------------------------
// YAML rendering
// ---------------------------------------------------------------------------

// RenderYAML generates the contracts: section for shirakami.yaml.
func RenderYAML(contracts []FoundContract) string {
	if len(contracts) == 0 {
		return "contracts: []\n"
	}

	// Sort for deterministic output
	sort.Slice(contracts, func(i, j int) bool {
		ki := contracts[i].CallerRepo + contracts[i].ProviderPath
		kj := contracts[j].CallerRepo + contracts[j].ProviderPath
		return ki < kj
	})

	var sb strings.Builder
	sb.WriteString("contracts:\n")
	for _, c := range contracts {
		sb.WriteString(fmt.Sprintf("  - provider:\n"))
		if c.ProviderRepo != "" {
			sb.WriteString(fmt.Sprintf("      repo: %s\n", c.ProviderRepo))
		}
		if c.ProviderPath != "" {
			sb.WriteString(fmt.Sprintf("      path: %q\n", c.ProviderPath))
		}
		sb.WriteString(fmt.Sprintf("    consumer:\n"))
		sb.WriteString(fmt.Sprintf("      repo: %s\n", c.CallerRepo))
		if c.CallerFunc != "" {
			sb.WriteString(fmt.Sprintf("      func: %s\n", c.CallerFunc))
		}
		sb.WriteString(fmt.Sprintf("    # kind=%s  file=%s:%d\n", c.Kind, c.CallerFile, c.CallerLine))
	}
	return sb.String()
}

// ---------------------------------------------------------------------------
// Terminal summary
// ---------------------------------------------------------------------------

// FormatSummary prints a human-readable discovery summary.
func FormatSummary(result *ScanResult, showAll bool) string {
	all := result.Contracts
	resolved := FilterResolved(all)
	unresolved := len(all) - len(resolved)

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("\n[Contract Scan Result]\n"))
	sb.WriteString(fmt.Sprintf("  Total discovered:  %d\n", len(all)))
	sb.WriteString(fmt.Sprintf("  Provider resolved: %d\n", len(resolved)))
	sb.WriteString(fmt.Sprintf("  Unresolved:        %d (URL could not be matched to a known repo)\n", unresolved))
	if len(result.Warnings) > 0 {
		sb.WriteString(fmt.Sprintf("  Warnings:          %d\n", len(result.Warnings)))
	}

	// Group resolved contracts by kind
	byKind := make(map[string][]FoundContract)
	for _, c := range resolved {
		byKind[c.Kind] = append(byKind[c.Kind], c)
	}
	for _, kind := range []string{"http", "grpc", "mq_publish", "mq_subscribe"} {
		if cs := byKind[kind]; len(cs) > 0 {
			sb.WriteString(fmt.Sprintf("\n  [%s] %d contracts:\n", kind, len(cs)))
			shown := cs
			if !showAll && len(shown) > 10 {
				shown = shown[:10]
			}
			for _, c := range shown {
				sb.WriteString(fmt.Sprintf("    %s:%s → %s %s\n",
					c.CallerRepo, c.CallerFunc, c.ProviderRepo, c.ProviderPath))
			}
			if !showAll && len(cs) > 10 {
				sb.WriteString(fmt.Sprintf("    ... and %d more\n", len(cs)-10))
			}
		}
	}

	if showAll && unresolved > 0 {
		unres := make([]FoundContract, 0)
		for _, c := range all {
			if c.ProviderRepo == "" {
				unres = append(unres, c)
			}
		}
		sb.WriteString(fmt.Sprintf("\n  [unresolved] sample:\n"))
		limit := 5
		if len(unres) < limit {
			limit = len(unres)
		}
		for _, c := range unres[:limit] {
			sb.WriteString(fmt.Sprintf("    %s:%s → %q\n", c.CallerRepo, c.CallerFunc, c.ProviderURL))
		}
	}

	return sb.String()
}

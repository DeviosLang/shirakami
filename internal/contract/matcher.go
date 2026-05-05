package contract

import (
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// ContractLink — matched provider-consumer pair
// ---------------------------------------------------------------------------

// ContractLink represents a resolved cross-repo call relationship with a
// confidence score. Built by Match() from a set of FoundContracts.
type ContractLink struct {
	// Consumer side (caller)
	ConsumerRepo string
	ConsumerFunc string
	ConsumerFile string
	ConsumerLine int

	// Provider side (callee)
	ProviderRepo string
	ProviderPath string // normalised path/topic/method

	Protocol   string  // "http", "grpc", "mq_publish", "mq_subscribe"
	MatchType  string  // "exact", "prefix", "wildcard", "manual"
	Confidence float64 // 1.0 = exact, 0.8 = prefix, 0.6 = wildcard
}

// Key returns a deduplication key for this link.
func (l ContractLink) Key() string {
	return fmt.Sprintf("%s|%s|%s|%s|%s",
		l.ConsumerRepo, l.ConsumerFunc,
		l.ProviderRepo, l.ProviderPath,
		l.Protocol)
}

// ---------------------------------------------------------------------------
// MatchConfig — optional filtering and noise suppression
// ---------------------------------------------------------------------------

// MatchConfig controls noise filtering for the matcher.
type MatchConfig struct {
	// ExcludePaths lists exact paths to exclude from matching (health checks, etc.).
	// Default: /health, /ping, /ready, /metrics, /favicon.ico
	ExcludePaths []string

	// ExcludeParamOnlyPaths drops paths that consist entirely of path parameters
	// (e.g. "/{id}/{action}"), which would create N×M false links.
	ExcludeParamOnlyPaths bool
}

// DefaultMatchConfig returns sensible defaults for production use.
func DefaultMatchConfig() MatchConfig {
	return MatchConfig{
		ExcludePaths: []string{
			"/health", "/healthz", "/ping", "/ready", "/readyz",
			"/metrics", "/favicon.ico", "/",
		},
		ExcludeParamOnlyPaths: true,
	}
}

// ---------------------------------------------------------------------------
// Match — core matching algorithm
// ---------------------------------------------------------------------------

// Match pairs provider and consumer FoundContracts, returning ContractLinks
// with confidence scores:
//
//	exact    (1.0) — path + protocol identical after normalisation
//	prefix   (0.8) — consumer path is a prefix of provider path (or vice versa)
//	wildcard (0.6) — gRPC service/* wildcard match
//
// Noise routes (health checks, param-only paths) are excluded before matching.
// The returned slice is deduplicated; for duplicate (consumer+provider+protocol)
// tuples the highest-confidence entry is kept.
func Match(contracts []FoundContract, cfg MatchConfig) []ContractLink {
	// Split into providers (receiving calls) and consumers (making calls)
	// For HTTP/gRPC: providers are repos that declare handler paths.
	// For MQ: mq_publish is consumer side; mq_subscribe would be provider side.
	// In practice, Scan() produces consumer-side entries (outgoing HTTP calls),
	// so we pair consumers with each other by matching ProviderPath → CallerFunc
	// within FoundContracts that resolved a ProviderRepo.

	// Build the effective exclude set
	excludeSet := make(map[string]bool, len(cfg.ExcludePaths))
	for _, p := range cfg.ExcludePaths {
		excludeSet[normaliseMatchPath(p)] = true
	}

	// Filter resolved contracts (we can only match when ProviderRepo is known)
	var resolved []FoundContract
	for _, c := range contracts {
		if c.ProviderRepo == "" {
			continue
		}
		normPath := normaliseMatchPath(c.ProviderPath)
		if excludeSet[normPath] {
			continue
		}
		if cfg.ExcludeParamOnlyPaths && isParamOnlyPath(normPath) {
			continue
		}
		resolved = append(resolved, c)
	}

	if len(resolved) == 0 {
		return nil
	}

	// Build an index of "provider paths" per repo for efficient prefix lookup.
	// Key: providerRepo → []FoundContract (all callers that target that repo)
	// We treat each FoundContract as a (consumer→provider) link candidate directly.
	// The "matching" step merges duplicates and assigns confidence.

	// For now each resolved FoundContract IS a direct link (consumer → provider).
	// Future: when we have a provider-side registry (e.g. from OpenAPI spec or
	// entry-role repo scanning), we can do exact/prefix/wildcard matching here.
	// For the current scanner output (consumer-side only), we assign confidence
	// based on path quality:
	//   - Static path (no params, no wildcards): confidence = 1.0 (exact)
	//   - Path with parameters (/{id}): confidence = 0.8 (prefix)
	//   - gRPC wildcard or empty path: confidence = 0.6 (wildcard)

	var links []ContractLink
	for _, c := range resolved {
		confidence, matchType := scoreMatch(c.ProviderPath, c.Kind)
		links = append(links, ContractLink{
			ConsumerRepo: c.CallerRepo,
			ConsumerFunc: c.CallerFunc,
			ConsumerFile: c.CallerFile,
			ConsumerLine: c.CallerLine,
			ProviderRepo: c.ProviderRepo,
			ProviderPath: c.ProviderPath,
			Protocol:     c.Kind,
			MatchType:    matchType,
			Confidence:   confidence,
		})
	}

	return dedupLinks(links)
}

// MatchManual converts config.ContractEntry-style (provider, consumer) pairs
// into ContractLinks with confidence=1.0 and match_type="manual".
// These are hand-declared relationships that bypass automatic scanning.
//
// The entries parameter uses plain structs to avoid a config package import cycle:
// each entry is a [4]string{consumerRepo, consumerFunc, providerRepo, providerPath}.
func MatchManual(entries [][4]string) []ContractLink {
	var links []ContractLink
	for _, e := range entries {
		consumerRepo, consumerFunc, providerRepo, providerPath := e[0], e[1], e[2], e[3]
		if consumerRepo == "" || providerRepo == "" {
			continue
		}
		links = append(links, ContractLink{
			ConsumerRepo: consumerRepo,
			ConsumerFunc: consumerFunc,
			ProviderRepo: providerRepo,
			ProviderPath: providerPath,
			Protocol:     "http", // default; manual entries usually don't specify protocol
			MatchType:    "manual",
			Confidence:   1.0,
		})
	}
	return dedupLinks(links)
}

// MergeLinks merges auto-discovered and manually declared links.
// For duplicate keys, the higher-confidence entry wins.
func MergeLinks(auto, manual []ContractLink) []ContractLink {
	return dedupLinks(append(auto, manual...))
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// scoreMatch assigns a confidence and match type based on path structure.
func scoreMatch(path, kind string) (float64, string) {
	switch kind {
	case "grpc":
		if path == "" || strings.HasSuffix(path, "/*") {
			return 0.6, "wildcard"
		}
		return 1.0, "exact"
	case "mq_publish", "mq_subscribe":
		if path == "" {
			return 0.6, "wildcard"
		}
		return 1.0, "exact"
	}

	// HTTP path scoring
	if path == "" {
		return 0.6, "wildcard"
	}
	if containsPathParam(path) {
		return 0.8, "prefix"
	}
	return 1.0, "exact"
}

// containsPathParam returns true if the path contains /{param} or :param segments.
func containsPathParam(path string) bool {
	for _, seg := range strings.Split(path, "/") {
		if len(seg) > 0 && (seg[0] == '{' || seg[0] == ':') {
			return true
		}
	}
	return false
}

// isParamOnlyPath returns true if every non-empty segment is a parameter.
// e.g. "/{id}/{action}" → true, "/api/v1/{id}" → false
func isParamOnlyPath(path string) bool {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	if len(segs) == 0 {
		return false
	}
	for _, seg := range segs {
		if seg == "" {
			continue
		}
		if seg[0] != '{' && seg[0] != ':' && seg[0] != '*' {
			return false // has at least one static segment
		}
	}
	return true
}

// normaliseMatchPath lowercases and strips trailing slashes for comparison.
func normaliseMatchPath(p string) string {
	p = strings.ToLower(strings.TrimRight(p, "/"))
	if p == "" {
		return "/"
	}
	return p
}

// dedupLinks deduplicates by Key(), keeping highest confidence per key.
func dedupLinks(links []ContractLink) []ContractLink {
	best := make(map[string]ContractLink, len(links))
	for _, l := range links {
		k := l.Key()
		if existing, ok := best[k]; !ok || l.Confidence > existing.Confidence {
			best[k] = l
		}
	}
	result := make([]ContractLink, 0, len(best))
	for _, l := range best {
		result = append(result, l)
	}
	return result
}

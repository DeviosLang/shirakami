package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Symbol represents a named code symbol extracted from a snippet.
type Symbol struct {
	Name string `json:"name"`
	Kind string `json:"kind"` // "function", "method", "class", "interface", "type"
	Line int    `json:"line"` // 1-based line number within the snippet
}

// SymbolTool extracts function/class/method names from a code snippet string.
type SymbolTool struct{}

func NewSymbolTool() *SymbolTool { return &SymbolTool{} }

func (t *SymbolTool) Name() string { return "symbol_extractor" }

func (t *SymbolTool) Description() string {
	return "Extract function names, method names, class names, and type names from a code snippet string. Useful for identifying symbols in file_reader output."
}

func (t *SymbolTool) InputSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"code": map[string]interface{}{
				"type":        "string",
				"description": "Code snippet to extract symbols from",
			},
			"language": map[string]interface{}{
				"type":        "string",
				"description": "Programming language hint (go, python, js, ts, java). Optional; auto-detected if omitted.",
			},
		},
		"required": []string{"code"},
	}
}

type symbolInput struct {
	Code     string `json:"code"`
	Language string `json:"language"`
}

// Symbol extraction patterns per language

var symbolPatterns = map[string][]*symbolPattern{
	"go": {
		// func name( or func (recv) name(
		{regexp.MustCompile(`^(?:\s*)func\s+(?:\([^)]+\)\s+)?(\w+)\s*\(`), "function"},
		// type Name struct/interface
		{regexp.MustCompile(`^(?:\s*)type\s+(\w+)\s+struct\b`), "class"},
		{regexp.MustCompile(`^(?:\s*)type\s+(\w+)\s+interface\b`), "interface"},
		{regexp.MustCompile(`^(?:\s*)type\s+(\w+)\s+`), "type"},
	},
	"python": {
		{regexp.MustCompile(`^(?:\s*)def\s+(\w+)\s*\(`), "function"},
		{regexp.MustCompile(`^(?:\s*)class\s+(\w+)\s*[:(]`), "class"},
	},
	"js": {
		{regexp.MustCompile(`^(?:\s*)(?:export\s+)?(?:async\s+)?function\s+(\w+)\s*\(`), "function"},
		{regexp.MustCompile(`^(?:\s*)(?:export\s+)?class\s+(\w+)\b`), "class"},
		{regexp.MustCompile(`^(?:\s*)(?:const|let|var)\s+(\w+)\s*=\s*(?:async\s*)?\(`), "function"},
		{regexp.MustCompile(`^(?:\s*)(?:const|let|var)\s+(\w+)\s*=\s*(?:async\s*)?function\b`), "function"},
	},
	"java": {
		{regexp.MustCompile(`^(?:\s*)(?:public|private|protected|static|final|\s)*\s+\w[\w<>[\],\s]*\s+(\w+)\s*\(`), "function"},
		{regexp.MustCompile(`^(?:\s*)(?:public|private|protected)?\s*(?:abstract\s+)?class\s+(\w+)\b`), "class"},
		{regexp.MustCompile(`^(?:\s*)(?:public|private|protected)?\s*interface\s+(\w+)\b`), "interface"},
	},
}

type symbolPattern struct {
	re   *regexp.Regexp
	kind string
}

// aliasLanguage normalizes language aliases.
func aliasLanguage(lang string) string {
	switch strings.ToLower(lang) {
	case "golang":
		return "go"
	case "typescript", "tsx", "jsx":
		return "js"
	case "javascript":
		return "js"
	case "python", "py":
		return "python"
	case "java", "kotlin":
		return "java"
	default:
		return strings.ToLower(lang)
	}
}

// detectLanguage makes a best-effort guess at the language from code content.
func detectLanguage(code string) string {
	// Look for distinctive keywords
	if regexp.MustCompile(`\bpackage\s+\w+`).MatchString(code) {
		return "go"
	}
	if regexp.MustCompile(`\bdef\s+\w+\s*\(`).MatchString(code) {
		return "python"
	}
	if regexp.MustCompile(`\bpublic\s+(static\s+)?(?:void|class|interface)\b`).MatchString(code) {
		return "java"
	}
	if regexp.MustCompile(`(?:import|export|const|let|var|=>)`).MatchString(code) {
		return "js"
	}
	return "go" // default
}

func (t *SymbolTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var inp symbolInput
	if err := json.Unmarshal(input, &inp); err != nil {
		return "", fmt.Errorf("symbol_extractor: invalid input: %w", err)
	}
	if inp.Code == "" {
		return "", fmt.Errorf("symbol_extractor: code is required")
	}

	lang := aliasLanguage(inp.Language)
	if lang == "" {
		lang = detectLanguage(inp.Code)
	}

	patterns, ok := symbolPatterns[lang]
	if !ok {
		// Fallback: try go + js patterns
		patterns = append(symbolPatterns["go"], symbolPatterns["js"]...)
	}

	var symbols []Symbol
	lines := strings.Split(inp.Code, "\n")
	for i, line := range lines {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}
		// Strip line-number prefix if present (e.g. "     5\t")
		stripped := stripLineNumberPrefix(line)
		for _, p := range patterns {
			if m := p.re.FindStringSubmatch(stripped); len(m) > 1 {
				symbols = append(symbols, Symbol{
					Name: m[1],
					Kind: p.kind,
					Line: i + 1,
				})
				break // one symbol per line
			}
		}
	}

	if len(symbols) == 0 {
		return "No symbols found.", nil
	}

	out, err := json.MarshalIndent(symbols, "", "  ")
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// lineNumberPrefixRe matches cat -n style prefixes like "     5\t".
var lineNumberPrefixRe = regexp.MustCompile(`^\s*\d+\t`)

func stripLineNumberPrefix(line string) string {
	if lineNumberPrefixRe.MatchString(line) {
		idx := strings.Index(line, "\t")
		if idx >= 0 {
			return line[idx+1:]
		}
	}
	return line
}

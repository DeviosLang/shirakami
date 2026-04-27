package report

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/DeviosLang/shirakami/pkg/schema"
)

// OutputFormat defines which output format to use.
type OutputFormat string

const (
	FormatTerminal OutputFormat = "terminal"
	FormatJSON     OutputFormat = "json"
	FormatMarkdown OutputFormat = "markdown"
)

// ANSI color codes for terminal output.
const (
	ansiReset  = "\033[0m"
	ansiBold   = "\033[1m"
	ansiCyan   = "\033[36m"
	ansiYellow = "\033[33m"
	ansiGreen  = "\033[32m"
	ansiBlue   = "\033[34m"
	ansiRed    = "\033[31m"
	ansiGray   = "\033[90m"
)

// Generate renders an AnalysisResult in the requested format.
func Generate(result *schema.AnalysisResult, format OutputFormat) (string, error) {
	switch format {
	case FormatJSON:
		return generateJSON(result)
	case FormatMarkdown:
		return generateMarkdown(result), nil
	case FormatTerminal:
		return generateTerminal(result), nil
	default:
		return "", fmt.Errorf("unknown output format: %q", format)
	}
}

// ---------------------------------------------------------------------------
// JSON
// ---------------------------------------------------------------------------

func generateJSON(result *schema.AnalysisResult) (string, error) {
	b, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal analysis result: %w", err)
	}
	return string(b), nil
}

// ---------------------------------------------------------------------------
// Terminal (ANSI)
// ---------------------------------------------------------------------------

func generateTerminal(result *schema.AnalysisResult) string {
	var b strings.Builder

	// Header: changed function
	if len(result.DownwardChain.Nodes) > 0 {
		changed := result.DownwardChain.Nodes[0]
		fmt.Fprintf(&b, "%s%s变更函数:%s %s[%s] %s (%s:%d)%s\n",
			ansiBold, ansiCyan, ansiReset,
			ansiYellow, changed.Repo, changed.FuncName, changed.FilePath, changed.Line,
			ansiReset,
		)
	}

	// Downward chain
	if len(result.DownwardChain.Nodes) > 1 {
		fmt.Fprintf(&b, "\n%s向下追踪（实现路径）:%s\n", ansiBold, ansiReset)
		writeTerminalTree(&b, result.DownwardChain.Nodes[1:], 1, false)
	}

	// Upward chains → entry points
	if len(result.UpwardChains) > 0 {
		fmt.Fprintf(&b, "\n%s向上追踪 → 集成测试入口:%s\n", ansiBold, ansiReset)
		for i, chain := range result.UpwardChains {
			ep := entryPointForChain(result.EntryPoints, i)
			if ep != nil {
				fmt.Fprintf(&b, "  %s入口%d [%s] %s%s\n",
					ansiGreen, i+1, ep.Protocol, ep.Path, ansiReset)
				writeTerminalChainPath(&b, chain, "    ")
			} else {
				fmt.Fprintf(&b, "  %s路径%d%s\n", ansiGreen, i+1, ansiReset)
				writeTerminalChainPath(&b, chain, "    ")
			}
		}
	}

	// Test scenarios
	if len(result.TestScenarios) > 0 {
		fmt.Fprintf(&b, "\n%s集成测试场景:%s\n", ansiBold, ansiReset)
		for _, ts := range result.TestScenarios {
			fmt.Fprintf(&b, "  %s[%s %s]%s %s\n",
				ansiBlue, ts.EntryProtocol, ts.EntryPath, ansiReset, ts.Description)
		}
	}

	// Impact summary
	fmt.Fprintf(&b, "\n%s影响范围:%s\n", ansiBold, ansiReset)
	fmt.Fprintf(&b, "  直接影响: %s%s 内 %d 个函数%s\n",
		ansiYellow, directRepo(result), result.ImpactSummary.DirectCount, ansiReset)
	if result.ImpactSummary.CrossRepoCount > 0 {
		fmt.Fprintf(&b, "  跨仓影响: %s%s%s\n",
			ansiRed, strings.Join(result.ImpactSummary.CrossRepoImpact, "、"), ansiReset)
	}

	// Self-check
	if result.SelfCheckReport != "" {
		fmt.Fprintf(&b, "\n%s自检报告:%s\n%s%s%s\n",
			ansiBold, ansiReset, ansiGray, result.SelfCheckReport, ansiReset)
	}

	return b.String()
}

func writeTerminalTree(b *strings.Builder, nodes []schema.CallNode, depth int, isLast bool) {
	_ = isLast
	for i, n := range nodes {
		prefix := strings.Repeat("     ", depth-1)
		last := i == len(nodes)-1
		connector := "└─"
		if !last {
			connector = "├─"
		}
		label := fmt.Sprintf("%s (%s:%d)", n.FuncName, n.FilePath, n.Line)
		if n.NodeType == schema.NodeTypeLeaf {
			label = fmt.Sprintf("%s%s (底层: %s)%s", ansiGray, n.FuncName, n.Repo, ansiReset)
		}
		fmt.Fprintf(b, "  %s%s %s\n", prefix, connector, label)
	}
}

func writeTerminalChainPath(b *strings.Builder, chain schema.CallChain, indent string) {
	nodes := chain.Nodes
	if len(nodes) == 0 {
		return
	}
	parts := make([]string, 0, len(nodes))
	for _, n := range nodes {
		parts = append(parts, fmt.Sprintf("%s (L%d)", n.FuncName, n.Line))
	}
	fmt.Fprintf(b, "%s└─ %s%s → [变更点]%s\n",
		indent, ansiGray, strings.Join(parts, " → "), ansiReset)
}

// ---------------------------------------------------------------------------
// Markdown
// ---------------------------------------------------------------------------

func generateMarkdown(result *schema.AnalysisResult) string {
	var b strings.Builder

	b.WriteString("# Shirakami 分析报告\n\n")

	// Changed function
	if len(result.DownwardChain.Nodes) > 0 {
		changed := result.DownwardChain.Nodes[0]
		fmt.Fprintf(&b, "**变更函数:** `[%s] %s` (`%s:%d`)\n\n",
			changed.Repo, changed.FuncName, changed.FilePath, changed.Line)
	}

	// Downward chain
	if len(result.DownwardChain.Nodes) > 1 {
		b.WriteString("## 向下追踪（实现路径）\n\n```\n")
		writeMarkdownTree(&b, result.DownwardChain.Nodes[1:])
		b.WriteString("```\n\n")
	}

	// Upward chains
	if len(result.UpwardChains) > 0 {
		b.WriteString("## 向上追踪 → 集成测试入口\n\n")
		for i, chain := range result.UpwardChains {
			ep := entryPointForChain(result.EntryPoints, i)
			if ep != nil {
				fmt.Fprintf(&b, "### 入口%d `[%s] %s`\n\n", i+1, ep.Protocol, ep.Path)
			} else {
				fmt.Fprintf(&b, "### 路径%d\n\n", i+1)
			}
			b.WriteString("```\n")
			writeMarkdownChainPath(&b, chain)
			b.WriteString("```\n\n")
		}
	}

	// Entry points with test scenarios
	if len(result.EntryPoints) > 0 {
		b.WriteString("## 集成测试入口\n\n")
		for i, ep := range result.EntryPoints {
			fmt.Fprintf(&b, "### 入口%d\n\n", i+1)
			fmt.Fprintf(&b, "- **协议:** %s\n", ep.Protocol)
			fmt.Fprintf(&b, "- **路径:** `%s`\n", ep.Path)
			fmt.Fprintf(&b, "- **函数:** `%s` (`%s:%d`)\n", ep.Node.FuncName, ep.Node.FilePath, ep.Node.Line)
			if len(ep.TestScenarios) > 0 {
				b.WriteString("- **测试场景:**\n")
				for _, ts := range ep.TestScenarios {
					fmt.Fprintf(&b, "  - %s\n", ts)
				}
			}
			b.WriteString("\n")
		}
	}

	// Test scenarios
	if len(result.TestScenarios) > 0 {
		b.WriteString("## 集成测试场景\n\n")
		for _, ts := range result.TestScenarios {
			fmt.Fprintf(&b, "- **[%s %s]** %s\n", ts.EntryProtocol, ts.EntryPath, ts.Description)
		}
		b.WriteString("\n")
	}

	// Impact summary
	b.WriteString("## 影响范围\n\n")
	fmt.Fprintf(&b, "- **直接影响:** %s 内 %d 个函数\n", directRepo(result), result.ImpactSummary.DirectCount)
	if len(result.ImpactSummary.DirectFunctions) > 0 {
		for _, fn := range result.ImpactSummary.DirectFunctions {
			fmt.Fprintf(&b, "  - `%s`\n", fn)
		}
	}
	if result.ImpactSummary.CrossRepoCount > 0 {
		fmt.Fprintf(&b, "- **跨仓影响:** %d 处\n", result.ImpactSummary.CrossRepoCount)
		for _, cr := range result.ImpactSummary.CrossRepoImpact {
			fmt.Fprintf(&b, "  - `%s`\n", cr)
		}
	}
	b.WriteString("\n")

	// Self-check
	if result.SelfCheckReport != "" {
		b.WriteString("## 自检报告\n\n")
		fmt.Fprintf(&b, "```\n%s\n```\n\n", result.SelfCheckReport)
	}

	return b.String()
}

func writeMarkdownTree(b *strings.Builder, nodes []schema.CallNode) {
	for i, n := range nodes {
		prefix := strings.Repeat("     ", i)
		if n.NodeType == schema.NodeTypeLeaf {
			fmt.Fprintf(b, "  %s└─ %s (底层: %s)\n", prefix, n.FuncName, n.Repo)
		} else {
			fmt.Fprintf(b, "  %s└─ %s (%s:%d)\n", prefix, n.FuncName, n.FilePath, n.Line)
		}
	}
}

func writeMarkdownChainPath(b *strings.Builder, chain schema.CallChain) {
	nodes := chain.Nodes
	if len(nodes) == 0 {
		return
	}
	parts := make([]string, 0, len(nodes))
	for _, n := range nodes {
		parts = append(parts, fmt.Sprintf("%s (L%d)", n.FuncName, n.Line))
	}
	fmt.Fprintf(b, "  └─ %s → [变更点]\n", strings.Join(parts, " → "))
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// entryPointForChain returns the entry point matching the i-th upward chain, if any.
func entryPointForChain(eps []schema.EntryPoint, i int) *schema.EntryPoint {
	if i < len(eps) {
		ep := eps[i]
		return &ep
	}
	return nil
}

// directRepo returns the repo name from the first downward chain node.
func directRepo(result *schema.AnalysisResult) string {
	if len(result.DownwardChain.Nodes) > 0 {
		return result.DownwardChain.Nodes[0].Repo
	}
	return "unknown"
}

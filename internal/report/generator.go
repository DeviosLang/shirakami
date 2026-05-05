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
	if result.Risk != "" {
		riskColor := ansiYellow
		switch result.Risk {
		case "CRITICAL":
			riskColor = ansiRed
		case "HIGH":
			riskColor = ansiRed
		case "LOW":
			riskColor = ansiGray
		}
		fmt.Fprintf(&b, "  风险等级: %s%s%s", riskColor, result.Risk, ansiReset)
		if result.IndexCoverage > 0 {
			fmt.Fprintf(&b, " %s(索引覆盖率 %.0f%%)%s", ansiGray, result.IndexCoverage*100, ansiReset)
		}
		fmt.Fprintf(&b, "\n")
	}
	if len(result.CrossRepoHops) > 0 {
		fmt.Fprintf(&b, "  跨仓调用:\n")
		for _, hop := range result.CrossRepoHops {
			fmt.Fprintf(&b, "    %s%s → %s%s (%s)\n",
				ansiYellow, hop.FromRepo, hop.ToRepo, ansiReset, hop.ToFunc)
		}
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

	// Header: source repo and changed function count
	sourceRepo := directRepo(result)
	fmt.Fprintf(&b, "**源仓库:** `%s`  \n", sourceRepo)
	fmt.Fprintf(&b, "**变更函数数量:** %d\n\n", result.ImpactSummary.DirectCount)

	// Call graph nodes table — only nodes with file paths (real analysis results).
	// Nodes without FilePath are internal placeholders and are excluded.
	nodesWithFile := make([]schema.CallNode, 0, len(result.DownwardChain.Nodes))
	for _, n := range result.DownwardChain.Nodes {
		if n.FilePath != "" {
			nodesWithFile = append(nodesWithFile, n)
		}
	}
	if len(nodesWithFile) > 0 {
		fmt.Fprintf(&b, "## 调用链节点 (%d)\n\n", len(nodesWithFile))
		b.WriteString("| 仓库 | 文件 | 行 | 函数 |\n")
		b.WriteString("|------|------|----|------|\n")
		for _, n := range nodesWithFile {
			fmt.Fprintf(&b, "| `%s` | `%s` | %d | `%s` |\n",
				n.Repo, n.FilePath, n.Line, n.FuncName)
		}
		b.WriteString("\n")
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

	// Entry points — grouped by repo for readability.
	if len(result.EntryPoints) > 0 {
		fmt.Fprintf(&b, "## 集成测试入口 (%d)\n\n", len(result.EntryPoints))

		// Group by repo.
		repoOrder := make([]string, 0)
		repoEps := make(map[string][]schema.EntryPoint)
		for _, ep := range result.EntryPoints {
			r := ep.Node.Repo
			if _, exists := repoEps[r]; !exists {
				repoOrder = append(repoOrder, r)
			}
			repoEps[r] = append(repoEps[r], ep)
		}
		for _, repo := range repoOrder {
			eps := repoEps[repo]
			fmt.Fprintf(&b, "### %s\n\n", repo)
			b.WriteString("| 文件 | 行 | 入口 |\n")
			b.WriteString("|------|----|----|----|\n")
			for _, ep := range eps {
				label := ep.Node.FuncName
				if ep.Path != "" {
					label = ep.Path
				}
				fmt.Fprintf(&b, "| `%s` | %d | `%s` |\n",
					ep.Node.FilePath, ep.Node.Line, label)
			}
			b.WriteString("\n")

			// Render per-entry test scenarios (from scenario follow-up).
			for _, ep := range eps {
				if len(ep.SuggestedScenarios) == 0 {
					continue
				}
				label := ep.Node.FuncName
				if ep.Path != "" {
					label = ep.Path
				}
				fmt.Fprintf(&b, "#### `%s` — 测试场景\n\n", label)
				if len(ep.ChangedVia) > 0 {
					fmt.Fprintf(&b, "**途经变更函数:** %s\n\n",
						"`"+strings.Join(ep.ChangedVia, "`, `")+"`")
				}
				if len(ep.Preconditions) > 0 {
					b.WriteString("**前置条件:**\n")
					for _, pre := range ep.Preconditions {
						fmt.Fprintf(&b, "- %s\n", pre)
					}
					b.WriteString("\n")
				}
				if ep.TypicalInputs != "" {
					fmt.Fprintf(&b, "**典型入参:** `%s`\n\n", ep.TypicalInputs)
				}
				b.WriteString("| 优先级 | 类型 | 场景描述 | 关键入参 | 预期结果 | 观察点 Oracle |\n")
				b.WriteString("|--------|------|----------|----------|----------|----------------|\n")
				for _, s := range ep.SuggestedScenarios {
					fmt.Fprintf(&b, "| %s | %s | %s | `%s` | %s | %s |\n",
						s.Priority, s.Type, s.Description, s.Input, s.Expected,
						formatOracles(s.Oracles))
				}
				b.WriteString("\n")
			}
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

	// Function analyses (constraints + suggested scenarios)
	if len(result.FunctionAnalyses) > 0 {
		b.WriteString("## 函数约束与测试建议\n\n")
		for _, fa := range result.FunctionAnalyses {
			fmt.Fprintf(&b, "### `%s` (%s)\n\n", fa.FuncName, fa.Repo)
			if len(fa.Constraints) > 0 {
				b.WriteString("**约束条件:**\n\n")
				for _, c := range fa.Constraints {
					fmt.Fprintf(&b, "- [%s] `%s` — %s (`%s:%d`)\n",
						c.Type, c.Condition, c.Note, c.FilePath, c.Line)
				}
				b.WriteString("\n")
			}
			if len(fa.SuggestedScenarios) > 0 {
				b.WriteString("**建议测试场景:**\n\n")
				for _, s := range fa.SuggestedScenarios {
					fmt.Fprintf(&b, "- [%s][%s] %s", s.Priority, s.Type, s.Description)
					if s.Input != "" {
						fmt.Fprintf(&b, "  \n  输入: `%s`", s.Input)
					}
					if s.Expected != "" {
						fmt.Fprintf(&b, "  \n  预期: `%s`", s.Expected)
					}
					b.WriteString("\n")
				}
				b.WriteString("\n")
			}
		}
	}

	// Unit test suggestions (per changed function).
	if len(result.UTSuggestions) > 0 {
		fmt.Fprintf(&b, "## 单元测试建议 (UT) — %d 个函数\n\n", len(result.UTSuggestions))
		b.WriteString("针对 diff 变更函数本身的 UT 建议（mock 级别、分支/边界/异常/兼容）。\n\n")
		for _, ut := range result.UTSuggestions {
			fmt.Fprintf(&b, "### `%s` (`%s/%s`)\n\n", ut.FuncName, ut.Repo, ut.FilePath)
			if ut.Summary != "" {
				fmt.Fprintf(&b, "**变更摘要:** %s\n\n", ut.Summary)
			}
			if len(ut.Constraints) > 0 {
				b.WriteString("**代码约束:**\n")
				for _, c := range ut.Constraints {
					fmt.Fprintf(&b, "- %s\n", c)
				}
				b.WriteString("\n")
			}
			if len(ut.ExistingTests) > 0 {
				b.WriteString("**现有 UT:** ")
				for i, t := range ut.ExistingTests {
					if i > 0 {
						b.WriteString(", ")
					}
					fmt.Fprintf(&b, "`%s`", t)
				}
				b.WriteString("\n\n")
			}
			if len(ut.Scenarios) > 0 {
				b.WriteString("| 优先级 | 类型 | 场景描述 | Mock 设置 | 断言 |\n")
				b.WriteString("|--------|------|----------|-----------|------|\n")
				for _, s := range ut.Scenarios {
					fmt.Fprintf(&b, "| %s | %s | %s | `%s` | %s |\n",
						s.Priority, s.Type, s.Description, s.MockSetup, s.Assertions)
				}
				b.WriteString("\n")
			}
		}
	}

	// Impact summary
	b.WriteString("## 影响范围\n\n")
	fmt.Fprintf(&b, "- **直接影响:** `%s` 内 %d 个函数\n", sourceRepo, result.ImpactSummary.DirectCount)
	if result.ImpactSummary.CrossRepoCount > 0 {
		fmt.Fprintf(&b, "- **跨仓影响 (%d):** %s\n",
			result.ImpactSummary.CrossRepoCount,
			strings.Join(result.ImpactSummary.CrossRepoImpact, "、"))
	}
	if result.Risk != "" {
		coverageNote := ""
		if result.IndexCoverage > 0 {
			coverageNote = fmt.Sprintf("（索引覆盖率 %.0f%%）", result.IndexCoverage*100)
		}
		fmt.Fprintf(&b, "- **风险等级:** %s%s\n", result.Risk, coverageNote)
	}
	if len(result.CrossRepoHops) > 0 {
		b.WriteString("\n### 跨仓调用明细\n\n")
		b.WriteString("| 深度 | 来源仓库 | 目标仓库 | 目标函数 | 边类型 |\n")
		b.WriteString("|------|----------|----------|----------|--------|\n")
		for _, hop := range result.CrossRepoHops {
			fmt.Fprintf(&b, "| %d | `%s` | `%s` | `%s` | %s |\n",
				hop.Depth, hop.FromRepo, hop.ToRepo, hop.ToFunc, hop.EdgeType)
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")

	// Worker raw output (shown when structured parsing produced no results)
	if result.SelfCheckReport != "" {
		b.WriteString("## Worker 分析详情\n\n")
		b.WriteString(result.SelfCheckReport)
		b.WriteString("\n")
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

// directRepo returns the source repository name for the impact summary line.
func directRepo(result *schema.AnalysisResult) string {
	if result.ImpactSummary.SourceRepo != "" {
		return result.ImpactSummary.SourceRepo
	}
	if len(result.DownwardChain.Nodes) > 0 {
		return result.DownwardChain.Nodes[0].Repo
	}
	return "unknown"
}

// formatOracles renders a list of TestOracle values into a compact string for
// in-table display. Multiple oracles are separated by "<br>" (GFM-friendly).
// Returns "-" when the scenario has no oracles.
func formatOracles(oracles []schema.TestOracle) string {
	if len(oracles) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(oracles))
	for _, o := range oracles {
		if o.Target == "" && o.Assertion == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("**[%s]** `%s` — %s", o.Type, o.Target, o.Assertion))
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, "<br>")
}

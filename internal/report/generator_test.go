package report_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/DeviosLang/shirakami/internal/report"
	"github.com/DeviosLang/shirakami/pkg/schema"
)

func sampleResult() *schema.AnalysisResult {
	changedNode := schema.CallNode{
		FuncName: "PaymentService.SetTimeout",
		FilePath: "handler/payment.go",
		Line:     42,
		Repo:     "payment-service",
		NodeType: schema.NodeTypeMiddle,
	}
	return &schema.AnalysisResult{
		TaskID:    "task-001",
		InputType: schema.InputTypeFuncName,
		DownwardChain: schema.CallChain{
			Direction: schema.DirectionDownward,
			Nodes: []schema.CallNode{
				changedNode,
				{FuncName: "config.UpdateTimeout", FilePath: "config/timeout.go", Line: 15, Repo: "payment-service", NodeType: schema.NodeTypeMiddle},
				{FuncName: "redis.Set", FilePath: "", Line: 0, Repo: "Redis", NodeType: schema.NodeTypeLeaf},
			},
			Edges: []schema.CallEdge{
				{From: changedNode, To: schema.CallNode{FuncName: "config.UpdateTimeout", FilePath: "config/timeout.go", Line: 15, Repo: "payment-service", NodeType: schema.NodeTypeMiddle}},
			},
		},
		UpwardChains: []schema.CallChain{
			{
				Direction: schema.DirectionUpward,
				Nodes: []schema.CallNode{
					{FuncName: "PaymentHandler.Process", FilePath: "handler/payment.go", Line: 88, Repo: "payment-service", NodeType: schema.NodeTypeEntry},
					{FuncName: "Execute", FilePath: "handler/payment.go", Line: 118, Repo: "payment-service", NodeType: schema.NodeTypeMiddle},
					changedNode,
				},
			},
			{
				Direction: schema.DirectionUpward,
				Nodes: []schema.CallNode{
					{FuncName: "PaymentService.BatchProcess", FilePath: "handler/batch.go", Line: 44, Repo: "payment-service", NodeType: schema.NodeTypeEntry},
					{FuncName: "BatchExecute", FilePath: "handler/batch.go", Line: 44, Repo: "payment-service", NodeType: schema.NodeTypeMiddle},
					changedNode,
				},
			},
		},
		EntryPoints: []schema.EntryPoint{
			{
				Node:     schema.CallNode{FuncName: "PaymentHandler.Process", FilePath: "handler/payment.go", Line: 88, Repo: "payment-service", NodeType: schema.NodeTypeEntry},
				Protocol: schema.ProtocolHTTP,
				Path:     "POST /api/v1/payment/process",
				TestScenarios: []string{
					"测试支付超时场景：验证超时后响应码和重试行为",
				},
			},
			{
				Node:     schema.CallNode{FuncName: "PaymentService.BatchProcess", FilePath: "handler/batch.go", Line: 44, Repo: "payment-service", NodeType: schema.NodeTypeEntry},
				Protocol: schema.ProtocolGRPC,
				Path:     "PaymentService.BatchProcess",
				TestScenarios: []string{
					"测试批量支付中超时传播",
				},
			},
		},
		ImpactSummary: schema.ImpactSummary{
			DirectFunctions: []string{"PaymentService.SetTimeout", "config.UpdateTimeout", "PaymentHandler.Process"},
			CrossRepoImpact: []string{"order-service/OrderClient（调用了变更函数）"},
			DirectCount:     3,
			CrossRepoCount:  1,
		},
		TestScenarios: []schema.TestScenario{
			{EntryProtocol: schema.ProtocolHTTP, EntryPath: "POST /api/v1/payment/process", Description: "测试支付超时场景：验证超时后响应码和重试行为"},
			{EntryProtocol: schema.ProtocolGRPC, EntryPath: "PaymentService.BatchProcess", Description: "测试批量支付中超时传播"},
		},
		SelfCheckReport: "所有路径已覆盖，未发现遗漏入口。",
	}
}

func TestGenerateJSON(t *testing.T) {
	out, err := report.Generate(sampleResult(), report.FormatJSON)
	if err != nil {
		t.Fatalf("Generate JSON error: %v", err)
	}
	var parsed schema.AnalysisResult
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("JSON not parseable by json.Unmarshal: %v", err)
	}
	if parsed.TaskID != "task-001" {
		t.Errorf("expected task_id task-001, got %s", parsed.TaskID)
	}
	if len(parsed.EntryPoints) != 2 {
		t.Errorf("expected 2 entry points, got %d", len(parsed.EntryPoints))
	}
	if len(parsed.TestScenarios) != 2 {
		t.Errorf("expected 2 test scenarios, got %d", len(parsed.TestScenarios))
	}
}

func TestGenerateTerminal(t *testing.T) {
	out, err := report.Generate(sampleResult(), report.FormatTerminal)
	if err != nil {
		t.Fatalf("Generate terminal error: %v", err)
	}
	checks := []string{"变更函数", "向下追踪", "向上追踪", "集成测试场景", "影响范围", "payment-service"}
	for _, needle := range checks {
		if !strings.Contains(out, needle) {
			t.Errorf("terminal output missing %q", needle)
		}
	}
}

func TestGenerateMarkdown(t *testing.T) {
	out, err := report.Generate(sampleResult(), report.FormatMarkdown)
	if err != nil {
		t.Fatalf("Generate markdown error: %v", err)
	}
	checks := []string{
		"# Shirakami 分析报告",
		"## 向下追踪",
		"## 向上追踪",
		"## 集成测试场景",
		"## 影响范围",
		"POST /api/v1/payment/process",
		"gRPC",
	}
	for _, needle := range checks {
		if !strings.Contains(out, needle) {
			t.Errorf("markdown output missing %q", needle)
		}
	}
	// Should not contain ANSI codes
	if strings.Contains(out, "\033[") {
		t.Error("markdown output contains ANSI escape codes")
	}
}

func TestGenerateUnknownFormat(t *testing.T) {
	_, err := report.Generate(sampleResult(), "xml")
	if err == nil {
		t.Error("expected error for unknown format")
	}
}

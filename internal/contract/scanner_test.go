package contract

import (
	"strings"
	"testing"
)

func TestNormalisePath(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"http://cvm-api.svc.cluster.local/api/v1/instance/create", "/api/v1/instance/create"},
		{"https://example.com/health", "/health"},
		{"/api/v1/users", "/api/v1/users"},
		{"${BASE_URL}/foo", ""},
		{"", ""},
		{"http://host-only", "/"},
	}
	for _, c := range cases {
		got := normalisePath(c.input)
		if got != c.want {
			t.Errorf("normalisePath(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

func TestRepoNameVariants(t *testing.T) {
	v := repoNameVariants("cvm_api")
	found := false
	for _, vv := range v {
		if vv == "cvm-api" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected cvm-api in variants of cvm_api, got %v", v)
	}
}

func TestResolveProvider(t *testing.T) {
	repos := []RepoInfo{
		{Name: "cvm_api", Path: "/ws/cvm_api"},
		{Name: "bill_calculation", Path: "/ws/bill_calculation"},
	}

	repo, path := resolveProvider("/api/v1/instance", "http://cvm-api.svc.cluster.local/api/v1/instance", repos)
	if repo != "cvm_api" {
		t.Errorf("expected cvm_api, got %q", repo)
	}
	if path != "/api/v1/instance" {
		t.Errorf("expected /api/v1/instance, got %q", path)
	}
}

func TestDeduplicate(t *testing.T) {
	contracts := []FoundContract{
		{CallerRepo: "r1", ProviderRepo: "r2", ProviderPath: "/api/v1", Kind: "http"},
		{CallerRepo: "r1", ProviderRepo: "r2", ProviderPath: "/api/v1", Kind: "http"}, // duplicate
		{CallerRepo: "r1", ProviderRepo: "r2", ProviderPath: "/api/v2", Kind: "http"},
	}
	deduped := Deduplicate(contracts)
	if len(deduped) != 2 {
		t.Errorf("expected 2 after dedup, got %d", len(deduped))
	}
}

func TestRenderYAML(t *testing.T) {
	contracts := []FoundContract{
		{CallerRepo: "consumer", CallerFunc: "CallCreate", ProviderRepo: "cvm_api", ProviderPath: "/api/v1/instance/create", Kind: "http", CallerFile: "client.py", CallerLine: 42},
	}
	yaml := RenderYAML(contracts)
	if !strings.Contains(yaml, "cvm_api") {
		t.Error("expected cvm_api in YAML output")
	}
	if !strings.Contains(yaml, "/api/v1/instance/create") {
		t.Error("expected path in YAML output")
	}
	if !strings.Contains(yaml, "consumer") {
		t.Error("expected consumer repo in YAML output")
	}
}

func TestFormatSummary(t *testing.T) {
	result := &ScanResult{
		Contracts: []FoundContract{
			{CallerRepo: "r1", ProviderRepo: "r2", ProviderPath: "/api", Kind: "http"},
			{CallerRepo: "r1", ProviderRepo: "", ProviderPath: "", Kind: "http", ProviderURL: "http://unknown/foo"},
		},
	}
	summary := FormatSummary(result, false)
	if !strings.Contains(summary, "Total discovered:  2") {
		t.Errorf("expected total 2 in summary, got: %s", summary)
	}
	if !strings.Contains(summary, "Provider resolved: 1") {
		t.Errorf("expected resolved 1 in summary, got: %s", summary)
	}
}

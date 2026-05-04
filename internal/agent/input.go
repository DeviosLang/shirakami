package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// AnalysisInputConfig describes a multi-patch analysis task loaded from a YAML file.
//
// Example YAML:
//
//	source_repo: vstation_compute
//	description: "MR1681 + MR1782 combined impact analysis"
//	patches:
//	  - path: /app/diffs/mr1681.patch
//	    description: "Python 2/3 compat fix"
//	  - path: /app/diffs/mr1782.patch
//	    description: "CBS encryption enhancement"
//	scope:
//	  only_cross_repos: [cvm_api, vstation_api]
type AnalysisInputConfig struct {
	SourceRepo  string          `yaml:"source_repo"`
	Description string          `yaml:"description"`
	Patches     []PatchConfig   `yaml:"patches"`
	Scope       *ScopeConfig    `yaml:"scope,omitempty"`
}

// PatchConfig describes one patch referenced by the YAML config.
type PatchConfig struct {
	Path        string `yaml:"path"`
	Description string `yaml:"description,omitempty"`
}

// ScopeConfig narrows the analysis scope.
type ScopeConfig struct {
	OnlyCrossRepos []string `yaml:"only_cross_repos,omitempty"`
}

// LoadAnalysisConfig reads and parses a YAML analysis config file.
// Relative patch paths are resolved against the config file's directory.
func LoadAnalysisConfig(path string) (*AnalysisInputConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read analysis config %s: %w", path, err)
	}
	var cfg AnalysisInputConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse analysis config %s: %w", path, err)
	}
	if cfg.SourceRepo == "" {
		return nil, fmt.Errorf("analysis config: source_repo is required")
	}
	if len(cfg.Patches) == 0 {
		return nil, fmt.Errorf("analysis config: at least one patch is required")
	}
	// Resolve relative paths against the config file's directory.
	configDir := filepath.Dir(path)
	for i := range cfg.Patches {
		p := cfg.Patches[i].Path
		if p == "" {
			return nil, fmt.Errorf("analysis config: patch #%d missing path", i)
		}
		if !filepath.IsAbs(p) {
			cfg.Patches[i].Path = filepath.Join(configDir, p)
		}
	}
	return &cfg, nil
}

// ToAnalysisInput converts a YAML config into an AnalysisInput by reading
// every patch file and concatenating their contents with visible markers so
// the LLM can treat them as one logical change set.
func (c *AnalysisInputConfig) ToAnalysisInput() (AnalysisInput, error) {
	var combined strings.Builder
	patchInfos := make([]PatchRef, 0, len(c.Patches))

	for _, p := range c.Patches {
		data, err := os.ReadFile(p.Path)
		if err != nil {
			return AnalysisInput{}, fmt.Errorf("read patch %s: %w", p.Path, err)
		}
		desc := p.Description
		if desc == "" {
			desc = filepath.Base(p.Path)
		}
		fmt.Fprintf(&combined, "# === PATCH: %s ===\n", desc)
		combined.Write(data)
		if len(data) > 0 && data[len(data)-1] != '\n' {
			combined.WriteByte('\n')
		}
		patchInfos = append(patchInfos, PatchRef{
			Path:        p.Path,
			Description: desc,
			Bytes:       len(data),
		})
	}

	input := AnalysisInput{
		Diff:        combined.String(),
		Description: c.Description,
		SourceRepo:  c.SourceRepo,
		PatchInfo:   patchInfos,
	}
	if c.Scope != nil {
		input.ScopeOnlyRepos = c.Scope.OnlyCrossRepos
	}
	return input, nil
}

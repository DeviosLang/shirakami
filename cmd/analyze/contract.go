package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/DeviosLang/shirakami/internal/config"
	"github.com/DeviosLang/shirakami/internal/contract"
)

// buildContractCmd creates the `shirakami contract` command group.
func buildContractCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "contract",
		Short: "Manage cross-repo contract declarations",
		Long: `Contract commands discover and manage declared cross-repo call relationships.
Contracts are injected into Worker prompts so the LLM doesn't need to
discover known relationships via ripgrep (faster, higher recall).`,
	}
	cmd.AddCommand(
		buildContractScanCmd(),
		buildContractShowCmd(),
	)
	return cmd
}

// ---------------------------------------------------------------------------
// shirakami contract scan
// ---------------------------------------------------------------------------

func buildContractScanCmd() *cobra.Command {
	var (
		repoName  string
		outputFile string
		showAll   bool
		dryRun    bool
	)

	cmd := &cobra.Command{
		Use:   "scan",
		Short: "Scan repos for cross-repo HTTP/gRPC/MQ calls and emit contracts",
		Long: `Scans workspace repos for outbound HTTP client calls, gRPC stub invocations,
and MQ publish patterns. Resolved contracts (where the callee repo can be identified)
are written to the output file (or stdout with --dry-run).

The output is YAML suitable for pasting into the contracts: section of shirakami.yaml.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(cfgFile)
			if err != nil {
				return err
			}

			if cfg.Workspace.Dir == "" {
				return fmt.Errorf("workspace.dir not configured (set in shirakami.yaml or --workspace flag)")
			}

			// Build repo list for scanner
			var repos []contract.RepoInfo
			for _, r := range cfg.Workspace.Repos {
				if repoName != "" && r.Name != repoName {
					continue
				}
				repoPath := filepath.Join(cfg.Workspace.Dir, r.Name)
				if _, err := os.Stat(repoPath); err != nil {
					continue // repo not checked out yet, skip
				}
				repos = append(repos, contract.RepoInfo{
					Name: r.Name,
					Path: repoPath,
					Role: r.Role,
				})
			}

			if len(repos) == 0 {
				return fmt.Errorf("no repos found in workspace dir %s (run 'shirakami workspace sync' first)", cfg.Workspace.Dir)
			}

			fmt.Printf("Scanning %d repos in %s ...\n", len(repos), cfg.Workspace.Dir)
			start := time.Now()

			scanner := contract.New(cfg.Workspace.Dir, repos)
			result := scanner.Scan()

			// Deduplicate
			result.Contracts = contract.Deduplicate(result.Contracts)

			fmt.Printf("Done in %.1fs\n", time.Since(start).Seconds())
			fmt.Print(contract.FormatSummary(result, showAll))

			// Filter to resolved contracts only
			resolved := contract.FilterResolved(result.Contracts)
			if len(resolved) == 0 {
				fmt.Println("\nNo resolved contracts found. Try checking workspace repos are synced.")
				return nil
			}

			yamlOutput := contract.RenderYAML(resolved)

			if dryRun {
				fmt.Println("\n# --- YAML output (dry run) ---")
				fmt.Println(yamlOutput)
				return nil
			}

			// Write to file
			if outputFile == "" {
				// Default: write to a timestamped file in the workspace dir
				outputFile = filepath.Join(cfg.Workspace.Dir, fmt.Sprintf("contracts-%s.yaml", time.Now().Format("2006-01-02")))
			}

			if err := os.WriteFile(outputFile, []byte(yamlOutput), 0644); err != nil {
				return fmt.Errorf("write output: %w", err)
			}
			fmt.Printf("\nContracts written to: %s\n", outputFile)
			fmt.Printf("Review the output and paste the contracts: section into your shirakami.yaml\n")

			return nil
		},
	}

	cmd.Flags().StringVar(&repoName, "repo", "", "scan only this repo (default: all repos in workspace)")
	cmd.Flags().StringVarP(&outputFile, "output", "o", "", "output file path (default: <workspace>/contracts-<date>.yaml)")
	cmd.Flags().BoolVar(&showAll, "all", false, "show all discovered contracts including unresolved")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print YAML to stdout instead of writing to file")
	return cmd
}

// ---------------------------------------------------------------------------
// shirakami contract show
// ---------------------------------------------------------------------------

func buildContractShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Show currently declared contracts from shirakami.yaml",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(cfgFile)
			if err != nil {
				return err
			}

			if len(cfg.Contracts) == 0 {
				fmt.Println("No contracts declared in shirakami.yaml.")
				fmt.Println("Run 'shirakami contract scan' to discover them automatically.")
				return nil
			}

			fmt.Printf("Declared contracts (%d):\n\n", len(cfg.Contracts))

			// Group by consumer repo
			byConsumer := make(map[string][]config.ContractEntry)
			for _, c := range cfg.Contracts {
				byConsumer[c.Consumer.Repo] = append(byConsumer[c.Consumer.Repo], c)
			}

			for consumerRepo, contracts := range byConsumer {
				fmt.Printf("  [%s]\n", consumerRepo)
				for _, c := range contracts {
					direction := fmt.Sprintf("%s → %s", c.Consumer.Func, c.Provider.Repo)
					if c.Provider.Path != "" {
						direction += " " + c.Provider.Path
					}
					fmt.Printf("    %s\n", direction)
				}
			}

			// Optionally render as YAML
			if v, _ := cmd.Flags().GetBool("yaml"); v {
				data := map[string]interface{}{"contracts": cfg.Contracts}
				out, err := yaml.Marshal(data)
				if err != nil {
					return err
				}
				fmt.Printf("\n# shirakami.yaml contracts section:\n%s", string(out))
			}

			return nil
		},
	}

	cmd.Flags().Bool("yaml", false, "output in YAML format")
	return cmd
}

// ---------------------------------------------------------------------------
// contractHintsFromConfig converts config.ContractEntry → []string hints
// for injection into Worker prompts. Exported for use by main.go.
// ---------------------------------------------------------------------------

// FormatContractHints returns human-readable hint strings from config contracts.
func FormatContractHints(contracts []config.ContractEntry) []string {
	var hints []string
	for _, c := range contracts {
		parts := []string{}
		if c.Consumer.Func != "" {
			parts = append(parts, fmt.Sprintf("%s.%s", c.Consumer.Repo, c.Consumer.Func))
		} else {
			parts = append(parts, c.Consumer.Repo)
		}

		providerStr := c.Provider.Repo
		if c.Provider.Path != "" {
			providerStr += " (" + c.Provider.Path + ")"
		}
		if c.Provider.Func != "" {
			providerStr += " → " + c.Provider.Func
		}
		parts = append(parts, "calls", providerStr)

		hints = append(hints, strings.Join(parts, " "))
	}
	return hints
}

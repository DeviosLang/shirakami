package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"

	"github.com/DeviosLang/shirakami/internal/config"
	"github.com/DeviosLang/shirakami/internal/index"
	"github.com/DeviosLang/shirakami/internal/logger"
)

// buildIndexCmd creates the `shirakami index` command group.
func buildIndexCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "index",
		Short: "Manage symbol index for deterministic call chain analysis",
	}
	cmd.AddCommand(
		buildIndexUpdateCmd(),
		buildIndexCheckCmd(),
		buildIndexRebuildCmd(),
		buildIndexStatusCmd(),
	)
	return cmd
}

// ---------------------------------------------------------------------------
// shirakami index update [--repo <name>]
// ---------------------------------------------------------------------------

func buildIndexUpdateCmd() *cobra.Command {
	var repoName string

	cmd := &cobra.Command{
		Use:   "update",
		Short: "Incrementally update symbol index (only changed files)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(cfgFile)
			if err != nil {
				return err
			}
			log := logger.Must("production")
			defer log.Sync()
			logger.SetDefault(log)

			ctx := context.Background()
			pool, err := pgxpool.New(ctx, cfg.DB.DSN)
			if err != nil {
				return fmt.Errorf("connect db: %w", err)
			}
			defer pool.Close()

			store := index.NewStore(pool)
			repos := getIndexableRepos(cfg, repoName)
			if len(repos) == 0 {
				return fmt.Errorf("no repos to index (check --repo or workspace.repos config)")
			}

			for _, repo := range repos {
				if err := indexRepo(ctx, store, repo, cfg.Workspace.Dir, false); err != nil {
					log.Sugar().Errorw("index.failed", "repo", repo.Name, "err", err)
					fmt.Fprintf(os.Stderr, "ERROR  %s: %v\n", repo.Name, err)
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&repoName, "repo", "", "index a specific repo (default: all Go repos)")
	return cmd
}

// ---------------------------------------------------------------------------
// shirakami index rebuild --repo <name>
// ---------------------------------------------------------------------------

func buildIndexRebuildCmd() *cobra.Command {
	var repoName string

	cmd := &cobra.Command{
		Use:   "rebuild",
		Short: "Full rebuild of symbol index (delete + reindex)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(cfgFile)
			if err != nil {
				return err
			}
			log := logger.Must("production")
			defer log.Sync()
			logger.SetDefault(log)

			ctx := context.Background()
			pool, err := pgxpool.New(ctx, cfg.DB.DSN)
			if err != nil {
				return fmt.Errorf("connect db: %w", err)
			}
			defer pool.Close()

			store := index.NewStore(pool)
			repos := getIndexableRepos(cfg, repoName)
			if len(repos) == 0 {
				return fmt.Errorf("no repos to index")
			}

			for _, repo := range repos {
				if err := indexRepo(ctx, store, repo, cfg.Workspace.Dir, true); err != nil {
					fmt.Fprintf(os.Stderr, "ERROR  %s: %v\n", repo.Name, err)
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&repoName, "repo", "", "repo to rebuild (required)")
	return cmd
}

// ---------------------------------------------------------------------------
// shirakami index check [--repo <name>]
// ---------------------------------------------------------------------------

func buildIndexCheckCmd() *cobra.Command {
	var repoName string

	cmd := &cobra.Command{
		Use:   "check",
		Short: "Check index staleness (read-only)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(cfgFile)
			if err != nil {
				return err
			}

			ctx := context.Background()
			pool, err := pgxpool.New(ctx, cfg.DB.DSN)
			if err != nil {
				return fmt.Errorf("connect db: %w", err)
			}
			defer pool.Close()

			store := index.NewStore(pool)
			repos := getIndexableRepos(cfg, repoName)

			for _, repo := range repos {
				repoPath := cfg.Workspace.Dir + "/" + repo.Name
				head := getGitHEAD(repoPath)

				meta, err := store.GetMetadata(ctx, repo.Name)
				if err != nil {
					fmt.Fprintf(os.Stderr, "ERROR  %s: %v\n", repo.Name, err)
					continue
				}

				if meta == nil {
					fmt.Printf("%-30s  NOT INDEXED\n", repo.Name)
					continue
				}

				status := "CURRENT"
				if meta.CommitHash != head {
					status = "STALE"
				}

				fmt.Printf("%-30s  indexed@%.7s  HEAD@%.7s  symbols=%d  edges=%d  %s\n",
					repo.Name, meta.CommitHash, head, meta.TotalSymbols, meta.TotalEdges, status)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&repoName, "repo", "", "check a specific repo")
	return cmd
}

// ---------------------------------------------------------------------------
// shirakami index status
// ---------------------------------------------------------------------------

func buildIndexStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show index status summary for all repos",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Same as check but with table formatting
			return buildIndexCheckCmd().RunE(cmd, args)
		},
	}
	return cmd
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

type indexableRepo struct {
	Name     string
	Language string
}

func getIndexableRepos(cfg *config.Config, filterRepo string) []indexableRepo {
	var repos []indexableRepo

	for _, r := range cfg.Workspace.Repos {
		if filterRepo != "" && r.Name != filterRepo {
			continue
		}
		// Detect language by checking for go.mod in the repo
		repoPath := cfg.Workspace.Dir + "/" + r.Name
		lang := detectLanguage(repoPath)
		if lang == "go" { // Currently only Go repos are indexable
			repos = append(repos, indexableRepo{Name: r.Name, Language: lang})
		}
	}

	return repos
}

func detectLanguage(repoPath string) string {
	if _, err := os.Stat(repoPath + "/go.mod"); err == nil {
		return "go"
	}
	if _, err := os.Stat(repoPath + "/setup.py"); err == nil {
		return "python"
	}
	if _, err := os.Stat(repoPath + "/requirements.txt"); err == nil {
		return "python"
	}
	if _, err := os.Stat(repoPath + "/pyproject.toml"); err == nil {
		return "python"
	}
	return "unknown"
}

func getGitHEAD(repoPath string) string {
	out, err := exec.Command("git", "-C", repoPath, "rev-parse", "HEAD").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

func indexRepo(ctx context.Context, store *index.Store, repo indexableRepo, workspaceDir string, fullRebuild bool) error {
	repoPath := workspaceDir + "/" + repo.Name
	head := getGitHEAD(repoPath)

	// Check staleness
	if !fullRebuild {
		meta, err := store.GetMetadata(ctx, repo.Name)
		if err == nil && meta != nil && meta.CommitHash == head {
			fmt.Printf("OK     %-30s  already indexed at %s\n", repo.Name, head[:7])
			return nil
		}
	}

	fmt.Printf("INDEX  %-30s  ...", repo.Name)
	start := time.Now()

	// Full rebuild: delete existing data first
	if fullRebuild {
		if err := store.DeleteByRepo(ctx, repo.Name); err != nil {
			return fmt.Errorf("delete existing index: %w", err)
		}
	}

	// Run Go indexer
	indexer := index.NewGoIndexer(repo.Name, repoPath, head)
	result, err := indexer.Index()
	if err != nil {
		return fmt.Errorf("index failed: %w", err)
	}

	// Persist
	if err := store.SaveNodes(ctx, result.Nodes); err != nil {
		return fmt.Errorf("save nodes: %w", err)
	}
	if err := store.SaveEdges(ctx, result.Edges); err != nil {
		return fmt.Errorf("save edges: %w", err)
	}

	duration := time.Since(start)

	// Save metadata
	meta := index.IndexMetadata{
		Repo:         repo.Name,
		CommitHash:   head,
		IndexedAt:    time.Now(),
		TotalFiles:   result.Files,
		TotalSymbols: len(result.Nodes),
		TotalEdges:   len(result.Edges),
		Language:     repo.Language,
		DurationMs:   int(duration.Milliseconds()),
	}
	if err := store.SaveMetadata(ctx, meta); err != nil {
		return fmt.Errorf("save metadata: %w", err)
	}

	fmt.Printf("  %d symbols, %d edges, %d files, %.1fs\n",
		len(result.Nodes), len(result.Edges), result.Files, duration.Seconds())
	return nil
}

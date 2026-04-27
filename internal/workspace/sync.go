package workspace

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// RepoConfig describes a single repository to clone/pull.
type RepoConfig struct {
	// Name is the short identifier used as the directory name inside WorkspaceDir.
	Name string
	// URL is the remote git URL.
	URL string
	// Role describes the repo's role in the system (e.g. "entry", "library").
	Role string
}

// SyncResult holds the outcome for a single repository sync.
type SyncResult struct {
	Name       string
	CommitHash string
	Err        error
}

// SyncAll clones or pulls every repo in the list concurrently.
// The workspace directory is created if it doesn't exist.
// A failure on one repo is recorded in SyncResult.Err but does not block others.
// Returns a map of repoName → SyncResult.
func SyncAll(ctx context.Context, workspaceDir string, repos []RepoConfig) map[string]SyncResult {
	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		// If we can't create the workspace dir all syncs fail.
		results := make(map[string]SyncResult, len(repos))
		for _, r := range repos {
			results[r.Name] = SyncResult{Name: r.Name, Err: fmt.Errorf("create workspace dir: %w", err)}
		}
		return results
	}

	var (
		mu      sync.Mutex
		results = make(map[string]SyncResult, len(repos))
		wg      sync.WaitGroup
	)

	for _, repo := range repos {
		repo := repo
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := syncRepo(ctx, workspaceDir, repo)
			mu.Lock()
			results[repo.Name] = res
			mu.Unlock()
		}()
	}

	wg.Wait()
	return results
}

// syncRepo clones the repo if it doesn't exist locally, otherwise pulls it.
func syncRepo(ctx context.Context, workspaceDir string, repo RepoConfig) SyncResult {
	repoDir := filepath.Join(workspaceDir, repo.Name)

	if _, err := os.Stat(filepath.Join(repoDir, ".git")); os.IsNotExist(err) {
		// Clone (shallow, depth 50).
		cmd := exec.CommandContext(ctx, "git", "clone", "--depth=50", repo.URL, repoDir)
		if out, err := cmd.CombinedOutput(); err != nil {
			return SyncResult{
				Name: repo.Name,
				Err:  fmt.Errorf("git clone %s: %w\n%s", repo.URL, err, out),
			}
		}
	} else {
		// Pull latest.
		cmd := exec.CommandContext(ctx, "git", "-C", repoDir, "pull", "--ff-only")
		if out, err := cmd.CombinedOutput(); err != nil {
			return SyncResult{
				Name: repo.Name,
				Err:  fmt.Errorf("git pull %s: %w\n%s", repo.Name, err, out),
			}
		}
	}

	hash, err := currentCommit(ctx, repoDir)
	if err != nil {
		return SyncResult{Name: repo.Name, Err: err}
	}
	return SyncResult{Name: repo.Name, CommitHash: hash}
}

// currentCommit returns the current HEAD commit hash for the repo at dir.
func currentCommit(ctx context.Context, dir string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

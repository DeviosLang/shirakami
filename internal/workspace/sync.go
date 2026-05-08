package workspace

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// WorktreesSubdir is the subdirectory under workspace.dir where all task worktrees live.
// Keeping it inside workspace.dir (a shirakami-owned NFS PVC mount) ensures cleanup
// never touches other directories on the shared /tmp filesystem.
const WorktreesSubdir = ".worktrees"

// WorktreeBase returns the absolute path to the worktree root for this workspace.
func WorktreeBase(workspaceDir string) string {
	return filepath.Join(workspaceDir, WorktreesSubdir)
}

// CreateWorktree creates a detached git worktree at worktreeDir checked out to ref.
// ref must already be fetchable in the repo at repoDir (e.g. "origin/feature/xxx").
// Any stale worktree registration or directory at worktreeDir is forcibly removed first.
func CreateWorktree(ctx context.Context, repoDir, worktreeDir, ref string) error {
	// Prune stale worktree registrations before adding a new one.
	_ = exec.CommandContext(ctx, "git", "-C", repoDir, "worktree", "prune").Run()
	// Force-remove any existing worktree entry for this path.
	_ = exec.CommandContext(ctx, "git", "-C", repoDir, "worktree", "remove", "--force", worktreeDir).Run()
	_ = os.RemoveAll(worktreeDir)

	if err := os.MkdirAll(filepath.Dir(worktreeDir), 0o755); err != nil {
		return fmt.Errorf("mkdir worktree parent: %w", err)
	}
	cmd := exec.CommandContext(ctx, "git", "-C", repoDir, "worktree", "add", "--detach", worktreeDir, ref)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git worktree add %s %s: %w\n%s", worktreeDir, ref, err, out)
	}
	return nil
}

// RemoveWorktree removes the git worktree at worktreeDir and prunes stale registrations.
// Errors are intentionally swallowed — cleanup is best-effort.
func RemoveWorktree(ctx context.Context, repoDir, worktreeDir string) {
	_ = exec.CommandContext(ctx, "git", "-C", repoDir, "worktree", "remove", "--force", worktreeDir).Run()
	_ = os.RemoveAll(worktreeDir)
	_ = exec.CommandContext(ctx, "git", "-C", repoDir, "worktree", "prune").Run()
}

// CleanupWorktrees removes task worktree directories under {workspaceDir}/.worktrees/
// that are older than maxAge. Pass maxAge=0 to remove everything (used at startup).
// Only operates inside workspace.dir — never touches other directories on /tmp.
func CleanupWorktrees(workspaceDir string, maxAge time.Duration) {
	wtBase := WorktreeBase(workspaceDir)
	entries, err := os.ReadDir(wtBase)
	if err != nil {
		return // directory doesn't exist yet — nothing to clean
	}
	now := time.Now()
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		taskDir := filepath.Join(wtBase, e.Name())
		if maxAge > 0 {
			info, infoErr := e.Info()
			if infoErr != nil || now.Sub(info.ModTime()) < maxAge {
				continue
			}
		}
		// Gracefully remove each repo worktree, then the task directory.
		repoEntries, _ := os.ReadDir(taskDir)
		for _, re := range repoEntries {
			if !re.IsDir() {
				continue
			}
			repoName := re.Name()
			repoDir := filepath.Join(workspaceDir, repoName)
			wtDir := filepath.Join(taskDir, repoName)
			wtCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			RemoveWorktree(wtCtx, repoDir, wtDir)
			cancel()
		}
		_ = os.RemoveAll(taskDir)
	}
}

// RepoConfig describes a single repository to clone/pull.
type RepoConfig struct {
	// Name is the short identifier used as the directory name inside WorkspaceDir.
	Name string
	// URL is the remote git URL.
	URL string
	// Branch is the branch to checkout (defaults to "master" if empty).
	Branch string
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

// gitInfoAttributes is written to {repoDir}/.git/info/attributes after every
// clone or pull. It activates git's built-in language-specific diff drivers so
// that `git diff` appends the enclosing function name after the closing @@ on
// hunk headers (e.g. "@@ -5,6 +5,6 @@ def foo():"). Without this file,
// repositories that lack a top-level .gitattributes produce bare @@ lines and
// ParseDiffFunctions cannot identify which functions were modified.
const gitInfoAttributes = `# Shirakami: activate built-in diff drivers for function-context hunk headers.
# git built-in drivers: python, java, golang, cpp, ruby, bibtex, fortran, html, matlab, objc, pascal, perl, php, python, ruby, rust, tex
*.py   diff=python
*.go   diff=golang
*.java diff=java
*.cpp  diff=cpp
*.cc   diff=cpp
*.cxx  diff=cpp
*.c    diff=cpp
*.h    diff=cpp
*.ts   diff=javascript
*.js   diff=javascript
*.jsx  diff=javascript
*.tsx  diff=javascript
*.rb   diff=ruby
*.rs   diff=rust
*.php  diff=php
`

// WriteGitInfoAttributes writes the language-specific diff driver mappings
// into {repoDir}/.git/info/attributes (exported for use by callers that
// clone repos outside of SyncAll, e.g. on-demand clone in the HTTP server).
func WriteGitInfoAttributes(repoDir string) {
	writeGitInfoAttributes(repoDir)
}

// writeGitInfoAttributes is the unexported implementation.
func writeGitInfoAttributes(repoDir string) {
	attrDir := filepath.Join(repoDir, ".git", "info")
	if err := os.MkdirAll(attrDir, 0o755); err != nil {
		return
	}
	attrFile := filepath.Join(attrDir, "attributes")
	// Only write if file is missing or does not already contain our marker.
	existing, err := os.ReadFile(attrFile)
	if err == nil && strings.Contains(string(existing), "Shirakami:") {
		return // already written
	}
	_ = os.WriteFile(attrFile, []byte(gitInfoAttributes), 0o644)
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

	// Ensure .git/info/attributes exists so that `git diff` emits function names
	// in @@ hunk headers regardless of whether the repo ships a .gitattributes.
	writeGitInfoAttributes(repoDir)

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

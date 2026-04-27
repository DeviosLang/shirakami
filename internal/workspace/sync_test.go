package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initBareRepo creates a minimal bare git repository with one commit and
// returns its absolute path (as a file:// URL usable by git clone).
func initBareRepo(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	bare := filepath.Join(base, "bare.git")
	work := filepath.Join(base, "work")

	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("command %v:\n%s\nerr: %v", args, out, err)
		}
		return strings.TrimSpace(string(out))
	}

	// Build initial commit in a normal (non-bare) work tree.
	run("git", "init", work)
	run("git", "-C", work, "config", "user.email", "test@test.com")
	run("git", "-C", work, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("git", "-C", work, "add", ".")
	run("git", "-C", work, "commit", "-m", "init")

	// Clone as bare so the default branch HEAD is set correctly.
	run("git", "clone", "--bare", work, bare)

	return bare
}

func TestSyncAll_Clone(t *testing.T) {
	bare := initBareRepo(t)
	workspaceDir := t.TempDir()

	repos := []RepoConfig{
		{Name: "testrepo", URL: bare, Role: "service"},
	}

	results := SyncAll(context.Background(), workspaceDir, repos)
	res, ok := results["testrepo"]
	if !ok {
		t.Fatal("no result for testrepo")
	}
	if res.Err != nil {
		t.Fatalf("SyncAll clone error: %v", res.Err)
	}
	if res.CommitHash == "" {
		t.Error("expected non-empty CommitHash after clone")
	}

	if _, err := os.Stat(filepath.Join(workspaceDir, "testrepo", ".git")); err != nil {
		t.Errorf(".git dir not found after clone: %v", err)
	}
}

func TestSyncAll_Pull(t *testing.T) {
	bare := initBareRepo(t)
	workspaceDir := t.TempDir()

	repos := []RepoConfig{
		{Name: "testrepo", URL: bare, Role: "service"},
	}

	// First sync – clone.
	SyncAll(context.Background(), workspaceDir, repos)
	hash1, err := currentCommit(context.Background(), filepath.Join(workspaceDir, "testrepo"))
	if err != nil {
		t.Fatalf("get initial hash: %v", err)
	}

	// Detect default branch name.
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		out, e := cmd.CombinedOutput()
		if e != nil {
			t.Fatalf("command %v:\n%s\nerr: %v", args, out, e)
		}
		return strings.TrimSpace(string(out))
	}
	branch := run("git", "-C", filepath.Join(workspaceDir, "testrepo"), "rev-parse", "--abbrev-ref", "HEAD")

	// Add a new commit from a second clone.
	work2 := filepath.Join(t.TempDir(), "work2")
	run("git", "clone", bare, work2)
	run("git", "-C", work2, "config", "user.email", "test@test.com")
	run("git", "-C", work2, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(work2, "extra.txt"), []byte("more\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("git", "-C", work2, "add", ".")
	run("git", "-C", work2, "commit", "-m", "second commit")
	run("git", "-C", work2, "push", "origin", "HEAD:refs/heads/"+branch)

	// Second sync – should pull the new commit.
	results := SyncAll(context.Background(), workspaceDir, repos)
	res := results["testrepo"]
	if res.Err != nil {
		t.Fatalf("SyncAll pull error: %v", res.Err)
	}

	hash2 := res.CommitHash
	if hash1 == hash2 {
		t.Error("commit hash did not change after pull – new commit was not fetched")
	}
}

func TestSyncAll_MultipleRepos(t *testing.T) {
	bare1 := initBareRepo(t)
	bare2 := initBareRepo(t)
	workspaceDir := t.TempDir()

	repos := []RepoConfig{
		{Name: "repo1", URL: bare1, Role: "entry"},
		{Name: "repo2", URL: bare2, Role: "service"},
	}

	results := SyncAll(context.Background(), workspaceDir, repos)
	if len(results) != 2 {
		t.Errorf("expected 2 results, got %d", len(results))
	}
	for _, name := range []string{"repo1", "repo2"} {
		if results[name].Err != nil {
			t.Errorf("%s: unexpected error: %v", name, results[name].Err)
		}
		if results[name].CommitHash == "" {
			t.Errorf("%s: empty commit hash", name)
		}
	}
}

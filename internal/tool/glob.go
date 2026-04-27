package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
)

// GlobTool lists files matching a glob pattern.
type GlobTool struct {
	// WorkspaceDir restricts glob searches to this root.
	WorkspaceDir string
}

func NewGlobTool(workspaceDir string) *GlobTool {
	return &GlobTool{WorkspaceDir: workspaceDir}
}

func (t *GlobTool) Name() string { return "glob" }

func (t *GlobTool) Description() string {
	return "List files matching a glob pattern (e.g. 'internal/**/*.go'). Returns a newline-separated list of matching file paths."
}

func (t *GlobTool) InputSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"pattern": map[string]interface{}{
				"type":        "string",
				"description": "Glob pattern to match files, e.g. 'internal/**/*.go'",
			},
		},
		"required": []string{"pattern"},
	}
}

type globInput struct {
	Pattern string `json:"pattern"`
}

func (t *GlobTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var inp globInput
	if err := json.Unmarshal(input, &inp); err != nil {
		return "", fmt.Errorf("glob: invalid input: %w", err)
	}
	if inp.Pattern == "" {
		return "", fmt.Errorf("glob: pattern is required")
	}

	// Resolve base directory
	base := t.WorkspaceDir
	if base == "" {
		base = "."
	}
	absBase, err := filepath.Abs(base)
	if err != nil {
		return "", fmt.Errorf("glob: cannot resolve workspace dir: %w", err)
	}

	matches, err := matchGlob(ctx, absBase, inp.Pattern)
	if err != nil {
		return "", fmt.Errorf("glob: %w", err)
	}

	// Make paths relative to workspace
	rel := make([]string, 0, len(matches))
	for _, m := range matches {
		if r, relErr := filepath.Rel(absBase, m); relErr == nil {
			rel = append(rel, r)
		} else {
			rel = append(rel, m)
		}
	}

	if len(rel) == 0 {
		return "No files matched.", nil
	}
	return strings.Join(rel, "\n"), nil
}

// matchGlob returns all files under root that match pattern.
// Pattern is relative to root and may contain ** for recursive matching.
func matchGlob(ctx context.Context, root, pattern string) ([]string, error) {
	// If pattern doesn't contain **, use standard filepath.Glob
	if !strings.Contains(pattern, "**") {
		fullPattern := filepath.Join(root, pattern)
		matches, err := filepath.Glob(fullPattern)
		if err != nil {
			return nil, fmt.Errorf("invalid pattern %q: %w", pattern, err)
		}
		return matches, nil
	}

	// For ** patterns, walk the tree and match each file
	var results []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil // skip unreadable entries
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if d.IsDir() {
			// Skip hidden directories (e.g. .git)
			if strings.HasPrefix(d.Name(), ".") && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		// Get path relative to root for pattern matching
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		matched, err := matchDoubleStarGlob(pattern, rel)
		if err != nil {
			return nil
		}
		if matched {
			results = append(results, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

// matchDoubleStarGlob matches a file path against a pattern that may contain **.
// ** matches any number of path segments (including zero).
func matchDoubleStarGlob(pattern, path string) (bool, error) {
	// Normalize separators
	pattern = filepath.ToSlash(pattern)
	path = filepath.ToSlash(path)

	patParts := strings.Split(pattern, "/")
	pathParts := strings.Split(path, "/")

	return matchParts(patParts, pathParts)
}

func matchParts(patParts, pathParts []string) (bool, error) {
	for len(patParts) > 0 && len(pathParts) > 0 {
		p := patParts[0]
		if p == "**" {
			// ** can match zero segments
			if len(patParts) == 1 {
				return true, nil
			}
			// Try matching remaining pattern against every suffix of pathParts
			for i := 0; i <= len(pathParts); i++ {
				ok, err := matchParts(patParts[1:], pathParts[i:])
				if err != nil {
					return false, err
				}
				if ok {
					return true, nil
				}
			}
			return false, nil
		}
		// Regular glob match for this segment
		matched, err := filepath.Match(p, pathParts[0])
		if err != nil {
			return false, err
		}
		if !matched {
			return false, nil
		}
		patParts = patParts[1:]
		pathParts = pathParts[1:]
	}
	// Consume trailing ** patterns
	for len(patParts) > 0 && patParts[0] == "**" {
		patParts = patParts[1:]
	}
	return len(patParts) == 0 && len(pathParts) == 0, nil
}


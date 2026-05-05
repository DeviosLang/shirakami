// Package contract provides automatic cross-repo contract discovery.
//
// It scans workspace repositories for HTTP client calls, gRPC stubs, and
// message-queue publish/subscribe patterns, then emits ContractEntry values
// suitable for injection into shirakami.yaml.
package contract

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// FoundContract represents a single discovered cross-repo call relationship.
type FoundContract struct {
	CallerRepo string // repo making the call
	CallerFile string // file relative to repo root
	CallerLine int    // approximate line number
	CallerFunc string // enclosing function (best-effort)

	// Provider side (may be partial if URL is dynamic)
	ProviderURL  string // raw URL / path fragment found in source
	ProviderRepo string // resolved repo name (empty if unknown)
	ProviderPath string // normalised HTTP/gRPC path

	Kind string // "http", "grpc", "mq_publish", "mq_subscribe"
}

// ScanResult aggregates all discovered contracts for a workspace.
type ScanResult struct {
	Contracts []FoundContract
	Warnings  []string // files that could not be read, etc.
}

// Scanner scans workspace repos for cross-repo contracts.
type Scanner struct {
	workspaceDir string
	repos        []RepoInfo
}

// RepoInfo holds the minimal info the scanner needs per repo.
type RepoInfo struct {
	Name string
	Path string // absolute path
	Role string // "entry", "service", etc.
}

// New creates a Scanner for the given workspace.
func New(workspaceDir string, repos []RepoInfo) *Scanner {
	return &Scanner{workspaceDir: workspaceDir, repos: repos}
}

// Scan walks all repos and returns discovered contracts.
func (s *Scanner) Scan() *ScanResult {
	result := &ScanResult{}
	for _, repo := range s.repos {
		found, warns := scanRepo(repo, s.repos)
		result.Contracts = append(result.Contracts, found...)
		result.Warnings = append(result.Warnings, warns...)
	}
	return result
}

// ---------------------------------------------------------------------------
// Per-repo scanning
// ---------------------------------------------------------------------------

func scanRepo(repo RepoInfo, allRepos []RepoInfo) ([]FoundContract, []string) {
	var contracts []FoundContract
	var warnings []string

	err := filepath.WalkDir(repo.Path, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			warnings = append(warnings, "walk error: "+path+": "+err.Error())
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			// Skip hidden dirs, vendor, node_modules, .git, __pycache__, venv, etc.
			if strings.HasPrefix(name, ".") ||
				name == "vendor" || name == "node_modules" ||
				name == "__pycache__" || name == "venv" || name == ".venv" ||
				name == "dist" || name == "build" || name == "migrations" {
				return filepath.SkipDir
			}
			return nil
		}

		ext := strings.ToLower(filepath.Ext(path))
		switch ext {
		case ".go", ".py":
		default:
			return nil
		}

		rel, _ := filepath.Rel(repo.Path, path)
		found, err := scanFile(path, rel, repo, allRepos)
		if err != nil {
			warnings = append(warnings, "read error: "+path+": "+err.Error())
			return nil
		}
		contracts = append(contracts, found...)
		return nil
	})
	if err != nil {
		warnings = append(warnings, "walk error: "+repo.Path+": "+err.Error())
	}
	return contracts, warnings
}

// ---------------------------------------------------------------------------
// Per-file scanning with line-by-line patterns
// ---------------------------------------------------------------------------

// Go patterns
var (
	// http.Get("http://hostname/path"), client.Get("http://..."), resty/req patterns
	goHTTPURLRe = regexp.MustCompile(`(?i)(?:Get|Post|Put|Patch|Delete|Do|Request)\s*\(\s*["` + "`" + `]([^"` + "`" + `]+)["` + "`" + `]`)

	// strings.Join / fmt.Sprintf constructed URLs - grab the path fragment
	goHTTPPathRe = regexp.MustCompile(`(?i)(?:url|path|endpoint)\s*:?=\s*["` + "`" + `]([/][^"` + "`" + `\s]+)["` + "`" + `]`)

	// grpc.Dial("hostname:port") or grpc.NewClient("...")
	goGRPCDialRe = regexp.MustCompile(`(?i)grpc\.(?:Dial|NewClient)\s*\(\s*["` + "`" + `]([^"` + "`" + `]+)["` + "`" + `]`)

	// MQ publish: producer.SendMessage, channel.Publish, topic.Publish
	goMQPublishRe = regexp.MustCompile(`(?i)(?:SendMessage|Publish|ProduceMessage)\s*\(`)

	// Enclosing func: func FuncName(
	goFuncDeclRe = regexp.MustCompile(`^func\s+(?:\([^)]+\)\s+)?(\w+)\s*\(`)
)

// Python patterns
var (
	// requests.get("url"), requests.post("url"), httpx.get(...)
	pyHTTPCallRe = regexp.MustCompile(`(?i)(?:requests|httpx|aiohttp|urllib)\s*\.\s*(?:get|post|put|patch|delete|request)\s*\(\s*["'f]([^"'\s)]+)`)

	// session.get("url"), self.client.get(...)
	pyClientCallRe = regexp.MustCompile(`(?i)(?:self\.\w+|client|session)\s*\.\s*(?:get|post|put|patch|delete)\s*\(\s*["'f]([^"'\s)]+)`)

	// path constants: PATH = "/api/v1/..."
	pyPathConstRe = regexp.MustCompile(`(?i)(?:URL|PATH|ENDPOINT|BASE_URL)\s*=\s*["'](https?://[^"'\s]+|/[^"'\s]+)["']`)

	// gRPC stub: stub.MethodName(request)
	pyGRPCStubRe = regexp.MustCompile(`(?i)(?:stub|channel)\s*\.\s*(\w+)\s*\(`)

	// MQ: producer.send, channel.basic_publish
	pyMQPublishRe = regexp.MustCompile(`(?i)(?:producer|channel|publisher)\s*\.\s*(?:send|publish|basic_publish|produce)\s*\(`)

	// Enclosing def: def func_name(
	pyFuncDeclRe = regexp.MustCompile(`^(?:async\s+)?def\s+(\w+)\s*\(`)
)

func scanFile(path, rel string, repo RepoInfo, allRepos []RepoInfo) ([]FoundContract, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	ext := strings.ToLower(filepath.Ext(path))

	var contracts []FoundContract
	currentFunc := ""
	lineNo := 0

	scanner := bufio.NewScanner(f)
	// Increase buffer for long lines
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, 64*1024)

	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		switch ext {
		case ".go":
			// Track current function
			if m := goFuncDeclRe.FindStringSubmatch(trimmed); m != nil {
				currentFunc = m[1]
			}
			// HTTP calls
			if m := goHTTPURLRe.FindStringSubmatch(line); m != nil {
				if c := makeContract(repo, rel, lineNo, currentFunc, m[1], "http", allRepos); c != nil {
					contracts = append(contracts, *c)
				}
			}
			if m := goHTTPPathRe.FindStringSubmatch(line); m != nil {
				if c := makeContract(repo, rel, lineNo, currentFunc, m[1], "http", allRepos); c != nil {
					contracts = append(contracts, *c)
				}
			}
			// gRPC
			if m := goGRPCDialRe.FindStringSubmatch(line); m != nil {
				if c := makeContract(repo, rel, lineNo, currentFunc, m[1], "grpc", allRepos); c != nil {
					contracts = append(contracts, *c)
				}
			}
			// MQ
			if goMQPublishRe.MatchString(line) {
				if c := makeContract(repo, rel, lineNo, currentFunc, "", "mq_publish", allRepos); c != nil {
					contracts = append(contracts, *c)
				}
			}

		case ".py":
			// Track current function
			if m := pyFuncDeclRe.FindStringSubmatch(trimmed); m != nil {
				currentFunc = m[1]
			}
			if m := pyHTTPCallRe.FindStringSubmatch(line); m != nil {
				if c := makeContract(repo, rel, lineNo, currentFunc, m[1], "http", allRepos); c != nil {
					contracts = append(contracts, *c)
				}
			}
			if m := pyClientCallRe.FindStringSubmatch(line); m != nil {
				if c := makeContract(repo, rel, lineNo, currentFunc, m[1], "http", allRepos); c != nil {
					contracts = append(contracts, *c)
				}
			}
			if m := pyPathConstRe.FindStringSubmatch(line); m != nil {
				if c := makeContract(repo, rel, lineNo, currentFunc, m[1], "http", allRepos); c != nil {
					contracts = append(contracts, *c)
				}
			}
			if pyGRPCStubRe.MatchString(line) {
				if c := makeContract(repo, rel, lineNo, currentFunc, "", "grpc", allRepos); c != nil {
					contracts = append(contracts, *c)
				}
			}
			if pyMQPublishRe.MatchString(line) {
				if c := makeContract(repo, rel, lineNo, currentFunc, "", "mq_publish", allRepos); c != nil {
					contracts = append(contracts, *c)
				}
			}
		}
	}
	return contracts, scanner.Err()
}

// ---------------------------------------------------------------------------
// Contract construction helpers
// ---------------------------------------------------------------------------

func makeContract(repo RepoInfo, file string, line int, callerFunc, rawURL, kind string, allRepos []RepoInfo) *FoundContract {
	// Filter out internal package references and test files
	if strings.Contains(file, "_test.go") || strings.Contains(file, "test_") {
		return nil
	}

	// Normalise the URL to a path
	path := normalisePath(rawURL)
	if path == "" && kind != "mq_publish" && kind != "grpc" {
		return nil
	}

	// Try to resolve which repo this path points to
	providerRepo, normPath := resolveProvider(path, rawURL, allRepos)
	// Skip if it points to itself (intra-repo)
	if providerRepo == repo.Name {
		return nil
	}

	return &FoundContract{
		CallerRepo:   repo.Name,
		CallerFile:   file,
		CallerLine:   line,
		CallerFunc:   callerFunc,
		ProviderURL:  rawURL,
		ProviderRepo: providerRepo,
		ProviderPath: normPath,
		Kind:         kind,
	}
}

// normalisePath extracts the path component from a URL or returns the path as-is.
func normalisePath(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	// Strip scheme+host from absolute URLs
	for _, prefix := range []string{"http://", "https://"} {
		if strings.HasPrefix(strings.ToLower(raw), prefix) {
			rest := raw[len(prefix):]
			if slash := strings.Index(rest, "/"); slash >= 0 {
				return rest[slash:] // path starting at /
			}
			return "/" // root
		}
	}
	// Already a path
	if strings.HasPrefix(raw, "/") {
		return raw
	}
	// Variable / template — skip
	if strings.ContainsAny(raw, "${%") {
		return ""
	}
	return ""
}

// resolveProvider tries to match a path against known repo names or URL patterns.
// Returns (repoName, normalisedPath).
func resolveProvider(path, rawURL string, allRepos []RepoInfo) (string, string) {
	if path == "" {
		return "", ""
	}

	// Match against repo names embedded in the URL hostname.
	// e.g. "cvm-api.svc.cluster.local/api/v1/instance" → cvm_api
	lower := strings.ToLower(rawURL)
	for _, r := range allRepos {
		// Normalise repo name for fuzzy matching: cvm_api → cvm-api, cvmapi
		variants := repoNameVariants(r.Name)
		for _, v := range variants {
			if strings.Contains(lower, v) {
				return r.Name, path
			}
		}
	}

	// Fallback: no provider resolved
	return "", path
}

// repoNameVariants returns lowercase slug variants of a repo name for URL matching.
func repoNameVariants(name string) []string {
	lower := strings.ToLower(name)
	variants := []string{lower}
	// underscore → dash
	if strings.Contains(lower, "_") {
		variants = append(variants, strings.ReplaceAll(lower, "_", "-"))
	}
	// underscore → nothing
	if strings.Contains(lower, "_") {
		variants = append(variants, strings.ReplaceAll(lower, "_", ""))
	}
	// dash → underscore (for path segments)
	if strings.Contains(lower, "-") {
		variants = append(variants, strings.ReplaceAll(lower, "-", "_"))
	}
	return variants
}

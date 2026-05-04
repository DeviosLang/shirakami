package tool

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	lspjson "encoding/json"
)

// CallNode represents a single call hierarchy node.
type CallNode struct {
	File     string `json:"file"`
	Line     int    `json:"line"`
	FuncName string `json:"func_name"`
	Repo     string `json:"repo"`
}

// LSPTool manages an LSP server process and exposes call hierarchy operations.
// It auto-detects the repository language and selects the appropriate server:
//   - Go        → gopls
//   - Python     → pyright (pyright-langserver)
//   - TypeScript/JavaScript → typescript-language-server
//   - (others)  → gopls as fallback (may not work, but won't panic)
type LSPTool struct {
	// WorkspaceDir is the root of the repository to analyse.
	WorkspaceDir string
	// Language is auto-detected from repository contents.
	// May be overridden before first use.
	Language string

	mu      sync.Mutex
	proc    *exec.Cmd
	stdin   *bufio.Writer
	stdout  *bufio.Reader
	nextID  int
	started bool
}

func NewLSPTool(workspaceDir string) *LSPTool {
	return &LSPTool{WorkspaceDir: workspaceDir}
}

func (t *LSPTool) Name() string { return "lsp_call_hierarchy" }

func (t *LSPTool) Description() string {
	lang := t.detectLanguage()
	server := lspServerForLanguage(lang)
	return fmt.Sprintf(
		"Query call hierarchy via %s LSP (%s). "+
			"Operations: incomingCalls (find callers), outgoingCalls (find callees). "+
			"Use 'repo' parameter to specify which repository to analyse.",
		server, lang,
	)
}

// detectLanguage returns the primary language of WorkspaceDir by scanning
// marker files and source file extensions.
func (t *LSPTool) detectLanguage() string {
	if t.Language != "" {
		return t.Language
	}
	lang := detectRepoLanguage(t.WorkspaceDir)
	t.Language = lang
	return lang
}

// detectRepoLanguage scans a directory for language marker files.
// Priority: marker files first, then dominant file extension.
func detectRepoLanguage(dir string) string {
	if dir == "" {
		return "go"
	}

	// Marker files → definitive language signal.
	markers := map[string]string{
		"go.mod":           "go",
		"go.sum":           "go",
		"pyproject.toml":   "python",
		"setup.py":         "python",
		"setup.cfg":        "python",
		"requirements.txt": "python",
		"Pipfile":          "python",
		"package.json":     "typescript",
		"tsconfig.json":    "typescript",
		"pom.xml":          "java",
		"build.gradle":     "java",
		"CMakeLists.txt":   "cpp",
		"Cargo.toml":       "rust",
	}
	for marker, lang := range markers {
		if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
			return lang
		}
	}

	// Count source files by extension.
	counts := map[string]int{}
	extLang := map[string]string{
		".go": "go", ".py": "python", ".ts": "typescript",
		".tsx": "typescript", ".js": "typescript",
		".java": "java", ".cpp": "cpp", ".cc": "cpp", ".rs": "rust",
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if lang, ok := extLang[ext]; ok {
			counts[lang]++
		}
	}
	best, bestCount := "go", 0
	for lang, count := range counts {
		if count > bestCount {
			best, bestCount = lang, count
		}
	}
	return best
}

// lspServerForLanguage returns the LSP server binary name for a language.
// Returns "" if no suitable server is known (caller should skip LSP).
func lspServerForLanguage(lang string) string {
	switch lang {
	case "go":
		return "gopls"
	case "python":
		// pyright supports callHierarchy; pylsp and jedi-language-server do not.
		return "pyright-langserver"
	case "typescript", "javascript":
		return "typescript-language-server"
	case "java":
		return "jdtls"
	case "cpp":
		return "clangd"
	case "rust":
		return "rust-analyzer"
	default:
		return "gopls" // best-effort fallback
	}
}

// lspServerArgs returns the command-line arguments for a language server.
func lspServerArgs(server string) []string {
	switch server {
	case "gopls":
		return []string{"-mode=stdio"}
	case "pyright-langserver":
		return []string{"--stdio"}
	case "typescript-language-server":
		return []string{"--stdio"}
	case "clangd":
		return []string{}
	default:
		return []string{"--stdio"}
	}
}

func (t *LSPTool) InputSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"operation": map[string]interface{}{
				"type":        "string",
				"enum":        []string{"incomingCalls", "outgoingCalls"},
				"description": "incomingCalls: find callers of the function; outgoingCalls: find callees",
			},
			"file_path": map[string]interface{}{
				"type":        "string",
				"description": "Absolute or relative path to the Go source file",
			},
			"line": map[string]interface{}{
				"type":        "integer",
				"description": "1-based line number of the function declaration",
			},
			"character": map[string]interface{}{
				"type":        "integer",
				"description": "1-based character offset within the line",
			},
		},
		"required": []string{"operation", "file_path", "line", "character"},
	}
}

type lspInput struct {
	Operation string `json:"operation"`
	FilePath  string `json:"file_path"`
	Line      int    `json:"line"`
	Character int    `json:"character"`
}

func (t *LSPTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var inp lspInput
	if err := json.Unmarshal(input, &inp); err != nil {
		return "", fmt.Errorf("lsp_call_hierarchy: invalid input: %w", err)
	}
	if inp.FilePath == "" {
		return "", fmt.Errorf("lsp_call_hierarchy: file_path is required")
	}
	if inp.Operation != "incomingCalls" && inp.Operation != "outgoingCalls" {
		return "", fmt.Errorf("lsp_call_hierarchy: operation must be incomingCalls or outgoingCalls")
	}

	absFile, err := filepath.Abs(inp.FilePath)
	if err != nil {
		return "", fmt.Errorf("lsp_call_hierarchy: cannot resolve file path: %w", err)
	}
	fileURI := pathToURI(absFile)

	t.mu.Lock()
	if err := t.ensureStarted(ctx); err != nil {
		t.mu.Unlock()
		return "", fmt.Errorf("lsp_call_hierarchy: failed to start gopls: %w", err)
	}
	t.mu.Unlock()

	// Step 1: textDocument/prepareCallHierarchy
	prepareParams := map[string]interface{}{
		"textDocument": map[string]interface{}{"uri": fileURI},
		"position": map[string]interface{}{
			"line":      inp.Line - 1,
			"character": inp.Character - 1,
		},
	}

	prepareResult, err := t.sendRequest(ctx, "textDocument/prepareCallHierarchy", prepareParams)
	if err != nil {
		return "", fmt.Errorf("lsp_call_hierarchy: prepareCallHierarchy failed: %w", err)
	}

	// Parse prepareCallHierarchy result (array of CallHierarchyItem)
	var items []lspCallHierarchyItem
	if err := lspjson.Unmarshal(prepareResult, &items); err != nil || len(items) == 0 {
		return "No call hierarchy item found at this position.", nil
	}

	// Step 2: call hierarchy incoming/outgoing calls using first item
	var callMethod string
	if inp.Operation == "incomingCalls" {
		callMethod = "callHierarchy/incomingCalls"
	} else {
		callMethod = "callHierarchy/outgoingCalls"
	}

	callParams := map[string]interface{}{"item": items[0]}
	callResult, err := t.sendRequest(ctx, callMethod, callParams)
	if err != nil {
		return "", fmt.Errorf("lsp_call_hierarchy: %s failed: %w", callMethod, err)
	}

	repo := t.repoName()

	var nodes []CallNode
	if inp.Operation == "incomingCalls" {
		var calls []lspIncomingCall
		if err := lspjson.Unmarshal(callResult, &calls); err != nil {
			return "", fmt.Errorf("lsp_call_hierarchy: parse incomingCalls failed: %w", err)
		}
		for _, c := range calls {
			filePath := uriToPath(c.From.URI)
			line := 1
			if len(c.FromRanges) > 0 {
				line = c.FromRanges[0].Start.Line + 1
			}
			nodes = append(nodes, CallNode{
				File:     filePath,
				Line:     line,
				FuncName: c.From.Name,
				Repo:     repo,
			})
		}
	} else {
		var calls []lspOutgoingCall
		if err := lspjson.Unmarshal(callResult, &calls); err != nil {
			return "", fmt.Errorf("lsp_call_hierarchy: parse outgoingCalls failed: %w", err)
		}
		for _, c := range calls {
			filePath := uriToPath(c.To.URI)
			line := c.To.Range.Start.Line + 1
			nodes = append(nodes, CallNode{
				File:     filePath,
				Line:     line,
				FuncName: c.To.Name,
				Repo:     repo,
			})
		}
	}

	if len(nodes) == 0 {
		return fmt.Sprintf("No %s found.", inp.Operation), nil
	}

	out, _ := lspjson.MarshalIndent(nodes, "", "  ")
	return string(out), nil
}

// ensureStarted starts gopls if not already running. Caller must hold t.mu.
// If the process previously started but has since died, it is automatically
// restarted (health check / auto-restart).
func (t *LSPTool) ensureStarted(ctx context.Context) error {
	if t.started {
		// Health check: verify the process is still alive.
		if t.proc != nil && t.proc.ProcessState != nil {
			// ProcessState is non-nil only after Wait() has been called or the
			// process has exited. Reset and fall through to restart.
			t.started = false
			t.proc = nil
		} else if t.proc != nil {
			// Try signal(0) to check liveness without actually killing.
			if err := t.proc.Process.Signal(syscall.Signal(0)); err != nil {
				// Process is gone — reset and restart.
				t.started = false
				t.proc = nil
			} else {
				return nil
			}
		} else {
			return nil
		}
	}

	workDir := t.WorkspaceDir
	if workDir == "" {
		workDir = "."
	}
	absWork, err := filepath.Abs(workDir)
	if err != nil {
		return err
	}

	lang := t.detectLanguage()
	server := lspServerForLanguage(lang)
	args := lspServerArgs(server)

	// Check if the language server binary exists; skip if not installed.
	if _, err := exec.LookPath(server); err != nil {
		return fmt.Errorf("lsp server %q not found (language: %s): %w", server, lang, err)
	}

	cmd := exec.CommandContext(ctx, server, args...)
	cmd.Dir = absWork

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("lsp stdin pipe: %w", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("lsp stdout pipe: %w", err)
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("lsp start (%s): %w", server, err)
	}

	t.proc = cmd
	t.stdin = bufio.NewWriter(stdinPipe)
	t.stdout = bufio.NewReader(stdoutPipe)
	t.nextID = 1
	t.started = true

	// Build language-specific initialization params.
	initParams := buildInitParams(absWork, lang)

	if _, err := t.sendRequest(ctx, "initialize", initParams); err != nil {
		return fmt.Errorf("lsp initialize (%s): %w", server, err)
	}
	// Send initialized notification
	if err := t.sendNotification("initialized", map[string]interface{}{}); err != nil {
		return fmt.Errorf("lsp initialized notification: %w", err)
	}
	// Delay to let the language server index the workspace.
	// pyright needs more time than gopls for large Python projects.
	delay := 500 * time.Millisecond
	if lang == "python" {
		delay = 2 * time.Second
	}
	time.Sleep(delay)

	return nil
}

// buildInitParams builds LSP initialize parameters tailored to each language.
//
// Key differences:
//   - pyright: needs pythonPath, venvPath, pythonVersion in initializationOptions
//              and workspace folders to correctly resolve imports
//   - gopls:   standard rootUri is sufficient
//   - others:  standard rootUri + workspace folders
func buildInitParams(absWork, lang string) map[string]interface{} {
	rootURI := pathToURI(absWork)

	// Common capabilities required by all language servers.
	capabilities := map[string]interface{}{
		"textDocument": map[string]interface{}{
			"callHierarchy": map[string]interface{}{
				"dynamicRegistration": false,
			},
			"synchronization": map[string]interface{}{
				"dynamicRegistration": false,
			},
		},
		"workspace": map[string]interface{}{
			"workspaceFolders": true,
		},
	}

	base := map[string]interface{}{
		"processId":        os.Getpid(),
		"rootUri":          rootURI,
		"rootPath":         absWork,
		"capabilities":     capabilities,
		"workspaceFolders": []map[string]interface{}{{"uri": rootURI, "name": filepath.Base(absWork)}},
	}

	switch lang {
	case "python":
		// pyright-specific initialization options.
		// pythonPath: look for common virtual environment locations.
		pythonPath := findPythonInterpreter(absWork)

		base["initializationOptions"] = map[string]interface{}{
			// Tell pyright which Python interpreter to use.
			"pythonPath": pythonPath,
			// Disable type-checking diagnostics — we only want call hierarchy.
			"typeCheckingMode": "off",
			// Use the project root as the include path.
			"include": []string{absWork},
			// Auto-search for virtual environments.
			"venvPath": absWork,
		}

		// Also write a minimal pyrightconfig.json if one doesn't exist,
		// so pyright can resolve imports correctly.
		writePyrightConfig(absWork)

	case "go":
		// gopls works well with just rootUri; no extra options needed.

	case "typescript":
		base["initializationOptions"] = map[string]interface{}{
			"preferences": map[string]interface{}{
				"includeInlayParameterNameHints": "none",
			},
		}
	}

	return base
}

// findPythonInterpreter searches common locations for a Python interpreter.
func findPythonInterpreter(projectRoot string) string {
	// 1. Virtual environment in project.
	candidates := []string{
		filepath.Join(projectRoot, ".venv", "bin", "python"),
		filepath.Join(projectRoot, "venv", "bin", "python"),
		filepath.Join(projectRoot, "env", "bin", "python"),
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	// 2. System Python.
	for _, name := range []string{"python3", "python"} {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	return "python3"
}

// writePyrightConfig writes a minimal pyrightconfig.json to projectRoot
// if one does not already exist.  This helps pyright resolve imports correctly
// for projects that don't ship a pyrightconfig.json.
func writePyrightConfig(projectRoot string) {
	cfgPath := filepath.Join(projectRoot, "pyrightconfig.json")
	if _, err := os.Stat(cfgPath); err == nil {
		return // already exists, don't overwrite
	}
	cfg := `{
  "include": ["."],
  "exclude": ["**/__pycache__", "**/node_modules", "**/.git"],
  "reportMissingImports": false,
  "reportMissingModuleSource": false,
  "typeCheckingMode": "off",
  "useLibraryCodeForTypes": true
}
`
	// Best-effort write; ignore errors (read-only FS, etc.)
	_ = os.WriteFile(cfgPath, []byte(cfg), 0644)
}

// lspRequest is a JSON-RPC 2.0 request.
type lspRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int         `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params"`
}

// lspNotification is a JSON-RPC 2.0 notification (no id).
type lspNotification struct {
	JSONRPC string      `json:"jsonrpc"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params"`
}

// lspResponse is a JSON-RPC 2.0 response.
type lspResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  lspjson.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (t *LSPTool) sendNotification(method string, params interface{}) error {
	notif := lspNotification{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
	}
	data, err := lspjson.Marshal(notif)
	if err != nil {
		return err
	}
	msg := fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(data), data)
	_, err = t.stdin.WriteString(msg)
	if err != nil {
		return err
	}
	return t.stdin.Flush()
}

func (t *LSPTool) sendRequest(ctx context.Context, method string, params interface{}) (lspjson.RawMessage, error) {
	t.mu.Lock()
	id := t.nextID
	t.nextID++
	req := lspRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}
	data, err := lspjson.Marshal(req)
	if err != nil {
		t.mu.Unlock()
		return nil, err
	}
	msg := fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(data), data)
	if _, err := t.stdin.WriteString(msg); err != nil {
		t.mu.Unlock()
		return nil, err
	}
	if err := t.stdin.Flush(); err != nil {
		t.mu.Unlock()
		return nil, err
	}
	t.mu.Unlock()

	// Read response, skipping notifications
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		resp, err := t.readMessage()
		if err != nil {
			return nil, fmt.Errorf("read LSP message: %w", err)
		}

		// Check if this is the response for our request
		if resp.ID == id {
			if resp.Error != nil {
				return nil, fmt.Errorf("LSP error %d: %s", resp.Error.Code, resp.Error.Message)
			}
			return resp.Result, nil
		}
		// Otherwise it's a notification or response for a different request; skip
	}
}

func (t *LSPTool) readMessage() (*lspResponse, error) {
	// Read headers
	var contentLength int
	for {
		line, err := t.stdout.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("read header: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if strings.HasPrefix(line, "Content-Length: ") {
			n, err := strconv.Atoi(strings.TrimPrefix(line, "Content-Length: "))
			if err != nil {
				return nil, fmt.Errorf("parse Content-Length: %w", err)
			}
			contentLength = n
		}
	}
	if contentLength == 0 {
		return nil, fmt.Errorf("missing Content-Length header")
	}

	body := make([]byte, contentLength)
	if _, err := t.stdout.Read(body); err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	var resp lspResponse
	if err := lspjson.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	return &resp, nil
}

// Close shuts down the gopls process.
func (t *LSPTool) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.started && t.proc != nil {
		_ = t.proc.Process.Kill()
		_ = t.proc.Wait()
		t.started = false
	}
}

func (t *LSPTool) repoName() string {
	if t.WorkspaceDir == "" {
		return ""
	}
	return filepath.Base(t.WorkspaceDir)
}

// LSP protocol types

type lspPosition struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type lspRange struct {
	Start lspPosition `json:"start"`
	End   lspPosition `json:"end"`
}

type lspCallHierarchyItem struct {
	Name           string   `json:"name"`
	Kind           int      `json:"kind"`
	URI            string   `json:"uri"`
	Range          lspRange `json:"range"`
	SelectionRange lspRange `json:"selectionRange"`
}

type lspIncomingCall struct {
	From       lspCallHierarchyItem `json:"from"`
	FromRanges []lspRange           `json:"fromRanges"`
}

type lspOutgoingCall struct {
	To         lspCallHierarchyItem `json:"to"`
	FromRanges []lspRange           `json:"fromRanges"`
}

// LSPManager is a process-level singleton that reuses gopls instances across
// multiple tool calls within the same analysis session. Each unique workspace
// directory gets exactly one gopls process.
//
// Usage:
//
//	tool := GlobalLSPManager.GetOrCreate("/path/to/workspace")
//	defer GlobalLSPManager.Close()   // call at session end
var GlobalLSPManager = &lspManager{tools: make(map[string]*LSPTool)}

type lspManager struct {
	mu    sync.Mutex
	tools map[string]*LSPTool
}

// GetOrCreate returns an LSPTool for the given workspace, creating one if it
// does not exist yet. The returned tool is shared — callers must not call
// Close() on it directly; use lspManager.Close() or lspManager.CloseOne().
func (m *lspManager) GetOrCreate(workspaceDir string) *LSPTool {
	abs, err := filepath.Abs(workspaceDir)
	if err != nil {
		abs = workspaceDir
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.tools[abs]; ok {
		return t
	}
	t := NewLSPTool(abs)
	m.tools[abs] = t
	return t
}

// CloseOne shuts down the gopls process for a specific workspace and removes
// it from the manager so a fresh one will be created on next GetOrCreate.
func (m *lspManager) CloseOne(workspaceDir string) {
	abs, err := filepath.Abs(workspaceDir)
	if err != nil {
		abs = workspaceDir
	}
	m.mu.Lock()
	t, ok := m.tools[abs]
	if ok {
		delete(m.tools, abs)
	}
	m.mu.Unlock()
	if ok {
		t.Close()
	}
}

// Close shuts down all managed gopls processes.
func (m *lspManager) Close() {
	m.mu.Lock()
	tools := make([]*LSPTool, 0, len(m.tools))
	for _, t := range m.tools {
		tools = append(tools, t)
	}
	m.tools = make(map[string]*LSPTool)
	m.mu.Unlock()
	for _, t := range tools {
		t.Close()
	}
}

// pathToURI converts an absolute file path to a file:// URI.
func pathToURI(path string) string {
	u := &url.URL{
		Scheme: "file",
		Path:   path,
	}
	return u.String()
}

// uriToPath converts a file:// URI back to a file path.
func uriToPath(uri string) string {
	u, err := url.Parse(uri)
	if err != nil {
		return uri
	}
	return u.Path
}

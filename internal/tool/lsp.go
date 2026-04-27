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

// LSPTool manages a gopls process and exposes call hierarchy operations.
type LSPTool struct {
	// WorkspaceDir is the root of the Go workspace/module.
	WorkspaceDir string

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
	return "Query the Go call hierarchy via gopls LSP. Operations: incomingCalls (callers of a function) or outgoingCalls (callees of a function)."
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
func (t *LSPTool) ensureStarted(ctx context.Context) error {
	if t.started {
		return nil
	}

	workDir := t.WorkspaceDir
	if workDir == "" {
		workDir = "."
	}
	absWork, err := filepath.Abs(workDir)
	if err != nil {
		return err
	}

	cmd := exec.CommandContext(ctx, "gopls", "-mode=stdio")
	cmd.Dir = absWork

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("gopls stdin pipe: %w", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("gopls stdout pipe: %w", err)
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("gopls start: %w", err)
	}

	t.proc = cmd
	t.stdin = bufio.NewWriter(stdinPipe)
	t.stdout = bufio.NewReader(stdoutPipe)
	t.nextID = 1
	t.started = true

	// Initialize LSP
	initParams := map[string]interface{}{
		"processId": os.Getpid(),
		"rootUri":   pathToURI(absWork),
		"capabilities": map[string]interface{}{
			"textDocument": map[string]interface{}{
				"callHierarchy": map[string]interface{}{
					"dynamicRegistration": false,
				},
			},
		},
	}
	if _, err := t.sendRequest(ctx, "initialize", initParams); err != nil {
		return fmt.Errorf("gopls initialize: %w", err)
	}
	// Send initialized notification
	if err := t.sendNotification("initialized", map[string]interface{}{}); err != nil {
		return fmt.Errorf("gopls initialized notification: %w", err)
	}
	// Small delay to let gopls process workspace
	time.Sleep(500 * time.Millisecond)

	return nil
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

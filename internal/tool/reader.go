package tool

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// ReaderTool reads files with line numbers.
type ReaderTool struct{}

func NewReaderTool() *ReaderTool { return &ReaderTool{} }

func (t *ReaderTool) Name() string { return "file_reader" }

func (t *ReaderTool) Description() string {
	return "Read a file with line numbers (cat -n format). Supports offset and limit for reading large files in segments."
}

func (t *ReaderTool) InputSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"file_path": map[string]interface{}{
				"type":        "string",
				"description": "Path to the file to read",
			},
			"offset": map[string]interface{}{
				"type":        "integer",
				"description": "1-based starting line number. Defaults to 1.",
			},
			"limit": map[string]interface{}{
				"type":        "integer",
				"description": "Number of lines to read. Defaults to 200.",
			},
		},
		"required": []string{"file_path"},
	}
}

type readerInput struct {
	FilePath string `json:"file_path"`
	Offset   int    `json:"offset"`
	Limit    int    `json:"limit"`
}

func (t *ReaderTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var inp readerInput
	if err := json.Unmarshal(input, &inp); err != nil {
		return "", fmt.Errorf("file_reader: invalid input: %w", err)
	}
	if inp.FilePath == "" {
		return "", fmt.Errorf("file_reader: file_path is required")
	}

	offset := inp.Offset
	if offset <= 0 {
		offset = 1
	}
	limit := inp.Limit
	if limit <= 0 {
		limit = 200
	}

	f, err := os.Open(inp.FilePath)
	if err != nil {
		return "", fmt.Errorf("file_reader: cannot open %q: %w", inp.FilePath, err)
	}
	defer f.Close()

	var sb strings.Builder
	scanner := bufio.NewScanner(f)
	lineNum := 0
	written := 0

	for scanner.Scan() {
		// Check context cancellation every line
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}

		lineNum++
		if lineNum < offset {
			continue
		}
		if written >= limit {
			break
		}

		// cat -n format: right-aligned 6-char line number followed by tab and content
		fmt.Fprintf(&sb, "%6d\t%s\n", lineNum, scanner.Text())
		written++
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("file_reader: read error: %w", err)
	}

	if written == 0 {
		return fmt.Sprintf("(file has fewer than %d lines)", offset), nil
	}
	return sb.String(), nil
}

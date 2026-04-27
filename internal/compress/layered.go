package compress

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/DeviosLang/shirakami/internal/tool"
)

// LayeredReader wraps the file_reader tool and enforces a maximum read level.
//
// Levels:
//
//	1 – filename + line number only (sourced from ripgrep results; no content returned)
//	2 – function signature context: offset = lineNum-5, limit = 30
//	3 – full file (unrestricted; blocked when token usage > 70%)
type LayeredReader struct {
	reader   *tool.ReaderTool
	maxLevel int
}

// NewLayeredReader creates a LayeredReader with maxLevel set to 3 (unrestricted).
func NewLayeredReader(reader *tool.ReaderTool) *LayeredReader {
	if reader == nil {
		reader = tool.NewReaderTool()
	}
	return &LayeredReader{
		reader:   reader,
		maxLevel: 3,
	}
}

// SetMaxLevel dynamically adjusts the maximum read level.
// Valid values are 1, 2, or 3. Values outside this range are clamped.
func (l *LayeredReader) SetMaxLevel(level int) {
	if level < 1 {
		level = 1
	}
	if level > 3 {
		level = 3
	}
	l.maxLevel = level
}

// MaxLevel returns the current maximum read level.
func (l *LayeredReader) MaxLevel() int {
	return l.maxLevel
}

// ReadFile reads the file according to the current maxLevel and the supplied
// parameters.
//
//   - level 1: returns only the filename and lineNum, no file I/O performed.
//   - level 2: reads up to 30 lines starting from max(1, lineNum-5).
//   - level 3: reads the full file (offset=1, limit=200 default).
//
// If requestedLevel > maxLevel the call is rejected with an error.
func (l *LayeredReader) ReadFile(ctx context.Context, filePath string, lineNum int, requestedLevel int) (string, error) {
	if requestedLevel < 1 {
		requestedLevel = 1
	}

	if requestedLevel > l.maxLevel {
		return "", fmt.Errorf("layered_reader: level %d is blocked (current max=%d, token budget exceeded)", requestedLevel, l.maxLevel)
	}

	switch requestedLevel {
	case 1:
		// Return only filename + line number – no actual file read.
		return fmt.Sprintf("%s:%d", filePath, lineNum), nil

	case 2:
		// Function signature context: offset = lineNum-5, limit = 30.
		offset := lineNum - 5
		if offset < 1 {
			offset = 1
		}
		input := readerInput{
			FilePath: filePath,
			Offset:   offset,
			Limit:    30,
		}
		raw, err := json.Marshal(input)
		if err != nil {
			return "", fmt.Errorf("layered_reader: marshal input: %w", err)
		}
		return l.reader.Execute(ctx, raw)

	default: // level 3
		// Full file read with default parameters.
		input := readerInput{
			FilePath: filePath,
			Offset:   1,
			Limit:    200,
		}
		raw, err := json.Marshal(input)
		if err != nil {
			return "", fmt.Errorf("layered_reader: marshal input: %w", err)
		}
		return l.reader.Execute(ctx, raw)
	}
}

// readerInput mirrors the JSON structure consumed by tool.ReaderTool.Execute.
type readerInput struct {
	FilePath string `json:"file_path"`
	Offset   int    `json:"offset"`
	Limit    int    `json:"limit"`
}

package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/DeviosLang/shirakami/internal/logger"
)

// Tool defines the interface that all tools must implement.
type Tool interface {
	// Name returns the unique name of the tool.
	Name() string
	// Description returns a human-readable description of the tool.
	Description() string
	// InputSchema returns the JSON Schema describing the tool's input parameters.
	InputSchema() map[string]interface{}
	// Execute runs the tool with the given JSON-encoded input and returns the result string.
	Execute(ctx context.Context, input json.RawMessage) (string, error)
}

// ToolDefinition is the LLM function calling format for a tool.
type ToolDefinition struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"input_schema"`
}

// Registry holds all registered tools.
type Registry struct {
	tools map[string]Tool
	cache *toolCache // optional; nil means caching is disabled
}

// NewRegistry creates an empty tool registry.
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

// WithCache enables the in-memory LRU result cache for this registry.
// maxSize is the maximum number of entries; ttl is the per-entry lifetime.
// Returns the receiver for fluent chaining.
func (r *Registry) WithCache(maxSize int, ttl time.Duration) *Registry {
	r.cache = newToolCache(maxSize, ttl)
	return r
}

// Register adds a tool to the registry.
// Returns an error if a tool with the same name already exists.
func (r *Registry) Register(t Tool) error {
	name := t.Name()
	if _, exists := r.tools[name]; exists {
		return fmt.Errorf("tool %q already registered", name)
	}
	r.tools[name] = t
	return nil
}

// MustRegister adds a tool to the registry and panics on error.
func (r *Registry) MustRegister(t Tool) {
	if err := r.Register(t); err != nil {
		panic(err)
	}
}

// Get returns the named tool or an error if not found.
func (r *Registry) Get(name string) (Tool, error) {
	t, ok := r.tools[name]
	if !ok {
		return nil, fmt.Errorf("tool %q not found", name)
	}
	return t, nil
}

// Definitions returns all registered tools in LLM function-calling format.
func (r *Registry) Definitions() []ToolDefinition {
	defs := make([]ToolDefinition, 0, len(r.tools))
	for _, t := range r.tools {
		defs = append(defs, ToolDefinition{
			Name:        t.Name(),
			Description: t.Description(),
			InputSchema: t.InputSchema(),
		})
	}
	return defs
}

// isCacheable reports whether results from the named tool may be cached.
// Only read-only, side-effect-free tools are eligible:
// ripgrep / rg, glob, file_reader, symbol_extractor.
// LSP (real-time call hierarchy) and gitdiff (state-dependent) are excluded.
func isCacheable(name string) bool {
	n := strings.ToLower(name)
	return strings.Contains(n, "ripgrep") ||
		strings.Contains(n, "rg") ||
		strings.Contains(n, "glob") ||
		strings.Contains(n, "reader") ||
		strings.Contains(n, "symbol")
}

// Execute dispatches an Execute call to the named tool.
func (r *Registry) Execute(ctx context.Context, name string, input json.RawMessage) (string, error) {
	t, err := r.Get(name)
	if err != nil {
		return "", err
	}

	// Cache look-up (only for read-only tools).
	if r.cache != nil && isCacheable(name) {
		key := cacheKey(name, input)
		if hit, ok := r.cache.get(key); ok {
			logger.S().Debugw("tool.cache_hit", "tool", name)
			return hit, nil
		}
		result, execErr := t.Execute(ctx, input)
		if execErr == nil {
			r.cache.set(key, result)
		}
		return result, execErr
	}

	return t.Execute(ctx, input)
}

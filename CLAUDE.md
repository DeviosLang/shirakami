# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Shirakami is a cross-repository code call-chain analysis system built on an LLM Agent Loop. Given code changes (diffs and/or text descriptions), it traces the complete call chain across multiple Git repositories, identifies integration test entry points, and generates test scenario suggestions.

**Language:** Go (module `github.com/DeviosLang/shirakami`)  
**External dependencies:** PostgreSQL 14+, Redis 7+, ripgrep (`rg`), gopls, pyright (for Python repos)

## Build & Run Commands

```bash
# Build CLI tool
make build                    # → bin/shirakami

# Build HTTP server
make build-server             # → bin/shirakami-server

# Build both
make build-all

# Run unit tests (no Docker needed)
make test                     # or: go test ./...

# Run a single package's tests
go test ./internal/agent/... -v

# Run integration tests (needs Docker for PostgreSQL + Redis)
make test-integration         # or: go test ./tests/e2e/... -v -count=1 -timeout=5m

# Lint
make lint                     # or: go vet ./...

# Start dependencies
docker compose up -d

# Run database migrations
goose -dir migrations postgres "postgres://shirakami:shirakami@localhost:5432/shirakami?sslmode=disable" up
```

## Architecture

### Entry Points (cmd/)

- `cmd/analyze/main.go` — CLI entrypoint using cobra. Subcommands: `analyze`, `results`, `feedback`, `workspace sync`.
- `cmd/server/main.go` — HTTP API server (POST /analyze, GET /tasks/{id}, POST /feedback, GET /metrics).

### Core Agent System (internal/agent/)

The analysis pipeline flows: **Input → Orchestrator → WorkerAgents → Report**

- **`orchestrator.go`** — Coordinates multi-repo analysis. Parses diffs to extract changed functions, launches concurrent WorkerAgents per repository, follows cross-repo calls iteratively (up to 10 rounds in deep mode, 3 in fast mode), merges results.
- **`loop.go`** — Core `AgentLoop` implementing an end_turn state machine (max 300 steps). Sends messages+tools to LLM; on `tool_use` stop reason, executes tools concurrently then loops. Supports checkpoint-based resume after crash.
- **`worker.go`** — `WorkerAgent` performs single-repo call-chain tracing using ripgrep. Produces structured JSON with nodes, cross_repo_calls, entry_points, search_results. Includes follow-up passes for scenario generation and UT suggestions.
- **`triage.go`** — `TriageAgent` classifies changed files by priority (P0/P1/P2) before Worker execution. P2 files get shallow tracing with a tighter step budget (50 steps).
- **`prompt.go`** — System prompt builder for Worker agents.
- **`input.go`** — YAML analysis config loader for multi-patch batch analysis.

### Memory Layer (internal/memory/)

Three-layer memory system:

- **Layer1** (`layer1.go`) — PostgreSQL-backed long-term knowledge base. Stores symbol-level semantic summaries keyed by (repo, symbol, commit_hash). Supports keyword-based retrieval filtered to current HEAD.
- **Layer2** (`layer2.go`) — Redis-backed task state and progress tracking. Stores current step count and analyzed node list per task (24h TTL).
- **Layer3** (`layer3.go`) — Builds dynamic system reminder strings (max 2000 tokens) by pulling relevant knowledge from Layer1 and progress from Layer2. Injected before each LLM call when token budget triggers.

### Token Budget Manager (internal/compress/)

Manages context window usage with four ascending thresholds (ABCD tiers):

| Threshold | Strategy | Action |
|-----------|----------|--------|
| 60% | Plan D | Inject condensed system reminder via Layer3 |
| 70% | Plan B | Restrict LayeredReader to level ≤ 2 (no full file reads) |
| 80% | Plan C | Clear code blocks from already-analyzed nodes |
| 92% | Plan A | Compress full conversation history via LLM |

- **`layered.go`** — `LayeredReader` wraps file reading at 3 levels: (1) filename+line only, (2) 30-line signature context, (3) full file. Level is dynamically restricted by token budget.

### Tool System (internal/tool/)

Tools available to agent loops (registered via `Registry`):

- **`ripgrep.go`** — Code symbol search via `rg`. Supports optional `repo` parameter for multi-repo workspaces.
- **`reader.go`** — File content reader with offset/limit support.
- **`glob.go`** — File pattern matching. Supports optional `repo` parameter.
- **`lsp.go`** — LSP integration (gopls for Go, pyright for Python). Provides call hierarchy queries.
- **`gitdiff.go`** — Changed function extraction from unified diffs.
- **`symbol.go`** — Symbol resolution utilities.

### Other Packages

- **`internal/config/`** — Viper-based config loading from `shirakami.yaml`. Environment variables (prefix `SHIRAKAMI_`) override file values.
- **`internal/llm/`** — OpenAI-compatible client (`client.go`), streaming support (`stream.go`), token counting (`token.go`).
- **`internal/checkpoint/`** — File-based checkpoint persistence for crash recovery. Saves messages + step count per task ID.
- **`internal/cache/`** — Redis-backed analysis result caching. Cache key derived from diff+description+source_repo.
- **`internal/storage/`** — PostgreSQL task/result persistence (tasks, task_results, feedback tables).
- **`internal/report/`** — Output rendering in terminal tree, JSON, or Markdown formats.
- **`internal/workspace/`** — Git clone/pull synchronization for configured repos.
- **`internal/feedback/`** — Feedback collection, self-check, and Prometheus metrics.
- **`pkg/schema/`** — Public schema types for `AnalysisResult`, `CallNode`, `EntryPoint`, etc. Used by report generation and HTTP API responses.

## Key Design Patterns

- **Orchestrator → Worker concurrency**: Workers run up to 6 in parallel (`maxWorkerConcurrency`), scheduled by triage priority tier (P0 first, then P1, then P2).
- **Cross-repo call extraction**: Triple-path merge strategy — LLM primary output + search result file paths + nodes from call tree. Maximizes recall.
- **FILE_CHANGED sentinels**: When LLM misses functions in a file, sentinel entries trigger broad file search in the next Worker.
- **Follow-up passes**: After main tracing, Workers run `RunFollowUpNoTools` for scenario generation and UT suggestions (avoids LLM wasting time on unnecessary tool calls).
- **Tool adapter pattern**: `internal/tool.Tool` interface is bridged to `agent.Tool` interface via `toolAdapter` in `cmd/analyze/main.go`.

## Configuration

Config file: `shirakami.yaml` (or `config/shirakami.example.yaml` for reference).

Key environment variables:
- `SHIRAKAMI_LLM_API_KEY` — LLM API key (required)
- `SHIRAKAMI_LLM_ENDPOINT` — LLM API base URL
- `SHIRAKAMI_LLM_MODEL` — Model name (default: gpt-4o)
- `SHIRAKAMI_DB_DSN` — PostgreSQL connection string (required)
- `SHIRAKAMI_REDIS_ADDR` — Redis address (default: localhost:6379)

## Testing

- Unit tests live alongside source files (`*_test.go` in same package).
- Integration tests requiring Docker: `tests/integration/` (PostgreSQL, Redis via testcontainers).
- E2E tests: `tests/e2e/` (full pipeline with real LLM calls).
- Integration test files use build tag or testcontainers auto-detection; run with `-timeout=5m`.

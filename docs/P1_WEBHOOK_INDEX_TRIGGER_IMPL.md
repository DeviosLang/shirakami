# Priority 1: Webhook 自动触发索引更新 - 实现指南

## 概述

当 GitLab MR 或 GitHub PR 事件到达时，自动在后台触发相关仓库的索引更新，确保随后的分析基于最新的代码符号图。

---

## 设计

### 信息流

```
GitHub/GitLab MR/PR Event
    ↓
webhook.Handler.ServeHTTP()  [internal/webhook/handler.go:87-162]
    ├─ 验证签名 ✓
    ├─ 解析事件 ✓
    ├─ 创建分析任务 ✓
    │
    └─ [NEW] 触发索引更新
        └─ ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
           IndexUpdater.UpdateIndexAsync(ctx, event.RepoName, "incremental")
```

### 关键设计决策

1. **异步执行**: 不阻塞 webhook 响应（使用 goroutine）
2. **5 分钟超时**: 防止索引更新进程挂起
3. **增量模式**: 使用 `git diff` 仅扫描变化的文件
4. **错误容忍**: 索引失败不影响分析任务创建

---

## 实现步骤

### Step 1: 定义 IndexUpdater 接口

**文件**: `internal/index/updater.go` (新文件)

```go
package index

import (
    "context"
    "fmt"
)

// UpdateMode specifies how to perform the index update.
type UpdateMode string

const (
    UpdateModeFull        UpdateMode = "full"
    UpdateModeIncremental UpdateMode = "incremental"
)

// Updater defines the interface for updating the symbol index.
type Updater interface {
    // UpdateIndex updates the index for a given repository.
    // mode can be "full" (rebuild all) or "incremental" (only changed files).
    // Returns error if update fails.
    UpdateIndex(ctx context.Context, repo string, mode UpdateMode) error
}

// AsyncUpdater wraps Updater and provides fire-and-forget semantics.
type AsyncUpdater struct {
    updater Updater
}

// NewAsyncUpdater wraps an Updater with async semantics.
func NewAsyncUpdater(u Updater) *AsyncUpdater {
    return &AsyncUpdater{updater: u}
}

// UpdateIndexAsync starts an async index update. Errors are logged but not returned.
func (a *AsyncUpdater) UpdateIndexAsync(ctx context.Context, repo string, mode UpdateMode) {
    // This is intentionally called WITHOUT goroutine at this layer;
    // the caller (webhook handler) decides whether to use goroutine.
    // This keeps separation of concerns clean.
    if err := a.updater.UpdateIndex(ctx, repo, mode); err != nil {
        // In production, this should log to structured logger:
        // log.WithError(err).Errorf("async index update failed for repo %s", repo)
        fmt.Printf("[ERROR] Index update failed for %s: %v\n", repo, err)
    }
}
```

### Step 2: Implement Updater in storage layer

**文件**: `internal/storage/index_updater.go` (新文件)

```go
package storage

import (
    "context"
    "fmt"
    "os"
    "path/filepath"

    "github.com/DeviosLang/shirakami/internal/index"
)

// IndexUpdaterImpl implements index.Updater using the storage layer.
type IndexUpdaterImpl struct {
    store  *Store
    workspaceDir string
}

// NewIndexUpdater creates a new IndexUpdater.
func NewIndexUpdater(store *Store, workspaceDir string) *IndexUpdaterImpl {
    return &IndexUpdaterImpl{
        store: store,
        workspaceDir: workspaceDir,
    }
}

// UpdateIndex updates the index for the given repository.
func (u *IndexUpdaterImpl) UpdateIndex(ctx context.Context, repoName string, mode index.UpdateMode) error {
    // Step 1: Get repository path
    repoPath := filepath.Join(u.workspaceDir, repoName)
    if _, err := os.Stat(repoPath); err != nil {
        return fmt.Errorf("repo not found at %s: %w", repoPath, err)
    }

    // Step 2: Determine update behavior
    fullRebuild := (mode == index.UpdateModeFull)

    // Step 3: Call existing indexRepo logic from cmd/analyze/index.go
    // (This should be refactored into a reusable function)
    return u.indexRepo(ctx, repoName, repoPath, fullRebuild)
}

// indexRepo performs the actual indexing.
// This is similar to cmd/analyze/index.go:indexRepo() but refactored here.
func (u *IndexUpdaterImpl) indexRepo(ctx context.Context, repoName, repoPath string, fullRebuild bool) error {
    // TODO: Extract indexRepo logic from cmd/analyze/index.go into a shared function
    // For now, this is a placeholder that shows the structure.
    
    // Step 1: Get current git HEAD
    head, err := getGitHEAD(repoPath)
    if err != nil {
        return fmt.Errorf("get git HEAD: %w", err)
    }

    // Step 2: Check if already indexed
    metadata, err := u.store.GetMetadata(ctx, repoName)
    if err == nil && metadata.CommitHash == head && !fullRebuild {
        // Already indexed at this commit, skip
        return nil
    }

    // Step 3: Full rebuild if requested
    if fullRebuild {
        if err := u.store.DeleteByRepo(ctx, repoName); err != nil {
            return fmt.Errorf("delete old index: %w", err)
        }
    }

    // Step 4: Detect language and index
    lang, err := detectLanguage(repoPath)
    if err != nil {
        return fmt.Errorf("detect language: %w", err)
    }

    var result *index.IndexResult
    switch lang {
    case "go":
        indexer := index.NewGoIndexer(repoName, repoPath, head)
        result, err = indexer.Index(ctx)
    case "python":
        indexer := index.NewPythonIndexer(repoName, repoPath, head)
        result, err = indexer.Index(ctx)
    default:
        return fmt.Errorf("unsupported language: %s", lang)
    }

    if err != nil {
        return fmt.Errorf("index: %w", err)
    }

    // Step 5: Save to database
    if err := u.store.SaveNodes(ctx, result.Nodes); err != nil {
        return fmt.Errorf("save nodes: %w", err)
    }
    if err := u.store.SaveEdges(ctx, result.Edges); err != nil {
        return fmt.Errorf("save edges: %w", err)
    }
    if err := u.store.SaveMetadata(ctx, &IndexMetadata{
        Repo: repoName,
        CommitHash: head,
        TotalFiles: int64(len(result.Nodes)),
        TotalSymbols: int64(len(result.Nodes)),
        TotalEdges: int64(len(result.Edges)),
        Language: lang,
    }); err != nil {
        return fmt.Errorf("save metadata: %w", err)
    }

    return nil
}

// Helper functions (should be extracted from cmd/analyze/index.go)
func getGitHEAD(repoPath string) (string, error) {
    // Implementation: similar to cmd/analyze/index.go:238-244
    return "", nil // TODO: Implement
}

func detectLanguage(repoPath string) (string, error) {
    // Implementation: similar to cmd/analyze/index.go:222-236
    return "", nil // TODO: Implement
}
```

### Step 3: Update webhook Config

**文件**: `internal/webhook/handler.go`

修改 `Config` 结构体（大约第 62 行）:

```go
// Config holds runtime options for the webhook handler.
type Config struct {
    // Secret is the shared secret for signature verification.
    // Empty = verification disabled (not recommended for production).
    Secret string

    // Commenter posts task IDs / analysis summaries back to the MR/PR.
    // nil = silent (no comments posted).
    Commenter Commenter

    // Launch starts the background analysis after task creation.
    Launch AnalysisLauncher

    // Updater triggers index updates for the affected repository.
    // nil = no index updates.
    Updater IndexUpdater  // [NEW]
}

// IndexUpdater defines the interface for triggering index updates.
type IndexUpdater interface {
    UpdateIndex(ctx context.Context, repo string, mode string) error
}
```

### Step 4: Update webhook ServeHTTP

**文件**: `internal/webhook/handler.go:142-162` (ServeHTTP 函数末尾)

修改 webhook 处理：

```go
// ... existing code ...

// Launch background analysis (non-blocking).
if h.cfg.Launch != nil {
    go h.cfg.Launch(rec.ID, event.Diff, event.Description, cacheKey)
}

// [NEW] Trigger index update (non-blocking, best-effort).
if h.cfg.Updater != nil {
    go func() {
        updateCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
        defer cancel()
        if err := h.cfg.Updater.UpdateIndex(updateCtx, event.RepoName, "incremental"); err != nil {
            // Log error but don't fail webhook response
            _ = err  // In production: log.Error("index update failed", err)
        }
    }()
}

// Post comment (non-blocking, best-effort).
if h.cfg.Commenter != nil {
    go func() {
        // ... existing comment posting code ...
    }()
}

w.Header().Set("Content-Type", "application/json")
w.WriteHeader(http.StatusAccepted)
json.NewEncoder(w).Encode(map[string]string{
    "status":  "accepted",
    "task_id": rec.ID,
})
```

### Step 5: Wire up in server initialization

**文件**: `cmd/server/main.go`

在服务器启动时集成索引更新器：

```go
package main

import (
    "github.com/DeviosLang/shirakami/internal/storage"
    "github.com/DeviosLang/shirakami/internal/webhook"
)

func main() {
    // ... existing setup ...

    // Initialize storage and index updater
    store := storage.NewStore(db)
    indexUpdater := storage.NewIndexUpdater(store, workspaceDir)

    // Create webhook config with index updater
    whConfig := webhook.Config{
        Secret: cfg.WebhookSecret,
        Commenter: &webhook.GitLabCommenter{
            Token: cfg.GitLabToken,
            BaseURL: cfg.GitLabURL,
        },
        Launch: orchestrator.LaunchAnalysis,
        Updater: indexUpdater,  // [NEW]
    }

    whHandler := webhook.New(store, whConfig)

    // ... rest of server setup ...
}
```

---

## Testing Strategy

### Unit Tests

**文件**: `internal/index/updater_test.go` (新文件)

```go
package index

import (
    "context"
    "testing"
)

func TestAsyncUpdater(t *testing.T) {
    // Test that errors are logged but not panicked
    mockUpdater := &mockUpdater{
        shouldFail: true,
    }
    asyncUpdater := NewAsyncUpdater(mockUpdater)
    
    ctx := context.Background()
    // Should not panic
    asyncUpdater.UpdateIndexAsync(ctx, "test-repo", UpdateModeIncremental)
}
```

### Integration Tests

**文件**: `tests/e2e/webhook_index_update_test.go` (新文件)

```go
package e2e

import (
    "context"
    "testing"
    "time"

    "github.com/DeviosLang/shirakami/internal/webhook"
)

func TestWebhookTriggersIndexUpdate(t *testing.T) {
    // Setup: create test webhook handler with tracking updater
    trackingUpdater := &TrackingUpdater{}
    
    config := webhook.Config{
        Secret: "test-secret",
        Updater: trackingUpdater,
    }
    handler := webhook.New(store, config)

    // Send test GitLab MR event
    req := createTestGitLabMREvent()
    w := httptest.NewRecorder()
    handler.ServeHTTP(w, req)

    // Verify response
    if w.Code != http.StatusAccepted {
        t.Fatalf("expected 202, got %d", w.Code)
    }

    // Wait for async update to complete
    time.Sleep(100 * time.Millisecond)

    // Verify index updater was called
    if !trackingUpdater.Called {
        t.Fatal("updater was not called")
    }
    if trackingUpdater.LastRepo != "test-repo" {
        t.Fatalf("expected repo 'test-repo', got %s", trackingUpdater.LastRepo)
    }
}
```

---

## Deployment Checklist

- [ ] Refactor `cmd/analyze/index.go:indexRepo()` into shared `internal/storage/index_updater.go`
- [ ] Create `internal/index/updater.go` with `Updater` interface
- [ ] Implement `IndexUpdaterImpl` in `internal/storage/index_updater.go`
- [ ] Update `internal/webhook/handler.go:Config` to include `Updater`
- [ ] Update `internal/webhook/handler.go:ServeHTTP()` to call updater
- [ ] Update `cmd/server/main.go` to wire up index updater
- [ ] Add unit tests in `internal/index/updater_test.go`
- [ ] Add integration tests in `tests/e2e/webhook_index_update_test.go`
- [ ] Update K8s deployment to include index updater initialization
- [ ] Test with real GitLab/GitHub webhook payloads
- [ ] Monitor production for index update latency and errors
- [ ] Document configuration options (timeout, retry policy)

---

## Rollback Plan

If issues arise in production:

1. **Quick disable**: Set `Updater: nil` in webhook config to disable index updates
2. **Revert code**: `git revert` to previous commit
3. **Monitor**: Check index staleness metrics to verify no regressions

---

## Performance Expectations

**Scenario**: 10 PRs/day, average repo size 50k LOC

- **Without incremental**: 10 × 2min = 20 min/day
- **With incremental**: 10 × 12sec = 2 min/day
- **Speedup**: ~10×

**Resource impact**:
- CPU: +5-10% during update (short lived)
- Memory: +100-200 MB (temporary)
- Database: Normal insert/update operations

---

## Monitoring

### Metrics to track

```
shirakami_webhook_index_update_count{repo="*", status="success|failed"}
shirakami_webhook_index_update_duration_seconds{repo="*"}
shirakami_index_staleness_seconds{repo="*"}
```

### Alerts

```
# Alert if index older than 1 hour
ALERT IndexStale = index_staleness_seconds > 3600
```


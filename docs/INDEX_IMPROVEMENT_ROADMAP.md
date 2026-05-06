# Shirakami 索引系统改进路线图

## 现状分析

从 Symbol Index Lifecycle 调查，我们发现：

### ✓ 已完善的部分
1. **存储层**: PostgreSQL 三表设计完整（symbol_nodes, symbol_edges, index_metadata）
2. **索引构建**: Go/Python 双语言支持，调用链完整
3. **图遍历**: InMemoryGraph 支持微秒级 BFS
4. **风险评估**: RiskLevel (LOW/MEDIUM/HIGH/CRITICAL) 完整实现
5. **Layer B**: DiffToSymbols() 能将 diff 映射到修改的符号

### ✗ 关键缺失

| 功能 | 现状 | 影响 |
|-----|------|------|
| Webhook 索引触发 | ✗ 缺失 | MR/PR 到达时索引可能过期 |
| K8s CronJob | ✗ 缺失 | 无定时更新机制 |
| 增量索引 | ✗ 缺失 | 全量扫描浪费资源 |
| 索引陈旧告警 | ⚠️ 仅显示 | 分析结果可能不准确 |

---

## 改进优先级

### Priority 1: Webhook 中自动触发索引更新

**文件**: `internal/webhook/handler.go:87-162`

**改进方案**:
```go
// 现有流程
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    // ... 验证签名，解析事件
    rec, err := h.store.CreateTask(...)  // 创建分析任务 ✓
    if h.cfg.Launch != nil {
        go h.cfg.Launch(rec.ID, event.Diff, event.Description, cacheKey)
    }
    // ...
}

// 改进版本（需要添加）
if h.cfg.Indexer != nil {
    go func() {
        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
        defer cancel()
        _ = h.cfg.Indexer.UpdateIndex(ctx, event.RepoName)  // 新增
    }()
}
```

**实现步骤**:
1. 在 `Config` 中添加 `IndexUpdater` 接口
2. 在 webhook 处理器接收 MR/PR 后触发索引更新
3. 使用背景 goroutine 避免阻塞 webhook 响应

---

### Priority 2: K8s CronJob 定时索引更新

**文件**: `k8s/job.yaml`（需创建 `k8s/cronjob.yaml`）

**改进方案**:
```yaml
apiVersion: batch/v1
kind: CronJob
metadata:
  name: shirakami-index-sync
spec:
  schedule: "0 */4 * * *"  # 每 4 小时
  jobTemplate:
    spec:
      template:
        spec:
          containers:
          - name: index-updater
            image: shirakami:latest
            args: ["index", "update", "--all"]
            volumeMounts:
            - name: repos
              mountPath: /workspace
          volumes:
          - name: repos
            persistentVolumeClaim:
              claimName: shirakami-repos
```

**关键参数**:
- `schedule`: 基于变更频率调整（高频改为 `*/2`，低频改为 `0 0 * * *`）
- `restartPolicy: OnFailure`: 重试机制
- 与 `job.yaml` 共用 PVC

---

### Priority 3: 增量索引实现

**文件**: `internal/index/indexer_go.go`, `internal/index/indexer_python.go`

**改进方案**:

对比模式：
```go
type Indexer interface {
    Index(ctx context.Context, opts IndexOptions) (*IndexResult, error)
}

type IndexOptions struct {
    Mode string // "full" or "incremental"
    SinceSHA string // git SHA for incremental mode
}
```

使用 git diff：
```bash
# 获取变化的文件
git diff --name-only <old-sha>..<new-sha>

# 获取变化的行号范围
git diff -U0 <old-sha>..<new-sha> -- <file>
```

**实现逻辑**:
1. 获取上次索引的 commit（从 `index_metadata`）
2. 执行 `git diff --name-only HEAD~1..HEAD`
3. 仅扫描变化的文件
4. 删除旧节点/边，插入新的

**性能收益**:
- 小改动: 10× 加速
- 大改动: 3-5× 加速

---

### Priority 4: 改进陈旧度检测和告警

**文件**: `cmd/analyze/main.go:139-450`

**改进方案**:

```go
// 现有：只打警告
if metadata.IsStale(currentCommit) {
    log.Warn("index stale", "repo", repo, "indexed_at", metadata.IndexedAt)
}

// 改进：支持 strict mode
if cfg.StrictIndexMode && metadata.IsStale(currentCommit) {
    return fmt.Errorf("index stale, run 'shirakami index update'")
}
```

**添加 CLI 标志**:
```bash
shirakami analyze --config <file> --strict-index
```

**效果**:
- 防止在陈旧索引上运行分析
- 确保结果可靠性

---

## 实现时间表

| 优先级 | 功能 | 工作量 | 目标日期 |
|-------|------|--------|---------|
| P1 | Webhook 触发 | 2-3 天 | Week 1 |
| P2 | K8s CronJob | 1-2 天 | Week 1 |
| P3 | 增量索引 | 4-5 天 | Week 2 |
| P4 | 陈旧度告警 | 1-2 天 | Week 2 |

---

## 详细实现建议

### P1 实现细节

**步骤 1**: 定义索引更新接口
```go
// internal/index/updater.go (新文件)
type Updater interface {
    UpdateIndex(ctx context.Context, repo string, mode string) error
}

type Config struct {
    Updater Updater
    // ...
}
```

**步骤 2**: 在 webhook handler 中集成
```go
// internal/webhook/handler.go
if h.cfg.Updater != nil {
    go func() {
        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
        defer cancel()
        if err := h.cfg.Updater.UpdateIndex(ctx, event.RepoName, "incremental"); err != nil {
            log.Error("index update failed", "repo", event.RepoName, "err", err)
        }
    }()
}
```

**步骤 3**: 在服务器初始化中连接
```go
// cmd/server/main.go
store := storage.NewStore(db)
indexUpdater := NewIndexUpdater(store, workspace)
whConfig := webhook.Config{
    Secret: cfg.WebhookSecret,
    Updater: indexUpdater,
    // ...
}
whHandler := webhook.New(store, whConfig)
```

### P3 增量索引实现细节

**关键数据结构**:
```go
type IncrementalOptions struct {
    LastSHA string        // 上次索引的 SHA
    CurrentSHA string     // 当前 SHA
    ChangedFiles []string // git diff --name-only 结果
}

type DeltaResult struct {
    ToDelete []*SymbolNode // 从已删除文件
    ToAdd    []*SymbolNode // 从新增/修改文件
    ToUpdate []*SymbolNode // 从修改文件
}
```

**执行流程**:
```
1. 获取 changed files: git diff --name-only <old>..<new>
2. 过滤扩展名（.go, .py 等）
3. 扫描仅这些文件
4. 标记旧版本节点为删除
5. 插入新版本节点
```

---

## 监控和指标

建议添加以下指标到监控系统：

```
shirakami_index_last_update_seconds_ago  # 上次更新以来的秒数
shirakami_index_staleness_seconds        # 索引滞后秒数
shirakami_index_build_duration_ms        # 构建耗时
shirakami_index_incremental_speedup      # 增量 vs 全量加速比
shirakami_webhook_index_trigger_count    # webhook 触发次数
```

---

## 验证清单

实现完成后验证：

- [ ] Webhook 到达时自动更新索引（5 分钟内）
- [ ] CronJob 每 4 小时运行一次
- [ ] 增量索引比全量快 3× 以上
- [ ] 分析时检查索引陈旧度
- [ ] 监控告警检测过期索引（>1 小时）

